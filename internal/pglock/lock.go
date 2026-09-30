// Package pglock holds PostgreSQL advisory locks: a session-level guard on a
// dedicated pooled connection (TryAcquire) and the Key derivation that
// transaction-level advisory locks taken through sqlc queries share.
//
// # Lock order
//
// The intended order for a transaction that takes more than one of these
// locks is:
//
//  1. advisory lock helmr:worker-group-create:<region> (worker group creation
//     and the bootstrap seed)
//  2. regions
//  3. advisory lock helmr:worker-group-lifecycle:<group> (worker group status
//     transitions and operator host-loss confirmation)
//  4. secrets
//  5. worker_groups
//  6. worker_pools
//  7. worker_hosts
//  8. vm_platforms and worker_pool_cpu_shapes
//  9. Computer
//  10. Computer instance
//  11. Session
//  12. Run lineage
//  13. Attempt
//  14. Run lease
//  15. Wait and checkpoint
//
// Placement locks worker_groups and worker_pools FOR SHARE; other worker
// supply operations lock them FOR UPDATE. Named exceptions:
//
//   - Fresh Run admission and Computer restore take run queue-scope advisory
//     transaction locks before secrets; restore takes the sorted union of its
//     members' queue scopes.
//   - Worker host credential authentication locks the credential, host, group
//     and pool rows in one FOR UPDATE statement rather than in separate steps.
//   - Session-level singleton locks are acquired before, and held around, the
//     transactions their holder runs. The stale worker fencer runs its
//     transaction on the guard's connection. A dispatcher run placement lane
//     (helmr.dispatcher.run_placement_lane.<n>) runs only lane discovery on the
//     guard's connection; the placements it starts open their transactions on
//     other pooled connections.
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
