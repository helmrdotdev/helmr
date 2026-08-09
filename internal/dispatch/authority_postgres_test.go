package dispatch

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/helmrdotdev/helmr/internal/db/dbtest"
	"github.com/helmrdotdev/helmr/internal/pgvalue"
)

func TestWorkerFenceCoordinatesAtWorkerGranularity(t *testing.T) {
	fixture := newRunPlacementFixture(t)
	workerID := uuid.New()
	serviceID := uuid.New()
	dbtest.MustExec(t, fixture.ctx, fixture.pool, `
INSERT INTO worker_instances (
    id, resource_id, worker_group_id, state,
    current_epoch, current_service_id, supervisor_version,
    supports_run, runtime_identity_id, substrate_format, substrate_contract,
    epoch_cpu_millis, epoch_memory_bytes, epoch_guest_ephemeral_disk_bytes,
    per_vm_cpu_millis, per_vm_memory_bytes,
    per_vm_guest_ephemeral_disk_bytes, max_vm_slots,
    max_runtime_starts, observed_at, epoch_started_at, activated_at
)
SELECT $2, $3, worker_group_id, state,
       current_epoch, $4, supervisor_version,
       supports_run, runtime_identity_id, substrate_format, substrate_contract,
       epoch_cpu_millis, epoch_memory_bytes, epoch_guest_ephemeral_disk_bytes,
       per_vm_cpu_millis, per_vm_memory_bytes,
       per_vm_guest_ephemeral_disk_bytes, max_vm_slots,
       max_runtime_starts, observed_at, epoch_started_at, activated_at
  FROM worker_instances
 WHERE id = $1`, fixture.workerID, workerID, workerID.String(), serviceID)

	first, err := fixture.authority.begin(fixture.ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer rollback(context.Background(), first)
	var isolation string
	if err := first.QueryRow(fixture.ctx, `SHOW transaction_isolation`).Scan(&isolation); err != nil {
		t.Fatal(err)
	}
	if isolation != "read committed" {
		t.Fatalf("placement transaction isolation = %q, want read committed", isolation)
	}
	if err := lockWorkerFence(fixture.ctx, first, workerFence{
		GroupID: fixture.groupID, RegionID: "us-east-1",
		WorkerInstanceID: pgvalue.UUID(fixture.workerID), WorkerEpoch: 1,
		Role: "run", RunArchitecture: platformArchitecture,
	}); err != nil {
		t.Fatal(err)
	}

	second, err := fixture.authority.begin(fixture.ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer rollback(context.Background(), second)
	secondCtx, cancelSecond := context.WithTimeout(fixture.ctx, time.Second)
	defer cancelSecond()
	if err := lockWorkerFence(secondCtx, second, workerFence{
		GroupID: fixture.groupID, RegionID: "us-east-1",
		WorkerInstanceID: pgvalue.UUID(workerID), WorkerEpoch: 1,
		Role: "run", RunArchitecture: platformArchitecture,
	}); err != nil {
		t.Fatalf("independent Worker fence blocked: %v", err)
	}

	transitionCtx, cancelTransition := context.WithTimeout(fixture.ctx, 5*time.Second)
	defer cancelTransition()
	transitioned := make(chan error, 1)
	go func() {
		_, err := fixture.pool.Exec(transitionCtx, `
/* worker fence group transition */
UPDATE worker_groups SET state = 'paused' WHERE id = $1`, fixture.groupID)
		transitioned <- err
	}()
	waitForBlockedQuery(t, fixture, "worker fence group transition", 1)

	if err := second.Commit(fixture.ctx); err != nil {
		t.Fatal(err)
	}
	if err := first.Commit(fixture.ctx); err != nil {
		t.Fatal(err)
	}
	if err := <-transitioned; err != nil {
		t.Fatal(err)
	}

	recheck, err := fixture.authority.begin(fixture.ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer rollback(context.Background(), recheck)
	err = lockWorkerFence(fixture.ctx, recheck, workerFence{
		GroupID: fixture.groupID, RegionID: "us-east-1",
		WorkerInstanceID: pgvalue.UUID(fixture.workerID), WorkerEpoch: 1,
		Role: "run", RunArchitecture: platformArchitecture,
	})
	if err == nil {
		t.Fatal("paused Worker Group remained eligible for placement")
	}
}

func TestConcurrentRunPlacementRechecksLockedWorkerCapacity(t *testing.T) {
	fixture := newRunPlacementFixture(t)
	seedDispatchMeasurement(t, fixture, 2, 2, 0, false)
	dbtest.MustExec(t, fixture.ctx, fixture.pool, `
UPDATE worker_instances
   SET max_vm_slots = 1,
       max_runtime_starts = 1
 WHERE id = $1`, fixture.workerID)

	candidates := listRunPlacementCandidates(t, fixture, 2)
	if len(candidates) != 2 {
		t.Fatalf("placement candidates = %d, want 2", len(candidates))
	}

	blocker, err := fixture.pool.Begin(fixture.ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer rollback(context.Background(), blocker)
	if _, err := blocker.Exec(fixture.ctx, `
SELECT id FROM worker_groups WHERE id = $1 FOR UPDATE`, fixture.groupID); err != nil {
		t.Fatal(err)
	}

	results := make(chan error, len(candidates))
	for _, row := range candidates {
		candidate := ReadyRunCandidate{
			OrgID: row.OrgID, RunID: row.RunID,
			ExpectedRunStateVersion: row.StateVersion,
		}
		go func() {
			_, err := fixture.authority.PlaceReadyRun(fixture.ctx, candidate)
			results <- err
		}()
	}
	waitForBlockedQuery(t, fixture, "FOR SHARE", len(candidates))
	if err := blocker.Commit(fixture.ctx); err != nil {
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
	if err := fixture.pool.QueryRow(fixture.ctx, `
SELECT count(*)
  FROM runtime_instances
 WHERE worker_instance_id = $1
   AND worker_epoch = 1
   AND reclaimed_at IS NULL`, fixture.workerID).Scan(&reservations); err != nil {
		t.Fatal(err)
	}
	if reservations != 1 {
		t.Fatalf("live runtime reservations = %d, want 1", reservations)
	}
}

func TestConcurrentRunAndBuildPlacementShareWorkerCapacity(t *testing.T) {
	fixture := newRunPlacementFixture(t)
	buildID := uuid.New()
	dbtest.MustExec(t, fixture.ctx, fixture.pool, `
INSERT INTO deployments (
    id, org_id, project_id, environment_id, build_region_id,
    build_node_version, build_runtime_digest, build_toolchain_digest,
    build_manager_name, build_manager_version, build_manager_digest,
    build_contract, image_cache_mode, version, content_hash,
    deployment_source_artifact_id, status
)
SELECT $2, org_id, project_id, environment_id, build_region_id,
       build_node_version, build_runtime_digest, build_toolchain_digest,
       build_manager_name, build_manager_version, build_manager_digest,
       build_contract, image_cache_mode, 'v2', $3,
       deployment_source_artifact_id, 'queued'
  FROM deployments
 WHERE id = $1`, fixture.deploymentID, buildID, "sha256:"+strings.Repeat("9", 64))
	dbtest.MustExec(t, fixture.ctx, fixture.pool, `
UPDATE worker_groups
   SET allows_build = true
 WHERE id = $1`, fixture.groupID)
	dbtest.MustExec(t, fixture.ctx, fixture.pool, `
UPDATE worker_instances
   SET supports_build = true,
       epoch_cpu_millis = 3000,
       epoch_memory_bytes = 4294967296,
       epoch_guest_ephemeral_disk_bytes = 34359738368,
       per_vm_cpu_millis = 2000,
       per_vm_memory_bytes = 2147483648,
       max_vm_slots = 1,
       max_runtime_starts = 1,
       max_build_executors = 1
 WHERE id = $1`, fixture.workerID)

	buildAuthority, err := NewBuildAuthority(fixture.pool)
	if err != nil {
		t.Fatal(err)
	}
	blocker, err := fixture.pool.Begin(fixture.ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer rollback(context.Background(), blocker)
	if _, err := blocker.Exec(fixture.ctx, `
SELECT id FROM worker_instances WHERE id = $1 FOR UPDATE`, fixture.workerID); err != nil {
		t.Fatal(err)
	}

	runResult := make(chan error, 1)
	go func() {
		_, err := fixture.authority.PlaceReadyRun(fixture.ctx, fixture.candidate())
		runResult <- err
	}()
	waitForBlockedQuery(t, fixture, "FOR UPDATE OF worker_instances", 1)
	buildResult := make(chan error, 1)
	go func() {
		_, err := buildAuthority.PlaceReadyBuild(fixture.ctx, ReadyBuildCandidate{
			OrgID: pgvalue.UUID(fixture.orgID), DeploymentID: pgvalue.UUID(buildID),
			BuildRegionID: "us-east-1", LeaseSequence: 1,
		})
		buildResult <- err
	}()
	waitForBlockedQuery(t, fixture, "FOR UPDATE OF worker_instances", 2)
	if err := blocker.Commit(fixture.ctx); err != nil {
		t.Fatal(err)
	}

	if err := <-runResult; err != nil {
		t.Fatalf("Run placement failed: %v", err)
	}
	if err := <-buildResult; !errors.Is(err, ErrCapacityUnavailable) {
		t.Fatalf("Build placement error = %v, want ErrCapacityUnavailable", err)
	}

	var runtimes, builds int
	if err := fixture.pool.QueryRow(fixture.ctx, `
SELECT (SELECT count(*) FROM runtime_instances
         WHERE worker_instance_id = $1 AND worker_epoch = 1 AND reclaimed_at IS NULL),
       (SELECT count(*) FROM deployment_build_leases
         WHERE worker_instance_id = $1 AND worker_epoch = 1
           AND state IN ('assigned', 'starting', 'running'))`, fixture.workerID).Scan(&runtimes, &builds); err != nil {
		t.Fatal(err)
	}
	if runtimes+builds != 1 {
		t.Fatalf("shared Worker allocations runtimes=%d builds=%d, want one total", runtimes, builds)
	}
}

func waitForBlockedQuery(t *testing.T, fixture runPlacementFixture, marker string, count int) {
	t.Helper()
	ctx, cancel := context.WithTimeout(fixture.ctx, 5*time.Second)
	defer cancel()
	ticker := time.NewTicker(10 * time.Millisecond)
	defer ticker.Stop()
	for {
		var waiting int
		err := fixture.pool.QueryRow(ctx, `
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
