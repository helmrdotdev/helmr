package dispatch

import (
	"context"
	"errors"
	"testing"
	"time"
	"uuid"

	"github.com/helmrdotdev/helmr/internal/db/dbtest"
	"github.com/helmrdotdev/helmr/internal/pgvalue"
	"github.com/helmrdotdev/helmr/internal/run/runtest"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

func TestWorkerFenceCoordinatesAtWorkerGranularity(t *testing.T) {
	f, _, authority := commandPlacementFixture(t)
	workerID := uuid.New()
	serviceID := uuid.New()
	dbtest.MustExec(t, t.Context(), f.Pool, `
INSERT INTO worker_hosts (
    id, resource_id, worker_group_id, worker_pool_id, status,
    current_epoch, current_service_id,
    vm_platform_id,
    epoch_cpu_millis, epoch_memory_bytes, epoch_guest_ephemeral_disk_bytes,
    per_vm_cpu_millis, per_vm_memory_bytes,
    per_vm_guest_ephemeral_disk_bytes, max_vm_slots,
    max_vm_starts, cpu_environment, cpu_environment_digest,
    observed_at, epoch_started_at, activated_at
)
SELECT $2, $3, worker_group_id, worker_pool_id, status,
       current_epoch, $4,
       vm_platform_id,
       epoch_cpu_millis, epoch_memory_bytes, epoch_guest_ephemeral_disk_bytes,
       per_vm_cpu_millis, per_vm_memory_bytes,
       per_vm_guest_ephemeral_disk_bytes, max_vm_slots,
       max_vm_starts, cpu_environment, cpu_environment_digest,
       observed_at, epoch_started_at, activated_at
  FROM worker_hosts
 WHERE id = $1`, f.WorkerID, workerID, workerID.String(), serviceID)

	first, err := authority.begin(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	defer rollback(context.Background(), first)
	var isolation string
	if err := first.QueryRow(t.Context(), `SHOW transaction_isolation`).Scan(&isolation); err != nil {
		t.Fatal(err)
	}
	if isolation != "read committed" {
		t.Fatalf("placement transaction isolation = %q, want read committed", isolation)
	}
	if err := lockWorkerFence(t.Context(), first, workerFence{
		GroupID: pgvalue.UUID(runtest.WorkerGroupID), RegionID: "us-east-1",
		WorkerHostID: pgvalue.UUID(f.WorkerID), WorkerEpoch: 1,
		RunArchitecture: runtimeArchitecture,
	}); err != nil {
		t.Fatal(err)
	}

	second, err := authority.begin(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	defer rollback(context.Background(), second)
	secondCtx, cancelSecond := context.WithTimeout(t.Context(), time.Second)
	defer cancelSecond()
	if err := lockWorkerFence(secondCtx, second, workerFence{
		GroupID: pgvalue.UUID(runtest.WorkerGroupID), RegionID: "us-east-1",
		WorkerHostID: pgvalue.UUID(workerID), WorkerEpoch: 1,
		RunArchitecture: runtimeArchitecture,
	}); err != nil {
		t.Fatalf("independent Worker fence blocked: %v", err)
	}

	transitionCtx, cancelTransition := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancelTransition()
	transitioned := make(chan error, 1)
	go func() {
		_, err := f.Pool.Exec(transitionCtx, `
/* worker fence group transition */
UPDATE worker_groups SET status = 'paused' WHERE id = $1`, pgvalue.UUID(runtest.WorkerGroupID))
		transitioned <- err
	}()
	waitForBlockedQuery(t, f.Pool, "worker fence group transition", 1)

	if err := second.Commit(t.Context()); err != nil {
		t.Fatal(err)
	}
	if err := first.Commit(t.Context()); err != nil {
		t.Fatal(err)
	}
	if err := <-transitioned; err != nil {
		t.Fatal(err)
	}

	recheck, err := authority.begin(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	defer rollback(context.Background(), recheck)
	err = lockWorkerFence(t.Context(), recheck, workerFence{
		GroupID: pgvalue.UUID(runtest.WorkerGroupID), RegionID: "us-east-1",
		WorkerHostID: pgvalue.UUID(f.WorkerID), WorkerEpoch: 1,
		RunArchitecture: runtimeArchitecture,
	})
	if err == nil {
		t.Fatal("paused Worker Group remained eligible for placement")
	}
}

func TestRuntimeAdmissionFenceRejectsRuntimePausedWorker(t *testing.T) {
	f, _, authority := commandPlacementFixture(t)
	dbtest.MustExec(t, t.Context(), f.Pool, `
UPDATE worker_hosts
   SET vm_paused_reason = 'runtime_health'
 WHERE id = $1`, f.WorkerID)

	tx, err := authority.begin(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	defer rollback(context.Background(), tx)
	if err := lockWorkerFence(t.Context(), tx, workerFence{
		GroupID: pgvalue.UUID(runtest.WorkerGroupID), RegionID: "us-east-1",
		WorkerHostID: pgvalue.UUID(f.WorkerID), WorkerEpoch: 1,
		RunArchitecture: runtimeArchitecture,
	}); err != nil {
		t.Fatalf("Run-domain fence rejected Runtime-only pause: %v", err)
	}
	err = checkLockedWorkerRuntimeAdmission(
		t.Context(), tx, pgvalue.UUID(f.WorkerID), 1,
	)
	if err == nil {
		t.Fatal("runtime-paused Worker remained eligible for Runtime admission")
	}
}

func TestWorkerFenceRequiresRunReadyWorker(t *testing.T) {
	for _, test := range []struct {
		name, sql     string
		allowDraining bool
		eligible      bool
	}{
		{"ready", ``, false, true},
		{"Run paused", `UPDATE worker_hosts SET run_paused_reason='startup_recovery_leak' WHERE id=$1`, false, false},
		{"Run paused with draining allowed", `UPDATE worker_hosts SET run_paused_reason='startup_recovery_leak' WHERE id=$1`, true, false},
		{"stale observation", `UPDATE worker_hosts SET observed_at=clock_timestamp()-interval '1 hour' WHERE id=$1`, false, false},
		{"unobserved", `UPDATE worker_hosts SET observed_at=NULL WHERE id=$1`, false, false},
		{"draining", `UPDATE worker_hosts SET status='draining',draining_at=clock_timestamp() WHERE id=$1`, false, false},
		{"draining allowed", `UPDATE worker_hosts SET status='draining',draining_at=clock_timestamp() WHERE id=$1`, true, true},
		{"stale draining allowed", `UPDATE worker_hosts SET status='draining',draining_at=clock_timestamp(),observed_at=clock_timestamp()-interval '1 hour' WHERE id=$1`, true, false},
	} {
		t.Run(test.name, func(t *testing.T) {
			f, _, authority := commandPlacementFixture(t)
			if test.sql != "" {
				dbtest.MustExec(t, t.Context(), f.Pool, test.sql, f.WorkerID)
			}
			tx, err := authority.begin(t.Context())
			if err != nil {
				t.Fatal(err)
			}
			defer rollback(context.Background(), tx)
			err = lockWorkerFence(t.Context(), tx, workerFence{
				GroupID: pgvalue.UUID(runtest.WorkerGroupID), RegionID: "us-east-1",
				WorkerHostID: pgvalue.UUID(f.WorkerID), WorkerEpoch: 1,
				RunArchitecture: runtimeArchitecture, AllowDraining: test.allowDraining,
			})
			if test.eligible && err != nil {
				t.Fatalf("eligible Worker rejected: %v", err)
			}
			if !test.eligible && !errors.Is(err, pgx.ErrNoRows) {
				t.Fatalf("ineligible Worker fence error = %v, want no rows", err)
			}
		})
	}
}

func TestConcurrentRunPlacementRechecksLockedWorkerCapacity(t *testing.T) {
	f, first, authority := commandPlacementFixture(t)
	second := f.AddRunLease(t, "running", time.Now().Add(-time.Minute))
	candidates := []ReadyRunCandidate{queuedSharedRun(t, f, first), queuedSharedRun(t, f, second)}
	for _, candidate := range candidates {
		dbtest.MustExec(t, t.Context(), f.Pool, `UPDATE runs SET concurrency_key=id::text WHERE id=$1`, candidate.RunID)
	}
	// Supply consists of two persistent Computers with no remaining physical instances.
	dbtest.MustExec(t, t.Context(), f.Pool, `UPDATE computer_instances SET desired_state='closed',desired_version=2,observed_state='closed',observed_desired_version=2,terminal_at=now(),terminal_reason_code='test_exclusion',reclaimed_at=now(),reclaim_evidence='{"method":"host_reconciled"}',admission_state='closed',mount_state='unmounted',unmounted_at=now()`)
	dbtest.MustExec(t, t.Context(), f.Pool, `UPDATE worker_hosts SET max_vm_slots=1,max_vm_starts=1,epoch_guest_ephemeral_disk_bytes=68719476736,per_vm_guest_ephemeral_disk_bytes=34359738368 WHERE id=$1`, f.WorkerID)

	blocker, err := f.Pool.Begin(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	defer rollback(context.Background(), blocker)
	if _, err := blocker.Exec(t.Context(), `
SELECT id FROM worker_groups WHERE id = $1 FOR UPDATE`, pgvalue.UUID(runtest.WorkerGroupID)); err != nil {
		t.Fatal(err)
	}

	results := make(chan error, len(candidates))
	for _, row := range candidates {
		candidate := ReadyRunCandidate{
			OrgID: row.OrgID, RunID: row.RunID,
			ExpectedRunRevision: row.ExpectedRunRevision,
		}
		go func() {
			_, err := authority.PlaceReadyRun(t.Context(), candidate)
			results <- err
		}()
	}
	deadline := time.Now().Add(5 * time.Second)
	for {
		var waiting int
		if err := f.Pool.QueryRow(t.Context(), `SELECT count(*) FROM pg_stat_activity WHERE $1=ANY(pg_blocking_pids(pid))`, blocker.Conn().PgConn().PID()).Scan(&waiting); err != nil {
			t.Fatal(err)
		}
		if waiting == len(candidates) {
			break
		}
		select {
		case err := <-results:
			t.Fatalf("placement returned before Worker fence: %v", err)
		default:
		}
		if time.Now().After(deadline) {
			t.Fatalf("blocked placements=%d", waiting)
		}
		time.Sleep(10 * time.Millisecond)
	}
	if err := blocker.Commit(t.Context()); err != nil {
		t.Fatal(err)
	}

	succeeded := 0
	capacityRejected := 0
	for range candidates {
		err := <-results
		switch {
		case err == nil:
			succeeded++
		case errors.Is(err, ErrCapacityUnavailable):
			capacityRejected++
		default:
			t.Fatalf("concurrent placement failed: %v", err)
		}
	}
	if succeeded != 1 || capacityRejected != 1 {
		t.Fatalf("placements succeeded=%d capacity_rejected=%d, want 1 and 1", succeeded, capacityRejected)
	}

	var reservations int
	if err := f.Pool.QueryRow(t.Context(), `
SELECT count(*)
  FROM computer_instances
 WHERE worker_host_id = $1
   AND worker_epoch = 1
   AND reclaimed_at IS NULL`, f.WorkerID).Scan(&reservations); err != nil {
		t.Fatal(err)
	}
	if reservations != 1 {
		t.Fatalf("live runtime reservations = %d, want 1", reservations)
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
