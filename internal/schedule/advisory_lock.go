package schedule

import (
	"context"
	"fmt"
	"hash/fnv"
	"math"

	"github.com/helmrdotdev/helmr/internal/db"
	"github.com/jackc/pgx/v5/pgxpool"
)

const reconcileLockName = "helmr.schedule.reconciler"

type ReconcileAdvisoryLock struct {
	pool *pgxpool.Pool
	key  int64
}

func NewReconcileAdvisoryLock(pool *pgxpool.Pool) (*ReconcileAdvisoryLock, error) {
	if pool == nil {
		return nil, fmt.Errorf("database pool is required")
	}
	return &ReconcileAdvisoryLock{
		pool: pool,
		key:  advisoryLockKey(reconcileLockName),
	}, nil
}

func (l *ReconcileAdvisoryLock) TryLock(ctx context.Context) (ReconcileLockGuard, bool, error) {
	conn, err := l.pool.Acquire(ctx)
	if err != nil {
		return nil, false, fmt.Errorf("acquire schedule reconcile lock connection: %w", err)
	}
	var locked bool
	if err := conn.QueryRow(ctx, "select pg_try_advisory_lock($1)", l.key).Scan(&locked); err != nil {
		conn.Release()
		return nil, false, fmt.Errorf("acquire schedule reconcile lock: %w", err)
	}
	if !locked {
		conn.Release()
		return nil, false, nil
	}
	return reconcileAdvisoryLockGuard{conn: conn, key: l.key}, true, nil
}

type reconcileAdvisoryLockGuard struct {
	conn *pgxpool.Conn
	key  int64
}

func (g reconcileAdvisoryLockGuard) Store(ReconcileStore) ReconcileStore {
	return db.New(g.conn)
}

func (g reconcileAdvisoryLockGuard) Unlock(ctx context.Context) error {
	defer g.conn.Release()
	var unlocked bool
	if err := g.conn.QueryRow(ctx, "select pg_advisory_unlock($1)", g.key).Scan(&unlocked); err != nil {
		return fmt.Errorf("release schedule reconcile lock: %w", err)
	}
	if !unlocked {
		return fmt.Errorf("schedule reconcile lock was not held")
	}
	return nil
}

func advisoryLockKey(name string) int64 {
	h := fnv.New64a()
	_, _ = h.Write([]byte(name))
	return int64(h.Sum64() & math.MaxInt64)
}
