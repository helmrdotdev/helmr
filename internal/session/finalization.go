package session

import (
	"context"
	"encoding/json"
	"github.com/helmrdotdev/helmr/internal/auditpolicy"
	"time"
	"uuid"

	"github.com/helmrdotdev/helmr/internal/computer"

	"github.com/jackc/pgx/v5"
)

func closeProcessing(ctx context.Context, auditPolicy auditpolicy.Policy, tx pgx.Tx, e Execution, turn uuid.UUID) error {
	o, err := lockSession(ctx, tx, e.EnvironmentID, e.SessionID)
	if err != nil {
		return err
	}
	if err = executionReady(ctx, tx, e, o); err != nil {
		return err
	}
	tag, err := tx.Exec(ctx, `UPDATE turns SET processing_closed_at=COALESCE(processing_closed_at,clock_timestamp())
          WHERE environment_id=$1 AND session_id=$2 AND id=$3 AND process_epoch=$4 AND status='running'`, e.EnvironmentID, e.SessionID, turn, e.ProcessEpoch)
	if err != nil {
		return err
	}
	if tag.RowsAffected() != 1 {
		return ErrNotReady
	}
	return rejectTurnMessages(ctx, auditPolicy, tx, e.EnvironmentID, e.SessionID, turn, false, "turn_closed")
}

func recordResult(ctx context.Context, tx pgx.Tx, e Execution, turn uuid.UUID, result json.RawMessage, digest [32]byte, drainEvidence string) error {
	o, err := lockSession(ctx, tx, e.EnvironmentID, e.SessionID)
	if err != nil {
		return err
	}
	var state string
	var epoch *int64
	var closed *time.Time
	var expired bool
	err = tx.QueryRow(ctx, `SELECT status,process_epoch,processing_closed_at,COALESCE(deadline_at<=clock_timestamp(),false) FROM turns
          WHERE environment_id=$1 AND session_id=$2 AND id=$3 FOR NO KEY UPDATE`, e.EnvironmentID, e.SessionID, turn).Scan(&state, &epoch, &closed, &expired)
	if err != nil {
		return err
	}
	if epoch == nil {
		return ErrNotReady
	}
	if *epoch != e.ProcessEpoch {
		return ErrDenied
	}
	if err = executionReady(ctx, tx, e, o); err != nil {
		return err
	}
	if state != "running" || closed == nil || expired {
		return ErrNotReady
	}
	var pending bool
	if err := tx.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM turn_asks WHERE environment_id=$1 AND session_id=$2 AND turn_id=$3 AND status='pending') OR EXISTS(SELECT 1 FROM turn_messages WHERE environment_id=$1 AND session_id=$2 AND turn_id=$3 AND status IN ('admitted','started'))`, e.EnvironmentID, e.SessionID, turn).Scan(&pending); err != nil {
		return err
	}
	if pending {
		return errResultDrainPending
	}
	_, err = tx.Exec(ctx, `UPDATE turns SET status='completed',terminal_at=clock_timestamp(),result=$3,result_digest=$4,drain_evidence=$5,result_recorded_at=clock_timestamp(),deadline_at=NULL WHERE environment_id=$1 AND id=$2`, e.EnvironmentID, turn, result, digest[:], drainEvidence)
	if err != nil {
		return err
	}
	// The save references the committed result. The Session lock above
	// already holds the Computer row, so recording the save here takes no new
	// lock out of order.
	_, err = computer.RequestSave(ctx, tx, e.EnvironmentID, o.computer, e.LeaseEpoch, turn)
	if err != nil {
		return err
	}
	return event(ctx, tx, e.EnvironmentID, e.SessionID, turn, "turn.completed")
}
