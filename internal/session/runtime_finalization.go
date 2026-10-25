package session

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"github.com/helmrdotdev/helmr/internal/audit"
	"github.com/helmrdotdev/helmr/internal/auditpolicy"
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
	return lockRuntimeOperationOwnerWithHostLocked(ctx, tx, original, allowStopped)
}

// The caller holds supply claims before taking any Slack/Session owner locks.
func lockRuntimeOperationOwnerWithHostLocked(ctx context.Context, tx pgx.Tx, original Execution, allowStopped bool) error {
	owner, err := lockSession(ctx, tx, original.EnvironmentID, original.SessionID)
	if err != nil {
		return err
	}
	current := original
	if err := tx.QueryRow(ctx, `SELECT computer_lease_epoch FROM session_processes WHERE environment_id=$1 AND session_id=$2 AND epoch=$3 FOR NO KEY UPDATE`, original.EnvironmentID, original.SessionID, original.ProcessEpoch).Scan(&current.LeaseEpoch); err != nil {
		return err
	}
	_, err = runtimeProcessAuthorityWithOwnerLocked(ctx, tx, current, allowStopped, owner)
	return err
}

func RuntimeCloseProcessing(ctx context.Context, auditPolicy auditpolicy.Policy, pool db.TxBeginner, host workergroup.HostPrincipal, e Execution, turn uuid.UUID) error {
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
		return closeProcessing(ctx, auditPolicy, tx, e, turn)
	}))
}

var errResultDrainPending = errors.Join(ErrNotReady, errors.New("callback receipts pending"))

// RuntimeFinalize records the original result or observes its exact terminal
// outcome. Nil means callback receipts are still pending.
func RuntimeFinalize(ctx context.Context, auditPolicy auditpolicy.Policy, pool db.TxBeginner, host workergroup.HostPrincipal, e Execution, turn uuid.UUID, result json.RawMessage, evidence string) (json.RawMessage, error) {
	digest, err := digestJSON(result)
	if err != nil {
		return nil, ErrInvalidInput
	}
	if evidence == "" {
		return nil, ErrInvalidInput
	}
	var outcome json.RawMessage
	err = db.RunTx(ctx, pool, func(tx pgx.Tx) error {
		if err := lockRuntimeHost(ctx, tx, Caller{Kind: "session", ID: e.SessionID, Execution: e, Host: &host}); err != nil {
			return err
		}
		publication, err := lockSlackPublication(ctx, tx, e.EnvironmentID, e.SessionID)
		if err != nil {
			return err
		}
		if err := lockRuntimeOperationOwnerWithHostLocked(ctx, tx, e, false); err != nil {
			return err
		}
		if err := expireSessionDeadlineTx(ctx, auditPolicy, tx, e.EnvironmentID, e.SessionID); err != nil {
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
			return lockRuntimeOperationOwnerWithHostLocked(ctx, tx, e, false)
		}
		if prior != nil {
			return lockRuntimeOperationOwnerWithHostLocked(ctx, tx, e, false)
		}
		err = recordResult(ctx, tx, e, turn, result, digest, evidence)
		if errors.Is(err, errResultDrainPending) {
			return nil
		}
		if err != nil {
			return err
		}
		if err = auditRuntimeEvent(ctx, tx, auditPolicy, e, turn, "turn.completed", "succeeded", audit.LifecycleDetails{}); err != nil {
			return err
		}
		var sequence int64
		var responseID *uuid.UUID
		if err = tx.QueryRow(ctx, `SELECT s.next_event_seq-1,t.response_id,t.response FROM sessions s JOIN turns t ON (t.environment_id,t.session_id)=(s.environment_id,s.id) WHERE s.environment_id=$1 AND s.id=$2 AND t.id=$3`, e.EnvironmentID, e.SessionID, turn).Scan(&sequence, &responseID, &response); err != nil {
			return err
		}
		if responseID != nil {
			if err = publication.accept(ctx, tx, e.EnvironmentID, e.SessionID, turn, sequence); err != nil {
				return err
			}
		}
		outcome, err = runtimeTurnOutcome("completed", result, nil, nil, response)
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
