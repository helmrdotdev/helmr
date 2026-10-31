package session

import (
	"context"
	"encoding/json"
	"github.com/helmrdotdev/helmr/internal/auditpolicy"
	"uuid"

	"github.com/helmrdotdev/helmr/internal/audit"
	"github.com/helmrdotdev/helmr/internal/db"
	"github.com/helmrdotdev/helmr/internal/workergroup"
	"github.com/jackc/pgx/v5"
)

// processFailureHoldThreshold is how many consecutive counted process failures
// hold a Session for attention. Fewer failures leave the queue running on a
// fresh process.
const processFailureHoldThreshold = 3

// Process failure reasons. Only harness failures count toward the hold
// threshold; a recoverable Computer loss or a revoked Secret is not one.
const (
	failureProcessFailed     = "process_failed"
	failureStopDeadline      = "stop_deadline_elapsed"
	failureExecutionLost     = "execution_lost"
	failureComputerLost      = "computer_lost"
	failureSecretUnavailable = "secret_unavailable"
)

func countedProcessFailure(reason string) bool {
	return reason == failureProcessFailed || reason == failureStopDeadline || reason == failureExecutionLost
}

// Consecutive counted failures follow the last harness-finished Turn and the
// last release of a process failure hold. A Turn finishes only while its
// process is ready, so a failure of that same process came after the success
// and counts.
const consecutiveProcessFailuresSQL = `SELECT count(*) FROM session_processes p WHERE p.environment_id=$1 AND p.session_id=$2
 AND p.failure_reason IN ('process_failed','stop_deadline_elapsed','execution_lost')
 AND p.epoch>=COALESCE((SELECT max(t.process_epoch) FROM turns t WHERE t.environment_id=$1 AND t.session_id=$2 AND t.status IN ('completed','failed')),0)
 AND p.failure_recorded_at>COALESCE((SELECT max(h.released_at) FROM session_holds h WHERE h.environment_id=$1 AND h.session_id=$2 AND h.kind='process_failed'),'-infinity'::timestamptz)`

// recordSessionFailure records the first failure fact of one logical process.
// The caller holds ownership/process locks. A counted failure is reported and
// holds the Session once the consecutive threshold is reached.
func recordSessionFailure(ctx context.Context, auditPolicy auditpolicy.Policy, tx pgx.Tx, actor audit.Evidence, e Execution, reason string) error {
	tag, err := tx.Exec(ctx, `UPDATE session_processes SET failure_recorded_at=clock_timestamp(),failure_reason=$4
 WHERE environment_id=$1 AND session_id=$2 AND epoch=$3 AND failure_recorded_at IS NULL`, e.EnvironmentID, e.SessionID, e.ProcessEpoch, reason)
	if err != nil || tag.RowsAffected() == 0 || !countedProcessFailure(reason) {
		return err
	}
	var terminal bool
	var failures int64
	if err = tx.QueryRow(ctx, `SELECT s.status IN ('closed','cancelled'),(`+consecutiveProcessFailuresSQL+`) FROM sessions s WHERE s.environment_id=$1 AND s.id=$2`, e.EnvironmentID, e.SessionID).Scan(&terminal, &failures); err != nil {
		return err
	}
	if terminal {
		return nil
	}
	var hold *uuid.UUID
	if failures >= processFailureHoldThreshold {
		if err = tx.QueryRow(ctx, `SELECT (SELECT id FROM session_holds WHERE environment_id=$1 AND session_id=$2 AND kind='process_failed' AND released_at IS NULL)`, e.EnvironmentID, e.SessionID).Scan(&hold); err != nil {
			return err
		}
		if hold == nil {
			id := uuid.NewV7()
			hold = &id
			if _, err = tx.Exec(ctx, `INSERT INTO session_holds(environment_id,id,session_id,kind,issuer_kind) VALUES($1,$2,$3,'process_failed','system')`, e.EnvironmentID, id, e.SessionID); err != nil {
				return err
			}
			if err = advanceControlAuthority(ctx, tx, e.EnvironmentID, []uuid.UUID{e.SessionID}); err != nil {
				return err
			}
			if err = auditActorFact(ctx, tx, auditPolicy, actor, audit.Event{EnvironmentID: e.EnvironmentID, SessionID: e.SessionID, Action: "session.hold_created", TargetKind: "session", TargetID: e.SessionID, Details: audit.SessionHoldDetails{Reason: reason, Generation: e.ProcessEpoch, HoldID: id}}); err != nil {
				return err
			}
		}
	}
	data, err := json.Marshal(struct {
		ProcessEpoch int64      `json:"processEpoch"`
		Reason       string     `json:"reason"`
		HoldID       *uuid.UUID `json:"holdId"`
	}{e.ProcessEpoch, reason, hold})
	if err != nil {
		return err
	}
	return eventData(ctx, tx, e.EnvironmentID, e.SessionID, uuid.Nil(), "session.process_failed", data)
}

// Definitive process failure or stop settles its exact running Turn.
func interruptRunningSessionTurn(ctx context.Context, auditPolicy auditpolicy.Policy, tx pgx.Tx, actor audit.Evidence, e Execution) error {
	return interruptRunningSessionTurnWithData(ctx, auditPolicy, tx, actor, e, nil)
}

func interruptRunningSessionTurnWithData(ctx context.Context, auditPolicy auditpolicy.Policy, tx pgx.Tx, actor audit.Evidence, e Execution, data json.RawMessage) error {
	if len(data) == 0 {
		data = json.RawMessage(`{}`)
	}
	// Completed results remain immutable; only an actively running handler is interrupted.
	rows, err := tx.Query(ctx, `UPDATE turns SET status='interrupted',terminal_at=clock_timestamp(),processing_closed_at=COALESCE(processing_closed_at,clock_timestamp())
 WHERE environment_id=$1 AND session_id=$2 AND process_epoch=$3 AND status='running' RETURNING id`, e.EnvironmentID, e.SessionID, e.ProcessEpoch)
	if err != nil {
		return err
	}
	turns, err := pgx.CollectRows(rows, pgx.RowTo[uuid.UUID])
	if err != nil {
		return err
	}
	for _, turn := range turns {
		if err = auditActorFact(ctx, tx, auditPolicy, actor, audit.Event{EnvironmentID: e.EnvironmentID, SessionID: e.SessionID, TurnID: turn, Action: "turn.interrupted", TargetKind: "turn", TargetID: turn, Details: audit.LifecycleDetails{Generation: e.ProcessEpoch}}); err != nil {
			return err
		}
		if err = cancelTurnAsks(ctx, auditPolicy, tx, e.EnvironmentID, e.SessionID, turn); err != nil {
			return err
		}
		if err = rejectTurnMessages(ctx, auditPolicy, tx, e.EnvironmentID, e.SessionID, turn, true, "turn_terminated"); err != nil {
			return err
		}
		if err = eventData(ctx, tx, e.EnvironmentID, e.SessionID, turn, "turn.interrupted", data); err != nil {
			return err
		}
	}
	return nil
}

// ObserveSessionFailure records a definitive runtime rejection independently of
// a possibly superseded control receipt. Transport loss is not such a rejection.
func ObserveSessionFailure(ctx context.Context, auditPolicy auditpolicy.Policy, pool db.TxBeginner, host workergroup.HostPrincipal, e Execution, attachment int64) error {
	return hideMissing(db.RunTx(ctx, pool, func(tx pgx.Tx) error {
		if err := lockRuntimeHost(ctx, tx, Caller{Kind: "session", ID: e.SessionID, Execution: e, Host: &host}); err != nil {
			return err
		}
		o, err := lockSession(ctx, tx, e.EnvironmentID, e.SessionID)
		if err != nil {
			return err
		}
		var currentAttachment int64
		var recorded, stopped bool
		if err = tx.QueryRow(ctx, `SELECT attachment_sequence,failure_recorded_at IS NOT NULL,status='stopped' AND fenced_at IS NOT NULL FROM session_processes
 WHERE environment_id=$1 AND session_id=$2 AND epoch=$3 AND computer_lease_epoch=$4 FOR NO KEY UPDATE`, e.EnvironmentID, e.SessionID, e.ProcessEpoch, e.LeaseEpoch).Scan(&currentAttachment, &recorded, &stopped); err != nil {
			return err
		}
		if attachment <= 0 || attachment != currentAttachment {
			return ErrDenied
		}
		var owned bool
		if err = tx.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM computer_leases WHERE environment_id=$1 AND computer_id=$2 AND epoch=$3 AND worker_host_id=$4 AND worker_epoch=$5
 AND status IN ('active','acquiring') AND fenced_at IS NULL AND expires_at>clock_timestamp())`, e.EnvironmentID, o.computer, e.LeaseEpoch, host.HostID, host.Epoch).Scan(&owned); err != nil {
			return err
		}
		if !owned {
			return ErrDenied
		}
		if recorded || stopped {
			return nil
		}
		return observeSessionFailure(ctx, auditPolicy, tx, auditWorkerActor(host, e), e, failureProcessFailed)
	}))
}

func observeSessionFailure(ctx context.Context, auditPolicy auditpolicy.Policy, tx pgx.Tx, actor audit.Evidence, e Execution, reason string) error {
	if err := recordSessionFailure(ctx, auditPolicy, tx, actor, e, reason); err != nil {
		return err
	}
	if err := interruptRunningSessionTurn(ctx, auditPolicy, tx, actor, e); err != nil {
		return err
	}
	// This is stop intent, never evidence that the physical process is absent.
	_, err := tx.Exec(ctx, `UPDATE session_processes SET status='stopping' WHERE environment_id=$1 AND session_id=$2 AND epoch=$3`, e.EnvironmentID, e.SessionID, e.ProcessEpoch)
	if err != nil {
		return err
	}
	return notifySessionsChanged(ctx, tx, e.EnvironmentID, e.SessionID)
}
