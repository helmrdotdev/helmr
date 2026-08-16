package dispatch

import (
	"context"
	"crypto/sha256"
	"encoding/binary"
	"errors"
	"fmt"
	"strings"

	"github.com/helmrdotdev/helmr/internal/pglock"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/jackc/pgx/v5/pgxpool"
)

const runLeaseRecoveryLockName = "helmr.dispatcher.run_resume_recovery"

type advisoryLock struct {
	pool *pgxpool.Pool
	key  int64
}

type RunLeaseRecoveryAdvisoryLock struct {
	lock *advisoryLock
}

func NewRunLeaseRecoveryAdvisoryLock(pool *pgxpool.Pool) (*RunLeaseRecoveryAdvisoryLock, error) {
	lock, err := newAdvisoryLock(pool, runLeaseRecoveryLockName)
	if err != nil {
		return nil, err
	}
	return &RunLeaseRecoveryAdvisoryLock{lock: lock}, nil
}

func (l *RunLeaseRecoveryAdvisoryLock) TryLock(ctx context.Context) (RunLeaseRecoveryLockGuard, bool, error) {
	guard, locked, err := l.lock.tryLock(ctx)
	if err != nil || !locked {
		return nil, locked, err
	}
	return guard, true, nil
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

func (l *advisoryLock) tryLock(ctx context.Context) (*advisoryLockGuard, bool, error) {
	guard, locked, err := pglock.TryAcquire(ctx, l.pool, l.key)
	if err != nil || !locked {
		return nil, locked, err
	}
	return &advisoryLockGuard{guard: guard}, true, nil
}

type advisoryLockGuard struct {
	guard *pglock.Guard
}

func (g *advisoryLockGuard) Unlock(context.Context) error {
	if g == nil || g.guard == nil {
		return errors.New("advisory lock guard is already released")
	}
	guard := g.guard
	g.guard = nil
	return guard.Unlock()
}

func advisoryLockKey(name string) int64 {
	return pglock.Key(name)
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
