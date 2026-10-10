package agent

import (
	"context"
	"encoding/json"
	"strings"
	"time"
	"uuid"

	"github.com/helmrdotdev/helmr/internal/db"
	"github.com/helmrdotdev/helmr/internal/workergroup"
	"github.com/jackc/pgx/v5"
)

type TurnError struct {
	Code    string `json:"code"`
	Message string `json:"message"`
}

// RuntimeFail settles an application exception only after native convergence.
// Retained terminal receipts remain observable without admitting fresh work.
func RuntimeFail(ctx context.Context, pool db.TxBeginner, host workergroup.HostPrincipal, e Execution, turn uuid.UUID, failure TurnError, evidence string) (json.RawMessage, error) {
	if failure.Code == "" || evidence == "" || strings.ContainsRune(failure.Code+evidence, 0) {
		return nil, ErrInvalidInput
	}
	var outcome json.RawMessage
	err := db.RunTx(ctx, pool, func(tx pgx.Tx) error {
		if err := lockRuntimeOperationOwner(ctx, tx, host, e); err != nil {
			return err
		}
		var status string
		var result, response json.RawMessage
		var code *string
		var message []byte
		var closed *time.Time
		if err := tx.QueryRow(ctx, `SELECT status,result,error_code,error_message,processing_closed_at,response FROM turns WHERE environment_id=$1 AND session_id=$2 AND id=$3 AND process_epoch=$4 FOR NO KEY UPDATE`, e.EnvironmentID, e.SessionID, turn, e.ProcessEpoch).Scan(&status, &result, &code, &message, &closed, &response); err != nil {
			return err
		}
		if code != nil && (*code != failure.Code || message == nil || string(message) != failure.Message) {
			return ErrConflict
		}
		if status == "completed" || status == "failed" || status == "interrupted" || status == "cancelled" {
			var err error
			outcome, err = runtimeTurnOutcome(status, result, code, message, response)
			if err != nil {
				return err
			}
			return lockRuntimeOperationOwner(ctx, tx, host, e)
		}
		if status != "running" || closed == nil {
			return ErrNotReady
		}
		if err := expireSessionDeadlineTx(ctx, tx, e.EnvironmentID, e.SessionID); err != nil {
			return err
		}
		if err := tx.QueryRow(ctx, `SELECT status FROM turns WHERE environment_id=$1 AND id=$2`, e.EnvironmentID, turn).Scan(&status); err != nil {
			return err
		}
		if status == "interrupted" {
			outcome, _ = runtimeTurnOutcome(status, nil, nil, nil, nil)
			return lockRuntimeOperationOwner(ctx, tx, host, e)
		}
		o, err := lockSession(ctx, tx, e.EnvironmentID, e.SessionID)
		if err != nil {
			return err
		}
		if err := executionReady(ctx, tx, e, o); err != nil {
			return err
		}
		if _, err := tx.Exec(ctx, `UPDATE turns SET status='failed',terminal_at=clock_timestamp(),error_code=$3,error_message=$4,drain_evidence=$5 WHERE environment_id=$1 AND id=$2`, e.EnvironmentID, turn, failure.Code, []byte(failure.Message), evidence); err != nil {
			return err
		}
		if err := cancelTurnAsks(ctx, tx, e.EnvironmentID, e.SessionID, turn); err != nil {
			return err
		}
		if err := rejectTurnMessages(ctx, tx, e.EnvironmentID, e.SessionID, turn, true, "turn_terminated"); err != nil {
			return err
		}
		if err := event(ctx, tx, e.EnvironmentID, e.SessionID, turn, "turn.failed"); err != nil {
			return err
		}
		outcome, err = runtimeTurnOutcome("failed", nil, &failure.Code, []byte(failure.Message), nil)
		return err
	})
	return outcome, hideMissing(err)
}
