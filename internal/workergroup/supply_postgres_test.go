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

func TestHostRuntimeAdmissionRejectsVMPausedHost(t *testing.T) {
	f := newSupplyFixture(t)
	hostID := f.activeHost(t, f.activePool(t, "default"), "host-1")
	dbtest.MustExec(t, t.Context(), f.pool, `UPDATE worker_hosts SET vm_paused_reason = 'runtime_health' WHERE id = $1`, hostID)

	tx := f.begin(t)
	if _, err := LockDispatchSupply(t.Context(), tx, f.dispatchSupply(hostID)); err != nil {
		t.Fatalf("Run supply fence rejected a VM-only pause: %v", err)
	}
	if err := CheckHostRuntimeAdmission(t.Context(), tx, pgvalue.UUID(hostID), 1); err == nil {
		t.Fatal("VM-paused host remained eligible for VM admission")
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
		{"draining", `UPDATE worker_hosts SET status='draining',draining_at=clock_timestamp() WHERE id=$1`, false, false},
		{"draining continuation", `UPDATE worker_hosts SET status='draining',draining_at=clock_timestamp() WHERE id=$1`, true, true},
		{"stale draining continuation", `UPDATE worker_hosts SET status='draining',draining_at=clock_timestamp(),observed_at=clock_timestamp()-interval '1 hour' WHERE id=$1`, true, false},
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
func TestExecutionHostClaimsDoNotReplaceEpochOrStateFences(t *testing.T) {
	for _, test := range []struct {
		name, sql string
		admission bool
		want      error
	}{
		{"current", ``, true, nil},
		{"host claims", `UPDATE worker_hosts SET claim_version=claim_version+1 WHERE id=$1`, true, ErrStaleClaims},
		{"draining host claims", `UPDATE worker_hosts SET status='draining',draining_at=now(),claim_version=claim_version+1 WHERE id=$1`, true, ErrStaleClaims},
		{"draining host", `UPDATE worker_hosts SET status='draining',draining_at=now() WHERE id=$1`, true, nil},
		{"new epoch", `UPDATE worker_hosts SET current_epoch=2 WHERE id=$1`, false, pgx.ErrNoRows},
		{"lost host", `UPDATE worker_hosts SET status='lost',lost_at=now() WHERE id=$1`, false, pgx.ErrNoRows},
		{"group claims", `UPDATE worker_groups SET claim_version=claim_version+1 WHERE id=(SELECT worker_group_id FROM worker_hosts WHERE id=$1)`, true, ErrStaleClaims},
		{"paused group admission", `UPDATE worker_groups SET status='paused' WHERE id=(SELECT worker_group_id FROM worker_hosts WHERE id=$1)`, true, pgx.ErrNoRows},
		{"paused group continuation", `UPDATE worker_groups SET status='paused' WHERE id=(SELECT worker_group_id FROM worker_hosts WHERE id=$1)`, false, nil},
		{"draining group admission", `UPDATE worker_groups SET status='draining',primary_pool_id=NULL WHERE id=(SELECT worker_group_id FROM worker_hosts WHERE id=$1)`, true, nil},
		{"disabled group", `UPDATE worker_groups SET status='disabled',primary_pool_id=NULL WHERE id=(SELECT worker_group_id FROM worker_hosts WHERE id=$1)`, false, pgx.ErrNoRows},
	} {
		t.Run(test.name, func(t *testing.T) {
			f := newSupplyFixture(t)
			hostID := f.activeHost(t, f.activePool(t, "default"), "host-1")
			_, hostClaims := f.hostState(t, hostID)
			groupClaims := f.currentGroup(t).ClaimVersion
			if test.sql != "" {
				dbtest.MustExec(t, t.Context(), f.pool, test.sql, hostID)
			}
			err := LockExecutionHost(t.Context(), db.New(f.begin(t)), ExecutionHost{
				GroupID: f.group.ID, RegionID: fixtureRegionID, HostID: pgvalue.UUID(hostID), Epoch: 1,
				GroupClaimVersion: groupClaims, HostClaimVersion: hostClaims, Admission: test.admission,
			})
			if test.want == nil && err != nil || test.want != nil && !errors.Is(err, test.want) {
				t.Fatalf("execution host error = %v, want %v", err, test.want)
			}
		})
	}
}

func TestLockHostReportsStaleClaimsAndContinuation(t *testing.T) {
	for _, test := range []struct {
		name, sql string
		stale     bool
		continues bool
	}{
		{"active", ``, false, true},
		{"host claims", `UPDATE worker_hosts SET claim_version=claim_version+1 WHERE id=$1`, true, false},
		{"group claims", `UPDATE worker_groups SET claim_version=claim_version+1 WHERE id=(SELECT worker_group_id FROM worker_hosts WHERE id=$1)`, true, false},
		{"draining host", `UPDATE worker_hosts SET status='draining',draining_at=now() WHERE id=$1`, false, true},
		{"new epoch", `UPDATE worker_hosts SET current_epoch=2 WHERE id=$1`, false, false},
		{"lost host", `UPDATE worker_hosts SET status='lost',lost_at=now() WHERE id=$1`, false, false},
		{"paused group", `UPDATE worker_groups SET status='paused' WHERE id=(SELECT worker_group_id FROM worker_hosts WHERE id=$1)`, false, true},
		{"draining group", `UPDATE worker_groups SET status='draining',primary_pool_id=NULL WHERE id=(SELECT worker_group_id FROM worker_hosts WHERE id=$1)`, false, true},
		{"disabled group", `UPDATE worker_groups SET status='disabled',primary_pool_id=NULL WHERE id=(SELECT worker_group_id FROM worker_hosts WHERE id=$1)`, false, false},
	} {
		t.Run(test.name, func(t *testing.T) {
			f := newSupplyFixture(t)
			hostID := f.activeHost(t, f.activePool(t, "default"), "host-1")
			_, hostClaims := f.hostState(t, hostID)
			principal := HostPrincipal{HostID: hostID, GroupID: f.groupID(), Epoch: 1, HostClaimVersion: hostClaims, GroupClaimVersion: f.currentGroup(t).ClaimVersion}
			if test.sql != "" {
				dbtest.MustExec(t, t.Context(), f.pool, test.sql, hostID)
			}
			locked, err := LockHost(t.Context(), db.New(f.begin(t)), principal)
			if test.stale {
				if !errors.Is(err, ErrStaleClaims) {
					t.Fatalf("lock host error = %v, want ErrStaleClaims", err)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			if locked.Host.ID != pgvalue.UUID(hostID) || locked.Group.ID != f.group.ID {
				t.Fatalf("locked host = %v group = %v", locked.Host.ID, locked.Group.ID)
			}
			if locked.Continues() != test.continues {
				t.Fatalf("continues = %v, want %v", locked.Continues(), test.continues)
			}
		})
	}
}

// LockHostUnchecked locks the supply whatever its claims, status or epoch and
// leaves the lifecycle check to Continues; LockHostIgnoringClaims rejects
// exactly the supply that does not continue admitted work.
func TestLockHostUncheckedLeavesContinuationToCaller(t *testing.T) {
	for _, test := range []struct {
		name, sql string
		continues bool
	}{
		{"active", ``, true},
		{"host claims", `UPDATE worker_hosts SET claim_version=claim_version+1 WHERE id=$1`, true},
		{"draining host", `UPDATE worker_hosts SET status='draining',draining_at=now() WHERE id=$1`, true},
		{"new epoch", `UPDATE worker_hosts SET current_epoch=2 WHERE id=$1`, false},
		{"lost host", `UPDATE worker_hosts SET status='lost',lost_at=now() WHERE id=$1`, false},
		{"paused group", `UPDATE worker_groups SET status='paused' WHERE id=(SELECT worker_group_id FROM worker_hosts WHERE id=$1)`, true},
		{"disabled group", `UPDATE worker_groups SET status='disabled',primary_pool_id=NULL WHERE id=(SELECT worker_group_id FROM worker_hosts WHERE id=$1)`, false},
	} {
		t.Run(test.name, func(t *testing.T) {
			f := newSupplyFixture(t)
			hostID := f.activeHost(t, f.activePool(t, "default"), "host-1")
			if test.sql != "" {
				dbtest.MustExec(t, t.Context(), f.pool, test.sql, hostID)
			}
			tx := f.begin(t)
			locked, err := LockHostUnchecked(t.Context(), db.New(tx), f.groupID(), fixtureRegionID, hostID, 1)
			if err != nil {
				t.Fatal(err)
			}
			if err = tx.Rollback(t.Context()); err != nil {
				t.Fatal(err)
			}
			if locked.Host.ID != pgvalue.UUID(hostID) || locked.Group.ID != f.group.ID || locked.Continues() != test.continues {
				t.Fatalf("locked host = %v group = %v continues = %v", locked.Host.ID, locked.Group.ID, locked.Continues())
			}
			_, err = LockHostIgnoringClaims(t.Context(), db.New(f.begin(t)), f.groupID(), fixtureRegionID, hostID, 1)
			if test.continues && err != nil || !test.continues && !errors.Is(err, pgx.ErrNoRows) {
				t.Fatalf("lock host ignoring claims error = %v, continues = %v", err, test.continues)
			}
		})
	}
}

// LockHostWithPool ignores claim versions and accepts supply that continues
// admitted work; DrainLockedHost drains only an active host.
func TestLockHostWithPoolContinuesAndDrainsActiveHost(t *testing.T) {
	for _, test := range []struct {
		name, sql string
		locked    bool
	}{
		{"active", ``, true},
		{"host claims", `UPDATE worker_hosts SET claim_version=claim_version+1 WHERE id=$1`, true},
		{"group claims", `UPDATE worker_groups SET claim_version=claim_version+1 WHERE id=(SELECT worker_group_id FROM worker_hosts WHERE id=$1)`, true},
		{"draining host", `UPDATE worker_hosts SET status='draining',draining_at=now() WHERE id=$1`, true},
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
			_, claimsBefore := f.hostState(t, hostID)
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
			if err = DrainLockedHost(t.Context(), db.New(tx), locked); err != nil {
				t.Fatal(err)
			}
			if err = tx.Commit(t.Context()); err != nil {
				t.Fatal(err)
			}
			status, claimsAfter := f.hostState(t, hostID)
			if status != db.WorkerHostStatusDraining || (locked.Host.Status == db.WorkerHostStatusDraining) != (claimsAfter == claimsBefore) {
				t.Fatalf("host after drain = %s claims %d -> %d", status, claimsBefore, claimsAfter)
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
