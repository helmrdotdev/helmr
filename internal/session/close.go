package session

import (
	"context"
	"errors"
	"fmt"
	"github.com/jackc/pgx/v5/pgtype"

	"github.com/helmrdotdev/helmr/internal/db"
	"github.com/jackc/pgx/v5"
)

// ReconcileClose applies the repairable portion of an already-authoritative
// close direction. Callers must lock the complete Computer Secret set before
// the Actor and pass both locked facts here.
func ReconcileClose(
	ctx context.Context,
	store db.Querier,
	actor db.Session,
	bindings []db.LockComputerSecretsForAdmissionRow,
) (db.Session, bool, error) {
	if actor.CancelRequestedAt.Valid && actor.Status == "closing" {
		var waiting bool
		var err error
		actor, waiting, err = reconcileCancellation(ctx, store, actor)
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
		return reconcileCurrentRunClose(ctx, store, actor)
	}
	computer, err := store.LockActorCloseComputer(ctx, db.LockActorCloseComputerParams{
		EnvironmentID: actor.EnvironmentID,
		ComputerID:    actor.ComputerID,
		SessionID:     actor.ID,
	})
	if errors.Is(err, pgx.ErrNoRows) {
		return actor, true, nil
	}
	if err != nil {
		return db.Session{}, false, err
	}
	activity, err := store.GetActorCloseComputerActivity(ctx, actor.ID)
	if err != nil {
		return db.Session{}, false, err
	}
	if actor.CommittedInputSequence < actor.CloseSequence.Int64 {
		if !bindingsCanAdmit(actor, bindings) {
			return actor, true, nil
		}
		if _, err := CreateContinuation(
			ctx,
			store,
			actor,
			db.Computer{ID: computer.ID},
			bindings,
		); err != nil {
			if errors.Is(err, pgx.ErrNoRows) {
				return actor, true, nil
			}
			return db.Session{}, false, fmt.Errorf("create actor close continuation: %w", err)
		}
		updated, err := store.GetActor(ctx, db.GetActorParams{
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
	closed, err := store.CompleteIdleActorClose(ctx, db.CompleteIdleActorCloseParams{
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
	store db.Querier,
	actor db.Session,
) (db.Session, bool, error) {
	run, err := store.LockActorInputCurrentRun(ctx, db.LockActorInputCurrentRunParams{
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
	computer, err := store.LockActorCloseComputer(ctx, db.LockActorCloseComputerParams{
		EnvironmentID: actor.EnvironmentID,
		ComputerID:    actor.ComputerID,
		SessionID:     actor.ID,
	})
	if errors.Is(err, pgx.ErrNoRows) {
		return actor, true, nil
	}
	if err != nil {
		return db.Session{}, false, err
	}
	attempt, err := store.LockRunLeaseClaimAttempt(ctx, db.LockRunLeaseClaimAttemptParams{
		RunID:      run.ID,
		Number:     run.CurrentAttemptNumber,
		ComputerID: computer.ID,
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
	wait, err := store.GetPendingActorInputRunWait(ctx, db.GetPendingActorInputRunWaitParams{
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
	if _, err := FailWait(ctx, store, wait, "session_closed"); err != nil {
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
func reconcileCancellation(ctx context.Context, q db.Querier, actor db.Session) (db.Session, bool, error) {
	if actor.ActiveTurnID.Valid {
		return actor, true, nil
	}
	if actor.CurrentRunID.Valid {
		current, err := q.LockActorInputCurrentRun(ctx, db.LockActorInputCurrentRunParams{EnvironmentID: actor.EnvironmentID, SessionID: actor.ID, RunID: actor.CurrentRunID})
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
		computer, err := q.LockActorCloseComputer(ctx, db.LockActorCloseComputerParams{EnvironmentID: actor.EnvironmentID, SessionID: actor.ID, ComputerID: actor.ComputerID})
		if err != nil {
			return actor, false, err
		}
		excluded, err := q.SessionExecutionScopesReconciled(ctx, actor.ID)
		if err != nil {
			return actor, false, err
		}
		if !excluded {
			return actor, true, nil
		}
		if err = CompleteInterruption(ctx, q, actor, computer.HeadDiskVersionID, ""); err != nil {
			return actor, false, err
		}
		actor, err = q.GetActor(ctx, db.GetActorParams{EnvironmentID: actor.EnvironmentID, ID: actor.ID})
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
