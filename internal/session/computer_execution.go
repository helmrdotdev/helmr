package session

import (
	"context"
	"strings"
	"unicode/utf8"
	"uuid"

	"github.com/helmrdotdev/helmr/internal/computer"
	"github.com/helmrdotdev/helmr/internal/db"
	"github.com/helmrdotdev/helmr/internal/org"
	"github.com/helmrdotdev/helmr/internal/workergroup"
	"github.com/jackc/pgx/v5"
)

type ComputerExecutionRequest struct {
	EnvironmentID  uuid.UUID
	ComputerID     uuid.UUID
	Kind           string
	IdempotencyKey string
	OpenSessionID  uuid.UUID
	ExpectedScope  string
}

// ControlComputerExecution owns membership and authorization for the whole VM.
// A Session hold is neither created nor released by this operation.
func ControlComputerExecution(ctx context.Context, pool db.TxBeginner, caller Caller, req ComputerExecutionRequest) (computer.ExecutionControl, error) {
	var result computer.ExecutionControl
	if req.EnvironmentID == uuid.Nil() || req.ComputerID == uuid.Nil() || caller.ID == uuid.Nil() || (req.Kind != "hibernate" && req.Kind != "wake") || req.IdempotencyKey == "" || len(req.IdempotencyKey) > 512 || !utf8.ValidString(req.IdempotencyKey) || strings.ContainsRune(req.IdempotencyKey, 0) {
		return result, ErrInvalidInput
	}
	err := db.RunTx(ctx, pool, func(tx pgx.Tx) error {
		if err := workergroup.LockExecutionSupply(ctx, tx, req.EnvironmentID); err != nil {
			return err
		}
		if err := authorizeComputerExecution(ctx, tx, caller, req.EnvironmentID); err != nil {
			return err
		}
		if _, err := org.LockAdmission(ctx, tx, req.EnvironmentID); err != nil {
			return err
		}
		if _, err := LockComputerMembers(ctx, tx, req.EnvironmentID, req.ComputerID); err != nil {
			return err
		}
		if err := authorizeComputerExecution(ctx, tx, caller, req.EnvironmentID); err != nil {
			return err
		}
		if req.OpenSessionID != uuid.Nil() {
			var open bool
			if err := tx.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM sessions WHERE environment_id=$1 AND id=$2 AND computer_id=$3 AND status='open')`, req.EnvironmentID, req.OpenSessionID, req.ComputerID).Scan(&open); err != nil {
				return err
			}
			if !open {
				return ErrTerminal
			}
		}
		var deleted bool
		if err := tx.QueryRow(ctx, `SELECT deleted_at IS NOT NULL FROM computers WHERE environment_id=$1 AND id=$2`, req.EnvironmentID, req.ComputerID).Scan(&deleted); err != nil {
			return err
		}
		if deleted {
			return computer.ErrDeleting
		}
		var found bool
		var err error
		result, found, err = computer.FindExecutionControl(ctx, tx, req.EnvironmentID, req.ComputerID, caller.Kind, caller.ID, req.Kind, req.IdempotencyKey)
		if err != nil || found {
			return err
		}
		if req.ExpectedScope != "" {
			scope, err := ReadComputerExecutionScope(ctx, tx, req.EnvironmentID, req.ComputerID)
			if err != nil {
				return err
			}
			if scope.Digest != req.ExpectedScope {
				return ErrConflict
			}
		}
		if err = computer.RequireSecretsNotRevoked(ctx, tx, req.EnvironmentID, req.ComputerID); err != nil {
			return err
		}
		var sleeping, awake bool
		if err = tx.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM checkpoints WHERE environment_id=$1 AND computer_id=$2 AND status='ready'),
   EXISTS(SELECT 1 FROM computer_leases l WHERE l.environment_id=$1 AND l.computer_id=$2 AND l.status='active' AND l.fenced_at IS NULL AND l.initialized_at IS NOT NULL AND NOT EXISTS(SELECT 1 FROM checkpoints cp WHERE cp.environment_id=l.environment_id AND cp.computer_id=l.computer_id AND (cp.status IN ('capturing','sealed','ready','restoring','aborting') OR (cp.status='consumed' AND cp.controls_reconciled_at IS NULL))))`, req.EnvironmentID, req.ComputerID).Scan(&sleeping, &awake); err != nil {
			return err
		}
		complete := sleeping
		if req.Kind == "hibernate" {
			busy, err := computerExecutionDemand(ctx, tx, req.EnvironmentID, req.ComputerID)
			if err != nil {
				return err
			}
			if busy {
				return computer.ErrBusy
			}
			if !sleeping && !awake {
				return ErrNotReady
			}
		} else {
			complete = awake
			if !awake && !sleeping {
				return ErrNotReady
			}
			if !awake {
				if err = computer.RequireExecutionCapacity(ctx, tx, req.EnvironmentID, req.ComputerID); err != nil {
					return err
				}
			}
			// Wake arms after_turn against a future completion, including on a VM
			// that was already awake. Existing holds and Turn identities are untouched.
			if _, err = tx.Exec(ctx, `UPDATE sessions SET suspension_after_seq=next_turn_seq-1 WHERE environment_id=$1 AND computer_id=$2 AND status='open'`, req.EnvironmentID, req.ComputerID); err != nil {
				return err
			}
		}
		result, err = computer.RequestExecutionControl(ctx, tx, req.EnvironmentID, req.ComputerID, caller.Kind, caller.ID, req.Kind, req.IdempotencyKey, complete)
		return err
	})
	if err != nil {
		return computer.ExecutionControl{}, hideMissing(err)
	}
	return result, nil
}

func authorizeComputerExecution(ctx context.Context, tx pgx.Tx, caller Caller, env uuid.UUID) error {
	var allowed bool
	var err error
	switch caller.Kind {
	case "user":
		err = tx.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM environments e JOIN org_members m ON m.org_id=e.org_id WHERE e.id=$1 AND m.user_id=$2 AND m.disabled_at IS NULL AND m.role IN ('owner','admin','developer'))`, env, caller.ID).Scan(&allowed)
	case "api_key":
		err = tx.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM api_keys k JOIN environments e ON (e.id,e.project_id,e.org_id)=(k.environment_id,k.project_id,k.org_id) WHERE e.id=$1 AND k.id=$2 AND k.revoked_at IS NULL AND (k.expires_at IS NULL OR k.expires_at>clock_timestamp()) AND k.role IN ('owner','admin','developer') AND 'computers.update'=ANY(k.permissions))`, env, caller.ID).Scan(&allowed)
	default:
		return ErrDenied
	}
	if err != nil {
		return err
	}
	if !allowed {
		return ErrDenied
	}
	return nil
}
