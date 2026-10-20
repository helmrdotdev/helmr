package dispatch

import (
	"context"
	"crypto/sha256"
	"crypto/subtle"
	"errors"
	"uuid"

	"github.com/helmrdotdev/helmr/internal/computer"
	"github.com/helmrdotdev/helmr/internal/db"
	"github.com/helmrdotdev/helmr/internal/org"
	"github.com/helmrdotdev/helmr/internal/workergroup"
	"github.com/jackc/pgx/v5"
)

func readBootSeedAllocation(ctx context.Context, q db.DBTX, env, id uuid.UUID) (computer.Allocation, error) {
	r := computer.Allocation{EnvironmentID: env, OwnerID: id}
	err := q.QueryRow(ctx, `SELECT allocation_id,executor_epoch,worker_host_id,worker_epoch,reserved_cpu_millis,reserved_memory_bytes,reserved_scratch_bytes,
 vm_platform_id,vm_vcpu_count,cpu_config_digest,status,delivered_at,executor_expires_at,fenced_at,channel_credential_digest FROM boot_seed_builds
 WHERE environment_id=$1 AND id=$2 AND worker_host_id IS NOT NULL`, env, id).Scan(&r.InstanceID, &r.Epoch, &r.HostID, &r.HostEpoch,
		&r.Shape.CPUMillis, &r.Shape.MemoryBytes, &r.Shape.ScratchBytes, &r.Shape.VMPlatformID, &r.Shape.VCPUCount, &r.Shape.CPUConfigDigest,
		&r.Status, &r.DeliveredAt, &r.ExpiresAt, &r.FencedAt, &r.CredentialDigest)
	return r, err
}

func (a *Allocator) AllocateBootSeed(ctx context.Context, env, id, host uuid.UUID) (computer.Allocation, error) {
	if env == uuid.Nil() || id == uuid.Nil() || host == uuid.Nil() {
		return computer.Allocation{}, ErrInvalidInput
	}
	if prior, err := readBootSeedAllocation(ctx, a.database, env, id); err == nil {
		return prior, nil
	} else if !errors.Is(err, pgx.ErrNoRows) {
		return computer.Allocation{}, err
	}
	var result computer.Allocation
	err := db.RunTx(ctx, a.database, func(tx pgx.Tx) error {
		if err := allocationLockTimeout(ctx, tx); err != nil {
			return err
		}
		supply, err := lockAllocationSupply(ctx, tx, env, host, true)
		if err != nil {
			return err
		}
		policy, err := org.LockAdmission(ctx, tx, env)
		if err != nil {
			return err
		}
		build, err := computer.LockBootSeedBuild(ctx, tx, env, id)
		if err != nil {
			return err
		}
		if prior, err := readBootSeedAllocation(ctx, tx, env, id); err == nil {
			result = prior
			return nil
		} else if !errors.Is(err, pgx.ErrNoRows) {
			return err
		}
		shape, err := qualifyAllocationShape(ctx, tx, supply, build.CPUMillis, build.MemoryBytes)
		if err != nil {
			return err
		}
		if err := requireAllocationCapacity(ctx, tx, env, supply, policy, shape, false); err != nil {
			return err
		}
		result = computer.Allocation{EnvironmentID: env, OwnerID: id, InstanceID: uuid.NewV7(), Epoch: 1, HostID: host, HostEpoch: supply.epoch, Shape: shape, Status: "running"}
		digest := sha256.Sum256(a.credential("helmr.boot-seed-build.v1\x00", result))
		result.CredentialDigest = digest[:]
		assigned, err := computer.AssignBootSeedBuild(ctx, tx, result)
		if err != nil {
			return err
		}
		if !assigned {
			return ErrNotReady
		}
		return requireAllocationFreshSupply(ctx, tx, supply)
	})
	if err != nil {
		return computer.Allocation{}, allocationError(err)
	}
	return result, nil
}

type BootSeedDelivery struct {
	computer.Allocation
	Executor computer.BootSeedExecutor
}

func (a *Allocator) DeliverBootSeed(ctx context.Context, host workergroup.HostPrincipal, identity HostAllocation) (BootSeedDelivery, error) {
	if !identity.valid() || identity.Kind != "seed" {
		return BootSeedDelivery{}, ErrInvalidInput
	}
	var result BootSeedDelivery
	err := db.RunTx(ctx, a.database, func(tx pgx.Tx) error {
		if err := allocationLockTimeout(ctx, tx); err != nil {
			return err
		}
		supply, err := lockMaterializationHost(ctx, tx, host)
		if err != nil {
			return err
		}
		if _, err := computer.LockBootSeedBuild(ctx, tx, identity.EnvironmentID, identity.OwnerID); err != nil {
			return err
		}
		r, err := readBootSeedAllocation(ctx, tx, identity.EnvironmentID, identity.OwnerID)
		if err != nil {
			return err
		}
		first := r.DeliveredAt == nil
		credential := a.credential("helmr.boot-seed-build.v1\x00", r)
		digest := sha256.Sum256(credential)
		if !allocationBelongsToHost(r, host) || r.InstanceID != identity.InstanceID || r.Epoch != identity.Epoch || subtle.ConstantTimeCompare(r.CredentialDigest, digest[:]) != 1 {
			return ErrDenied
		}
		r.DeliveredAt, r.ExpiresAt, err = computer.DeliverBootSeedBuild(ctx, tx, identity.EnvironmentID, identity.OwnerID)
		if errors.Is(err, pgx.ErrNoRows) {
			return ErrAllocationClosed
		}
		if err != nil {
			return err
		}
		if first {
			if err := supply.firstExecution(); err != nil {
				return err
			}
		}
		result = BootSeedDelivery{Allocation: r, Executor: computer.BootSeedExecutor{EnvironmentID: r.EnvironmentID, BuildID: r.OwnerID, InstanceID: r.InstanceID, Epoch: r.Epoch, ChannelCredential: credential}}
		return nil
	})
	if err != nil {
		return BootSeedDelivery{}, allocationError(err)
	}
	return result, nil
}

func ObserveBootSeedStopped(ctx context.Context, database db.TxBeginner, host workergroup.HostPrincipal, identity HostAllocation) error {
	if !identity.valid() || identity.Kind != "seed" {
		return ErrInvalidInput
	}
	return allocationError(db.RunTx(ctx, database, func(tx pgx.Tx) error {
		if err := computer.LockHost(ctx, tx, host); err != nil {
			return err
		}
		if _, err := computer.LockBootSeedBuild(ctx, tx, identity.EnvironmentID, identity.OwnerID); err != nil {
			return err
		}
		r, err := readBootSeedAllocation(ctx, tx, identity.EnvironmentID, identity.OwnerID)
		if err != nil {
			return err
		}
		if !allocationBelongsToHost(r, host) || r.InstanceID != identity.InstanceID || r.Epoch != identity.Epoch {
			return ErrDenied
		}
		return computer.RecordBootSeedStopped(ctx, tx, r.EnvironmentID, r.OwnerID, "owning worker joined builder and qualification VM closure")
	}))
}

func expireBootSeed(ctx context.Context, database db.TxBeginner, env, id uuid.UUID) error {
	return allocationError(db.RunTx(ctx, database, func(tx pgx.Tx) error {
		if err := allocationLockTimeout(ctx, tx); err != nil {
			return err
		}
		var host, group uuid.UUID
		if err := tx.QueryRow(ctx, `SELECT b.worker_host_id,h.worker_group_id FROM boot_seed_builds b
 JOIN worker_hosts h ON h.id=b.worker_host_id WHERE b.environment_id=$1 AND b.id=$2`, env, id).Scan(&host, &group); err != nil {
			return err
		}
		if _, err := tx.Exec(ctx, `SELECT id FROM worker_groups WHERE id=$1 FOR SHARE`, group); err != nil {
			return err
		}
		if _, err := tx.Exec(ctx, `SELECT id FROM worker_hosts WHERE id=$1 FOR SHARE`, host); err != nil {
			return err
		}
		if _, err := computer.LockBootSeedBuild(ctx, tx, env, id); err != nil {
			return err
		}
		var expired bool
		if err := tx.QueryRow(ctx, `SELECT b.status='running' AND (b.deadline_at<=clock_timestamp()
 OR COALESCE(b.executor_expires_at<=clock_timestamp(),false) OR h.current_epoch IS DISTINCT FROM b.worker_epoch
 OR h.status NOT IN ('active','draining')) FROM boot_seed_builds b JOIN worker_hosts h ON h.id=b.worker_host_id
 WHERE b.environment_id=$1 AND b.id=$2`, env, id).Scan(&expired); err != nil {
			return err
		}
		if !expired {
			return nil
		}
		return computer.TerminateBootSeedBuild(ctx, tx, env, id, "executor_lost")
	}))
}
