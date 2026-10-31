package computer

import (
	"context"
	"encoding/json"
	"errors"
	"github.com/helmrdotdev/helmr/internal/audit"
	"github.com/helmrdotdev/helmr/internal/auditpolicy"
	"github.com/helmrdotdev/helmr/internal/auth"
	"uuid"

	"github.com/helmrdotdev/helmr/internal/db"
	"github.com/helmrdotdev/helmr/internal/idempotency"
	"github.com/helmrdotdev/helmr/internal/pgvalue"
	"github.com/jackc/pgx/v5"
)

type Deletion struct {
	Principal      auth.Principal
	Scope          Scope
	ComputerID     uuid.UUID
	IdempotencyKey string
	// DryRun counts what deletion would cancel without changing anything.
	DryRun bool
	// CancelSessions cancels, or with dryRun counts, the Sessions placed on
	// the Computer and the Sessions they own. It runs before the Computer lock
	// and takes the Environment admission lock. It is required.
	CancelSessions func(ctx context.Context, tx pgx.Tx, env, computerID uuid.UUID, actor audit.Evidence, dryRun bool) (int64, error)
}
type Deleted struct {
	ComputerID        uuid.UUID
	Replayed          bool
	SessionsCancelled int64
	CommandsCancelled int64
}
type deleteReceipt struct {
	ComputerID        string `json:"computerId"`
	SessionsCancelled int64  `json:"sessionsCancelled"`
	CommandsCancelled int64  `json:"commandsCancelled"`
}

// Delete cancels the Computer's Sessions and live Commands, then retains the
// Computer identity in a deleting state. Process fencing, Command
// reconciliation and in-flight saves settle through lease release. A lease
// remains charged and exclusive until its owning worker or independent fence
// proves it stopped.
func Delete(ctx context.Context, auditPolicy auditpolicy.Policy, txb db.TxBeginner, deletion Deletion) (Deleted, error) {
	var result Deleted
	env, id := deletion.Scope.EnvironmentID, deletion.ComputerID
	if deletion.CancelSessions == nil {
		return result, errors.New("computer deletion requires Session cancellation")
	}
	cancelSessions := func(tx pgx.Tx, actor audit.Evidence, dryRun bool) (int64, error) {
		return deletion.CancelSessions(ctx, tx, env, id, actor, dryRun)
	}
	err := db.RunTx(ctx, txb, func(tx pgx.Tx) error {
		var found bool
		if err := tx.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM computers c JOIN environments e ON e.id=c.environment_id WHERE c.environment_id=$1 AND c.id=$2 AND e.org_id=$3 AND e.project_id=$4)`, env, id, deletion.Scope.OrgID, deletion.Scope.ProjectID).Scan(&found); err != nil {
			return err
		}
		if !found {
			return ErrNotFound
		}
		actor, err := operationActor(ctx, tx, deletion.Principal, deletion.Scope, auth.PermissionComputersDelete)
		if err != nil {
			return err
		}
		if deletion.DryRun {
			result = Deleted{ComputerID: id}
			if result.SessionsCancelled, err = cancelSessions(tx, actor, true); err != nil {
				return err
			}
			return tx.QueryRow(ctx, `SELECT count(*) FROM commands WHERE environment_id=$1 AND computer_id=$2 AND terminal_at IS NULL`, env, id).Scan(&result.CommandsCancelled)
		}
		var request *idempotency.Request
		if deletion.IdempotencyKey != "" {
			request, err = idempotency.NewComputerDeleteRequest(env, id, deletion.IdempotencyKey)
			if err != nil {
				return err
			}
		}
		return idempotency.Run(ctx, tx, request, func(tx pgx.Tx, acquired idempotency.Result) (idempotency.Completion, error) {
			if !acquired.New {
				var receipt deleteReceipt
				if json.Unmarshal(acquired.Claim.Receipt, &receipt) != nil || receipt.ComputerID != id.String() || !acquired.Claim.ComputerID.Valid || uuid.UUID(acquired.Claim.ComputerID.Bytes) != id {
					return idempotency.Completion{}, ErrReceiptInvalid
				}
				result = Deleted{ComputerID: id, Replayed: true, SessionsCancelled: receipt.SessionsCancelled, CommandsCancelled: receipt.CommandsCancelled}
				return idempotency.Completion{}, nil
			}
			result = Deleted{ComputerID: id}
			if result.SessionsCancelled, err = cancelSessions(tx, actor, false); err != nil {
				return idempotency.Completion{}, err
			}
			var deleted bool
			if err = tx.QueryRow(ctx, `SELECT deleted_at IS NOT NULL FROM computers WHERE environment_id=$1 AND id=$2 FOR NO KEY UPDATE`, env, id).Scan(&deleted); err != nil {
				return idempotency.Completion{}, err
			}
			if !deleted {
				// Invariant check: Session cancellation holds the admission lock, so
				// no Session can join the Computer after it; this is unreachable.
				var members bool
				if err = tx.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM sessions WHERE environment_id=$1 AND computer_id=$2 AND status<>'closed')`, env, id).Scan(&members); err != nil {
					return idempotency.Completion{}, err
				}
				if members {
					return idempotency.Completion{}, errors.New("deleted Computer retains a live Session")
				}
				if result.CommandsCancelled, err = cancelLiveCommands(ctx, auditPolicy, tx, env, id, actor); err != nil {
					return idempotency.Completion{}, err
				}
			}
			if err = deleteLocked(ctx, tx, env, id, deleted); err != nil {
				return idempotency.Completion{}, err
			}
			receipt, err := json.Marshal(deleteReceipt{ComputerID: id.String(), SessionsCancelled: result.SessionsCancelled, CommandsCancelled: result.CommandsCancelled})
			if err != nil {
				return idempotency.Completion{}, err
			}
			if !deleted {
				if err = audit.Append(ctx, tx, auditPolicy, audit.Event{Evidence: actor, EnvironmentID: env, ComputerID: id, Action: "computer.deleted", TargetKind: "computer", TargetID: id}); err != nil {
					return idempotency.Completion{}, err
				}
			}
			return idempotency.Completion{Target: idempotency.Target{ComputerID: id}, Receipt: receipt}, nil
		})
	})
	if errors.Is(err, pgx.ErrNoRows) {
		err = ErrNotFound
	}
	if err != nil {
		return Deleted{}, err
	}
	return result, nil
}

// cancelLiveCommands cancels every unfinished Command under the Computer lock
// through the same transition and audit facts as a Command cancellation. An
// unassigned Command ends now; an assigned one stops and settles when its
// lease is released.
func cancelLiveCommands(ctx context.Context, auditPolicy auditpolicy.Policy, tx pgx.Tx, env, id uuid.UUID, actor audit.Evidence) (int64, error) {
	rows, err := tx.Query(ctx, `SELECT id FROM commands WHERE environment_id=$1 AND computer_id=$2 AND terminal_at IS NULL ORDER BY id`, env, id)
	if err != nil {
		return 0, err
	}
	live, err := pgx.CollectRows(rows, pgx.RowTo[uuid.UUID])
	if err != nil {
		return 0, err
	}
	q := db.New(tx)
	for _, command := range live {
		changed, err := q.RequestCommandCancellation(ctx, db.RequestCommandCancellationParams{EnvironmentID: pgvalue.UUID(env), CommandID: pgvalue.UUID(command)})
		if err != nil {
			return 0, err
		}
		if err = audit.Append(ctx, tx, auditPolicy, audit.Event{Evidence: actor, EnvironmentID: env, ComputerID: id, Action: "command.cancel_requested", Outcome: "accepted", TargetKind: "command", TargetID: command, Details: audit.LifecycleDetails{}}); err != nil {
			return 0, err
		}
		if changed.Status == "cancelled" {
			if err = audit.Append(ctx, tx, auditPolicy, audit.Event{Evidence: actor, EnvironmentID: env, ComputerID: id, Action: "command.cancelled", Outcome: "cancelled", TargetKind: "command", TargetID: command, Details: audit.LifecycleDetails{}}); err != nil {
				return 0, err
			}
		}
	}
	return int64(len(live)), nil
}

// unusedComputerBusySQL reports work that keeps a Computer in use: a Session,
// a process or a Command. It also reports, separately, a save in progress,
// which only defers deletion and never restarts the unused clock. Explicit
// deletion cancels these instead.
const unusedComputerBusySQL = `SELECT EXISTS(SELECT 1 FROM sessions WHERE environment_id=$1 AND computer_id=$2 AND status IN ('open','closing'))
 OR EXISTS(SELECT 1 FROM session_processes WHERE environment_id=$1 AND computer_id=$2 AND fenced_at IS NULL)
 OR EXISTS(SELECT 1 FROM commands WHERE environment_id=$1 AND computer_id=$2 AND (terminal_at IS NULL OR (computer_lease_epoch IS NOT NULL AND process_reconciled_at IS NULL))),
 EXISTS(SELECT 1 FROM computer_saves WHERE environment_id=$1 AND computer_id=$2 AND status IN ('requested','captured'))`

// deleteLocked marks a locked Computer deleted. The caller has settled its
// logical members.
func deleteLocked(ctx context.Context, tx pgx.Tx, env, id uuid.UUID, deleted bool) error {
	var err error
	if !deleted {
		if _, err = tx.Exec(ctx, `UPDATE computers SET deleted_at=clock_timestamp(),updated_at=clock_timestamp() WHERE environment_id=$1 AND id=$2`, env, id); err != nil {
			return err
		}
		if _, err = tx.Exec(ctx, `UPDATE checkpoints SET status='lost',capture_request=NULL,terminal_evidence='Computer deletion retired continuation' WHERE environment_id=$1 AND computer_id=$2 AND status IN ('capturing','sealed','ready','restoring','aborting')`, env, id); err != nil {
			return err
		}
		if _, err = tx.Exec(ctx, `UPDATE computer_leases SET status='releasing' WHERE environment_id=$1 AND computer_id=$2 AND fenced_at IS NULL AND status IN ('acquiring','active')`, env, id); err != nil {
			return err
		}
	}
	if _, err = tx.Exec(ctx, `UPDATE computers c SET key=NULL WHERE environment_id=$1 AND id=$2 AND NOT EXISTS(SELECT 1 FROM computer_leases l WHERE l.environment_id=c.environment_id AND l.computer_id=c.id AND l.fenced_at IS NULL)`, env, id); err != nil {
		return err
	}
	return notifyComputerChanged(ctx, tx, env, id)
}
