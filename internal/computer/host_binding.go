package computer

import (
	"context"
	"uuid"

	"github.com/helmrdotdev/helmr/internal/workergroup"
	"github.com/jackc/pgx/v5"
)

// Host routes first check, without locking, that the authenticated host epoch
// holds the identified allocation. A target that is missing or held by another
// host, group or organization is then refused identically with ErrDenied,
// before any of its rows are locked, so neither the response nor a lock wait
// distinguishes it. The checks after the target's locks still decide whether
// the host may act on it now.

// RequireHostLease requires the host epoch to hold the Computer lease.
func RequireHostLease(ctx context.Context, tx pgx.Tx, host workergroup.HostPrincipal, env, computerID uuid.UUID, epoch int64) error {
	var held bool
	err := tx.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM computer_leases
 WHERE environment_id=$1 AND computer_id=$2 AND epoch=$3 AND worker_host_id=$4 AND worker_epoch=$5)`, env, computerID, epoch, host.HostID, host.Epoch).Scan(&held)
	return heldOrDenied(held, err)
}

// RequireHostImageBuild requires the host epoch to hold the image build.
func RequireHostImageBuild(ctx context.Context, tx pgx.Tx, host workergroup.HostPrincipal, env, build uuid.UUID) error {
	var held bool
	err := tx.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM image_builds
 WHERE environment_id=$1 AND id=$2 AND worker_host_id=$3 AND worker_epoch=$4)`, env, build, host.HostID, host.Epoch).Scan(&held)
	return heldOrDenied(held, err)
}

// RequireHostBootSeedBuild requires the host epoch to hold the boot seed build.
func RequireHostBootSeedBuild(ctx context.Context, tx pgx.Tx, host workergroup.HostPrincipal, env, build uuid.UUID) error {
	var held bool
	err := tx.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM boot_seed_builds
 WHERE environment_id=$1 AND id=$2 AND worker_host_id=$3 AND worker_epoch=$4)`, env, build, host.HostID, host.Epoch).Scan(&held)
	return heldOrDenied(held, err)
}

// RequireHostCheckpointSource requires the host epoch to hold the
// checkpoint's source lease, for capture, publication and source abort.
func RequireHostCheckpointSource(ctx context.Context, tx pgx.Tx, host workergroup.HostPrincipal, env, checkpoint uuid.UUID) error {
	var held bool
	err := tx.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM checkpoints cp
 JOIN computer_leases l ON (l.environment_id,l.computer_id,l.epoch)=(cp.environment_id,cp.computer_id,cp.source_lease_epoch)
 WHERE cp.environment_id=$1 AND cp.id=$2 AND l.worker_host_id=$3 AND l.worker_epoch=$4)`, env, checkpoint, host.HostID, host.Epoch).Scan(&held)
	return heldOrDenied(held, err)
}

// RequireHostCheckpointRestore requires the host epoch to hold the restore
// target lease the checkpoint records.
func RequireHostCheckpointRestore(ctx context.Context, tx pgx.Tx, host workergroup.HostPrincipal, env, checkpoint uuid.UUID) error {
	var held bool
	err := tx.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM checkpoints cp
 JOIN computer_leases l ON (l.environment_id,l.computer_id,l.epoch)=(cp.environment_id,cp.computer_id,cp.target_lease_epoch)
 WHERE cp.environment_id=$1 AND cp.id=$2 AND l.worker_host_id=$3 AND l.worker_epoch=$4)`, env, checkpoint, host.HostID, host.Epoch).Scan(&held)
	return heldOrDenied(held, err)
}

// RequireHostCheckpointTarget requires the host epoch to hold the lease that
// a restore names as its target, before the checkpoint records that target: a
// later lease of the checkpoint's Computer restored from the checkpoint's Save.
func RequireHostCheckpointTarget(ctx context.Context, tx pgx.Tx, host workergroup.HostPrincipal, env, checkpoint uuid.UUID, epoch int64) error {
	var held bool
	err := tx.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM checkpoints cp
 JOIN computer_leases l ON (l.environment_id,l.computer_id,l.restored_from_save_id)=(cp.environment_id,cp.computer_id,cp.disk_save_id)
 WHERE cp.environment_id=$1 AND cp.id=$2 AND l.epoch=$3 AND l.epoch>cp.source_lease_epoch
 AND l.worker_host_id=$4 AND l.worker_epoch=$5)`, env, checkpoint, epoch, host.HostID, host.Epoch).Scan(&held)
	return heldOrDenied(held, err)
}

// RequireHostSave requires the host epoch to hold the Save's source lease.
func RequireHostSave(ctx context.Context, tx pgx.Tx, host workergroup.HostPrincipal, env, save uuid.UUID) error {
	var held bool
	err := tx.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM computer_saves s
 JOIN computer_leases l ON (l.environment_id,l.computer_id,l.epoch)=(s.environment_id,s.computer_id,s.computer_lease_epoch)
 WHERE s.environment_id=$1 AND s.id=$2 AND l.worker_host_id=$3 AND l.worker_epoch=$4)`, env, save, host.HostID, host.Epoch).Scan(&held)
	return heldOrDenied(held, err)
}

func heldOrDenied(held bool, err error) error {
	if err != nil {
		return err
	}
	if !held {
		return ErrDenied
	}
	return nil
}
