package dispatch

import (
	"context"
	"crypto/sha256"
	"encoding/binary"
	"errors"
	"fmt"
	"hash/fnv"
	"math"
	"strings"

	"github.com/helmrdotdev/helmr/internal/db"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/jackc/pgx/v5/pgxpool"
)

const (
	runQueueReconcileLockName   = "helmr.dispatcher.run_queue_reconciler"
	buildQueueReconcileLockName = "helmr.dispatcher.build_queue_reconciler"
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

func (l *QueueReconcileAdvisoryLock) TryLock(ctx context.Context) (QueueReconcileLockGuard, bool, error) {
	guard, locked, err := l.lock.tryLock(ctx)
	if err != nil || !locked {
		return nil, locked, err
	}
	return queueReconcileAdvisoryLockGuard{guard: guard}, true, nil
}

func (l *advisoryLock) tryLock(ctx context.Context) (*advisoryLockGuard, bool, error) {
	conn, err := l.pool.Acquire(ctx)
	if err != nil {
		return nil, false, fmt.Errorf("acquire advisory lock connection: %w", err)
	}
	var locked bool
	if err := conn.QueryRow(ctx, "select pg_try_advisory_lock($1)", l.key).Scan(&locked); err != nil {
		discardAdvisoryLockConnection(conn)
		return nil, false, fmt.Errorf("acquire advisory lock: %w", err)
	}
	if !locked {
		conn.Release()
		return nil, false, nil
	}
	return &advisoryLockGuard{conn: conn, key: l.key}, true, nil
}

type advisoryLockGuard struct {
	conn *pgxpool.Conn
	key  int64
}

type queueReconcileAdvisoryLockGuard struct {
	guard *advisoryLockGuard
}

func (g queueReconcileAdvisoryLockGuard) Store(QueueReconcilerStore) QueueReconcilerStore {
	return db.New(g.guard.conn)
}

func (g queueReconcileAdvisoryLockGuard) Unlock(ctx context.Context) error {
	return g.guard.Unlock(ctx)
}

func (g *advisoryLockGuard) Unlock(ctx context.Context) error {
	if g == nil || g.conn == nil {
		return errors.New("advisory lock guard is already released")
	}
	conn := g.conn
	g.conn = nil
	var unlocked bool
	if err := conn.QueryRow(ctx, "select pg_advisory_unlock($1)", g.key).Scan(&unlocked); err != nil {
		discardAdvisoryLockConnection(conn)
		return fmt.Errorf("release advisory lock: %w", err)
	}
	if !unlocked {
		discardAdvisoryLockConnection(conn)
		return fmt.Errorf("advisory lock was not held")
	}
	conn.Release()
	return nil
}

func discardAdvisoryLockConnection(conn *pgxpool.Conn) {
	raw := conn.Hijack()
	_ = raw.Close(context.Background())
}

func advisoryLockKey(name string) int64 {
	h := fnv.New64a()
	_, _ = h.Write([]byte(name))
	return int64(h.Sum64() & math.MaxInt64)
}

func queueScopeLockKey(environmentID pgtype.UUID, queueName string, concurrencyKey pgtype.Text) (int64, error) {
	if !environmentID.Valid {
		return 0, errors.New("environment ID is required")
	}
	if strings.TrimSpace(queueName) == "" {
		return 0, errors.New("queue name is required")
	}
	if concurrencyKey.Valid && concurrencyKey.String == "" {
		return 0, errors.New("persisted concurrency key cannot be empty")
	}

	queue := []byte(queueName)
	concurrency := []byte{}
	if concurrencyKey.Valid {
		concurrency = []byte(concurrencyKey.String)
	}
	body := make([]byte, 0, len("helmr.lock.queue.v0\x00")+16+8+len(queue)+len(concurrency))
	body = append(body, "helmr.lock.queue.v0\x00"...)
	body = appendLengthPrefixed(body, environmentID.Bytes[:])
	body = appendLengthPrefixed(body, queue)
	body = appendLengthPrefixed(body, concurrency)
	digest := sha256.Sum256(body)
	return int64(binary.BigEndian.Uint64(digest[:8])), nil
}

func appendLengthPrefixed(dst, value []byte) []byte {
	var length [4]byte
	binary.BigEndian.PutUint32(length[:], uint32(len(value)))
	dst = append(dst, length[:]...)
	return append(dst, value...)
}
