package agent

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"time"
	"uuid"

	"github.com/helmrdotdev/helmr/internal/computer"

	"github.com/helmrdotdev/helmr/internal/db"
	"github.com/jackc/pgx/v5"
)

func closeProcessing(ctx context.Context, tx pgx.Tx, e Execution, turn uuid.UUID) error {
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
	return rejectTurnMessages(ctx, tx, e.EnvironmentID, e.SessionID, turn, false, "turn_closed")
}

func recordResult(ctx context.Context, tx pgx.Tx, e Execution, turn uuid.UUID, result json.RawMessage, digest [32]byte, drainEvidence string, save *computer.SaveRequest) error {
	o, err := lockSession(ctx, tx, e.EnvironmentID, e.SessionID)
	if err != nil {
		return err
	}
	var state string
	var prior []byte
	var epoch *int64
	var closed *time.Time
	var expired bool
	err = tx.QueryRow(ctx, `SELECT status,result_digest,process_epoch,processing_closed_at,COALESCE(deadline_at<=clock_timestamp(),false) FROM turns
          WHERE environment_id=$1 AND session_id=$2 AND id=$3 FOR NO KEY UPDATE`, e.EnvironmentID, e.SessionID, turn).Scan(&state, &prior, &epoch, &closed, &expired)
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
	if prior != nil {
		if !bytes.Equal(prior, digest[:]) {
			return ErrConflict
		}
		return tx.QueryRow(ctx, `SELECT id,computer_id,computer_lease_epoch,seq,requested_at FROM computer_saves WHERE environment_id=$1 AND turn_id=$2`, e.EnvironmentID, turn).Scan(&save.ID, &save.ComputerID, &save.LeaseEpoch, &save.Sequence, &save.RequestedAt)
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
	_, err = tx.Exec(ctx, `UPDATE turns SET status='finalizing',result=$3,result_digest=$4,drain_evidence=$5,result_recorded_at=clock_timestamp(),deadline_at=NULL WHERE environment_id=$1 AND id=$2`, e.EnvironmentID, turn, result, digest[:], drainEvidence)
	if err != nil {
		return err
	}
	// The save may reference only a finalizing Turn. The Session lock above
	// already holds the Computer row, so recording the save here takes no new
	// lock out of order.
	requested, err := computer.RequestSave(ctx, tx, e.EnvironmentID, o.computer, e.LeaseEpoch, turn)
	if err != nil {
		return err
	}
	*save = requested
	return event(ctx, tx, e.EnvironmentID, e.SessionID, turn, "turn.finalizing")
}

// Complete reconciles the Turn's own published save, even after source loss.
// A newer recovery head cannot stand in for that save. Cancellation and
// integrity failure still win until this transaction commits.
func Complete(ctx context.Context, pool db.TxBeginner, env, session, turn uuid.UUID) error {
	return db.RunTx(ctx, pool, func(tx pgx.Tx) error {
		publication, err := lockSlackPublication(ctx, tx, env, session)
		if err != nil {
			return err
		}
		o, err := lockSession(ctx, tx, env, session)
		if err != nil {
			return err
		}
		var state string
		if err = tx.QueryRow(ctx, `SELECT status FROM turns WHERE environment_id=$1 AND session_id=$2 AND id=$3 FOR NO KEY UPDATE`, env, session, turn).Scan(&state); err != nil {
			return err
		}
		if state == "completed" {
			return nil
		}
		if state != "finalizing" {
			return ErrTerminal
		}
		if o.lifecycle == "cancelled" {
			return ErrTerminal
		}
		var now time.Time
		var fault *time.Time
		if err = tx.QueryRow(ctx, `SELECT clock_timestamp(),integrity_fault_at FROM computers WHERE environment_id=$1 AND id=$2`, env, o.computer).Scan(&now, &fault); err != nil {
			return err
		}
		if fault != nil {
			return ErrNotReady
		}
		var save uuid.UUID
		err = tx.QueryRow(ctx, `SELECT id FROM computer_saves WHERE environment_id=$1 AND computer_id=$2 AND turn_id=$3 AND status='published'`, env, o.computer, turn).Scan(&save)
		if errors.Is(err, pgx.ErrNoRows) {
			return ErrNotReady
		}
		if err != nil {
			return err
		}
		_, err = tx.Exec(ctx, `UPDATE turns SET status='completed',terminal_at=clock_timestamp(),completion_save_id=$3 WHERE environment_id=$1 AND id=$2`, env, turn, save)
		if err != nil {
			return err
		}
		if err = event(ctx, tx, env, session, turn, "turn.completed"); err != nil {
			return err
		}
		var sequence int64
		var response bool
		if err = tx.QueryRow(ctx, `SELECT s.next_event_seq-1,t.response_id IS NOT NULL FROM sessions s JOIN turns t ON (t.environment_id,t.session_id)=(s.environment_id,s.id) WHERE s.environment_id=$1 AND s.id=$2 AND t.id=$3`, env, session, turn).Scan(&sequence, &response); err != nil {
			return err
		}
		if response {
			return publication.accept(ctx, tx, env, session, turn, sequence)
		}
		return nil
	})
}
