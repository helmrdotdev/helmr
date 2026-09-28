package session

import (
	"context"
	"encoding/json"
	"uuid"

	"github.com/helmrdotdev/helmr/internal/db"
	"github.com/helmrdotdev/helmr/internal/pgvalue"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
)

// The Computer, Instance, Session and admission secrets are already locked.
// Only reconciled process scopes can receive replacement logical authority.
func reconcileLostExecution(ctx context.Context, tx pgx.Tx, actor db.Session, bindings []db.LockComputerSecretsForAdmissionRow) (db.Session, bool, error) {
	if actor.DispatchHoldReason.String != "recovery_required" || !actor.CurrentRunID.Valid ||
		(actor.Status != "open" && actor.Status != "closing") {
		return actor, false, nil
	}
	q := db.New(tx)
	current, err := q.LockActorInputCurrentRun(ctx, db.LockActorInputCurrentRunParams{EnvironmentID: actor.EnvironmentID, SessionID: actor.ID, RunID: actor.CurrentRunID})
	if err != nil {
		return actor, false, err
	}
	switch current.Status {
	case db.RunStatusFailed, db.RunStatusSystemFailed, db.RunStatusExpired, db.RunStatusCancelled:
	default:
		return actor, true, nil
	}
	ws, err := q.LockActorCloseComputer(ctx, db.LockActorCloseComputerParams{EnvironmentID: actor.EnvironmentID, ComputerID: actor.ComputerID, SessionID: actor.ID})
	if err != nil {
		return actor, false, err
	}
	source, episode := ws.HeadDiskVersionID, ws.RecoveryID
	repairFailure := ws.RecoveryFailure
	reconciled, err := q.SessionExecutionScopesReconciled(ctx, actor.ID)
	if err != nil || !reconciled {
		return actor, true, err
	}
	owned, err := q.OwnedRunScopesReconciled(ctx, current.ID)
	if err != nil || !owned {
		return actor, true, err
	}
	if len(repairFailure) > 0 {
		if actor.CancelRequestedAt.Valid {
			return settleCancelledFailedExecution(ctx, tx, actor, repairFailure)
		}
		now, err := q.GetTaskCompletionTime(ctx)
		if err != nil {
			return actor, false, err
		}
		if err = FailExecution(ctx, q, actor, repairFailure, "", now); err != nil {
			return actor, false, err
		}
		actor, err = q.GetActor(ctx, db.GetActorParams{EnvironmentID: actor.EnvironmentID, ID: actor.ID})
		return actor, false, err
	}
	if current.Status == db.RunStatusFailed {
		// A known terminal failure before Computer loss (for example exhausted
		// initial preparation) cannot be repaired by replaying the Actor.
		if len(current.Failure) == 0 {
			return actor, false, ErrAuthority
		}
		now, err := q.GetTaskCompletionTime(ctx)
		if err != nil {
			return actor, false, err
		}
		if actor.CancelRequestedAt.Valid {
			return settleCancelledFailedExecution(ctx, tx, actor, current.Failure)
		}
		if err = FailExecution(ctx, q, actor, current.Failure, "", now); err != nil {
			return actor, false, err
		}
		actor, err = q.GetActor(ctx, db.GetActorParams{EnvironmentID: actor.EnvironmentID, ID: actor.ID})
		return actor, false, err
	}
	if actor.CancelRequestedAt.Valid {
		return settleCancelledFailedExecution(ctx, tx, actor, current.Failure)
	}
	if ws.Status != db.ComputerStatusActive || ws.DesiredState != db.ComputerDesiredStateActive ||
		ws.DirtyState == db.ComputerDirtyStateCaptureFailed || ws.DirtyState == db.ComputerDirtyStateDirtyStateLost {
		return actor, true, nil
	}
	committed, err := q.SessionRecoveryHeadCommitted(ctx, db.SessionRecoveryHeadCommittedParams{EnvironmentID: actor.EnvironmentID, ComputerID: actor.ComputerID, ID: source})
	if err != nil || !committed {
		return actor, true, err
	}
	// Do not consume the durable wakeup before credentials permit continuation.
	if !actor.CancelRequestedAt.Valid && !bindingsCanAdmit(actor, bindings) {
		return actor, true, nil
	}
	if !actor.CancelRequestedAt.Valid && actor.ConsecutiveExecutionLosses >= 7 {
		reason := "execution_loss_limit"
		failure, _ := json.Marshal(map[string]any{"code": reason, "message": "Execution could not be resumed within the recovery limit", "details": map[string]any{}})
		now, err := q.GetTaskCompletionTime(ctx)
		if err == nil {
			err = FailExecution(ctx, q, actor, failure, "", now)
		}
		if err != nil {
			return actor, false, err
		}
		actor, err = q.GetActor(ctx, db.GetActorParams{EnvironmentID: actor.EnvironmentID, ID: actor.ID})
		return actor, false, err
	}
	var sequence pgtype.Int8
	// Run-level interruption is also valid between Turns. Its immutable event
	// survives replacement of the hold by the infrastructure-loss fence.
	var interrupted bool
	if err = tx.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM session_events WHERE session_id=$1
	 AND producer_run_id=$2 AND run_generation=$3 AND kind='session.held'
	 AND data->>'reason'='interrupt_requested')`, actor.ID, current.ID, actor.RunGeneration).Scan(&interrupted); err != nil {
		return actor, false, err
	}
	interrupted = interrupted || actor.CancelRequestedAt.Valid
	if actor.ActiveTurnID.Valid {
		turn, err := q.LockSessionTurnInput(ctx, db.LockSessionTurnInputParams{EnvironmentID: actor.EnvironmentID, SessionID: actor.ID, ID: actor.ActiveTurnID})
		if err != nil {
			return actor, false, err
		}
		if turn.Status != "running" || turn.Sequence != actor.CommittedInputSequence+1 {
			return actor, false, ErrAuthority
		}
		interrupted = interrupted || turn.InterruptRequestedAt.Valid
		if err = finishUnsettledMessages(ctx, q, actor, turn.ID, "execution_lost"); err != nil {
			return actor, false, err
		}
		body, _ := json.Marshal(map[string]any{"error": map[string]string{"code": "execution_lost"}, "recovery_id": pgvalue.UUIDString(episode)})
		event, err := appendLifecycleEvent(ctx, q, actor, turn.ID, pgtype.UUID{}, "turn.failed", body, source)
		if err != nil {
			return actor, false, err
		}
		if _, err = q.SettleHeldSessionTurn(ctx, db.SettleHeldSessionTurnParams{EnvironmentID: actor.EnvironmentID, SessionID: actor.ID, TurnID: turn.ID, Status: "failed", EventID: event.ID}); err != nil {
			return actor, false, err
		}
		sequence = pgtype.Int8{Int64: turn.Sequence, Valid: true}
	}
	body, _ := json.Marshal(map[string]any{"recovery_id": pgvalue.UUIDString(episode), "version_id": pgvalue.UUIDString(source), "reason": "execution_lost"})
	if _, err = appendLifecycleEvent(ctx, q, actor, pgtype.UUID{}, pgtype.UUID{}, "session.execution_lost", body, source); err != nil {
		return actor, false, err
	}
	var hold pgtype.UUID
	var reason pgtype.Text
	if interrupted {
		hold = pgvalue.UUID(uuid.NewV7())
		reason = pgvalue.Text("interrupted")
	}
	_, err = tx.Exec(ctx, `UPDATE sessions SET current_run_id=NULL,active_turn_id=NULL,run_generation=run_generation+1,
		consecutive_execution_losses=consecutive_execution_losses+CASE WHEN cancel_requested_at IS NULL THEN 1 ELSE 0 END,committed_input_sequence=coalesce($3,committed_input_sequence),
		dispatch_hold_id=$4,dispatch_hold_reason=$5,dispatch_hold_run_id=CASE WHEN $4::uuid IS NULL THEN NULL ELSE dispatch_hold_run_id END,
		dispatch_hold_attempt_number=CASE WHEN $4::uuid IS NULL THEN NULL ELSE dispatch_hold_attempt_number END,
		dispatch_hold_run_generation=CASE WHEN $4::uuid IS NULL THEN NULL ELSE dispatch_hold_run_generation END,
		revision=revision+1,updated_at=now() WHERE id=$1 AND current_run_id=$2`, actor.ID, actor.CurrentRunID, sequence, hold, reason)
	if err != nil {
		return actor, false, err
	}
	actor, err = q.GetActor(ctx, db.GetActorParams{EnvironmentID: actor.EnvironmentID, ID: actor.ID})
	if err != nil {
		return actor, false, err
	}
	if actor.Status == "open" && !interrupted && actor.CommittedInputSequence < actor.NextInputSequence-1 {
		if _, err = CreateContinuation(ctx, q, actor, db.Computer{ID: actor.ComputerID}, bindings); err != nil {
			return actor, false, err
		}
	}
	return actor, false, nil
}

// Stopping a Session retires only its execution. Peer scopes and their live
// filesystem remain attached to the Computer.
func reconcileStoppedExecution(ctx context.Context, tx pgx.Tx, actor db.Session) (db.Session, bool, error) {
	if actor.DispatchHoldReason.String != "interrupt_requested" || !actor.CurrentRunID.Valid || (actor.Status != "open" && actor.Status != "closing") {
		return actor, false, nil
	}
	q := db.New(tx)
	current, err := q.LockActorInputCurrentRun(ctx, db.LockActorInputCurrentRunParams{EnvironmentID: actor.EnvironmentID, SessionID: actor.ID, RunID: actor.CurrentRunID})
	if err != nil {
		return actor, false, err
	}
	if current.Status != db.RunStatusCancelled || current.CurrentRunLeaseID.Valid {
		return actor, true, nil
	}
	ws, err := q.LockActorCloseComputer(ctx, db.LockActorCloseComputerParams{EnvironmentID: actor.EnvironmentID, SessionID: actor.ID, ComputerID: actor.ComputerID})
	if err != nil {
		return actor, false, err
	}
	if actor.ActiveTurnID.Valid && !ws.HeadDiskVersionID.Valid {
		return actor, true, nil
	}
	excluded, err := q.SessionExecutionScopesReconciled(ctx, actor.ID)
	if err != nil || !excluded {
		return actor, true, err
	}
	owned, err := q.OwnedRunScopesReconciled(ctx, current.ID)
	if err != nil || !owned {
		return actor, true, err
	}
	var versionID pgtype.UUID
	if ws.HeadDiskVersionID.Valid {
		committed, err := q.SessionRecoveryHeadCommitted(ctx, db.SessionRecoveryHeadCommittedParams{EnvironmentID: actor.EnvironmentID, ComputerID: actor.ComputerID, ID: ws.HeadDiskVersionID})
		if err != nil {
			return actor, false, err
		}
		if committed {
			versionID = ws.HeadDiskVersionID
		}
	}
	if actor.ActiveTurnID.Valid && !versionID.Valid {
		return actor, true, nil
	}
	if err = CompleteInterruption(ctx, q, actor, versionID, ""); err != nil {
		return actor, false, err
	}
	actor, err = q.GetActor(ctx, db.GetActorParams{EnvironmentID: actor.EnvironmentID, ID: actor.ID})
	return actor, false, err
}

// Cancellation still closes the Session when a known execution failure won first.
// Preserve that failure on the active Turn, then drain only cancelled inputs.
func settleCancelledFailedExecution(ctx context.Context, tx pgx.Tx, actor db.Session, failure json.RawMessage) (db.Session, bool, error) {
	q := db.New(tx)
	sequence := actor.CommittedInputSequence
	if actor.ActiveTurnID.Valid {
		turn, err := q.LockSessionTurnInput(ctx, db.LockSessionTurnInputParams{EnvironmentID: actor.EnvironmentID, SessionID: actor.ID, ID: actor.ActiveTurnID})
		if err != nil {
			return actor, false, err
		}
		if turn.Status != "running" || turn.Sequence != sequence+1 {
			return actor, false, ErrAuthority
		}
		if err = finishUnsettledMessages(ctx, q, actor, turn.ID, "session_cancelled"); err != nil {
			return actor, false, err
		}
		body, _ := json.Marshal(map[string]any{"error": failure})
		event, err := appendLifecycleEvent(ctx, q, actor, turn.ID, pgtype.UUID{}, "turn.failed", body, pgtype.UUID{})
		if err != nil {
			return actor, false, err
		}
		if _, err = q.SettleHeldSessionTurn(ctx, db.SettleHeldSessionTurnParams{EnvironmentID: actor.EnvironmentID, SessionID: actor.ID, TurnID: turn.ID, Status: "failed", EventID: event.ID}); err != nil {
			return actor, false, err
		}
		sequence = turn.Sequence
	}
	if _, err := tx.Exec(ctx, `UPDATE sessions SET current_run_id=NULL,active_turn_id=NULL,dispatch_hold_id=NULL,dispatch_hold_reason=NULL,dispatch_hold_run_id=NULL,dispatch_hold_attempt_number=NULL,dispatch_hold_run_generation=NULL,committed_input_sequence=$2,run_generation=run_generation+1,revision=revision+1,updated_at=now() WHERE id=$1 AND cancel_requested_at IS NOT NULL`, actor.ID, sequence); err != nil {
		return actor, false, err
	}
	actor, err := q.GetActor(ctx, db.GetActorParams{EnvironmentID: actor.EnvironmentID, ID: actor.ID})
	return actor, false, err
}
