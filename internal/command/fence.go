package command

import (
	"context"
	"errors"
	"time"
	"uuid"

	"github.com/helmrdotdev/helmr/internal/db"
	"github.com/helmrdotdev/helmr/internal/pgvalue"
	"github.com/helmrdotdev/helmr/internal/workergroup"
	"github.com/jackc/pgx/v5"
)

// Lease is the exact physical writer to which a Command is bound. A restored
// Computer has a new epoch; it can never resume a Command from its old lease.
type Lease struct {
	EnvironmentID uuid.UUID
	ComputerID    uuid.UUID
	InstanceID    uuid.UUID
	Epoch         int64
	HostID        uuid.UUID
	WorkerEpoch   int64
	Status        string
	ExpiresAt     *time.Time
	FencedAt      *time.Time
}

func lockHost(ctx context.Context, tx pgx.Tx, host workergroup.HostPrincipal) error {
	eligible, err := workergroup.LockRuntimeHost(ctx, tx, host)
	if err != nil {
		return err
	}
	if !eligible {
		return pgx.ErrNoRows
	}
	return nil
}

func lockComputer(ctx context.Context, tx pgx.Tx, env, computer uuid.UUID) error {
	var id uuid.UUID
	return tx.QueryRow(ctx, `SELECT id FROM computers WHERE environment_id=$1 AND id=$2 FOR NO KEY UPDATE`, env, computer).Scan(&id)
}

const leaseColumns = `l.environment_id,l.computer_id,l.computer_instance_id,l.epoch,l.worker_host_id,l.worker_epoch,l.status,l.expires_at,l.fenced_at`

func readLease(row pgx.Row) (Lease, error) {
	var l Lease
	err := row.Scan(&l.EnvironmentID, &l.ComputerID, &l.InstanceID, &l.Epoch, &l.HostID, &l.WorkerEpoch, &l.Status, &l.ExpiresAt, &l.FencedAt)
	return l, err
}

// lockCommandLease reads only the immutable historical assignment. In particular,
// a pending Command does not acquire whichever newer lease happens to exist.
func lockCommandLease(ctx context.Context, tx pgx.Tx, target db.GetComputerCommandTargetRow, id uuid.UUID) (Lease, error) {
	env, computer := uuid.UUID(target.EnvironmentID.Bytes), uuid.UUID(target.ComputerID.Bytes)
	if err := lockComputer(ctx, tx, env, computer); err != nil {
		return Lease{}, err
	}
	l, err := readLease(tx.QueryRow(ctx, `SELECT `+leaseColumns+` FROM computer_leases l JOIN computer_commands c
 ON (c.environment_id,c.computer_id,c.computer_lease_epoch)=(l.environment_id,l.computer_id,l.epoch)
 WHERE c.environment_id=$1 AND c.id=$2 FOR NO KEY UPDATE OF l`, env, id))
	if errors.Is(err, pgx.ErrNoRows) {
		return Lease{}, nil
	}
	return l, err
}
func (l Lease) matches(host workergroup.HostPrincipal, instance uuid.UUID, epoch int64) bool {
	return l.Epoch > 0 && l.InstanceID == instance && l.Epoch == epoch && l.HostID == host.HostID && l.WorkerEpoch == host.Epoch
}

// Recheck under current wall-clock time after the final owner lock. Cleanup and
// output can continue during release; new launch requires a still-active lease.
func producerStillAuthorized(ctx context.Context, tx db.DBTX, host workergroup.HostPrincipal, l Lease, launch bool) error {
	var ok bool
	err := tx.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM computer_leases l
 JOIN worker_hosts h ON h.id=l.worker_host_id AND h.current_epoch=l.worker_epoch
 JOIN worker_groups g ON g.id=h.worker_group_id
 WHERE l.environment_id=$1 AND l.computer_id=$2 AND l.epoch=$3 AND l.computer_instance_id=$4
 AND l.worker_host_id=$5 AND l.worker_epoch=$6 AND h.worker_group_id=$7
 AND h.claim_version=$8 AND g.claim_version=$9 AND h.status IN ('active','draining') AND g.status IN ('active','paused','draining')
 AND l.fenced_at IS NULL AND l.expires_at>clock_timestamp()
 AND (l.status='active' OR (NOT $10 AND l.status='releasing')))`, l.EnvironmentID, l.ComputerID, l.Epoch, l.InstanceID, host.HostID, host.Epoch, host.GroupID, host.HostClaimVersion, host.GroupClaimVersion, launch).Scan(&ok)
	if err != nil {
		return err
	}
	if !ok {
		return pgx.ErrNoRows
	}
	return nil
}

func lockCommand(ctx context.Context, tx pgx.Tx, env, computer, id uuid.UUID) (db.ComputerCommand, error) {
	return db.New(tx).LockComputerCommand(ctx, db.LockComputerCommandParams{EnvironmentID: pgvalue.UUID(env), ComputerID: pgvalue.UUID(computer), CommandID: pgvalue.UUID(id)})
}
