package session

import (
	"context"
	"errors"
	"uuid"

	"github.com/helmrdotdev/helmr/internal/computer"
	"github.com/helmrdotdev/helmr/internal/db"
	"github.com/helmrdotdev/helmr/internal/workergroup"
	"github.com/jackc/pgx/v5"
)

type AllocationProgram struct {
	DeploymentID uuid.UUID
	BundleDigest string
}

// ReadAllocationProgram selects immutable bytes from the initial admitted cohort.
// It grants no attachment, setup, secret exposure or execution authority. Later
// process admission requires an active lease, so acquisition retries see the
// same cohort even if one of its processes has subsequently been fenced.
func ReadAllocationProgram(ctx context.Context, database db.TxBeginner, host workergroup.HostPrincipal, identity computer.LeaseIdentity) (*AllocationProgram, error) {
	if !identity.Valid() {
		return nil, ErrInvalidInput
	}
	var result *AllocationProgram
	err := db.RunTx(ctx, database, func(tx pgx.Tx) error {
		if err := shortLockTimeout(ctx, tx); err != nil {
			return err
		}
		if err := computer.LockHost(ctx, tx, host); err != nil {
			return err
		}
		var owned, eligible bool
		if err := tx.QueryRow(ctx, `SELECT l.worker_host_id=$5 AND l.worker_epoch=$6 AND l.allocation_id=$4 AND l.fenced_at IS NULL AND c.deleted_at IS NULL,
 l.status='acquiring' AND l.initialized_at IS NULL AND (l.restored_from_save_id IS NULL OR l.disk_only_recovery)
 FROM computer_leases l JOIN computers c ON(c.environment_id,c.id)=(l.environment_id,l.computer_id)
 WHERE l.environment_id=$1 AND l.computer_id=$2 AND l.epoch=$3`, identity.EnvironmentID, identity.ComputerID, identity.Epoch, identity.InstanceID, host.HostID, host.Epoch).Scan(&owned, &eligible); err != nil {
			return err
		}
		if !owned {
			return ErrDenied
		}
		if !eligible {
			return nil
		}
		var program AllocationProgram
		err := tx.QueryRow(ctx, `SELECT s.deployment_id,d.bundle_digest FROM session_processes p
 JOIN sessions s ON(s.environment_id,s.id)=(p.environment_id,p.session_id)
 JOIN deployments d ON(d.environment_id,d.id)=(s.environment_id,s.deployment_id)
 WHERE p.environment_id=$1 AND p.computer_id=$2 AND p.computer_lease_epoch=$3
 ORDER BY p.session_id,p.epoch LIMIT 1`, identity.EnvironmentID, identity.ComputerID, identity.Epoch).Scan(&program.DeploymentID, &program.BundleDigest)
		if errors.Is(err, pgx.ErrNoRows) {
			return nil
		}
		if err != nil {
			return err
		}
		result = &program
		return nil
	})
	return result, notReadyOnLock(err)
}
