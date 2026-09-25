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

// The Session and admission secrets are already locked. A terminal execution and
// physical writer exclusion are required before replacing its logical authority.
func reconcileLostExecution(ctx context.Context, tx pgx.Tx, actor db.Session, bindings []db.LockWorkspaceSecretsForAdmissionRow) (db.Session, bool, error) {
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
	ws, err := q.LockActorCloseWorkspace(ctx, db.LockActorCloseWorkspaceParams{EnvironmentID: actor.EnvironmentID, WorkspaceID: actor.WorkspaceID, SessionID: actor.ID})
	if err != nil {
		return actor, false, err
	}
	var source, episode pgtype.UUID
	var attempts int
	var repairFailure []byte
	var excluded bool
	err = tx.QueryRow(ctx, `SELECT recovery_version_id,recovery_id,recovery_preparation_count,recovery_failure,
		NOT EXISTS(SELECT 1 FROM workspace_leases WHERE workspace_id=c.id AND status IN ('active','releasing'))
		AND NOT EXISTS(SELECT 1 FROM workspace_processes WHERE workspace_id=c.id AND status IN ('pending','starting','running','exit_requested'))
		AND NOT EXISTS(SELECT 1 FROM runtime_instances WHERE workspace_id=c.id AND reclaimed_at IS NULL)
		FROM computers c WHERE id=$1`, actor.WorkspaceID).Scan(&source, &episode, &attempts, &repairFailure, &excluded)
	if err != nil || !excluded {
		return actor, true, err
	}
	ownedExcluded, err := q.SessionOwnedExecutionsExcluded(ctx, current.ID)
	if err != nil || !ownedExcluded {
		return actor, true, err
	}
	// The physical Runtime has been reclaimed. Its old mount cannot remain an
	// active logical writer and prevent the replacement from mounting.
	if _, err = tx.Exec(ctx, `UPDATE workspace_mounts m SET status='failed',failed_at=now(),terminal_at=now(),
	 terminal_reason_code='execution_lost',updated_at=now() FROM runtime_instances r
	 WHERE m.workspace_id=$1 AND m.runtime_instance_id=r.id AND r.reclaimed_at IS NOT NULL
	 AND m.status IN ('mounting','mounted','unmounting')`, actor.WorkspaceID); err != nil {
		return actor, false, err
	}
	if len(repairFailure) == 0 && attempts >= 8 {
		repairFailure = []byte(`{"code":"computer_recovery_exhausted","message":"Computer preparation limit reached","details":{}}`)
		if _, err = tx.Exec(ctx, `UPDATE computers SET recovery_failure=$2,status='recovery_required',desired_state='stopped',dirty_state='dirty_state_lost',revision=revision+1,updated_at=now() WHERE id=$1`, actor.WorkspaceID, repairFailure); err != nil {
			return actor, false, err
		}
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
		if _, err = q.ReleaseActorWorkspaceOwner(ctx, db.ReleaseActorWorkspaceOwnerParams{CompletedAt: now, ID: ws.ID, EnvironmentID: actor.EnvironmentID, SessionID: actor.ID, OwnershipGeneration: ws.OwnershipGeneration, WriterGeneration: ws.WriterGeneration}); err != nil {
			return actor, false, err
		}
		actor, err = q.GetActor(ctx, db.GetActorParams{EnvironmentID: actor.EnvironmentID, ID: actor.ID})
		return actor, false, err
	}
	if !episode.Valid {
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
		if _, err = q.ReleaseActorWorkspaceOwner(ctx, db.ReleaseActorWorkspaceOwnerParams{CompletedAt: now, ID: ws.ID, EnvironmentID: actor.EnvironmentID, SessionID: actor.ID, OwnershipGeneration: ws.OwnershipGeneration, WriterGeneration: ws.WriterGeneration}); err != nil {
			return actor, false, err
		}
		actor, err = q.GetActor(ctx, db.GetActorParams{EnvironmentID: actor.EnvironmentID, ID: actor.ID})
		return actor, false, err
	}
	if source != ws.HeadVersionID {
		return actor, true, nil
	}
	committed, err := q.SessionRecoveryHeadCommitted(ctx, db.SessionRecoveryHeadCommittedParams{EnvironmentID: actor.EnvironmentID, ComputerID: actor.WorkspaceID, ID: source})
	if err != nil || !committed {
		return actor, true, err
	}
	// Do not consume the durable wakeup before credentials permit continuation.
	if !actor.CancelRequestedAt.Valid && !bindingsCanAdmit(actor, bindings) {
		return actor, true, nil
	}
	if !actor.CancelRequestedAt.Valid && (attempts >= 8 || actor.ConsecutiveExecutionLosses >= 7) {
		reason := "execution_loss_limit"
		if attempts >= 8 {
			reason = "computer_recovery_exhausted"
		}
		failure, _ := json.Marshal(map[string]any{"code": reason, "message": "Execution could not be resumed within the recovery limit", "details": map[string]any{}})
		now, err := q.GetTaskCompletionTime(ctx)
		if err == nil {
			err = FailExecution(ctx, q, actor, failure, "", now)
		}
		if err != nil {
			return actor, false, err
		}
		// The bounded Session bootstrap loop must not poison the Computer.
		// A Computer whose own budget is exhausted remains unavailable.
		if attempts < 8 {
			_, err = tx.Exec(ctx, `UPDATE computers SET status='active',desired_state='active',dirty_state='clean',
			 owner_session_id=NULL,ownership_generation=ownership_generation+1,revision=revision+1,updated_at=now()
			 WHERE id=$1 AND recovery_id=$2 AND owner_session_id=$3`, actor.WorkspaceID, episode, actor.ID)
			if err != nil {
				return actor, false, err
			}
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
	// Active here admits physical preparation, not customer execution; the
	// episode stays pending until the exact Runtime is ready.
	if _, err = tx.Exec(ctx, `UPDATE computers SET status='active',desired_state='active',dirty_state='clean',revision=revision+1,updated_at=now()
		WHERE id=$1 AND recovery_id=$2 AND status='recovery_required'`, actor.WorkspaceID, episode); err != nil {
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
		if _, err = CreateContinuation(ctx, q, actor, db.LockActorInputWorkspaceRow{ID: actor.WorkspaceID}, bindings); err != nil {
			return actor, false, err
		}
	}
	return actor, false, nil
}

// A parked or never-entered execution can be stopped without running customer
// code again. Cancellation retires its continuation; physical exclusion is still
// required. Only the published head is retained, not unpublished process state.
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
	ws, err := q.LockActorCloseWorkspace(ctx, db.LockActorCloseWorkspaceParams{EnvironmentID: actor.EnvironmentID, SessionID: actor.ID, WorkspaceID: actor.WorkspaceID})
	if err != nil {
		return actor, false, err
	}
	if ws.Status != db.WorkspaceStatusActive || ws.DirtyState != db.WorkspaceDirtyStateClean || !ws.HeadVersionID.Valid {
		return actor, true, nil
	}
	excluded, err := q.SessionWriterExcluded(ctx, actor.WorkspaceID)
	if err != nil || !excluded.Valid || !excluded.Bool {
		return actor, true, err
	}
	owned, err := q.SessionOwnedExecutionsExcluded(ctx, current.ID)
	if err != nil || !owned {
		return actor, true, err
	}
	committed, err := q.SessionRecoveryHeadCommitted(ctx, db.SessionRecoveryHeadCommittedParams{EnvironmentID: actor.EnvironmentID, ComputerID: actor.WorkspaceID, ID: ws.HeadVersionID})
	if err != nil || !committed {
		return actor, true, err
	}
	if _, err = tx.Exec(ctx, `UPDATE workspace_mounts SET status='failed',failed_at=now(),terminal_at=now(),terminal_reason_code='execution_interrupted',updated_at=now() WHERE workspace_id=$1 AND status IN ('mounting','mounted','unmounting')`, actor.WorkspaceID); err != nil {
		return actor, false, err
	}
	if err = CompleteInterruption(ctx, q, actor, ws.HeadVersionID, ""); err != nil {
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
