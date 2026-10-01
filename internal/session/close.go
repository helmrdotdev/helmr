package session

import (
	"context"
	"errors"
	"fmt"
	"github.com/jackc/pgx/v5/pgtype"

	"github.com/helmrdotdev/helmr/internal/computer"
	"github.com/helmrdotdev/helmr/internal/db"
	"github.com/jackc/pgx/v5"
)

// ReconcileClose applies the repairable portion of an already-authoritative
// close direction. Callers must lock the complete Computer Secret set before
// the Actor and pass both locked facts here.
func ReconcileClose(
	ctx context.Context,
	tx pgx.Tx,
	session db.Session,
	bindings []db.LockComputerSecretsForAdmissionRow,
) (db.Session, bool, error) {
	store := db.New(tx)
	if session.CancelRequestedAt.Valid && session.Status == "closing" {
		var waiting bool
		var err error
		session, waiting, err = reconcileCancellation(ctx, tx, session)
		if err != nil || waiting {
			return session, waiting, err
		}
	}
	if session.Status != "closing" || !session.CloseSequence.Valid || session.ActiveTurnID.Valid {
		return session, false, nil
	}
	if session.DispatchHoldID.Valid && (session.CurrentRunID.Valid || session.CommittedInputSequence < session.CloseSequence.Int64 ||
		session.DispatchHoldReason.String != "interrupted") {
		return session, false, nil
	}
	if session.CurrentRunID.Valid {
		return reconcileCurrentRunClose(ctx, tx, session)
	}
	locked, err := computer.LockSessionComputer(ctx, tx, sessionComputerRef(session))
	if errors.Is(err, pgx.ErrNoRows) {
		return session, true, nil
	}
	if err != nil {
		return db.Session{}, false, err
	}
	sessionComputer := locked.Computer()
	activity, err := store.GetSessionCloseComputerActivity(ctx, session.ID)
	if err != nil {
		return db.Session{}, false, err
	}
	if session.CommittedInputSequence < session.CloseSequence.Int64 {
		if !bindingsCanAdmit(session, bindings) {
			return session, true, nil
		}
		if _, err := CreateContinuation(
			ctx,
			tx,
			session,
			db.Computer{ID: sessionComputer.ID},
			bindings,
		); err != nil {
			if errors.Is(err, pgx.ErrNoRows) {
				return session, true, nil
			}
			return db.Session{}, false, fmt.Errorf("create session close continuation: %w", err)
		}
		updated, err := store.GetSession(ctx, db.GetSessionParams{
			EnvironmentID: session.EnvironmentID,
			ID:            session.ID,
		})
		return updated, false, err
	}
	if activity.HasActiveLease || activity.HasActiveChild {
		return session, true, nil
	}
	reconciled, err := store.SessionExecutionScopesReconciled(ctx, session.ID)
	if err != nil {
		return db.Session{}, false, err
	}
	if !reconciled {
		return session, true, nil
	}
	now, err := store.GetRunLeaseRenewalTime(ctx)
	if err != nil || !now.Valid {
		if err == nil {
			err = ErrAuthority
		}
		return db.Session{}, false, fmt.Errorf("load session close time: %w", err)
	}
	if session.DispatchHoldID.Valid {
		session, err = store.ClearSessionDispatchHold(ctx, db.ClearSessionDispatchHoldParams{EnvironmentID: session.EnvironmentID, ID: session.ID, DispatchHoldID: session.DispatchHoldID})
		if err != nil {
			return db.Session{}, false, err
		}
	}
	closed, err := store.CompleteIdleSessionClose(ctx, db.CompleteIdleSessionCloseParams{
		ClosedAt:      now,
		EnvironmentID: session.EnvironmentID,
		SessionID:     session.ID,
		ComputerID:    session.ComputerID,
	})
	if err != nil {
		return db.Session{}, false, fmt.Errorf("complete idle session close: %w", err)
	}
	_, err = appendLifecycleEvent(ctx, store, closed, pgtype.UUID{}, pgtype.UUID{}, "session.closed", closeEventData(closed), pgtype.UUID{})
	return closed, false, err
}

func reconcileCurrentRunClose(
	ctx context.Context,
	tx pgx.Tx,
	session db.Session,
) (db.Session, bool, error) {
	store := db.New(tx)
	run, err := store.LockSessionInputCurrentRun(ctx, db.LockSessionInputCurrentRunParams{
		EnvironmentID: session.EnvironmentID,
		RunID:         session.CurrentRunID,
		SessionID:     session.ID,
	})
	if errors.Is(err, pgx.ErrNoRows) {
		return session, true, nil
	}
	if err != nil {
		return db.Session{}, false, err
	}
	locked, err := computer.LockSessionComputer(ctx, tx, sessionComputerRef(session))
	if errors.Is(err, pgx.ErrNoRows) {
		return session, true, nil
	}
	if err != nil {
		return db.Session{}, false, err
	}
	sessionComputer := locked.Computer()
	attempt, err := store.LockRunLeaseClaimAttempt(ctx, db.LockRunLeaseClaimAttemptParams{
		RunID:      run.ID,
		Number:     run.CurrentAttemptNumber,
		ComputerID: sessionComputer.ID,
	})
	if errors.Is(err, pgx.ErrNoRows) {
		return session, true, nil
	}
	if err != nil {
		return db.Session{}, false, err
	}
	if attempt.TerminalAt.Valid || session.CommittedInputSequence < session.CloseSequence.Int64 {
		return session, false, nil
	}
	wait, err := store.GetPendingSessionInputRunWait(ctx, db.GetPendingSessionInputRunWaitParams{
		EnvironmentID:      session.EnvironmentID,
		RunID:              run.ID,
		AttemptNumber:      run.CurrentAttemptNumber,
		SessionID:          session.ID,
		AfterInputSequence: session.CloseSequence,
	})
	if errors.Is(err, pgx.ErrNoRows) {
		return session, false, nil
	}
	if err != nil {
		return db.Session{}, false, err
	}
	if _, err := FailWait(ctx, tx, wait, "session_closed"); err != nil {
		return db.Session{}, false, fmt.Errorf("complete session close input wait: %w", err)
	}
	return session, false, nil
}

func bindingsCanAdmit(
	session db.Session,
	bindings []db.LockComputerSecretsForAdmissionRow,
) bool {
	for _, binding := range bindings {
		if binding.ComputerID != session.ComputerID ||
			binding.EnvironmentID != session.EnvironmentID ||
			binding.SecretStatus != "active" ||
			!binding.CurrentVersionID.Valid {
			return false
		}
	}
	return true
}

func closeEventData(session db.Session) []byte {
	if session.CancelRequestedAt.Valid {
		return []byte(`{"reason":"cancelled"}`)
	}
	return []byte(`{}`)
}

// Cancellation never runs code to consume the cancelled suffix. Keep the
// durable reconciler alive while the existing process-stop path is pending.
func reconcileCancellation(ctx context.Context, tx pgx.Tx, session db.Session) (db.Session, bool, error) {
	q := db.New(tx)
	if session.ActiveTurnID.Valid {
		return session, true, nil
	}
	if session.CurrentRunID.Valid {
		current, err := q.LockSessionInputCurrentRun(ctx, db.LockSessionInputCurrentRunParams{EnvironmentID: session.EnvironmentID, SessionID: session.ID, RunID: session.CurrentRunID})
		if err != nil {
			return session, false, err
		}
		attempt, err := q.LockRunLeaseClaimAttempt(ctx, db.LockRunLeaseClaimAttemptParams{RunID: current.ID, Number: current.CurrentAttemptNumber, ComputerID: session.ComputerID})
		if err != nil {
			return session, false, err
		}
		// A never-entered execution settles only after its process scopes are reconciled.
		if !attempt.TerminalAt.Valid || attempt.EntrypointEnteredAt.Valid || session.DispatchHoldReason.String != "interrupt_requested" {
			return session, true, nil
		}
		locked, err := computer.LockSessionComputer(ctx, tx, sessionComputerRef(session))
		if err != nil {
			return session, false, err
		}
		sessionComputer := locked.Computer()
		excluded, err := q.SessionExecutionScopesReconciled(ctx, session.ID)
		if err != nil {
			return session, false, err
		}
		if !excluded {
			return session, true, nil
		}
		if err = CompleteInterruption(ctx, tx, session, sessionComputer.HeadDiskVersionID, ""); err != nil {
			return session, false, err
		}
		session, err = q.GetSession(ctx, db.GetSessionParams{EnvironmentID: session.EnvironmentID, ID: session.ID})
		if err != nil {
			return session, false, err
		}
	}
	if session.DispatchHoldID.Valid && session.DispatchHoldReason.String != "interrupted" {
		return session, true, nil
	}
	session, err := q.AdvanceCancelledSessionInputs(ctx, db.AdvanceCancelledSessionInputsParams{EnvironmentID: session.EnvironmentID, ID: session.ID})
	return session, false, err
}
