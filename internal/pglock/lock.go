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
// Dispatch locks worker_groups and worker_pools FOR SHARE; other worker
// supply operations lock them FOR UPDATE. Named exceptions:
//
//   - Fresh Run admission and Computer restore take run queue-scope advisory
//     transaction locks before secrets; restore takes the sorted union of its
//     members' queue scopes.
//   - Public Task start, through the run owner, acquires its idempotency
//     claim, then locks its environment FOR NO KEY UPDATE with the current
//     deployment's Task, then the Computer's secrets, then the Computer
//     through the computer owner's admission lock.
//   - Actor start, through the session owner, acquires its idempotency claim,
//     then locks its environment FOR NO KEY UPDATE with the current
//     deployment's Actor, then, for a keyed start, takes the transaction
//     advisory lock on the environment, Actor and key, then locks the
//     Computer's secrets and then the Computer through the computer owner's
//     admission lock. A run-sourced start takes the worker Session operation
//     locks below, with the start Computer addressed, after the key lock (on
//     replay, right after the claim), and then locks the start Computer's
//     secrets again without comparing them with the first lock.
//   - Public Computer creation acquires its idempotency claim before secrets.
//     Run-sourced creation locks secrets, then the live source Run, then the
//     idempotency claim, including on replay.
//   - A schedule fire locks its environment FOR NO KEY UPDATE, then the
//     schedule, then the schedule's secrets before it creates the Computer.
//     The environment lock serializes fires with deployment promotion, which
//     locks the environment, the scheduled secrets and then the schedules.
//   - Computer deletion acquires its idempotency claim first; a run-sourced
//     deletion then locks the live source Run with the target Computer.
//     Under the Computer lock, deletion updates the Computer's checkpoint
//     rows before it locks the Computer instance.
//   - Worker host credential authentication locks the credential, host, group
//     and pool rows in one FOR UPDATE statement rather than in separate steps.
//   - Computer Instance operations take the order above through the computer
//     owner. Channel claim, writer renewal and the restore plan lock secrets,
//     worker_groups, worker_hosts, the Computer and then its Instance; the
//     restore plan then locks the restored members' Run leases. Run cleanup
//     omits secrets. Readiness and the restore fence share worker_groups and
//     worker_pools like dispatch. A failure report locks worker_groups,
//     worker_pools and worker_hosts FOR UPDATE; an invalid-epoch drain runs in
//     its own transaction before and, when the Instance fence no longer holds,
//     after it. Close, expiry and preparation settlement lock only the
//     Computer and its Instance.
//   - Computer preparation (initial and source key delivery, initial object
//     recording and initial version publication) locks worker_groups and
//     worker_pools FOR SHARE like dispatch, then worker_hosts, the Computer
//     and its Instance, and reads the disk version last; it then compares the
//     worker's claim versions with the locked worker_hosts and worker_groups
//     rows.
//   - Checkpoint capture, which the computer owner begins for explicit, idle
//     and program-replacement captures, locks worker_groups and worker_hosts
//     without comparing claim versions, the Computer and its Instance, then
//     the Session, Run, Attempt, Run lease, Wait and Session turn rows of the
//     Instance's unreconciled leases, and checks deadlines after the last
//     lock. Program replacement, through the computer owner, takes the
//     capture fence (worker_groups, worker_hosts without comparing claim
//     versions, the Computer, its Instance) before the capture's member
//     locks. When only a ready checkpoint remains, it locks the Computer,
//     the checkpoint's source Instance and then the checkpoint, and it
//     commits the checkpoint's disk version after the Run assignment locks.
//   - Checkpoint object recording locks worker_groups, worker_hosts, the
//     Computer and its Instance, then the Session, Run, Attempt, Run lease and
//     Wait rows of the Instance's unreconciled leases, then the checkpoint,
//     without comparing claim versions, and repeats the whole fence before
//     commit. Checkpoint registration, readiness and failure take the same
//     checkpoint source locks; readiness takes them in a checking transaction
//     and again in the completing transaction, and reads object storage
//     between the two.
//   - Saves (admission, object recording, publication, adoption and
//     abandonment) lock the Computer's secrets, worker_groups, worker_hosts,
//     the Computer and then its Instance.
//   - A restore commit takes its queue-scope advisory locks and then the
//     restored members' Secret locks before the restore fence, and locks the
//     restored members after it: Session, Run, Attempt, Wait, then the
//     checkpoint. A restore acknowledgement adds the Run leases after the
//     Attempts and the Session turns after the Waits, before the checkpoint.
//   - Run lease operations, through the run owner, lock the execution host (a
//     lease claim first locks its attempt's Secrets), then every Computer the
//     Run lineage reaches in id order (with an addressed target Computer in
//     the same statement), then those Computers' unreclaimed Instances in id
//     order, before re-locking the lease's own Computer and Instance and the
//     Session, Run, Attempt and lease. Run cancellation takes the same ordered
//     Computer and Instance locks before any member lock.
//   - Run lease operations that deliver Secrets (live operations with
//     Secrets, finalization begin, task completion and waits) lock the source
//     attempt's Secrets through the run owner before the execution host.
//   - Worker Session operations that address another Session or Computer
//     (send, enqueue, close, event reads, run-sourced Actor start) lock the
//     source Computer's Secrets, plus an addressed Computer's (never an
//     addressed Session's Computer's), then the execution fence with the
//     target in the same ordered statements, then the source attempt's
//     delivery Secrets. That delivery lock reads the source Computer's
//     bindings again without comparing them with the first lock, so a
//     binding added before the execution fence locks the source Computer is
//     locked after the fence; these operations have no re-read check.
//   - Child Task invocation, through the run owner, acquires its idempotency
//     claim, then locks the target Computer's secrets, then the execution
//     fence with the target Computer in the same ordered statements. A new
//     child then re-locks the already-held target Computer through the
//     computer owner's admission lock. The live execution is locked again
//     before commit.
//   - Worker Session controls (cancel, interrupt, resume) lock the sorted
//     union of the source Computer's and the target Session's Computer's
//     Secrets, then the execution fence, then re-read the union after the
//     Session and Computer fences and fail if a binding changed, re-locking
//     only the same Secrets.
//   - Public Session cancel and interrupt with a current Run lock that Run's
//     attempt Secrets, then its owned finalization graph, then re-lock the
//     Session's Computer (already locked by the graph) and its Instance, then
//     the Session. With no current Run they lock the Session before its
//     Computer and Instance, an inversion of the order above. Public resume
//     takes its own admission path: it locks the Session's Computer's
//     Secrets, then the Computer, its Instance and the Session, then its
//     idempotency claim, and then re-locks the already-held Computer through
//     the Session predicate.
//   - Session timeout reconciliation locks the Computer's Secrets, the
//     Computer and its Instance, then the Session and Run, re-locks the
//     already-held Computer through the open-Session predicate, then locks
//     the Attempt and Wait.
//   - Session operations take their Computer and Instance locks through the
//     computer owner: the admission lock and the unreclaimed Instance before
//     the Session, and the Session-scoped Computer re-locks at the positions
//     described above.
//   - Timer and Token wait reconciliation lock the Run's own Computer and its
//     unreclaimed Instance through the computer owner (timer: after that
//     Computer's Secrets and within the Run's scope, locking no Instance when
//     the scope no longer matches), then the Session, Run, Attempt and Wait
//     (Token: then the Token), without worker supply or lineage locks. Token
//     wait registration, staged through the run owner, locks worker_groups
//     and worker_hosts without comparing claim versions, then the Computer,
//     its Instance, the Session, Run, Attempt, Run lease, Wait and Token.
//   - Command operations, through the command owner, lock Secrets first when
//     they deliver or validate them (claim, recovery). Worker-reported
//     operations then lock worker_groups and worker_hosts, comparing claim
//     versions. All of them then lock the Computer and the Command's
//     Instance, then the Command. A pending-Command failure also locks an
//     Instance that was bound after discovery before it rejects. Command
//     creation acquires its idempotency claim before secrets, then locks the
//     Computer. Public cancellation acquires its idempotency claim and then
//     updates the Command without Computer or Instance locks. Result
//     retention prunes Command rows directly.
//   - Session-level singleton locks are acquired before, and held around, the
//     transactions their holder runs. The stale worker fencer runs its
//     transaction on the guard's connection. A dispatcher run lane
//     (helmr.dispatcher.run_lane.<n>) runs only lane discovery on the
//     guard's connection; the assignments it starts open their transactions on
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
