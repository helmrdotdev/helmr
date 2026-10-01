package session

import (
	"context"
	"encoding/json"
	"uuid"

	"github.com/helmrdotdev/helmr/internal/computer"
	"github.com/helmrdotdev/helmr/internal/db"
	"github.com/helmrdotdev/helmr/internal/pgvalue"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
)

// The Computer, Instance, Session and admission secrets are already locked.
// Only reconciled process scopes can receive replacement logical authority.
func reconcileLostExecution(ctx context.Context, tx pgx.Tx, session db.Session, bindings []db.LockComputerSecretsForAdmissionRow) (db.Session, bool, error) {
	if session.DispatchHoldReason.String != "recovery_required" || !session.CurrentRunID.Valid ||
		(session.Status != "open" && session.Status != "closing") {
		return session, false, nil
	}
	q := db.New(tx)
	current, err := q.LockSessionInputCurrentRun(ctx, db.LockSessionInputCurrentRunParams{EnvironmentID: session.EnvironmentID, SessionID: session.ID, RunID: session.CurrentRunID})
	if err != nil {
		return session, false, err
	}
	switch current.Status {
	case db.RunStatusFailed, db.RunStatusSystemFailed, db.RunStatusExpired, db.RunStatusCancelled:
	default:
		return session, true, nil
	}
	locked, err := computer.LockSessionComputer(ctx, tx, sessionComputerRef(session))
	if err != nil {
		return session, false, err
	}
	ws := locked.Computer()
	source, episode := ws.HeadDiskVersionID, ws.RecoveryID
	repairFailure := ws.RecoveryFailure
	reconciled, err := q.SessionExecutionScopesReconciled(ctx, session.ID)
	if err != nil || !reconciled {
		return session, true, err
	}
	owned, err := q.OwnedRunScopesReconciled(ctx, current.ID)
	if err != nil || !owned {
		return session, true, err
	}
	if len(repairFailure) > 0 {
		if session.CancelRequestedAt.Valid {
			return settleCancelledFailedExecution(ctx, tx, session, repairFailure)
		}
		now, err := q.GetTaskCompletionTime(ctx)
		if err != nil {
			return session, false, err
		}
		if err = FailExecution(ctx, tx, session, repairFailure, "", now); err != nil {
			return session, false, err
		}
		session, err = q.GetSession(ctx, db.GetSessionParams{EnvironmentID: session.EnvironmentID, ID: session.ID})
		return session, false, err
	}
	if current.Status == db.RunStatusFailed {
		// A known terminal failure before Computer loss (for example exhausted
		// initial preparation) cannot be repaired by replaying the Actor.
		if len(current.Failure) == 0 {
			return session, false, ErrAuthority
		}
		now, err := q.GetTaskCompletionTime(ctx)
		if err != nil {
			return session, false, err
		}
		if session.CancelRequestedAt.Valid {
			return settleCancelledFailedExecution(ctx, tx, session, current.Failure)
		}
		if err = FailExecution(ctx, tx, session, current.Failure, "", now); err != nil {
			return session, false, err
		}
		session, err = q.GetSession(ctx, db.GetSessionParams{EnvironmentID: session.EnvironmentID, ID: session.ID})
		return session, false, err
	}
	if session.CancelRequestedAt.Valid {
		return settleCancelledFailedExecution(ctx, tx, session, current.Failure)
	}
	if ws.Status != db.ComputerStatusActive || ws.DesiredState != db.ComputerDesiredStateActive ||
		ws.DirtyState == db.ComputerDirtyStateCaptureFailed || ws.DirtyState == db.ComputerDirtyStateDirtyStateLost {
		return session, true, nil
	}
	committed, err := q.SessionRecoveryHeadCommitted(ctx, db.SessionRecoveryHeadCommittedParams{EnvironmentID: session.EnvironmentID, ComputerID: session.ComputerID, ID: source})
	if err != nil || !committed {
		return session, true, err
	}
	// Do not consume the durable wakeup before credentials permit continuation.
	if !session.CancelRequestedAt.Valid && !bindingsCanAdmit(session, bindings) {
		return session, true, nil
	}
	if !session.CancelRequestedAt.Valid && session.ConsecutiveExecutionLosses >= 7 {
		reason := "execution_loss_limit"
		failure, _ := json.Marshal(map[string]any{"code": reason, "message": "Execution could not be resumed within the recovery limit", "details": map[string]any{}})
		now, err := q.GetTaskCompletionTime(ctx)
		if err == nil {
			err = FailExecution(ctx, tx, session, failure, "", now)
		}
		if err != nil {
			return session, false, err
		}
		session, err = q.GetSession(ctx, db.GetSessionParams{EnvironmentID: session.EnvironmentID, ID: session.ID})
		return session, false, err
	}
	var sequence pgtype.Int8
	// Run-level interruption is also valid between Turns. Its immutable event
	// survives replacement of the hold by the infrastructure-loss fence.
	var interrupted bool
	if err = tx.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM session_events WHERE session_id=$1
	 AND producer_run_id=$2 AND run_generation=$3 AND kind='session.held'
	 AND data->>'reason'='interrupt_requested')`, session.ID, current.ID, session.RunGeneration).Scan(&interrupted); err != nil {
		return session, false, err
	}
	interrupted = interrupted || session.CancelRequestedAt.Valid
	if session.ActiveTurnID.Valid {
		turn, err := q.LockSessionTurnInput(ctx, db.LockSessionTurnInputParams{EnvironmentID: session.EnvironmentID, SessionID: session.ID, ID: session.ActiveTurnID})
		if err != nil {
			return session, false, err
		}
		if turn.Status != "running" || turn.Sequence != session.CommittedInputSequence+1 {
			return session, false, ErrAuthority
		}
		interrupted = interrupted || turn.InterruptRequestedAt.Valid
		if err = finishUnsettledMessages(ctx, q, session, turn.ID, "execution_lost"); err != nil {
			return session, false, err
		}
		body, _ := json.Marshal(map[string]any{"error": map[string]string{"code": "execution_lost"}, "recovery_id": pgvalue.UUIDString(episode)})
		event, err := appendLifecycleEvent(ctx, q, session, turn.ID, pgtype.UUID{}, "turn.failed", body, source)
		if err != nil {
			return session, false, err
		}
		if _, err = q.SettleHeldSessionTurn(ctx, db.SettleHeldSessionTurnParams{EnvironmentID: session.EnvironmentID, SessionID: session.ID, TurnID: turn.ID, Status: "failed", EventID: event.ID}); err != nil {
			return session, false, err
		}
		sequence = pgtype.Int8{Int64: turn.Sequence, Valid: true}
	}
	body, _ := json.Marshal(map[string]any{"recovery_id": pgvalue.UUIDString(episode), "version_id": pgvalue.UUIDString(source), "reason": "execution_lost"})
	if _, err = appendLifecycleEvent(ctx, q, session, pgtype.UUID{}, pgtype.UUID{}, "session.execution_lost", body, source); err != nil {
		return session, false, err
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
		revision=revision+1,updated_at=now() WHERE id=$1 AND current_run_id=$2`, session.ID, session.CurrentRunID, sequence, hold, reason)
	if err != nil {
		return session, false, err
	}
	session, err = q.GetSession(ctx, db.GetSessionParams{EnvironmentID: session.EnvironmentID, ID: session.ID})
	if err != nil {
		return session, false, err
	}
	if session.Status == "open" && !interrupted && session.CommittedInputSequence < session.NextInputSequence-1 {
		if _, err = CreateContinuation(ctx, tx, session, db.Computer{ID: session.ComputerID}, bindings); err != nil {
			return session, false, err
		}
	}
	return session, false, nil
}

// Stopping a Session retires only its execution. Peer scopes and their live
// filesystem remain attached to the Computer.
func reconcileStoppedExecution(ctx context.Context, tx pgx.Tx, session db.Session) (db.Session, bool, error) {
	if session.DispatchHoldReason.String != "interrupt_requested" || !session.CurrentRunID.Valid || (session.Status != "open" && session.Status != "closing") {
		return session, false, nil
	}
	q := db.New(tx)
	current, err := q.LockSessionInputCurrentRun(ctx, db.LockSessionInputCurrentRunParams{EnvironmentID: session.EnvironmentID, SessionID: session.ID, RunID: session.CurrentRunID})
	if err != nil {
		return session, false, err
	}
	if current.Status != db.RunStatusCancelled || current.CurrentRunLeaseID.Valid {
		return session, true, nil
	}
	locked, err := computer.LockSessionComputer(ctx, tx, sessionComputerRef(session))
	if err != nil {
		return session, false, err
	}
	ws := locked.Computer()
	if session.ActiveTurnID.Valid && !ws.HeadDiskVersionID.Valid {
		return session, true, nil
	}
	excluded, err := q.SessionExecutionScopesReconciled(ctx, session.ID)
	if err != nil || !excluded {
		return session, true, err
	}
	owned, err := q.OwnedRunScopesReconciled(ctx, current.ID)
	if err != nil || !owned {
		return session, true, err
	}
	var versionID pgtype.UUID
	if ws.HeadDiskVersionID.Valid {
		committed, err := q.SessionRecoveryHeadCommitted(ctx, db.SessionRecoveryHeadCommittedParams{EnvironmentID: session.EnvironmentID, ComputerID: session.ComputerID, ID: ws.HeadDiskVersionID})
		if err != nil {
			return session, false, err
		}
		if committed {
			versionID = ws.HeadDiskVersionID
		}
	}
	if session.ActiveTurnID.Valid && !versionID.Valid {
		return session, true, nil
	}
	if err = CompleteInterruption(ctx, tx, session, versionID, ""); err != nil {
		return session, false, err
	}
	session, err = q.GetSession(ctx, db.GetSessionParams{EnvironmentID: session.EnvironmentID, ID: session.ID})
	return session, false, err
}

// Cancellation still closes the Session when a known execution failure won first.
// Preserve that failure on the active Turn, then drain only cancelled inputs.
func settleCancelledFailedExecution(ctx context.Context, tx pgx.Tx, session db.Session, failure json.RawMessage) (db.Session, bool, error) {
	q := db.New(tx)
	sequence := session.CommittedInputSequence
	if session.ActiveTurnID.Valid {
		turn, err := q.LockSessionTurnInput(ctx, db.LockSessionTurnInputParams{EnvironmentID: session.EnvironmentID, SessionID: session.ID, ID: session.ActiveTurnID})
		if err != nil {
			return session, false, err
		}
		if turn.Status != "running" || turn.Sequence != sequence+1 {
			return session, false, ErrAuthority
		}
		if err = finishUnsettledMessages(ctx, q, session, turn.ID, "session_cancelled"); err != nil {
			return session, false, err
		}
		body, _ := json.Marshal(map[string]any{"error": failure})
		event, err := appendLifecycleEvent(ctx, q, session, turn.ID, pgtype.UUID{}, "turn.failed", body, pgtype.UUID{})
		if err != nil {
			return session, false, err
		}
		if _, err = q.SettleHeldSessionTurn(ctx, db.SettleHeldSessionTurnParams{EnvironmentID: session.EnvironmentID, SessionID: session.ID, TurnID: turn.ID, Status: "failed", EventID: event.ID}); err != nil {
			return session, false, err
		}
		sequence = turn.Sequence
	}
	if _, err := tx.Exec(ctx, `UPDATE sessions SET current_run_id=NULL,active_turn_id=NULL,dispatch_hold_id=NULL,dispatch_hold_reason=NULL,dispatch_hold_run_id=NULL,dispatch_hold_attempt_number=NULL,dispatch_hold_run_generation=NULL,committed_input_sequence=$2,run_generation=run_generation+1,revision=revision+1,updated_at=now() WHERE id=$1 AND cancel_requested_at IS NOT NULL`, session.ID, sequence); err != nil {
		return session, false, err
	}
	session, err := q.GetSession(ctx, db.GetSessionParams{EnvironmentID: session.EnvironmentID, ID: session.ID})
	return session, false, err
}
