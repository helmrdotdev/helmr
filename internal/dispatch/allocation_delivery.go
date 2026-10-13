package dispatch

import (
	"context"
	"crypto/sha256"
	"crypto/subtle"
	"errors"
	"uuid"

	"github.com/helmrdotdev/helmr/internal/computer"
	"github.com/helmrdotdev/helmr/internal/session"

	"github.com/helmrdotdev/helmr/internal/db"
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
type ImageBuildDelivery struct {
	computer.Allocation
	Executor computer.ImageBuildExecutor
}
type LeaseDelivery struct {
	computer.ComputerAllocation
	ChannelCredential string
}

func allocationBelongsToHost(r computer.Allocation, host workergroup.HostPrincipal) bool {
	return r.HostID == host.HostID && r.HostEpoch == host.Epoch
}

// Materialization preserves committed assignments through planned Host drain.
// Group/Pool admission policy, host identity and freshness are checked separately.
func lockMaterializationHost(ctx context.Context, tx pgx.Tx, host workergroup.HostPrincipal) (materializationSupply, error) {
	var region string
	if err := tx.QueryRow(ctx, `SELECT region_id FROM worker_groups WHERE id=$1`, host.GroupID).Scan(&region); err != nil {
		return materializationSupply{}, err
	}
	admitting, err := workergroup.LockDispatchSupply(ctx, tx, workergroup.DispatchSupply{GroupID: pgvalue.UUID(host.GroupID), HostID: pgvalue.UUID(host.HostID), Epoch: host.Epoch, RegionID: region, Continuation: true})
	if err != nil {
		return materializationSupply{}, err
	}
	if err := workergroup.CheckClaims(ctx, tx, host); err != nil {
		return materializationSupply{}, err
	}
	var draining, hostDrainContinuation, healthy bool
	if err := tx.QueryRow(ctx, `SELECT g.status='draining' OR p.status='draining' OR h.status='draining', h.status='draining' AND g.status='active' AND p.status='active', h.admission_paused_reason IS NULL FROM worker_hosts h JOIN worker_groups g ON g.id=h.worker_group_id JOIN worker_pools p ON p.id=h.worker_pool_id WHERE h.id=$1`, host.HostID).Scan(&draining, &hostDrainContinuation, &healthy); err != nil {
		return materializationSupply{}, err
	}
	return materializationSupply{admitting: admitting, draining: draining, healthy: healthy, hostDrainContinuation: hostDrainContinuation}, nil
}

func (a *Allocator) DeliverImageBuild(ctx context.Context, host workergroup.HostPrincipal, identity computer.ImageBuildIdentity) (ImageBuildDelivery, error) {
	if identity.EnvironmentID == uuid.Nil() || identity.ImageBuildID == uuid.Nil() || identity.InstanceID == uuid.Nil() || identity.Epoch <= 0 {
		return ImageBuildDelivery{}, ErrInvalidInput
	}
	env, preparation := identity.EnvironmentID, identity.ImageBuildID
	var result ImageBuildDelivery
	err := db.RunTx(ctx, a.database, func(tx pgx.Tx) error {
		if err := allocationLockTimeout(ctx, tx); err != nil {
			return err
		}
		supply, err := lockMaterializationHost(ctx, tx, host)
		if err != nil {
			return err
		}
		p, err := computer.ReadImageBuild(ctx, tx, env, preparation)
		if err != nil {
			return err
		}
		if err := computer.LockImageSpecSecrets(ctx, tx, env, p.SpecID); err != nil {
			return err
		}
		if err := computer.LockImageSpec(ctx, tx, env, p.SpecID); err != nil {
			return err
		}
		var digest []byte
		if err := tx.QueryRow(ctx, `SELECT channel_credential_digest FROM image_builds WHERE environment_id=$1 AND id=$2 FOR NO KEY UPDATE`, env, preparation).Scan(&digest); err != nil {
			return err
		}
		r, err := ReadImageBuildAllocation(ctx, tx, env, preparation)
		if err != nil {
			return err
		}
		firstDelivery := r.DeliveredAt == nil
		credential := a.imageBuildCredential(r)
		expected := sha256.Sum256(credential)
		if !allocationBelongsToHost(r, host) || r.InstanceID != identity.InstanceID || r.Epoch != identity.Epoch || subtle.ConstantTimeCompare(digest, expected[:]) != 1 {
			return ErrDenied
		}
		if r.DeliveredAt, r.ExpiresAt, err = computer.DeliverImageBuild(ctx, tx, env, preparation, p.SpecID); err != nil {
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
		result = ImageBuildDelivery{Allocation: r, Executor: computer.ImageBuildExecutor{EnvironmentID: env, ImageBuildID: preparation, InstanceID: r.InstanceID, Epoch: r.Epoch, ChannelCredential: credential}}
		return nil
	})
	if err != nil {
		return ImageBuildDelivery{}, allocationError(err)
	}
	return result, nil
}

func (a *Allocator) DeliverLease(ctx context.Context, host workergroup.HostPrincipal, identity computer.LeaseIdentity) (LeaseDelivery, error) {
	if !identity.Valid() {
		return LeaseDelivery{}, ErrInvalidInput
	}
	var result LeaseDelivery
	err := db.RunTx(ctx, a.database, func(tx pgx.Tx) error {
		if err := allocationLockTimeout(ctx, tx); err != nil {
			return err
		}
		supply, err := lockMaterializationHost(ctx, tx, host)
		if err != nil {
			return err
		}
		if _, err := session.LockComputerMembers(ctx, tx, identity.EnvironmentID, identity.ComputerID); err != nil {
			return err
		}
		var digest []byte
		if err := tx.QueryRow(ctx, `SELECT channel_credential_digest FROM computer_leases WHERE environment_id=$1 AND computer_id=$2 AND epoch=$3 FOR NO KEY UPDATE`, identity.EnvironmentID, identity.ComputerID, identity.Epoch).Scan(&digest); err != nil {
			return err
		}
		r, err := ReadLeaseAllocation(ctx, tx, identity.EnvironmentID, identity.ComputerID, identity.Epoch)
		if err != nil {
			return err
		}
		firstDelivery := r.DeliveredAt == nil
		credential := a.leaseCredential(r.Allocation)
		expected := sha256.Sum256([]byte(credential))
		if !allocationBelongsToHost(r.Allocation, host) || r.InstanceID != identity.InstanceID || subtle.ConstantTimeCompare(digest, expected[:]) != 1 {
			return ErrDenied
		}
		if err := computer.RequireSecretsNotRevoked(ctx, tx, identity.EnvironmentID, identity.ComputerID); err != nil {
			if errors.Is(err, ErrDenied) {
				return ErrAllocationClosed
			}
			return err
		}
		if r.DeliveredAt, r.ExpiresAt, err = computer.DeliverLease(ctx, tx, identity.EnvironmentID, identity.ComputerID, identity.Epoch); err != nil {
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
		result = LeaseDelivery{ComputerAllocation: r, ChannelCredential: credential}
		return nil
	})
	if err != nil {
		return LeaseDelivery{}, allocationError(err)
	}
	return result, nil
}

// ObserveFreshLeaseReady accepts the owning Host's exact materialization
// receipt before any Session process can be admitted. Restores have their own
// checkpoint-member validation and may not use this fresh-boot transition.
func ObserveFreshLeaseReady(ctx context.Context, database db.TxBeginner, host workergroup.HostPrincipal, identity computer.LeaseIdentity, shape computer.AllocationShape, base string) error {
	if !identity.Valid() {
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
		if _, err := session.LockComputerMembers(ctx, tx, identity.EnvironmentID, identity.ComputerID); err != nil {
			return err
		}
		r, err := ReadLeaseAllocation(ctx, tx, identity.EnvironmentID, identity.ComputerID, identity.Epoch)
		if err != nil {
			return err
		}
		if !allocationBelongsToHost(r.Allocation, host) || r.InstanceID != identity.InstanceID || r.Shape != shape || r.BaseVersion != base || r.RestoredFrom != nil {
			return ErrDenied
		}
		if err := computer.RequireSecretsNotRevoked(ctx, tx, identity.EnvironmentID, identity.ComputerID); err != nil {
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
		initialized, err := computer.InitializeLease(ctx, tx, identity.EnvironmentID, identity.ComputerID, identity.Epoch)
		if err != nil {
			return err
		}
		if !initialized {
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
