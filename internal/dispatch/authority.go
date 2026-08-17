package dispatch

import (
	"context"
	"errors"
	"fmt"

	"github.com/helmrdotdev/helmr/capacityapi"
	"github.com/helmrdotdev/helmr/internal/workerapi"
	"github.com/helmrdotdev/helmr/internal/workspace"
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
	pool          *pgxpool.Pool
	fencingKey    workspace.FencingKey
	nestedResumes nestedResumeCursor
}

func NewRunAuthority(
	pool *pgxpool.Pool,
	fencingKey workspace.FencingKey,
) (*Authority, error) {
	authority, err := newAuthority(pool)
	if err != nil {
		return nil, err
	}
	if !fencingKey.Valid() {
		return nil, errors.New("run authority workspace fencing key is required")
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
	GroupID          string
	RegionID         string
	WorkerInstanceID pgtype.UUID
	WorkerEpoch      int64
	RunArchitecture  string
	RequirePrimary   bool
}

// lockWorkerFence takes a shared worker-group lock before the worker lock,
// matching the global execution lock order. Placements in the same group may
// proceed on independent Workers, while a group lifecycle change waits for all
// in-flight placements. Observation freshness is rechecked while those
// authority rows remain locked.
func lockWorkerFence(ctx context.Context, tx pgx.Tx, fence workerFence) error {
	var groupID string
	err := tx.QueryRow(ctx, `
SELECT id
  FROM worker_groups
 WHERE id = $1 AND region_id = $2 AND state = 'active'
 FOR SHARE`, fence.GroupID, fence.RegionID).Scan(&groupID)
	if err != nil {
		return fmt.Errorf("lock eligible worker group: %w", err)
	}
	var poolID pgtype.UUID
	err = tx.QueryRow(ctx, `
SELECT worker_pools.id
  FROM worker_pools
  JOIN worker_instances
    ON worker_instances.worker_pool_id = worker_pools.id
   AND worker_instances.worker_group_id = worker_pools.worker_group_id
	JOIN worker_groups
	  ON worker_groups.id = worker_pools.worker_group_id
 WHERE worker_instances.id = $1
   AND worker_instances.worker_group_id = $2
   AND worker_pools.state = 'active'
	AND (NOT $3::boolean OR worker_groups.primary_pool_id = worker_pools.id)
	FOR SHARE OF worker_pools`, fence.WorkerInstanceID, fence.GroupID, fence.RequirePrimary).Scan(&poolID)
	if err != nil {
		return fmt.Errorf("lock eligible worker pool: %w", err)
	}

	var workerID pgtype.UUID
	err = tx.QueryRow(ctx, `
SELECT worker_instances.id
  FROM worker_instances
  JOIN worker_groups
    ON worker_groups.id = worker_instances.worker_group_id
  JOIN worker_pools
    ON worker_pools.id = worker_instances.worker_pool_id
   AND worker_pools.worker_group_id = worker_instances.worker_group_id
  LEFT JOIN runtime_identities
    ON runtime_identities.id = worker_instances.runtime_identity_id
 WHERE worker_instances.id = $1
   AND worker_instances.worker_group_id = $2
   AND worker_instances.current_epoch = $3
   AND worker_instances.state = 'active'
   AND worker_pools.state = 'active'
	AND worker_instances.observed_at >= transaction_timestamp() - $5 * interval '1 second'
	AND worker_instances.run_paused_reason IS NULL
	AND runtime_identities.runtime_arch = $4
	   AND runtime_identities.vm_runtime_contract = $6
	FOR UPDATE OF worker_instances`, fence.WorkerInstanceID, fence.GroupID,
		fence.WorkerEpoch, fence.RunArchitecture,
		workerapi.WorkerObservationFreshnessSeconds, capacityapi.RuntimeContract,
	).Scan(&workerID)
	if err != nil {
		return fmt.Errorf("lock eligible worker epoch: %w", err)
	}
	return nil
}

// checkLockedWorkerRuntimeAdmission keeps Runtime-slot admission separate from
// the Run-domain fence. Callers must already hold the worker row lock. A
// Runtime pause prevents creating or reclaiming VM state, but does not prevent
// a Run from reusing an already-ready Workspace Runtime.
func checkLockedWorkerRuntimeAdmission(
	ctx context.Context,
	tx pgx.Tx,
	workerInstanceID pgtype.UUID,
	workerEpoch int64,
) error {
	var workerID pgtype.UUID
	return tx.QueryRow(ctx, `
SELECT id
  FROM worker_instances
 WHERE id = $1
   AND current_epoch = $2
   AND runtime_paused_reason IS NULL
 FOR UPDATE`, workerInstanceID, workerEpoch).Scan(&workerID)
}
