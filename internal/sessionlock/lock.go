package sessionlock

import (
	"context"
	"crypto/sha256"
	"encoding/binary"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
)

const unlockTimeout = 5 * time.Second

func Key(name string) int64 {
	digest := sha256.Sum256(append([]byte("helmr.session-lock.v0\x00"), name...))
	return int64(binary.BigEndian.Uint64(digest[:8]))
}

type Guard struct {
	conn *pgxpool.Conn
	keys []int64
}

func Acquire(ctx context.Context, pool *pgxpool.Pool, keys []int64) (*Guard, error) {
	if pool == nil {
		return nil, errors.New("session lock pool is required")
	}
	if len(keys) == 0 {
		return nil, errors.New("session lock key is required")
	}
	conn, err := pool.Acquire(ctx)
	if err != nil {
		return nil, fmt.Errorf("acquire session lock connection: %w", err)
	}
	guard := &Guard{conn: conn}
	for _, key := range keys {
		if _, err := conn.Exec(ctx, "SELECT pg_advisory_lock($1)", key); err != nil {
			guard.discard()
			return nil, fmt.Errorf("acquire session lock: %w", err)
		}
		guard.keys = append(guard.keys, key)
	}
	return guard, nil
}

func TryAcquire(ctx context.Context, pool *pgxpool.Pool, key int64) (*Guard, bool, error) {
	if pool == nil {
		return nil, false, errors.New("session lock pool is required")
	}
	conn, err := pool.Acquire(ctx)
	if err != nil {
		return nil, false, fmt.Errorf("acquire session lock connection: %w", err)
	}
	var acquired bool
	if err := conn.QueryRow(ctx, "SELECT pg_try_advisory_lock($1)", key).Scan(&acquired); err != nil {
		guard := &Guard{conn: conn}
		guard.discard()
		return nil, false, fmt.Errorf("acquire session lock: %w", err)
	}
	if !acquired {
		conn.Release()
		return nil, false, nil
	}
	return &Guard{conn: conn, keys: []int64{key}}, true, nil
}

func (g *Guard) Conn() *pgxpool.Conn {
	if g == nil {
		return nil
	}
	return g.conn
}

func (g *Guard) Unlock() error {
	if g == nil || g.conn == nil {
		return errors.New("session lock guard is already released")
	}
	conn := g.conn
	ctx, cancel := context.WithTimeout(context.Background(), unlockTimeout)
	defer cancel()
	for index := len(g.keys) - 1; index >= 0; index-- {
		var unlocked bool
		if err := conn.QueryRow(ctx, "SELECT pg_advisory_unlock($1)", g.keys[index]).Scan(&unlocked); err != nil || !unlocked {
			if err == nil {
				err = errors.New("session lock was not held")
			}
			g.discard()
			return fmt.Errorf("release session lock: %w", err)
		}
	}
	g.conn = nil
	g.keys = nil
	conn.Release()
	return nil
}

func (g *Guard) discard() {
	if g == nil || g.conn == nil {
		return
	}
	conn := g.conn.Hijack()
	g.conn = nil
	g.keys = nil
	ctx, cancel := context.WithTimeout(context.Background(), unlockTimeout)
	defer cancel()
	_ = conn.Close(ctx)
}
