package agent

import (
	"context"
	"crypto/sha256"
	"crypto/subtle"
	"errors"
	"uuid"

	"github.com/helmrdotdev/helmr/internal/db"
	"github.com/helmrdotdev/helmr/internal/definition"
	"github.com/helmrdotdev/helmr/internal/pgvalue"
	"github.com/helmrdotdev/helmr/internal/workergroup"
	"github.com/jackc/pgx/v5"
)

// ErrAllocationClosed requires the owning Host to stop or confirm absence of
// this exact allocation. Only its physical receipt releases the reservation.
var ErrAllocationClosed = errors.New("allocation no longer admits execution")

type materializationSupply struct {
	admitting, draining   bool
	healthy               bool
	hostDrainContinuation bool
}

func (s materializationSupply) firstExecution() error {
	// The physical assignment committed before planned Host drain. Delivery
	// supplies its execution credential; it does not place new work.
	if s.hostDrainContinuation {
		if !s.healthy {
			return ErrNotReady
		}
		return nil
	}
	if s.draining {
		return ErrAllocationClosed
	}
	if !s.admitting {
		return ErrNotReady
	}
	return nil
}

// Delivery starts the execution lease exactly once. A retry returns the same
// credential and remaining lifetime; it cannot renew or resurrect authority.
type PreparationDelivery struct {
	Allocation
	Executor PreparationExecutor
}
type ComputerDelivery struct {
	ComputerAllocation
	ChannelCredential string
}

func allocationBelongsToHost(r Allocation, host workergroup.HostPrincipal) bool {
	return r.HostID == host.HostID && r.HostEpoch == host.Epoch
}

// Materialization preserves committed assignments through planned Host drain.
// Group/Pool admission policy, host identity and freshness are checked separately.
func lockMaterializationHost(ctx context.Context, tx pgx.Tx, host workergroup.HostPrincipal) (materializationSupply, error) {
	var region string
	if err := tx.QueryRow(ctx, `SELECT region_id FROM worker_groups WHERE id=$1`, host.GroupID).Scan(&region); err != nil {
		return materializationSupply{}, err
	}
	admitting, err := workergroup.LockDispatchSupply(ctx, tx, workergroup.DispatchSupply{GroupID: pgvalue.UUID(host.GroupID), HostID: pgvalue.UUID(host.HostID), Epoch: host.Epoch, RegionID: region, RunArchitecture: string(definition.ArchitectureX8664), Continuation: true})
	if err != nil {
		return materializationSupply{}, err
	}
	if err := workergroup.CheckClaims(ctx, tx, host); err != nil {
		return materializationSupply{}, err
	}
	var draining, hostDrainContinuation, healthy bool
	if err := tx.QueryRow(ctx, `SELECT g.status='draining' OR p.status='draining' OR h.status='draining', h.status='draining' AND g.status='active' AND p.status='active', h.run_paused_reason IS NULL AND h.vm_paused_reason IS NULL FROM worker_hosts h JOIN worker_groups g ON g.id=h.worker_group_id JOIN worker_pools p ON p.id=h.worker_pool_id WHERE h.id=$1`, host.HostID).Scan(&draining, &hostDrainContinuation, &healthy); err != nil {
		return materializationSupply{}, err
	}
	return materializationSupply{admitting: admitting, draining: draining, healthy: healthy, hostDrainContinuation: hostDrainContinuation}, nil
}

func (a *Allocator) DeliverPreparation(ctx context.Context, host workergroup.HostPrincipal, identity PreparationIdentity) (PreparationDelivery, error) {
	if identity.EnvironmentID == uuid.Nil() || identity.PreparationID == uuid.Nil() || identity.InstanceID == uuid.Nil() || identity.Epoch <= 0 {
		return PreparationDelivery{}, ErrInvalidInput
	}
	env, preparation := identity.EnvironmentID, identity.PreparationID
	var result PreparationDelivery
	err := db.RunTx(ctx, a.database, func(tx pgx.Tx) error {
		if err := allocationLockTimeout(ctx, tx); err != nil {
			return err
		}
		supply, err := lockMaterializationHost(ctx, tx, host)
		if err != nil {
			return err
		}
		p, err := readPreparation(ctx, tx, env, preparation)
		if err != nil {
			return err
		}
		if err := lockPreparationImageSecrets(ctx, tx, env, p.SpecID); err != nil {
			return err
		}
		if err := lockPreparationSpec(ctx, tx, env, p.SpecID); err != nil {
			return err
		}
		var digest []byte
		if err := tx.QueryRow(ctx, `SELECT channel_credential_digest FROM computer_preparations WHERE environment_id=$1 AND id=$2 FOR NO KEY UPDATE`, env, preparation).Scan(&digest); err != nil {
			return err
		}
		r, err := readPreparationAllocation(ctx, tx, env, preparation)
		if err != nil {
			return err
		}
		firstDelivery := r.DeliveredAt == nil
		credential := a.preparationCredential(r)
		expected := sha256.Sum256(credential)
		if !allocationBelongsToHost(r, host) || r.InstanceID != identity.InstanceID || r.Epoch != identity.Epoch || subtle.ConstantTimeCompare(digest, expected[:]) != 1 {
			return ErrDenied
		}
		if err := tx.QueryRow(ctx, `UPDATE computer_preparations p SET delivered_at=COALESCE(delivered_at,clock_timestamp()),
 executor_expires_at=COALESCE(executor_expires_at,LEAST(deadline_at,clock_timestamp()+interval '1 minute'))
 WHERE environment_id=$1 AND id=$2 AND status='running' AND fenced_at IS NULL AND deadline_at>clock_timestamp()
 AND (delivered_at IS NULL OR executor_expires_at>clock_timestamp())
 AND NOT EXISTS(SELECT 1 FROM computer_secret_bindings binding JOIN secrets s ON s.environment_id=binding.environment_id AND s.id=binding.secret_id
 WHERE binding.environment_id=$1 AND binding.preparation_spec_id=$3 AND s.status='revoked')
 RETURNING delivered_at,executor_expires_at`, env, preparation, p.SpecID).Scan(&r.DeliveredAt, &r.ExpiresAt); err != nil {
			if errors.Is(err, pgx.ErrNoRows) {
				return ErrAllocationClosed
			}
			return err
		}
		if firstDelivery {
			if err := supply.firstExecution(); err != nil {
				return err
			}
		}
		result = PreparationDelivery{Allocation: r, Executor: PreparationExecutor{EnvironmentID: env, PreparationID: preparation, InstanceID: r.InstanceID, Epoch: r.Epoch, ChannelCredential: credential}}
		return nil
	})
	if err != nil {
		return PreparationDelivery{}, allocationError(err)
	}
	return result, nil
}

func (a *Allocator) DeliverComputer(ctx context.Context, host workergroup.HostPrincipal, identity ComputerLeaseIdentity) (ComputerDelivery, error) {
	if !identity.valid() {
		return ComputerDelivery{}, ErrInvalidInput
	}
	var result ComputerDelivery
	err := db.RunTx(ctx, a.database, func(tx pgx.Tx) error {
		if err := allocationLockTimeout(ctx, tx); err != nil {
			return err
		}
		supply, err := lockMaterializationHost(ctx, tx, host)
		if err != nil {
			return err
		}
		if _, err := lockComputerMembers(ctx, tx, identity.EnvironmentID, identity.ComputerID); err != nil {
			return err
		}
		var digest []byte
		if err := tx.QueryRow(ctx, `SELECT channel_credential_digest FROM computer_leases WHERE environment_id=$1 AND computer_id=$2 AND epoch=$3 FOR NO KEY UPDATE`, identity.EnvironmentID, identity.ComputerID, identity.Epoch).Scan(&digest); err != nil {
			return err
		}
		r, err := readComputerAllocation(ctx, tx, identity.EnvironmentID, identity.ComputerID, identity.Epoch)
		if err != nil {
			return err
		}
		firstDelivery := r.DeliveredAt == nil
		credential := a.computerCredential(r.Allocation)
		expected := sha256.Sum256([]byte(credential))
		if !allocationBelongsToHost(r.Allocation, host) || r.InstanceID != identity.InstanceID || subtle.ConstantTimeCompare(digest, expected[:]) != 1 {
			return ErrDenied
		}
		if err := requireComputerImageAllowed(ctx, tx, identity.EnvironmentID, identity.ComputerID); err != nil {
			if errors.Is(err, ErrDenied) {
				return ErrAllocationClosed
			}
			return err
		}
		if err := tx.QueryRow(ctx, `UPDATE computer_leases SET delivered_at=COALESCE(delivered_at,clock_timestamp()),expires_at=COALESCE(expires_at,clock_timestamp()+interval '1 minute')
 WHERE environment_id=$1 AND computer_id=$2 AND epoch=$3 AND status IN ('acquiring','active') AND fenced_at IS NULL
 AND (delivered_at IS NULL OR expires_at>clock_timestamp()) RETURNING delivered_at,expires_at`, identity.EnvironmentID, identity.ComputerID, identity.Epoch).Scan(&r.DeliveredAt, &r.ExpiresAt); err != nil {
			if errors.Is(err, pgx.ErrNoRows) {
				return ErrAllocationClosed
			}
			return err
		}
		if firstDelivery {
			if err := supply.firstExecution(); err != nil {
				return err
			}
		}
		result = ComputerDelivery{ComputerAllocation: r, ChannelCredential: credential}
		return nil
	})
	if err != nil {
		return ComputerDelivery{}, allocationError(err)
	}
	return result, nil
}

// ObserveFreshComputerReady accepts the owning Host's exact materialization
// receipt before any Session process can be admitted. Restores have their own
// checkpoint-member validation and may not use this fresh-boot transition.
func ObserveFreshComputerReady(ctx context.Context, database db.TxBeginner, host workergroup.HostPrincipal, identity ComputerLeaseIdentity, shape AllocationShape, base string) error {
	if !identity.valid() {
		return ErrInvalidInput
	}
	return allocationError(db.RunTx(ctx, database, func(tx pgx.Tx) error {
		if err := allocationLockTimeout(ctx, tx); err != nil {
			return err
		}
		supply, err := lockMaterializationHost(ctx, tx, host)
		if err != nil {
			return err
		}
		if _, err := lockComputerMembers(ctx, tx, identity.EnvironmentID, identity.ComputerID); err != nil {
			return err
		}
		r, err := readComputerAllocation(ctx, tx, identity.EnvironmentID, identity.ComputerID, identity.Epoch)
		if err != nil {
			return err
		}
		if !allocationBelongsToHost(r.Allocation, host) || r.InstanceID != identity.InstanceID || r.Shape != shape || r.BaseVersion != base || r.RestoredFrom != nil {
			return ErrDenied
		}
		if err := requireComputerImageAllowed(ctx, tx, identity.EnvironmentID, identity.ComputerID); err != nil {
			if errors.Is(err, ErrDenied) {
				return ErrAllocationClosed
			}
			return err
		}
		if r.FencedAt != nil || (r.Status != "acquiring" && r.Status != "active") {
			return ErrAllocationClosed
		}
		if r.DeliveredAt == nil {
			return ErrNotReady
		}
		changed, err := tx.Exec(ctx, `UPDATE computer_leases SET status='active',initialized_at=COALESCE(initialized_at,clock_timestamp())
 WHERE environment_id=$1 AND computer_id=$2 AND epoch=$3 AND status IN ('acquiring','active') AND delivered_at IS NOT NULL AND expires_at>clock_timestamp() AND fenced_at IS NULL`, identity.EnvironmentID, identity.ComputerID, identity.Epoch)
		if err != nil {
			return err
		}
		if changed.RowsAffected() != 1 {
			return ErrAllocationClosed
		}
		if r.InitializedAt == nil {
			return supply.firstExecution()
		}
		// The committed allocation already admitted this physical start. Host drain
		// cannot revoke it while materialization reports completion. Group/Pool
		// availability and all identity, lease and image fences remain above.
		return nil
	}))
}
