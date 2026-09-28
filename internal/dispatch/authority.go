package dispatch

import (
	"context"
	"errors"
	"fmt"

	"github.com/helmrdotdev/helmr/internal/computer"
	"github.com/helmrdotdev/helmr/internal/vmplatform"
	"github.com/helmrdotdev/helmr/internal/workerapi"
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
	fencingKey computer.FencingKey
}

func NewRunAuthority(
	pool *pgxpool.Pool,
	fencingKey computer.FencingKey,
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

type workerFence struct {
	GroupID         pgtype.UUID
	RegionID        string
	WorkerHostID    pgtype.UUID
	WorkerEpoch     int64
	RunArchitecture string
	RequirePrimary  bool
	AllowDraining   bool
}

// lockWorkerFence takes a shared worker-group lock before the worker lock,
// matching the global execution lock order. Placements in the same group may
// proceed on independent Workers, while a group lifecycle change waits for all
// in-flight placements. Observation freshness is rechecked while those
// authority rows remain locked.
func lockWorkerFence(ctx context.Context, tx pgx.Tx, fence workerFence) error {
	var groupID pgtype.UUID
	err := tx.QueryRow(ctx, `
SELECT id
  FROM worker_groups
 WHERE id = $1 AND region_id = $2 AND status = 'active'
 FOR SHARE`, fence.GroupID, fence.RegionID).Scan(&groupID)
	if err != nil {
		return fmt.Errorf("lock eligible worker group: %w", err)
	}
	var poolID pgtype.UUID
	err = tx.QueryRow(ctx, `
SELECT worker_pools.id
  FROM worker_pools
  JOIN worker_hosts
    ON worker_hosts.worker_pool_id = worker_pools.id
   AND worker_hosts.worker_group_id = worker_pools.worker_group_id
	JOIN worker_groups
	  ON worker_groups.id = worker_pools.worker_group_id
 WHERE worker_hosts.id = $1
   AND worker_hosts.worker_group_id = $2
   AND worker_pools.status = 'active'
	AND (NOT $3::boolean OR worker_groups.primary_pool_id = worker_pools.id)
	FOR SHARE OF worker_pools`, fence.WorkerHostID, fence.GroupID, fence.RequirePrimary).Scan(&poolID)
	if err != nil {
		return fmt.Errorf("lock eligible worker pool: %w", err)
	}

	var workerID pgtype.UUID
	err = tx.QueryRow(ctx, `
SELECT worker_hosts.id
  FROM worker_hosts
  JOIN worker_groups
    ON worker_groups.id = worker_hosts.worker_group_id
  JOIN worker_pools
    ON worker_pools.id = worker_hosts.worker_pool_id
   AND worker_pools.worker_group_id = worker_hosts.worker_group_id
  LEFT JOIN vm_platforms
    ON vm_platforms.id = worker_hosts.vm_platform_id
 WHERE worker_hosts.id = $1
   AND worker_hosts.worker_group_id = $2
   AND worker_hosts.current_epoch = $3
   AND (worker_hosts.status = 'active' OR ($7::boolean AND worker_hosts.status='draining'))
   AND worker_pools.status = 'active'
	AND worker_hosts.observed_at >= clock_timestamp() - $5 * interval '1 second'
	AND worker_hosts.run_paused_reason IS NULL
	AND vm_platforms.arch = $4
	   AND vm_platforms.contract = $6
	FOR UPDATE OF worker_hosts`, fence.WorkerHostID, fence.GroupID,
		fence.WorkerEpoch, fence.RunArchitecture,
		workerapi.WorkerObservationFreshnessSeconds, vmplatform.Contract, fence.AllowDraining,
	).Scan(&workerID)
	if err != nil {
		return fmt.Errorf("lock eligible worker epoch: %w", err)
	}
	return nil
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
