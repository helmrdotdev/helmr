package agent

import (
	"context"
	"uuid"

	"github.com/helmrdotdev/helmr/internal/db"
	"github.com/helmrdotdev/helmr/internal/workergroup"
	"github.com/jackc/pgx/v5"
)

// HostAllocation identifies retained physical custody, including assignments
// whose execution delivery has closed. It carries no execution credential.
type HostAllocation struct {
	Kind          string
	EnvironmentID uuid.UUID
	OwnerID       uuid.UUID
	Epoch         int64
	InstanceID    uuid.UUID
}

func (r HostAllocation) valid() bool {
	return (r.Kind == "computer" || r.Kind == "preparation") && r.EnvironmentID != uuid.Nil() && r.OwnerID != uuid.Nil() && r.InstanceID != uuid.Nil() && r.Epoch > 0
}

// ListHostAllocations enumerates this authenticated incarnation's unfenced
// assignments regardless of expiry, revocation or logical status. Only joined
// physical closure removes an assignment. An empty page finishes the scan;
// callers start their next poll from nil so newly inserted identities are seen.
func ListHostAllocations(ctx context.Context, database db.TxBeginner, host workergroup.HostPrincipal, after *HostAllocation) ([]HostAllocation, error) {
	cursor := HostAllocation{}
	if after != nil {
		if !after.valid() {
			return nil, ErrInvalidInput
		}
		cursor = *after
	}
	var result []HostAllocation
	err := db.RunTx(ctx, database, func(tx pgx.Tx) error {
		if err := allocationLockTimeout(ctx, tx); err != nil {
			return err
		}
		if err := lockComputerHost(ctx, tx, host); err != nil {
			return err
		}
		rows, err := tx.Query(ctx, `SELECT kind,environment_id,owner_id,epoch,instance_id FROM (
 SELECT 'computer'::text kind,environment_id,computer_id owner_id,epoch,computer_instance_id instance_id FROM computer_leases
 WHERE worker_host_id=$1 AND worker_epoch=$2 AND fenced_at IS NULL
 UNION ALL
 SELECT 'preparation',environment_id,id,executor_epoch,instance_id FROM computer_preparations
 WHERE worker_host_id=$1 AND worker_epoch=$2 AND fenced_at IS NULL
 ) assignments WHERE (kind,environment_id,owner_id,epoch)>($3,$4,$5,$6)
 ORDER BY kind,environment_id,owner_id,epoch LIMIT 100`, host.HostID, host.Epoch, cursor.Kind, cursor.EnvironmentID, cursor.OwnerID, cursor.Epoch)
		if err != nil {
			return err
		}
		result, err = pgx.CollectRows(rows, func(row pgx.CollectableRow) (HostAllocation, error) {
			var r HostAllocation
			err := row.Scan(&r.Kind, &r.EnvironmentID, &r.OwnerID, &r.Epoch, &r.InstanceID)
			return r, err
		})
		return err
	})
	if err != nil {
		return nil, allocationError(err)
	}
	return result, nil
}
