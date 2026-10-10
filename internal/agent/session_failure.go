package agent

import (
	"context"
	"encoding/json"
	"uuid"

	"github.com/helmrdotdev/helmr/internal/db"
	"github.com/helmrdotdev/helmr/internal/workergroup"
	"github.com/jackc/pgx/v5"
)

// The caller holds ownership/process locks and records the idempotent failure fact.
func recordSessionFailure(ctx context.Context, tx pgx.Tx, e Execution, reason string) error {
	hold := uuid.NewV7()
	if _, err := tx.Exec(ctx, `INSERT INTO session_holds(environment_id,id,session_id,scope,reason)
 VALUES($1,$2,$3,'local',$4)`, e.EnvironmentID, hold, e.SessionID, reason); err != nil {
		return err
	}
	if err := advanceControlAuthority(ctx, tx, e.EnvironmentID, []uuid.UUID{e.SessionID}); err != nil {
		return err
	}
	data, err := json.Marshal(struct {
		HoldID       uuid.UUID `json:"holdId"`
		ProcessEpoch int64     `json:"processEpoch"`
	}{hold, e.ProcessEpoch})
	if err != nil {
		return err
	}
	return eventData(ctx, tx, e.EnvironmentID, e.SessionID, uuid.Nil(), "session.process_failed", data)
}

// Definitive process failure or stop settles its exact running Turn, including revocation.
func interruptRunningSessionTurn(ctx context.Context, tx pgx.Tx, e Execution) error {
	// A recorded result may still complete from its exact published save. Keep
	// finalization and its ordered save intact for independent reconciliation.
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
		if err = cancelTurnAsks(ctx, tx, e.EnvironmentID, e.SessionID, turn); err != nil {
			return err
		}
		if err = rejectTurnMessages(ctx, tx, e.EnvironmentID, e.SessionID, turn, true, "turn_terminated"); err != nil {
			return err
		}
		if err = event(ctx, tx, e.EnvironmentID, e.SessionID, turn, "turn.interrupted"); err != nil {
			return err
		}
	}
	return nil
}

// ObserveSessionFailure records a definitive runtime rejection independently of
// a possibly superseded control receipt. Transport loss is not such a rejection.
func ObserveSessionFailure(ctx context.Context, pool db.TxBeginner, host workergroup.HostPrincipal, e Execution, attachment int64) error {
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
		return observeSessionFailure(ctx, tx, e, "Session runtime control failed")
	}))
}

func observeSessionFailure(ctx context.Context, tx pgx.Tx, e Execution, reason string) error {
	var terminal bool
	if err := tx.QueryRow(ctx, `SELECT s.status IN ('closed','cancelled') OR d.execution_revoked_at IS NOT NULL OR EXISTS(SELECT 1 FROM computer_secret_revocations revoked WHERE revoked.environment_id=s.environment_id AND revoked.computer_id=s.computer_id) FROM sessions s JOIN deployments d ON (d.environment_id,d.id)=(s.environment_id,s.deployment_id) WHERE s.environment_id=$1 AND s.id=$2`, e.EnvironmentID, e.SessionID).Scan(&terminal); err != nil {
		return err
	}
	revocationRecorded, err := recordImageRevocationFailure(ctx, tx, e)
	if err != nil {
		return err
	}
	if !terminal && !revocationRecorded {
		if err = recordSessionFailure(ctx, tx, e, reason); err != nil {
			return err
		}
	}
	if err = interruptRunningSessionTurn(ctx, tx, e); err != nil {
		return err
	}
	// This is stop intent, never evidence that the physical process is absent.
	_, err = tx.Exec(ctx, `UPDATE session_processes SET failure_recorded_at=clock_timestamp(),status='stopping' WHERE environment_id=$1 AND session_id=$2 AND epoch=$3`, e.EnvironmentID, e.SessionID, e.ProcessEpoch)
	return err
}
