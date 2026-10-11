package agent

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"uuid"

	"github.com/helmrdotdev/helmr/internal/conversation"
	"github.com/helmrdotdev/helmr/internal/db"
	"github.com/helmrdotdev/helmr/internal/workergroup"
	"github.com/jackc/pgx/v5"
)

var ErrResponseAlreadyStaged = errors.New("response already staged")

// Output is durable progress, independent of the result's mandatory own save.
// The same operation returns its original sequence, including after settlement.
func RuntimeOutput(ctx context.Context, pool db.TxBeginner, host workergroup.HostPrincipal, e Execution, turn, operation uuid.UUID, raw json.RawMessage) (int64, error) {
	value, err := conversation.Content(raw, true)
	if err != nil {
		return 0, err
	}
	if operation == uuid.Nil() {
		return 0, ErrInvalidInput
	}
	digest := sha256.Sum256(value)
	var sequence int64
	err = db.RunTx(ctx, pool, func(tx pgx.Tx) error {
		if err := lockRuntimeHost(ctx, tx, Caller{Kind: "session", ID: e.SessionID, Execution: e, Host: &host}); err != nil {
			return err
		}
		publication, err := lockSlackPublication(ctx, tx, e.EnvironmentID, e.SessionID)
		if err != nil {
			return err
		}
		if err := lockRuntimeOperationOwner(ctx, tx, host, e); err != nil {
			return err
		}
		if err := lockRuntimeContentTurn(ctx, tx, e, turn); err != nil {
			return err
		}
		var prior []byte
		err = tx.QueryRow(ctx, `SELECT seq,operation_digest FROM session_events WHERE environment_id=$1 AND session_id=$2 AND turn_id=$3 AND operation_id=$4`, e.EnvironmentID, e.SessionID, turn, operation).Scan(&sequence, &prior)
		if err == nil {
			if !bytes.Equal(prior, digest[:]) {
				return ErrConflict
			}
			return lockRuntimeOperationOwner(ctx, tx, host, e)
		}
		if !errors.Is(err, pgx.ErrNoRows) {
			return err
		}
		if err := runtimeContentReady(ctx, tx, e, turn, false); err != nil {
			return err
		}
		tag, err := tx.Exec(ctx, `UPDATE turns SET progress_bytes=progress_bytes+$3 WHERE environment_id=$1 AND id=$2 AND progress_bytes+$3<=8388608`, e.EnvironmentID, turn, len(value))
		if err != nil {
			return err
		}
		if tag.RowsAffected() != 1 {
			return fmt.Errorf("%w: Turn progress and questions exceed 8 MiB", conversation.ErrLimit)
		}
		err = tx.QueryRow(ctx, `WITH allocated AS (UPDATE sessions SET next_event_seq=next_event_seq+1 WHERE environment_id=$1 AND id=$2 RETURNING next_event_seq-1 AS seq)
 INSERT INTO session_events(environment_id,session_id,seq,turn_id,kind,data,operation_id,operation_digest)
 SELECT $1,$2,seq,$3,'turn.output',$4,$5,$6 FROM allocated RETURNING seq`, e.EnvironmentID, e.SessionID, turn, []byte(value), operation, digest[:]).Scan(&sequence)
		if err != nil {
			return err
		}
		if !slackContentNonempty(value) {
			return nil
		}
		return publication.accept(ctx, tx, e.EnvironmentID, e.SessionID, turn, sequence)
	})
	return sequence, hideMissing(err)
}

// RuntimeRespond stages one optional answer. It is exposed only by a Completed
// outcome; accepting it does not complete processing or consume progress budget.
func RuntimeRespond(ctx context.Context, pool db.TxBeginner, host workergroup.HostPrincipal, e Execution, turn, operation uuid.UUID, raw json.RawMessage) error {
	value, err := conversation.Content(raw, true)
	if err != nil {
		return err
	}
	if operation == uuid.Nil() {
		return ErrInvalidInput
	}
	digest := sha256.Sum256(value)
	return hideMissing(db.RunTx(ctx, pool, func(tx pgx.Tx) error {
		if err := lockRuntimeOperationOwner(ctx, tx, host, e); err != nil {
			return err
		}
		if err := lockRuntimeContentTurn(ctx, tx, e, turn); err != nil {
			return err
		}
		var priorID *uuid.UUID
		var prior []byte
		if err := tx.QueryRow(ctx, `SELECT response_id,response_digest FROM turns WHERE environment_id=$1 AND id=$2`, e.EnvironmentID, turn).Scan(&priorID, &prior); err != nil {
			return err
		}
		if priorID != nil {
			if *priorID != operation {
				return ErrResponseAlreadyStaged
			}
			if !bytes.Equal(prior, digest[:]) {
				return ErrConflict
			}
			return lockRuntimeOperationOwner(ctx, tx, host, e)
		}
		if err := runtimeContentReady(ctx, tx, e, turn, true); err != nil {
			return err
		}
		_, err := tx.Exec(ctx, `UPDATE turns SET response_id=$3,response_digest=$4,response=$5,response_staged_at=clock_timestamp() WHERE environment_id=$1 AND id=$2`, e.EnvironmentID, turn, operation, digest[:], []byte(value))
		return err
	}))
}

func lockRuntimeContentTurn(ctx context.Context, tx pgx.Tx, e Execution, turn uuid.UUID) error {
	var id uuid.UUID
	return tx.QueryRow(ctx, `SELECT id FROM turns WHERE environment_id=$1 AND session_id=$2 AND id=$3 AND process_epoch=$4 FOR NO KEY UPDATE`, e.EnvironmentID, e.SessionID, turn, e.ProcessEpoch).Scan(&id)
}

func runtimeContentReady(ctx context.Context, tx pgx.Tx, e Execution, turn uuid.UUID, requireOpen bool) error {
	o, err := lockSession(ctx, tx, e.EnvironmentID, e.SessionID)
	if err != nil {
		return err
	}
	if err := executionReady(ctx, tx, e, o); err != nil {
		return err
	}
	var open bool
	if err := tx.QueryRow(ctx, `SELECT status='running' AND (NOT $3 OR processing_closed_at IS NULL) AND (deadline_at IS NULL OR deadline_at>clock_timestamp()) FROM turns WHERE environment_id=$1 AND id=$2`, e.EnvironmentID, turn, requireOpen).Scan(&open); err != nil {
		return err
	}
	if !open {
		return ErrNotReady
	}
	return nil
}
