package agent

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"time"
	"uuid"

	"github.com/helmrdotdev/helmr/internal/db"
	"github.com/helmrdotdev/helmr/internal/sha256sum"
	"github.com/jackc/pgx/v5"
)

func CloseProcessing(ctx context.Context, pool db.TxBeginner, e Execution, turn uuid.UUID) error {
	return db.RunTx(ctx, pool, func(tx pgx.Tx) error {
		return closeProcessing(ctx, tx, e, turn)
	})
}

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

// RecordResult is called after native registration, callback and output drainage.
// The worker attestation must identify that completed drainage, not just an empty
// database queue. The operation records the result and exact save request together.
func RecordResult(ctx context.Context, pool db.TxBeginner, e Execution, turn uuid.UUID, result json.RawMessage, drainEvidence string) (SaveRequest, error) {
	var save SaveRequest
	digest, err := digestJSON(result)
	if err != nil {
		return save, err
	}
	if drainEvidence == "" {
		return save, ErrNotReady
	}
	err = db.RunTx(ctx, pool, func(tx pgx.Tx) error {
		return recordResult(ctx, tx, e, turn, result, digest, drainEvidence, &save)
	})
	if err != nil {
		return SaveRequest{}, err
	}
	return save, nil
}

func recordResult(ctx context.Context, tx pgx.Tx, e Execution, turn uuid.UUID, result json.RawMessage, digest [32]byte, drainEvidence string, save *SaveRequest) error {
	o, err := lockSession(ctx, tx, e.EnvironmentID, e.SessionID)
	if err != nil {
		return err
	}
	var state string
	var prior []byte
	var epoch *int64
	var closed *time.Time
	err = tx.QueryRow(ctx, `SELECT status,result_digest,process_epoch,processing_closed_at FROM turns
          WHERE environment_id=$1 AND session_id=$2 AND id=$3 FOR NO KEY UPDATE`, e.EnvironmentID, e.SessionID, turn).Scan(&state, &prior, &epoch, &closed)
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
	if state != "running" || closed == nil {
		return ErrNotReady
	}
	var pending bool
	if err := tx.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM turn_asks WHERE environment_id=$1 AND session_id=$2 AND turn_id=$3 AND status='pending') OR EXISTS(SELECT 1 FROM turn_messages WHERE environment_id=$1 AND session_id=$2 AND turn_id=$3 AND status IN ('admitted','started'))`, e.EnvironmentID, e.SessionID, turn).Scan(&pending); err != nil {
		return err
	}
	if pending {
		return errResultDrainPending
	}
	save.ID = uuid.NewV7()
	save.ComputerID = o.computer
	save.LeaseEpoch = e.LeaseEpoch
	if err = tx.QueryRow(ctx, `UPDATE computers SET next_save_seq=next_save_seq+1 WHERE environment_id=$1 AND id=$2 RETURNING next_save_seq-1`, e.EnvironmentID, o.computer).Scan(&save.Sequence); err != nil {
		return err
	}
	_, err = tx.Exec(ctx, `UPDATE turns SET status='finalizing',result=$3,result_digest=$4,drain_evidence=$5,result_recorded_at=clock_timestamp() WHERE environment_id=$1 AND id=$2`, e.EnvironmentID, turn, result, digest[:], drainEvidence)
	if err != nil {
		return err
	}
	err = tx.QueryRow(ctx, `INSERT INTO computer_saves(environment_id,id,computer_id,computer_lease_epoch,seq,turn_id) VALUES($1,$2,$3,$4,$5,$6) RETURNING requested_at`, e.EnvironmentID, save.ID, save.ComputerID, e.LeaseEpoch, save.Sequence, turn).Scan(&save.RequestedAt)
	if err != nil {
		return err
	}
	return event(ctx, tx, e.EnvironmentID, e.SessionID, turn, "turn.finalizing")
}

type CaptureEvidence struct {
	EnvironmentID uuid.UUID
	SaveID        uuid.UUID
	LeaseEpoch    int64
	DiskRoot      string
	Evidence      string
}

// RecordCapture records a verified operation-bound capture receipt. The worker
// protocol establishes flush and capture ordering after receipt of the committed
// save request; database timestamps record observation, not synchronized host time.
func RecordCapture(ctx context.Context, pool db.TxBeginner, c CaptureEvidence) error {
	if !sha256sum.ValidDigest(c.DiskRoot) {
		return ErrInvalidInput
	}
	if c.Evidence == "" {
		return ErrNotReady
	}
	return db.RunTx(ctx, pool, func(tx pgx.Tx) error { return recordCapture(ctx, tx, c) })
}

func recordCapture(ctx context.Context, tx pgx.Tx, c CaptureEvidence) error {
	var computer uuid.UUID
	if err := tx.QueryRow(ctx, `SELECT computer_id FROM computer_saves WHERE environment_id=$1 AND id=$2`, c.EnvironmentID, c.SaveID).Scan(&computer); err != nil {
		return err
	}
	if _, err := tx.Exec(ctx, `SELECT id FROM computers WHERE environment_id=$1 AND id=$2 FOR NO KEY UPDATE`, c.EnvironmentID, computer); err != nil {
		return err
	}
	var state string
	var seq, epoch int64
	var oldRoot *string
	err := tx.QueryRow(ctx, `SELECT status,seq,computer_lease_epoch,'sha256:'||encode(captured_root_digest,'hex') FROM computer_saves WHERE environment_id=$1 AND id=$2 FOR NO KEY UPDATE`, c.EnvironmentID, c.SaveID).Scan(&state, &seq, &epoch, &oldRoot)
	if err != nil {
		return err
	}
	if epoch != c.LeaseEpoch {
		return ErrDenied
	}
	if state == "captured" || state == "published" {
		if oldRoot == nil || *oldRoot != c.DiskRoot {
			return ErrConflict
		}
		return nil
	}
	if state != "requested" {
		return ErrTerminal
	}
	var earlier bool
	if err = tx.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM computer_saves WHERE environment_id=$1 AND computer_id=$2 AND seq<$3 AND status='requested')`, c.EnvironmentID, computer, seq).Scan(&earlier); err != nil {
		return err
	}
	if earlier {
		return ErrNotReady
	}
	_, err = tx.Exec(ctx, `WITH observed AS MATERIALIZED (SELECT clock_timestamp() AS at) UPDATE computer_saves SET status='captured',flush_acknowledged_at=observed.at,captured_at=observed.at,captured_root_digest=decode(substring($3::text from 8),'hex'),capture_evidence=$4 FROM observed WHERE environment_id=$1 AND id=$2`, c.EnvironmentID, c.SaveID, c.DiskRoot, c.Evidence)
	return err
}

// ReconcileSavePublication observes the committed receipt after an uncertain
// response. It performs no new publication and is safe after writer expiry.
// Uploaded objects without a committed publication remain unresolved.
func ReconcileSavePublication(ctx context.Context, pool db.TxBeginner, env, saveID uuid.UUID, diskRoot string) error {
	if !sha256sum.ValidDigest(diskRoot) {
		return ErrInvalidInput
	}
	return hideMissing(db.RunTx(ctx, pool, func(tx pgx.Tx) error {
		var state string
		var root *string
		if err := tx.QueryRow(ctx, `SELECT status,'sha256:'||encode(captured_root_digest,'hex') FROM computer_saves WHERE environment_id=$1 AND id=$2`, env, saveID).Scan(&state, &root); err != nil {
			return err
		}
		if state == "failed" {
			return ErrTerminal
		}
		if root != nil && *root != diskRoot {
			return ErrConflict
		}
		if state != "published" {
			return ErrNotReady
		}
		return nil
	}))
}

// Complete reconciles the Turn's own published save, even after source loss.
// A newer recovery head cannot stand in for that save. Cancellation, deadline and
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
		var deadline *time.Time
		if err = tx.QueryRow(ctx, `SELECT status,deadline_at FROM turns WHERE environment_id=$1 AND session_id=$2 AND id=$3 FOR NO KEY UPDATE`, env, session, turn).Scan(&state, &deadline); err != nil {
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
		if deadline != nil && !now.Before(*deadline) {
			return ErrTerminal
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
