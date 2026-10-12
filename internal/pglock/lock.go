// Package pglock holds PostgreSQL advisory locks: a session-level guard on a
// dedicated pooled connection (TryAcquire) and the Key derivation shared by
// session-level and transaction-level advisory locks.
//
// # Global lock order
//
// Every transaction takes exclusive row locks (FOR UPDATE, FOR NO KEY UPDATE)
// and advisory locks in this order; share locks are listed where they gate.
// Multi-row steps lock in key order, and operations spanning Environments use
// the Environment ID as the outer key. Owners name the step they take beside
// the code that takes it.
//
//  1. Advisory gates: idempotency receipts, Slack admission.
//  2. Worker supply: group, pool, host (share for runtime callers; host
//     FOR UPDATE when allocating). Runtime-caller operations take this first.
//  3. Slack: registration, installation, connection, channel binding.
//  4. Environment.
//  5. Deployment (share) and trigger activations. Enqueue takes the Deployment
//     share lock after its Session locks; no transaction takes deployments
//     exclusively while holding Session locks.
//  6. Image spec, then image build. Secrets are taken FOR SHARE here; the only
//     exclusive Secret locks are rotate and revoke, which take nothing
//     exclusive afterwards.
//  7. Session roots, then Computers, then member Sessions.
//  8. Rows owned under those: processes, checkpoints, leases, saves, commands,
//     posts.
//
// Rows inserted by the same transaction take no position. Implicit foreign-key
// and unique waits follow the parent's step. Lease and checkpoint rows are
// taken in either order under the same Computer lock. Computer-only operations
// enter at step 7 with the Computer.
package pglock

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

// Guard holds one session-level advisory lock on its own pooled connection.
type Guard struct {
	conn *pgxpool.Conn
	key  int64
}

func TryAcquire(ctx context.Context, pool *pgxpool.Pool, key int64) (*Guard, bool, error) {
	if pool == nil {
		return nil, false, errors.New("PostgreSQL advisory lock pool is required")
	}
	conn, err := pool.Acquire(ctx)
	if err != nil {
		return nil, false, fmt.Errorf("acquire PostgreSQL advisory lock connection: %w", err)
	}
	var acquired bool
	if err := conn.QueryRow(ctx, "SELECT pg_try_advisory_lock($1)", key).Scan(&acquired); err != nil {
		guard := &Guard{conn: conn}
		guard.discard()
		return nil, false, fmt.Errorf("acquire PostgreSQL advisory lock: %w", err)
	}
	if !acquired {
		conn.Release()
		return nil, false, nil
	}
	return &Guard{conn: conn, key: key}, true, nil
}

func (g *Guard) Conn() *pgxpool.Conn {
	if g == nil {
		return nil
	}
	return g.conn
}

func (g *Guard) Unlock() error {
	if g == nil || g.conn == nil {
		return errors.New("PostgreSQL advisory lock guard is already released")
	}
	conn := g.conn
	ctx, cancel := context.WithTimeout(context.Background(), unlockTimeout)
	defer cancel()
	var unlocked bool
	if err := conn.QueryRow(ctx, "SELECT pg_advisory_unlock($1)", g.key).Scan(&unlocked); err != nil || !unlocked {
		if err == nil {
			err = errors.New("PostgreSQL advisory lock was not held")
		}
		g.discard()
		return fmt.Errorf("release PostgreSQL advisory lock: %w", err)
	}
	g.conn = nil
	conn.Release()
	return nil
}

func (g *Guard) discard() {
	if g == nil || g.conn == nil {
		return
	}
	conn := g.conn.Hijack()
	g.conn = nil
	ctx, cancel := context.WithTimeout(context.Background(), unlockTimeout)
	defer cancel()
	_ = conn.Close(ctx)
}
