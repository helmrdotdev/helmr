package control

import (
	"context"
	"fmt"
	"hash/fnv"
	"math"

	"github.com/helmrdotdev/helmr/internal/db"
	"github.com/jackc/pgx/v5/pgxpool"
)

const sweeperLockName = "helmr.control.sweeper"

type AdvisoryLock struct {
	pool *pgxpool.Pool
	key  int64
}

func NewSweeperAdvisoryLock(pool *pgxpool.Pool) (*AdvisoryLock, error) {
	if pool == nil {
		return nil, fmt.Errorf("database pool is required")
	}
	return &AdvisoryLock{
		pool: pool,
		key:  advisoryLockKey(sweeperLockName),
	}, nil
}

func (l *AdvisoryLock) TryLock(ctx context.Context) (SweepLockGuard, bool, error) {
	conn, err := l.pool.Acquire(ctx)
	if err != nil {
		return nil, false, fmt.Errorf("acquire advisory lock connection: %w", err)
	}
	var locked bool
	if err := conn.QueryRow(ctx, "select pg_try_advisory_lock($1)", l.key).Scan(&locked); err != nil {
		conn.Release()
		return nil, false, fmt.Errorf("acquire advisory lock: %w", err)
	}
	if !locked {
		conn.Release()
		return nil, false, nil
	}
	return advisoryLockGuard{conn: conn, key: l.key}, true, nil
}

type advisoryLockGuard struct {
	conn *pgxpool.Conn
	key  int64
}

func (g advisoryLockGuard) Store(SweeperStore) SweeperStore {
	return db.New(g.conn)
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
