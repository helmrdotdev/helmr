package dispatch

import (
	"context"
	"fmt"
	"hash/fnv"
	"math"

	"github.com/helmrdotdev/helmr/internal/db"
	"github.com/jackc/pgx/v5/pgxpool"
)

const (
	runQueueReconcileLockName   = "helmr.dispatcher.run_queue_reconciler"
	buildQueueReconcileLockName = "helmr.dispatcher.build_queue_reconciler"
	preparedRuntimeWarmName     = "helmr.dispatcher.runtime_preparer"
)

type advisoryLock struct {
	pool *pgxpool.Pool
	key  int64
}

func newAdvisoryLock(pool *pgxpool.Pool, name string) (*advisoryLock, error) {
	if pool == nil {
		return nil, fmt.Errorf("database pool is required")
	}
	return &advisoryLock{
		pool: pool,
		key:  advisoryLockKey(name),
	}, nil
}

type QueueReconcileAdvisoryLock struct {
	lock *advisoryLock
}

func NewQueueReconcileAdvisoryLock(pool *pgxpool.Pool) (*QueueReconcileAdvisoryLock, error) {
	return newQueueReconcileAdvisoryLock(pool, runQueueReconcileLockName)
}

func NewBuildQueueReconcileAdvisoryLock(pool *pgxpool.Pool) (*QueueReconcileAdvisoryLock, error) {
	return newQueueReconcileAdvisoryLock(pool, buildQueueReconcileLockName)
}

func newQueueReconcileAdvisoryLock(pool *pgxpool.Pool, name string) (*QueueReconcileAdvisoryLock, error) {
	if pool == nil {
		return nil, fmt.Errorf("database pool is required")
	}
	lock, err := newAdvisoryLock(pool, name)
	if err != nil {
		return nil, err
	}
	return &QueueReconcileAdvisoryLock{lock: lock}, nil
}

type RuntimePrepareAdvisoryLock struct {
	lock *advisoryLock
}

func NewRuntimePrepareAdvisoryLock(pool *pgxpool.Pool) (*RuntimePrepareAdvisoryLock, error) {
	if pool == nil {
		return nil, fmt.Errorf("database pool is required")
	}
	lock, err := newAdvisoryLock(pool, preparedRuntimeWarmName)
	if err != nil {
		return nil, err
	}
	return &RuntimePrepareAdvisoryLock{lock: lock}, nil
}

func (l *RuntimePrepareAdvisoryLock) TryLock(ctx context.Context) (RuntimePrepareLockGuard, bool, error) {
	guard, locked, err := l.lock.tryLock(ctx)
	if err != nil || !locked {
		return nil, locked, err
	}
	return preparedRuntimeWarmAdvisoryLockGuard{guard: guard}, true, nil
}

func (l *QueueReconcileAdvisoryLock) TryLock(ctx context.Context) (QueueReconcileLockGuard, bool, error) {
	guard, locked, err := l.lock.tryLock(ctx)
	if err != nil || !locked {
		return nil, locked, err
	}
	return queueReconcileAdvisoryLockGuard{guard: guard}, true, nil
}

func (l *advisoryLock) tryLock(ctx context.Context) (advisoryLockGuard, bool, error) {
	conn, err := l.pool.Acquire(ctx)
	if err != nil {
		return advisoryLockGuard{}, false, fmt.Errorf("acquire advisory lock connection: %w", err)
	}
	var locked bool
	if err := conn.QueryRow(ctx, "select pg_try_advisory_lock($1)", l.key).Scan(&locked); err != nil {
		conn.Release()
		return advisoryLockGuard{}, false, fmt.Errorf("acquire advisory lock: %w", err)
	}
	if !locked {
		conn.Release()
		return advisoryLockGuard{}, false, nil
	}
	return advisoryLockGuard{conn: conn, key: l.key}, true, nil
}

type advisoryLockGuard struct {
	conn *pgxpool.Conn
	key  int64
}

type queueReconcileAdvisoryLockGuard struct {
	guard advisoryLockGuard
}

func (g queueReconcileAdvisoryLockGuard) Store(QueueReconcilerStore) QueueReconcilerStore {
	return db.New(g.guard.conn)
}

func (g queueReconcileAdvisoryLockGuard) Unlock(ctx context.Context) error {
	return g.guard.Unlock(ctx)
}

type preparedRuntimeWarmAdvisoryLockGuard struct {
	guard advisoryLockGuard
}

func (g preparedRuntimeWarmAdvisoryLockGuard) Unlock(ctx context.Context) error {
	return g.guard.Unlock(ctx)
}

func (g advisoryLockGuard) Unlock(ctx context.Context) error {
	defer g.conn.Release()
	var unlocked bool
	if err := g.conn.QueryRow(ctx, "select pg_advisory_unlock($1)", g.key).Scan(&unlocked); err != nil {
		return fmt.Errorf("release advisory lock: %w", err)
	}
	if !unlocked {
		return fmt.Errorf("advisory lock was not held")
	}
	return nil
}

func advisoryLockKey(name string) int64 {
	h := fnv.New64a()
	_, _ = h.Write([]byte(name))
	return int64(h.Sum64() & math.MaxInt64)
}
