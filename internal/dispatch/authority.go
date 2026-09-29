package dispatch

import (
	"context"
	"errors"
	"fmt"

	"github.com/helmrdotdev/helmr/internal/db"
	"github.com/helmrdotdev/helmr/internal/disk"
	"github.com/helmrdotdev/helmr/internal/vmplatform"
	"github.com/helmrdotdev/helmr/internal/workergroup"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/jackc/pgx/v5/pgxpool"
)

var (
	ErrNilPool             = errors.New("dispatch: nil pgx pool")
	ErrCapacityUnavailable = errors.New("dispatch: ready capacity unavailable")
	ErrCandidateChanged    = errors.New("dispatch: placement candidate changed while locking")
)

const runtimeArchitecture = "x86_64"

type Authority struct {
	pool       *pgxpool.Pool
	fencingKey disk.FencingKey
}

func NewRunAuthority(
	pool *pgxpool.Pool,
	fencingKey disk.FencingKey,
) (*Authority, error) {
	authority, err := newAuthority(pool)
	if err != nil {
		return nil, err
	}
	if !fencingKey.Valid() {
		return nil, errors.New("run authority computer fencing key is required")
	}
	authority.fencingKey = fencingKey
	return authority, nil
}

func newAuthority(pool *pgxpool.Pool) (*Authority, error) {
	if pool == nil {
		return nil, ErrNilPool
	}
	return &Authority{pool: pool}, nil
}

func (d *Authority) begin(ctx context.Context) (pgx.Tx, error) {
	// Dispatch authority transactions lock each mutable scope explicitly. READ
	// COMMITTED lets a statement that follows a blocking scope or Worker lock
	// re-read the state committed by the previous owner before it applies new
	// authority.
	return d.pool.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.ReadCommitted})
}

func rollback(ctx context.Context, tx pgx.Tx) {
	_ = tx.Rollback(ctx)
}

// Worker supply lifecycle rule, shared by this fence and run.lockExecutionWorker:
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
type workerFence struct {
	GroupID         pgtype.UUID
	RegionID        string
	WorkerHostID    pgtype.UUID
	WorkerEpoch     int64
	RunArchitecture string
	RequirePrimary  bool
	// Continuation continues already admitted work: it accepts a paused or
	// draining Group, a draining Pool or Host and Run pauses. Admission
	// requires active supply without a Run pause.
	Continuation bool
}

// lockWorkerFence takes a shared worker-group lock before the worker lock,
// matching the global execution lock order. Placements in the same group may
// proceed on independent Workers, while a group lifecycle change waits for all
// in-flight placements. Observation freshness is rechecked while those
// authority rows remain locked. It reports whether the locked supply could
// admit new work: active Group, Pool and Host without a Run or VM pause.
func lockWorkerFence(ctx context.Context, tx pgx.Tx, fence workerFence) (bool, error) {
	var groupActive bool
	err := tx.QueryRow(ctx, `
SELECT status = 'active'
  FROM worker_groups
 WHERE id = $1 AND region_id = $2 AND (status = 'active' OR ($3::boolean AND status IN ('paused', 'draining')))
 FOR SHARE`, fence.GroupID, fence.RegionID, fence.Continuation).Scan(&groupActive)
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
	FOR SHARE OF worker_pools`, fence.WorkerHostID, fence.GroupID, fence.RequirePrimary, fence.Continuation).Scan(&poolActive)
	if err != nil {
		return false, fmt.Errorf("lock eligible worker pool: %w", err)
	}

	hostAdmitting, err := db.New(tx).LockRunEligibleWorkerHost(ctx, db.LockRunEligibleWorkerHostParams{
		ID: fence.WorkerHostID, WorkerGroupID: fence.GroupID, WorkerEpoch: fence.WorkerEpoch,
		Continuation: fence.Continuation, WorkerFreshnessSeconds: workergroup.ObservationFreshnessSeconds,
		RunArchitecture: fence.RunArchitecture, Contract: vmplatform.Contract,
	})
	if err != nil {
		return false, fmt.Errorf("lock eligible worker epoch: %w", err)
	}
	return groupActive && poolActive && hostAdmitting, nil
}

// checkLockedWorkerRuntimeAdmission keeps Runtime-slot admission separate from
// the Run-domain fence. Callers must already hold the worker row lock. A
// Runtime pause prevents creating or reclaiming VM state, but does not prevent
// a Run from reusing an already-ready Computer Runtime.
func checkLockedWorkerRuntimeAdmission(
	ctx context.Context,
	tx pgx.Tx,
	workerHostID pgtype.UUID,
	workerEpoch int64,
) error {
	var workerID pgtype.UUID
	return tx.QueryRow(ctx, `
SELECT id
  FROM worker_hosts
 WHERE id = $1
   AND current_epoch = $2
   AND vm_paused_reason IS NULL
 FOR UPDATE`, workerHostID, workerEpoch).Scan(&workerID)
}
