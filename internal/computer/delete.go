package computer

import (
	"context"
	"encoding/json"
	"errors"
	"uuid"

	"github.com/helmrdotdev/helmr/internal/db"
	"github.com/helmrdotdev/helmr/internal/idempotency"
	"github.com/jackc/pgx/v5"
)

type Deletion struct {
	Scope          Scope
	ComputerID     uuid.UUID
	IdempotencyKey string
}
type Deleted struct {
	ComputerID uuid.UUID
	Replayed   bool
}
type deleteReceipt struct {
	ComputerID string `json:"computerId"`
}

// Delete retains the Computer identity and records physical release intent only
// after logical members and command scopes have settled. A lease remains charged
// and exclusive until its owning worker or independent fence proves it stopped.
func Delete(ctx context.Context, txb db.TxBeginner, deletion Deletion) (Deleted, error) {
	var result Deleted
	err := db.RunTx(ctx, txb, func(tx pgx.Tx) error {
		var found bool
		if err := tx.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM computers c JOIN environments e ON e.id=c.environment_id WHERE c.environment_id=$1 AND c.id=$2 AND e.org_id=$3 AND e.project_id=$4)`, deletion.Scope.EnvironmentID, deletion.ComputerID, deletion.Scope.OrgID, deletion.Scope.ProjectID).Scan(&found); err != nil {
			return err
		}
		if !found {
			return ErrNotFound
		}
		claims, err := idempotency.TransactionFor(tx)
		if err != nil {
			return err
		}
		var accepted *db.PlatformRetryKey
		if deletion.IdempotencyKey != "" {
			request, err := idempotency.NewComputerDeleteRequest(deletion.Scope.EnvironmentID, deletion.ComputerID, deletion.IdempotencyKey)
			if err != nil {
				return err
			}
			acquired, err := claims.Acquire(ctx, request)
			if err != nil {
				return err
			}
			if !acquired.New {
				var receipt deleteReceipt
				if json.Unmarshal(acquired.Claim.Receipt, &receipt) != nil || receipt.ComputerID != deletion.ComputerID.String() || !acquired.Claim.ComputerID.Valid || uuid.UUID(acquired.Claim.ComputerID.Bytes) != deletion.ComputerID {
					return ErrReceiptInvalid
				}
				result = Deleted{ComputerID: deletion.ComputerID, Replayed: true}
				return nil
			}
			accepted = &acquired.Claim
		}
		var deleted bool
		if err = tx.QueryRow(ctx, `SELECT deleted_at IS NOT NULL FROM computers WHERE environment_id=$1 AND id=$2 FOR NO KEY UPDATE`, deletion.Scope.EnvironmentID, deletion.ComputerID).Scan(&deleted); err != nil {
			return err
		}
		if !deleted {
			var busy bool
			if err = tx.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM sessions WHERE environment_id=$1 AND computer_id=$2 AND status IN ('open','closing'))
 OR EXISTS(SELECT 1 FROM session_processes WHERE environment_id=$1 AND computer_id=$2 AND fenced_at IS NULL)
 OR EXISTS(SELECT 1 FROM computer_commands WHERE environment_id=$1 AND computer_id=$2 AND (terminal_at IS NULL OR (computer_lease_epoch IS NOT NULL AND process_reconciled_at IS NULL)))
 OR EXISTS(SELECT 1 FROM computer_saves WHERE environment_id=$1 AND computer_id=$2 AND status IN ('requested','captured'))`, deletion.Scope.EnvironmentID, deletion.ComputerID).Scan(&busy); err != nil {
				return err
			}
			if busy {
				return ErrBusy
			}
			if _, err = tx.Exec(ctx, `UPDATE computers SET deleted_at=clock_timestamp(),updated_at=clock_timestamp() WHERE environment_id=$1 AND id=$2`, deletion.Scope.EnvironmentID, deletion.ComputerID); err != nil {
				return err
			}
			if _, err = tx.Exec(ctx, `UPDATE computer_checkpoints SET status='lost',capture_request=NULL,terminal_evidence='Computer deletion retired continuation' WHERE environment_id=$1 AND computer_id=$2 AND status IN ('capturing','sealed','ready','restoring','aborting')`, deletion.Scope.EnvironmentID, deletion.ComputerID); err != nil {
				return err
			}
			if _, err = tx.Exec(ctx, `UPDATE computer_leases SET status='releasing' WHERE environment_id=$1 AND computer_id=$2 AND fenced_at IS NULL AND status IN ('acquiring','active')`, deletion.Scope.EnvironmentID, deletion.ComputerID); err != nil {
				return err
			}
		}
		if _, err = tx.Exec(ctx, `UPDATE computers c SET key=NULL WHERE environment_id=$1 AND id=$2 AND NOT EXISTS(SELECT 1 FROM computer_leases l WHERE l.environment_id=c.environment_id AND l.computer_id=c.id AND l.fenced_at IS NULL)`, deletion.Scope.EnvironmentID, deletion.ComputerID); err != nil {
			return err
		}
		if accepted != nil {
			receipt, err := json.Marshal(deleteReceipt{ComputerID: deletion.ComputerID.String()})
			if err != nil {
				return err
			}
			if _, err = claims.Complete(ctx, *accepted, idempotency.Target{ComputerID: deletion.ComputerID}, receipt); err != nil {
				return err
			}
		}
		result = Deleted{ComputerID: deletion.ComputerID}
		return nil
	})
	if errors.Is(err, pgx.ErrNoRows) {
		err = ErrNotFound
	}
	if err != nil {
		return Deleted{}, err
	}
	return result, nil
}
