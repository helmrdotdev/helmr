package agent

import (
	"context"
	"encoding/json"
	"errors"
	"uuid"

	"github.com/helmrdotdev/helmr/internal/db"
	"github.com/helmrdotdev/helmr/internal/workergroup"
	"github.com/jackc/pgx/v5"
)

// DispatchRuntimeTurn admits work only from the current physical attachment.
// A nil assignment means this process has no dispatchable work at present.
func DispatchRuntimeTurn(ctx context.Context, pool db.TxBeginner, host workergroup.HostPrincipal, e Execution, attachment int64) (*TurnDispatch, error) {
	var result *TurnDispatch
	err := db.RunTx(ctx, pool, func(tx pgx.Tx) error {
		authority, err := lockRuntimeAuthority(ctx, tx, host, e)
		if err != nil {
			return err
		}
		var current int64
		if err := tx.QueryRow(ctx, `SELECT attachment_sequence FROM session_processes
 WHERE environment_id=$1 AND session_id=$2 AND epoch=$3 FOR NO KEY UPDATE`, e.EnvironmentID, e.SessionID, e.ProcessEpoch).Scan(&current); err != nil {
			return err
		}
		if attachment <= 0 || attachment != current || e.AuthorityGeneration != authority.Generation {
			return ErrDenied
		}
		var assignment TurnDispatch
		err = dispatchTurn(ctx, tx, e, &assignment)
		if err != nil && !errors.Is(err, ErrNotReady) {
			return err
		}
		if err == nil {
			result = &assignment
		}
		// Lock waits must not admit work after the physical lease expires.
		_, err = lockRuntimeAuthority(ctx, tx, host, e)
		return err
	})
	if err != nil {
		return nil, hideMissing(err)
	}
	return result, nil
}

// ObserveRuntimeTurn verifies the exact retained runtime outcome before the host
// releases its delivery. Observation needs current physical ownership, not fresh
// business authority: a hold may have arrived after the terminal commit.
func ObserveRuntimeTurn(ctx context.Context, pool db.TxBeginner, host workergroup.HostPrincipal, e Execution, attachment int64, turn uuid.UUID, outcome json.RawMessage) (int64, error) {
	digest, err := digestJSON(outcome)
	if err != nil {
		return 0, ErrInvalidInput
	}
	var sequence int64
	err = db.RunTx(ctx, pool, func(tx pgx.Tx) error {
		if _, err := lockRuntimeAuthority(ctx, tx, host, e); err != nil {
			return err
		}
		var current int64
		if err := tx.QueryRow(ctx, `SELECT attachment_sequence FROM session_processes WHERE environment_id=$1 AND session_id=$2 AND epoch=$3 FOR NO KEY UPDATE`, e.EnvironmentID, e.SessionID, e.ProcessEpoch).Scan(&current); err != nil {
			return err
		}
		if attachment <= 0 || attachment != current {
			return ErrDenied
		}
		var status string
		var result, response json.RawMessage
		var code *string
		var message []byte
		if err := tx.QueryRow(ctx, `SELECT seq,status,result,error_code,error_message,response FROM turns WHERE environment_id=$1 AND session_id=$2 AND id=$3 AND process_epoch=$4 AND terminal_at IS NOT NULL`, e.EnvironmentID, e.SessionID, turn, e.ProcessEpoch).Scan(&sequence, &status, &result, &code, &message, &response); err != nil {
			return err
		}
		canonical, err := runtimeTurnOutcome(status, result, code, message, response)
		if err != nil {
			return err
		}
		expected, err := digestJSON(canonical)
		if err != nil {
			return err
		}
		if digest != expected {
			return ErrConflict
		}
		_, err = lockRuntimeAuthority(ctx, tx, host, e)
		return err
	})
	if err != nil {
		return 0, hideMissing(err)
	}
	return sequence, nil
}
