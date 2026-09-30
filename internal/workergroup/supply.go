package workergroup

import (
	"context"
	"fmt"

	"github.com/helmrdotdev/helmr/internal/db"
	"github.com/helmrdotdev/helmr/internal/pgvalue"
	"github.com/helmrdotdev/helmr/internal/vmplatform"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
)

// Worker supply lifecycle rule, applied by LockPlacementSupply,
// LockExecutionHost and LockedHost.Continues:
//   - A paused Group holds: no placements, claims, starts or restore
//     activations; work that is already running continues.
//   - A draining Group, Pool or Host lets already dispatched work finish,
//     including claim and start of leases assigned before the drain.
//   - Any operation that opens admission requires admitting supply (active
//     Group, Pool and Host without Run or VM pauses): placement, preparation,
//     the first allocated-to-ready transition, and the first restore commit and
//     activation that opens an Instance. Starting an assigned lease on an
//     already open Instance does not open admission; host drain already blocks
//     it through the Instance admission state.
//   - Continuing admitted work (receipt replay, committed receipt inspection,
//     readiness of a ready Instance, capture, renewal, completion) accepts
//     paused or draining supply and ignores Run and VM pauses, keeping epoch,
//     status and freshness checks.
//
// Each fence keeps its own lock strength and check order: placement shares the
// Group and Pool so independent hosts proceed concurrently, while execution and
// host-authenticated operations update-lock the Group and Host.

// PlacementSupply identifies the worker host a placement or Computer
// preparation locks.
type PlacementSupply struct {
	GroupID         pgtype.UUID
	RegionID        string
	HostID          pgtype.UUID
	Epoch           int64
	RunArchitecture string
	RequirePrimary  bool
	// Continuation continues already admitted work: it accepts a paused or
	// draining Group, a draining Pool or Host and Run pauses. Admission
	// requires active supply without a Run pause.
	Continuation bool
}

// LockPlacementSupply takes a shared worker-group lock before the worker host
// lock, matching the global execution lock order. Placements in the same group
// may proceed on independent hosts, while a group lifecycle change waits for
// all in-flight placements. Observation freshness is rechecked while those
// authority rows remain locked. It reports whether the locked supply could
// admit new work: active Group, Pool and Host without a Run or VM pause.
func LockPlacementSupply(ctx context.Context, tx pgx.Tx, supply PlacementSupply) (bool, error) {
	var groupActive bool
	err := tx.QueryRow(ctx, `
SELECT status = 'active'
  FROM worker_groups
 WHERE id = $1 AND region_id = $2 AND (status = 'active' OR ($3::boolean AND status IN ('paused', 'draining')))
 FOR SHARE`, supply.GroupID, supply.RegionID, supply.Continuation).Scan(&groupActive)
	if err != nil {
		return false, fmt.Errorf("lock eligible worker group: %w", err)
	}
	var poolActive bool
	err = tx.QueryRow(ctx, `
SELECT worker_pools.status = 'active'
  FROM worker_pools
  JOIN worker_hosts
    ON worker_hosts.worker_pool_id = worker_pools.id
   AND worker_hosts.worker_group_id = worker_pools.worker_group_id
	JOIN worker_groups
	  ON worker_groups.id = worker_pools.worker_group_id
 WHERE worker_hosts.id = $1
   AND worker_hosts.worker_group_id = $2
   AND (worker_pools.status = 'active' OR ($4::boolean AND worker_pools.status = 'draining'))
	AND (NOT $3::boolean OR worker_groups.primary_pool_id = worker_pools.id)
	FOR SHARE OF worker_pools`, supply.HostID, supply.GroupID, supply.RequirePrimary, supply.Continuation).Scan(&poolActive)
	if err != nil {
		return false, fmt.Errorf("lock eligible worker pool: %w", err)
	}

	hostAdmitting, err := db.New(tx).LockRunEligibleWorkerHost(ctx, db.LockRunEligibleWorkerHostParams{
		ID: supply.HostID, WorkerGroupID: supply.GroupID, WorkerEpoch: supply.Epoch,
		Continuation: supply.Continuation, WorkerFreshnessSeconds: ObservationFreshnessSeconds,
		RunArchitecture: supply.RunArchitecture, Contract: vmplatform.Contract,
	})
	if err != nil {
		return false, fmt.Errorf("lock eligible worker epoch: %w", err)
	}
	return groupActive && poolActive && hostAdmitting, nil
}

// CheckHostRuntimeAdmission keeps VM-slot admission separate from the Run
// supply fence. Callers must already hold the worker host row lock. A VM pause
// prevents creating or reclaiming VM state, but does not prevent a Run from
// reusing an already-ready Computer Instance.
func CheckHostRuntimeAdmission(ctx context.Context, tx pgx.Tx, hostID pgtype.UUID, epoch int64) error {
	var id pgtype.UUID
	return tx.QueryRow(ctx, `
SELECT id
  FROM worker_hosts
 WHERE id = $1
   AND current_epoch = $2
   AND vm_paused_reason IS NULL
 FOR UPDATE`, hostID, epoch).Scan(&id)
}

// ExecutionHost identifies the worker host a Run lease operation locks, with
// the claim versions the worker authenticated with.
type ExecutionHost struct {
	GroupID           pgtype.UUID
	RegionID          string
	HostID            pgtype.UUID
	Epoch             int64
	GroupClaimVersion int64
	HostClaimVersion  int64
	// Admission claims or starts an assigned lease: it accepts an active or
	// draining Group, so already dispatched leases finish, but rejects a
	// paused Group. Operations continuing an already started Run also accept
	// a paused Group.
	Admission bool
}

// LockExecutionHost update-locks the Group, compares its claim version, then
// update-locks the Host and compares its claim version. A changed claim
// version returns ErrStaleClaims; a Group or Host outside the supply
// lifecycle rule, or another epoch, returns pgx.ErrNoRows.
func LockExecutionHost(ctx context.Context, q db.Querier, host ExecutionHost) error {
	group, err := q.LockRunLeaseClaimWorkerGroup(ctx, db.LockRunLeaseClaimWorkerGroupParams{ID: host.GroupID, RegionID: host.RegionID})
	if err != nil {
		return err
	}
	if group.ClaimVersion != host.GroupClaimVersion {
		return ErrStaleClaims
	}
	if group.Status != db.WorkerGroupStatusActive && group.Status != db.WorkerGroupStatusDraining && (host.Admission || group.Status != db.WorkerGroupStatusPaused) {
		return pgx.ErrNoRows
	}
	locked, err := q.LockRunLeaseClaimWorker(ctx, db.LockRunLeaseClaimWorkerParams{ID: host.HostID, WorkerGroupID: host.GroupID})
	if err != nil {
		return err
	}
	if locked.ClaimVersion != host.HostClaimVersion {
		return ErrStaleClaims
	}
	if !locked.CurrentEpoch.Valid || locked.CurrentEpoch.Int64 != host.Epoch || (locked.Status != db.WorkerHostStatusActive && locked.Status != db.WorkerHostStatusDraining) {
		return pgx.ErrNoRows
	}
	return nil
}

// LockedHost is the Group and Host rows an authenticated worker host holds
// update locks on.
type LockedHost struct {
	Group db.WorkerGroup
	Host  db.WorkerHost
	epoch int64
}

// LockHost update-locks the principal's Group, then its Host, and compares the
// authenticated claim versions. A changed claim version returns
// ErrStaleClaims. Status and epoch checks stay with the caller's operation;
// LockedHost.Continues is the check for continuing admitted work.
func LockHost(ctx context.Context, q db.Querier, principal HostPrincipal) (LockedHost, error) {
	group, err := q.LockWorkerGroupForPoolMutation(ctx, pgvalue.UUID(principal.GroupID))
	if err != nil {
		return LockedHost{}, err
	}
	host, err := q.LockRunLeaseClaimWorker(ctx, db.LockRunLeaseClaimWorkerParams{ID: pgvalue.UUID(principal.HostID), WorkerGroupID: pgvalue.UUID(principal.GroupID)})
	if err != nil {
		return LockedHost{}, err
	}
	if err = principal.checkLockedClaims(host, group); err != nil {
		return LockedHost{}, err
	}
	return LockedHost{Group: group, Host: host, epoch: principal.Epoch}, nil
}

// Continues reports whether the locked supply may continue admitted work: the
// Host is at the authenticated epoch and active or draining, and the Group is
// active, paused or draining.
func (l LockedHost) Continues() bool {
	return l.Host.CurrentEpoch.Valid && l.Host.CurrentEpoch.Int64 == l.epoch &&
		(l.Host.Status == db.WorkerHostStatusActive || l.Host.Status == db.WorkerHostStatusDraining) &&
		(l.Group.Status == db.WorkerGroupStatusActive || l.Group.Status == db.WorkerGroupStatusPaused || l.Group.Status == db.WorkerGroupStatusDraining)
}
