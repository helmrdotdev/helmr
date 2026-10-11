package workergroup

import (
	"context"
	"errors"
	"testing"
	"time"
	"uuid"

	"github.com/helmrdotdev/helmr/internal/db"
	"github.com/helmrdotdev/helmr/internal/db/dbtest"
	"github.com/helmrdotdev/helmr/internal/pgvalue"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

func (f supplyFixture) begin(t *testing.T) pgx.Tx {
	t.Helper()
	tx, err := f.pool.Begin(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = tx.Rollback(context.Background()) })
	return tx
}

func (f supplyFixture) dispatchSupply(hostID uuid.UUID) DispatchSupply {
	return DispatchSupply{
		GroupID: f.group.ID, RegionID: fixtureRegionID,
		HostID: pgvalue.UUID(hostID), Epoch: 1, RunArchitecture: "x86_64",
	}
}

func TestDispatchSupplyCoordinatesAtHostGranularity(t *testing.T) {
	f := newSupplyFixture(t)
	pool := f.activePool(t, "default")
	firstHost := f.activeHost(t, pool, "host-1")
	secondHost := f.activeHost(t, pool, "host-2")

	first := f.begin(t)
	if _, err := LockDispatchSupply(t.Context(), first, f.dispatchSupply(firstHost)); err != nil {
		t.Fatal(err)
	}
	second := f.begin(t)
	secondCtx, cancelSecond := context.WithTimeout(t.Context(), time.Second)
	defer cancelSecond()
	if _, err := LockDispatchSupply(secondCtx, second, f.dispatchSupply(secondHost)); err != nil {
		t.Fatalf("independent host supply blocked: %v", err)
	}

	transitionCtx, cancelTransition := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancelTransition()
	transitioned := make(chan error, 1)
	go func() {
		_, err := f.pool.Exec(transitionCtx, `
/* dispatch supply group transition */
UPDATE worker_groups SET status = 'paused' WHERE id = $1`, f.group.ID)
		transitioned <- err
	}()
	waitForBlockedQuery(t, f.pool, "dispatch supply group transition", 1)

	if err := second.Commit(t.Context()); err != nil {
		t.Fatal(err)
	}
	if err := first.Commit(t.Context()); err != nil {
		t.Fatal(err)
	}
	if err := <-transitioned; err != nil {
		t.Fatal(err)
	}

	if _, err := LockDispatchSupply(t.Context(), f.begin(t), f.dispatchSupply(firstHost)); err == nil {
		t.Fatal("paused worker group remained eligible for dispatch")
	}
}

func TestDispatchSupplyRequiresRunReadyHost(t *testing.T) {
	for _, test := range []struct {
		name, sql    string
		continuation bool
		eligible     bool
	}{
		{"ready", ``, false, true},
		{"Run paused", `UPDATE worker_hosts SET run_paused_reason='startup_recovery_leak' WHERE id=$1`, false, false},
		{"Run paused continuation", `UPDATE worker_hosts SET run_paused_reason='startup_recovery_leak' WHERE id=$1`, true, true},
		{"stale observation", `UPDATE worker_hosts SET observed_at=clock_timestamp()-interval '1 hour' WHERE id=$1`, false, false},
		{"unobserved", `UPDATE worker_hosts SET observed_at=NULL WHERE id=$1`, false, false},
		{"draining", `UPDATE worker_hosts SET status='draining',draining_at = clock_timestamp(), drain_reason = 'shutdown' WHERE id=$1`, false, false},
		{"draining continuation", `UPDATE worker_hosts SET status='draining',draining_at = clock_timestamp(), drain_reason = 'shutdown' WHERE id=$1`, true, true},
		{"stale draining continuation", `UPDATE worker_hosts SET status='draining',draining_at = clock_timestamp(), drain_reason = 'shutdown',observed_at=clock_timestamp()-interval '1 hour' WHERE id=$1`, true, false},
		{"paused Group", `UPDATE worker_groups SET status='paused' WHERE id=(SELECT worker_group_id FROM worker_hosts WHERE id=$1)`, false, false},
		{"paused Group continuation", `UPDATE worker_groups SET status='paused' WHERE id=(SELECT worker_group_id FROM worker_hosts WHERE id=$1)`, true, true},
		{"draining Group continuation", `UPDATE worker_groups SET status='draining',primary_pool_id=NULL WHERE id=(SELECT worker_group_id FROM worker_hosts WHERE id=$1)`, true, true},
		{"draining Pool", `UPDATE worker_pools SET status='draining' WHERE id=(SELECT worker_pool_id FROM worker_hosts WHERE id=$1)`, false, false},
		{"draining Pool continuation", `UPDATE worker_pools SET status='draining' WHERE id=(SELECT worker_pool_id FROM worker_hosts WHERE id=$1)`, true, true},
	} {
		t.Run(test.name, func(t *testing.T) {
			f := newSupplyFixture(t)
			hostID := f.activeHost(t, f.activePool(t, "default"), "host-1")
			if test.sql != "" {
				dbtest.MustExec(t, t.Context(), f.pool, test.sql, hostID)
			}
			supply := f.dispatchSupply(hostID)
			supply.Continuation = test.continuation
			_, err := LockDispatchSupply(t.Context(), f.begin(t), supply)
			if test.eligible && err != nil {
				t.Fatalf("eligible host rejected: %v", err)
			}
			if !test.eligible && !errors.Is(err, pgx.ErrNoRows) {
				t.Fatalf("ineligible host supply error = %v, want no rows", err)
			}
		})
	}
}

// Claim versions only signal credential freshness: a changed claim version on
// a host that still passes the epoch and status fences reports ErrStaleClaims,
// while supply outside those fences reports no rows.
func TestLockHostWithPoolEnforcesContinuationFences(t *testing.T) {
	for _, test := range []struct {
		name, sql string
		locked    bool
	}{
		{"active", ``, true},
		{"host claims", `UPDATE worker_hosts SET claim_version=claim_version+1 WHERE id=$1`, true},
		{"group claims", `UPDATE worker_groups SET claim_version=claim_version+1 WHERE id=(SELECT worker_group_id FROM worker_hosts WHERE id=$1)`, true},
		{"draining host", `UPDATE worker_hosts SET status='draining',draining_at = now(), drain_reason = 'shutdown' WHERE id=$1`, true},
		{"paused group", `UPDATE worker_groups SET status='paused' WHERE id=(SELECT worker_group_id FROM worker_hosts WHERE id=$1)`, true},
		{"draining pool", `UPDATE worker_pools SET status='draining' WHERE id=(SELECT worker_pool_id FROM worker_hosts WHERE id=$1)`, true},
		{"new epoch", `UPDATE worker_hosts SET current_epoch=2 WHERE id=$1`, false},
		{"lost host", `UPDATE worker_hosts SET status='lost',lost_at=now() WHERE id=$1`, false},
		{"disabled group", `UPDATE worker_groups SET status='disabled',primary_pool_id=NULL WHERE id=(SELECT worker_group_id FROM worker_hosts WHERE id=$1)`, false},
	} {
		t.Run(test.name, func(t *testing.T) {
			f := newSupplyFixture(t)
			pool := f.activePool(t, "default")
			hostID := f.activeHost(t, pool, "host-1")
			if test.sql != "" {
				dbtest.MustExec(t, t.Context(), f.pool, test.sql, hostID)
			}
			tx := f.begin(t)
			locked, err := LockHostWithPool(t.Context(), db.New(tx), f.groupID(), pgvalue.MustUUIDValue(pool.ID), hostID, 1)
			if !test.locked {
				if !errors.Is(err, pgx.ErrNoRows) {
					t.Fatalf("lock host with pool error = %v, want pgx.ErrNoRows", err)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			if locked.Host.ID != pgvalue.UUID(hostID) || locked.Group.ID != f.group.ID || !locked.Continues() {
				t.Fatalf("locked host = %v group = %v continues = %v", locked.Host.ID, locked.Group.ID, locked.Continues())
			}

		})
	}
}

func waitForBlockedQuery(t *testing.T, pool *pgxpool.Pool, marker string, count int) {
	t.Helper()
	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()
	ticker := time.NewTicker(10 * time.Millisecond)
	defer ticker.Stop()
	for {
		var waiting int
		err := pool.QueryRow(ctx, `
SELECT count(*)
  FROM pg_stat_activity
 WHERE datname = current_database()
   AND pid <> pg_backend_pid()
   AND wait_event_type = 'Lock'
   AND query LIKE '%' || $1 || '%'`, marker).Scan(&waiting)
		if err != nil {
			if errors.Is(err, context.DeadlineExceeded) {
				t.Fatalf("timed out waiting for %d blocked queries containing %q", count, marker)
			}
			t.Fatal(err)
		}
		if waiting >= count {
			return
		}
		select {
		case <-ctx.Done():
			t.Fatalf("timed out waiting for %d blocked queries containing %q", count, marker)
		case <-ticker.C:
		}
	}
}
