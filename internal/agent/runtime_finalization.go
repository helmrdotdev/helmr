package agent

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"strings"
	"time"
	"uuid"

	"github.com/helmrdotdev/helmr/internal/db"
	"github.com/helmrdotdev/helmr/internal/workergroup"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
)

// Retained operations carry their original business grant. A restored physical
// owner can observe their committed receipts through the same logical process;
// this check never promotes that old grant into authority for a new mutation.
func lockRuntimeOperationOwner(ctx context.Context, tx pgx.Tx, host workergroup.HostPrincipal, original Execution) error {
	return lockRuntimeOperationOwnerState(ctx, tx, host, original, false)
}

func lockRuntimeOperationOwnerState(ctx context.Context, tx pgx.Tx, host workergroup.HostPrincipal, original Execution, allowStopped bool) error {
	if err := lockRuntimeHost(ctx, tx, Caller{Kind: "session", ID: original.SessionID, Execution: original, Host: &host}); err != nil {
		return err
	}
	if _, err := lockSession(ctx, tx, original.EnvironmentID, original.SessionID); err != nil {
		return err
	}
	current := original
	if err := tx.QueryRow(ctx, `SELECT computer_lease_epoch FROM session_processes WHERE environment_id=$1 AND session_id=$2 AND epoch=$3 FOR NO KEY UPDATE`, original.EnvironmentID, original.SessionID, original.ProcessEpoch).Scan(&current.LeaseEpoch); err != nil {
		return err
	}
	_, err := lockRuntimeProcessAuthority(ctx, tx, host, current, allowStopped)
	return err
}

func RuntimeCloseProcessing(ctx context.Context, pool db.TxBeginner, host workergroup.HostPrincipal, e Execution, turn uuid.UUID) error {
	return hideMissing(db.RunTx(ctx, pool, func(tx pgx.Tx) error {
		if err := lockRuntimeOperationOwner(ctx, tx, host, e); err != nil {
			return err
		}
		var closed *time.Time
		if err := tx.QueryRow(ctx, `SELECT processing_closed_at FROM turns WHERE environment_id=$1 AND session_id=$2 AND id=$3 AND process_epoch=$4 FOR NO KEY UPDATE`, e.EnvironmentID, e.SessionID, turn, e.ProcessEpoch).Scan(&closed); err != nil {
			return err
		}
		if closed != nil {
			return lockRuntimeOperationOwner(ctx, tx, host, e)
		}
		return closeProcessing(ctx, tx, e, turn)
	}))
}

var errResultDrainPending = errors.Join(ErrNotReady, errors.New("callback receipts pending"))

// RuntimeFinalize records the original result or observes its exact terminal
// outcome. Nil means callback receipts or the mandatory own save are pending, never success.
func RuntimeFinalize(ctx context.Context, pool db.TxBeginner, host workergroup.HostPrincipal, e Execution, turn uuid.UUID, result json.RawMessage, evidence string) (json.RawMessage, error) {
	digest, err := digestJSON(result)
	if err != nil {
		return nil, ErrInvalidInput
	}
	if evidence == "" {
		return nil, ErrInvalidInput
	}
	var outcome json.RawMessage
	err = db.RunTx(ctx, pool, func(tx pgx.Tx) error {
		if err := lockRuntimeOperationOwner(ctx, tx, host, e); err != nil {
			return err
		}
		var status string
		var prior []byte
		var stored, response json.RawMessage
		var code *string
		var message []byte
		if err := tx.QueryRow(ctx, `SELECT status,result_digest,result,error_code,error_message,response FROM turns WHERE environment_id=$1 AND session_id=$2 AND id=$3 AND process_epoch=$4 FOR NO KEY UPDATE`, e.EnvironmentID, e.SessionID, turn, e.ProcessEpoch).Scan(&status, &prior, &stored, &code, &message, &response); err != nil {
			return err
		}
		if prior != nil && !bytes.Equal(prior, digest[:]) {
			return ErrConflict
		}
		if status == "completed" || status == "failed" || status == "interrupted" || status == "cancelled" {
			var err error
			outcome, err = runtimeTurnOutcome(status, stored, code, message, response)
			if err != nil {
				return err
			}
			return lockRuntimeOperationOwner(ctx, tx, host, e)
		}
		if prior != nil {
			return lockRuntimeOperationOwner(ctx, tx, host, e)
		}
		var save SaveRequest
		err := recordResult(ctx, tx, e, turn, result, digest, evidence, &save)
		if errors.Is(err, errResultDrainPending) {
			return nil
		}
		return err
	})
	if err != nil {
		var invalid *pgconn.PgError
		if errors.As(err, &invalid) && strings.HasPrefix(invalid.Code, "22") {
			return nil, ErrInvalidInput
		}
		return nil, hideMissing(err)
	}
	return outcome, nil
}

func runtimeTurnOutcome(status string, result json.RawMessage, code *string, message []byte, response json.RawMessage) (json.RawMessage, error) {
	value := map[string]any{"status": status}
	if code != nil && message != nil {
		value["error"] = TurnError{Code: *code, Message: string(message)}
	}
	if status == "completed" {
		if len(result) == 0 {
			return nil, ErrNotReady
		}
		value["result"] = result
		if response != nil {
			value["response"] = response
		}
	}
	return json.Marshal(value)
}
