package dispatch

import (
	"context"
	"errors"
	"fmt"

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

const platformArchitecture = "x86_64"

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

func NewBuildAuthority(pool *pgxpool.Pool) (*Authority, error) {
	return newAuthority(pool)
}

func newAuthority(pool *pgxpool.Pool) (*Authority, error) {
	if pool == nil {
		return nil, ErrNilPool
	}
	return &Authority{pool: pool}, nil
}

func (d *Authority) begin(ctx context.Context) (pgx.Tx, error) {
	return d.pool.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.Serializable})
}

func rollback(ctx context.Context, tx pgx.Tx) {
	_ = tx.Rollback(ctx)
}

type workerFence struct {
	GroupID          string
	RegionID         string
	WorkerInstanceID pgtype.UUID
	WorkerEpoch      int64
	Role             string
	RunArchitecture  string
}

// lockWorkerFence takes the worker-group lock before the worker lock, matching
// the global execution lock order. Observation freshness is rechecked while
// those authority rows remain locked.
func lockWorkerFence(ctx context.Context, tx pgx.Tx, fence workerFence) error {
	var groupID string
	err := tx.QueryRow(ctx, `
SELECT id
  FROM worker_groups
 WHERE id = $1 AND region_id = $2 AND state = 'active'
   AND (($3 = 'run' AND allows_run) OR ($3 = 'build' AND allows_build))
 FOR UPDATE`, fence.GroupID, fence.RegionID, fence.Role).Scan(&groupID)
	if err != nil {
		return fmt.Errorf("lock eligible worker group: %w", err)
	}

	architecture := fence.RunArchitecture
	if fence.Role == "build" {
		architecture = platformArchitecture
	}
	var workerID pgtype.UUID
	err = tx.QueryRow(ctx, `
SELECT worker_instances.id
  FROM worker_instances
  JOIN worker_groups
    ON worker_groups.id = worker_instances.worker_group_id
  LEFT JOIN runtime_identities
    ON runtime_identities.id = worker_instances.runtime_identity_id
 WHERE worker_instances.id = $1
   AND worker_instances.worker_group_id = $2
   AND worker_instances.current_epoch = $3
   AND worker_instances.state = 'active'
   AND worker_instances.observed_at >= transaction_timestamp() - $6 * interval '1 second'
	AND (($4 = 'run' AND worker_instances.supports_run)
	     OR ($4 = 'build' AND worker_instances.supports_build))
	AND (($4 = 'run' AND worker_instances.run_paused_reason IS NULL)
	     OR ($4 = 'build' AND worker_instances.build_paused_reason IS NULL))
	AND runtime_identities.runtime_arch = $5
	   AND runtime_identities.vm_runtime_contract = 'helmr.vm-runtime.v0'
	FOR UPDATE OF worker_instances`, fence.WorkerInstanceID, fence.GroupID,
		fence.WorkerEpoch, fence.Role, architecture,
		workerapi.WorkerObservationFreshnessSeconds,
	).Scan(&workerID)
	if err != nil {
		return fmt.Errorf("lock eligible worker epoch: %w", err)
	}
	return nil
}

func lockSource(ctx context.Context, tx pgx.Tx, kind string, id pgtype.UUID) error {
	_, err := tx.Exec(ctx, `SELECT pg_advisory_xact_lock(hashtextextended(concat_ws(':', $1::text, $2::uuid::text), 0))`, kind, id)
	if err != nil {
		return fmt.Errorf("lock %s source: %w", kind, err)
	}
	return nil
}
