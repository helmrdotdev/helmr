package dispatch

import (
	"context"
	"errors"
	"fmt"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/jackc/pgx/v5/pgxpool"
)

var (
	ErrNilPool             = errors.New("dispatch: nil pgx pool")
	ErrCapacityUnavailable = errors.New("dispatch: certified capacity unavailable")
	ErrCandidateChanged    = errors.New("dispatch: placement candidate changed while locking")
)

type Authority struct {
	pool *pgxpool.Pool
}

func NewAuthority(pool *pgxpool.Pool) (*Authority, error) {
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
	GroupID               string
	RegionID              string
	WorkerInstanceID      pgtype.UUID
	WorkerEpoch           int64
	WorkerProtocolVersion string
	ObservationFreshAfter pgtype.Timestamptz
	Role                  string
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
   AND protocol_version = $3
   AND (($4 = 'run' AND allows_run) OR ($4 = 'build' AND allows_build))
 FOR UPDATE`, fence.GroupID, fence.RegionID, fence.WorkerProtocolVersion, fence.Role).Scan(&groupID)
	if err != nil {
		return fmt.Errorf("lock eligible worker group: %w", err)
	}

	var workerID pgtype.UUID
	err = tx.QueryRow(ctx, `
SELECT worker_instances.id
  FROM worker_instances
  JOIN worker_observations
    ON worker_observations.worker_instance_id = worker_instances.id
   AND worker_observations.worker_epoch = worker_instances.current_epoch
 WHERE worker_instances.id = $1
   AND worker_instances.worker_group_id = $2
   AND worker_instances.current_epoch = $3
   AND worker_instances.state = 'active'
   AND worker_instances.protocol_version = $4
   AND worker_observations.observed_at >= $5
   AND (($6 = 'run' AND worker_instances.supports_run)
        OR ($6 = 'build' AND worker_instances.supports_build))
   AND (($6 = 'run' AND worker_observations.run_paused_reason IS NULL)
        OR ($6 = 'build' AND worker_observations.build_paused_reason IS NULL))
 FOR UPDATE OF worker_instances`, fence.WorkerInstanceID, fence.GroupID,
		fence.WorkerEpoch, fence.WorkerProtocolVersion, fence.ObservationFreshAfter,
		fence.Role).Scan(&workerID)
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
