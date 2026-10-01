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
	actor db.Session,
	bindings []db.LockComputerSecretsForAdmissionRow,
) (db.Session, bool, error) {
	store := db.New(tx)
	if actor.CancelRequestedAt.Valid && actor.Status == "closing" {
		var waiting bool
		var err error
		actor, waiting, err = reconcileCancellation(ctx, tx, actor)
		if err != nil || waiting {
			return actor, waiting, err
		}
	}
	if actor.Status != "closing" || !actor.CloseSequence.Valid || actor.ActiveTurnID.Valid {
		return actor, false, nil
	}
	if actor.DispatchHoldID.Valid && (actor.CurrentRunID.Valid || actor.CommittedInputSequence < actor.CloseSequence.Int64 ||
		actor.DispatchHoldReason.String != "interrupted") {
		return actor, false, nil
	}
	if actor.CurrentRunID.Valid {
		return reconcileCurrentRunClose(ctx, tx, actor)
	}
	locked, err := computer.LockSessionComputer(ctx, tx, sessionComputerRef(actor))
	if errors.Is(err, pgx.ErrNoRows) {
		return actor, true, nil
	}
	if err != nil {
		return db.Session{}, false, err
	}
	sessionComputer := locked.Computer()
	activity, err := store.GetSessionCloseComputerActivity(ctx, actor.ID)
	if err != nil {
		return db.Session{}, false, err
	}
	if actor.CommittedInputSequence < actor.CloseSequence.Int64 {
		if !bindingsCanAdmit(actor, bindings) {
			return actor, true, nil
		}
		if _, err := CreateContinuation(
			ctx,
			tx,
			actor,
			db.Computer{ID: sessionComputer.ID},
			bindings,
		); err != nil {
			if errors.Is(err, pgx.ErrNoRows) {
				return actor, true, nil
			}
			return db.Session{}, false, fmt.Errorf("create actor close continuation: %w", err)
		}
		updated, err := store.GetSession(ctx, db.GetSessionParams{
			EnvironmentID: actor.EnvironmentID,
			ID:            actor.ID,
		})
		return updated, false, err
	}
	if activity.HasActiveLease || activity.HasActiveChild {
		return actor, true, nil
	}
	reconciled, err := store.SessionExecutionScopesReconciled(ctx, actor.ID)
	if err != nil {
		return db.Session{}, false, err
	}
	if !reconciled {
		return actor, true, nil
	}
	now, err := store.GetRunLeaseRenewalTime(ctx)
	if err != nil || !now.Valid {
		if err == nil {
			err = ErrAuthority
		}
		return db.Session{}, false, fmt.Errorf("load actor close time: %w", err)
	}
	if actor.DispatchHoldID.Valid {
		actor, err = store.ClearSessionDispatchHold(ctx, db.ClearSessionDispatchHoldParams{EnvironmentID: actor.EnvironmentID, ID: actor.ID, DispatchHoldID: actor.DispatchHoldID})
		if err != nil {
			return db.Session{}, false, err
		}
	}
	closed, err := store.CompleteIdleSessionClose(ctx, db.CompleteIdleSessionCloseParams{
		ClosedAt:      now,
		EnvironmentID: actor.EnvironmentID,
		SessionID:     actor.ID,
		ComputerID:    actor.ComputerID,
	})
	if err != nil {
		return db.Session{}, false, fmt.Errorf("complete idle actor close: %w", err)
	}
	_, err = appendLifecycleEvent(ctx, store, closed, pgtype.UUID{}, pgtype.UUID{}, "session.closed", closeEventData(closed), pgtype.UUID{})
	return closed, false, err
}

func reconcileCurrentRunClose(
	ctx context.Context,
	tx pgx.Tx,
	actor db.Session,
) (db.Session, bool, error) {
	store := db.New(tx)
	run, err := store.LockSessionInputCurrentRun(ctx, db.LockSessionInputCurrentRunParams{
		EnvironmentID: actor.EnvironmentID,
		RunID:         actor.CurrentRunID,
		SessionID:     actor.ID,
	})
	if errors.Is(err, pgx.ErrNoRows) {
		return actor, true, nil
	}
	if err != nil {
		return db.Session{}, false, err
	}
	locked, err := computer.LockSessionComputer(ctx, tx, sessionComputerRef(actor))
	if errors.Is(err, pgx.ErrNoRows) {
		return actor, true, nil
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
		return actor, true, nil
	}
	if err != nil {
		return db.Session{}, false, err
	}
	if attempt.TerminalAt.Valid || actor.CommittedInputSequence < actor.CloseSequence.Int64 {
		return actor, false, nil
	}
	wait, err := store.GetPendingSessionInputRunWait(ctx, db.GetPendingSessionInputRunWaitParams{
		EnvironmentID:      actor.EnvironmentID,
		RunID:              run.ID,
		AttemptNumber:      run.CurrentAttemptNumber,
		SessionID:          actor.ID,
		AfterInputSequence: actor.CloseSequence,
	})
	if errors.Is(err, pgx.ErrNoRows) {
		return actor, false, nil
	}
	if err != nil {
		return db.Session{}, false, err
	}
	if _, err := FailWait(ctx, tx, wait, "session_closed"); err != nil {
		return db.Session{}, false, fmt.Errorf("complete actor close input wait: %w", err)
	}
	return actor, false, nil
}

func bindingsCanAdmit(
	actor db.Session,
	bindings []db.LockComputerSecretsForAdmissionRow,
) bool {
	for _, binding := range bindings {
		if binding.ComputerID != actor.ComputerID ||
			binding.EnvironmentID != actor.EnvironmentID ||
			binding.SecretStatus != "active" ||
			!binding.CurrentVersionID.Valid {
			return false
		}
	}
	return true
}

func closeEventData(actor db.Session) []byte {
	if actor.CancelRequestedAt.Valid {
		return []byte(`{"reason":"cancelled"}`)
	}
	return []byte(`{}`)
}

// Cancellation never runs code to consume the cancelled suffix. Keep the
// durable reconciler alive while the existing process-stop path is pending.
func reconcileCancellation(ctx context.Context, tx pgx.Tx, actor db.Session) (db.Session, bool, error) {
	q := db.New(tx)
	if actor.ActiveTurnID.Valid {
		return actor, true, nil
	}
	if actor.CurrentRunID.Valid {
		current, err := q.LockSessionInputCurrentRun(ctx, db.LockSessionInputCurrentRunParams{EnvironmentID: actor.EnvironmentID, SessionID: actor.ID, RunID: actor.CurrentRunID})
		if err != nil {
			return actor, false, err
		}
		attempt, err := q.LockRunLeaseClaimAttempt(ctx, db.LockRunLeaseClaimAttemptParams{RunID: current.ID, Number: current.CurrentAttemptNumber, ComputerID: actor.ComputerID})
		if err != nil {
			return actor, false, err
		}
		// A never-entered execution settles only after its process scopes are reconciled.
		if !attempt.TerminalAt.Valid || attempt.EntrypointEnteredAt.Valid || actor.DispatchHoldReason.String != "interrupt_requested" {
			return actor, true, nil
		}
		locked, err := computer.LockSessionComputer(ctx, tx, sessionComputerRef(actor))
		if err != nil {
			return actor, false, err
		}
		sessionComputer := locked.Computer()
		excluded, err := q.SessionExecutionScopesReconciled(ctx, actor.ID)
		if err != nil {
			return actor, false, err
		}
		if !excluded {
			return actor, true, nil
		}
		if err = CompleteInterruption(ctx, tx, actor, sessionComputer.HeadDiskVersionID, ""); err != nil {
			return actor, false, err
		}
		actor, err = q.GetSession(ctx, db.GetSessionParams{EnvironmentID: actor.EnvironmentID, ID: actor.ID})
		if err != nil {
			return actor, false, err
		}
	}
	if actor.DispatchHoldID.Valid && actor.DispatchHoldReason.String != "interrupted" {
		return actor, true, nil
	}
	actor, err := q.AdvanceCancelledSessionInputs(ctx, db.AdvanceCancelledSessionInputsParams{EnvironmentID: actor.EnvironmentID, ID: actor.ID})
	return actor, false, err
}
