package db_test

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/helmrdotdev/helmr/internal/api"
	"github.com/helmrdotdev/helmr/internal/db"
	"github.com/helmrdotdev/helmr/internal/ids"
	"github.com/helmrdotdev/helmr/internal/tracing"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/jackc/pgx/v5/pgxpool"
)

func TestPrepareQueuedRunQueueItemBuildsRequirementsFromDeploymentTask(t *testing.T) {
	ctx := context.Background()
	queries, pool := newPostgresTestDB(t, ctx)
	orgID := ids.ToPG(ids.DefaultOrgID)

	scope := seedPostgresTestConfiguredScope(t, ctx, pool, queries, orgID)
	upsertTestWorkerInstance(t, ctx, queries, "instance-runtime-release")
	runID := seedComputeDispatchRunWithResources(t, ctx, pool, orgID, scope.ProjectID, scope.EnvironmentID, 3000, 4096, 32768)

	prepared, err := queries.PrepareQueuedRunQueueItem(ctx, db.PrepareQueuedRunQueueItemParams{
		OrgID: orgID,
		RunID: runID,
	})
	if err != nil {
		t.Fatal(err)
	}
	if prepared.QueueName != "task/deploy" || prepared.Priority != 0 {
		t.Fatalf("prepared dispatch = %+v", prepared)
	}
	if prepared.RequestedMilliCpu != 3000 || prepared.RequestedMemoryMib != 4096 || prepared.RequestedDiskMib != 32768 || prepared.RequestedExecutionSlots != 1 {
		t.Fatalf("prepared requirements = %+v", prepared)
	}

	marked, err := queries.MarkRunQueueItemEnqueued(ctx, db.MarkRunQueueItemEnqueuedParams{
		OrgID:                      orgID,
		RunID:                      runID,
		DispatchMessageID:          pgText("redis-message-1"),
		ExpectedDispatchGeneration: prepared.DispatchGeneration,
	})
	if err != nil {
		t.Fatal(err)
	}
	if marked.DispatchMessageID.String != "redis-message-1" || marked.LastError != "" {
		t.Fatalf("marked dispatch = %+v", marked)
	}

	candidates, err := queries.ListQueuedRunQueueItemCandidatesForScope(ctx, db.ListQueuedRunQueueItemCandidatesForScopeParams{
		OrgID:         orgID,
		ProjectID:     scope.ProjectID,
		EnvironmentID: scope.EnvironmentID,
		QueueName:     "task/deploy",
		RowLimit:      10,
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(candidates) != 0 {
		t.Fatalf("candidates = %+v", candidates)
	}
}

func TestListQueuedRunCandidateScopesGroupsQueuedRunsByQueue(t *testing.T) {
	ctx := context.Background()
	queries, pool := newPostgresTestDB(t, ctx)
	orgA := ids.ToPG(ids.New())
	orgB := ids.ToPG(ids.New())
	scopeA := seedPostgresTestConfiguredScope(t, ctx, pool, queries, orgA)
	scopeB := seedPostgresTestConfiguredScope(t, ctx, pool, queries, orgB)
	siblingEnvironment, err := queries.CreateEnvironment(ctx, db.CreateEnvironmentParams{
		ID:        ids.ToPG(ids.New()),
		OrgID:     orgA,
		ProjectID: scopeA.ProjectID,
		Slug:      "sibling",
		Name:      "Sibling",
		ColorHex:  "#4F46E5",
	})
	if err != nil {
		t.Fatal(err)
	}
	runA1 := seedComputeDispatchRun(t, ctx, pool, orgA, scopeA.ProjectID, scopeA.EnvironmentID)
	runA2 := seedComputeDispatchRun(t, ctx, pool, orgA, scopeA.ProjectID, scopeA.EnvironmentID)
	runA3 := seedComputeDispatchRun(t, ctx, pool, orgA, scopeA.ProjectID, siblingEnvironment.ID)
	runB := seedComputeDispatchRun(t, ctx, pool, orgB, scopeB.ProjectID, scopeB.EnvironmentID)
	setRunDispatchFields(t, ctx, pool, orgA, runA1, "queue-a", 0, time.Now())
	setRunDispatchFields(t, ctx, pool, orgA, runA2, "queue-b", 0, time.Now())
	setRunDispatchFields(t, ctx, pool, orgA, runA3, "queue-a", 0, time.Now())
	setRunDispatchFields(t, ctx, pool, orgB, runB, "queue-a", 0, time.Now())

	scopes, err := queries.ListQueuedRunCandidateScopes(ctx, db.ListQueuedRunCandidateScopesParams{
		ScanSeed: "test",
		RowLimit: 10,
	})
	if err != nil {
		t.Fatal(err)
	}
	seen := map[string]bool{}
	for _, scope := range scopes {
		seen[ids.MustFromPG(scope.OrgID).String()+":"+ids.MustFromPG(scope.ProjectID).String()+":"+ids.MustFromPG(scope.EnvironmentID).String()+":"+scope.QueueName] = true
	}
	if !seen[ids.MustFromPG(orgA).String()+":"+ids.MustFromPG(scopeA.ProjectID).String()+":"+ids.MustFromPG(scopeA.EnvironmentID).String()+":queue-a"] ||
		!seen[ids.MustFromPG(orgA).String()+":"+ids.MustFromPG(scopeA.ProjectID).String()+":"+ids.MustFromPG(scopeA.EnvironmentID).String()+":queue-b"] ||
		!seen[ids.MustFromPG(orgA).String()+":"+ids.MustFromPG(scopeA.ProjectID).String()+":"+ids.MustFromPG(siblingEnvironment.ID).String()+":queue-a"] ||
		!seen[ids.MustFromPG(orgB).String()+":"+ids.MustFromPG(scopeB.ProjectID).String()+":"+ids.MustFromPG(scopeB.EnvironmentID).String()+":queue-a"] {
		t.Fatalf("candidate scopes = %+v", scopes)
	}
}

func TestListQueuedRunQueueItemCandidatesForScopeIsQueueScopedAndDispatchOrdered(t *testing.T) {
	ctx := context.Background()
	queries, pool := newPostgresTestDB(t, ctx)
	orgID := ids.ToPG(ids.New())
	scope := seedPostgresTestConfiguredScope(t, ctx, pool, queries, orgID)
	siblingEnvironment, err := queries.CreateEnvironment(ctx, db.CreateEnvironmentParams{
		ID:        ids.ToPG(ids.New()),
		OrgID:     orgID,
		ProjectID: scope.ProjectID,
		Slug:      "sibling",
		Name:      "Sibling",
		ColorHex:  "#4F46E5",
	})
	if err != nil {
		t.Fatal(err)
	}
	base := time.Now().Add(-time.Hour).UTC()
	lowPriority := seedComputeDispatchRun(t, ctx, pool, orgID, scope.ProjectID, scope.EnvironmentID)
	highPriorityLater := seedComputeDispatchRun(t, ctx, pool, orgID, scope.ProjectID, scope.EnvironmentID)
	highPriorityEarlier := seedComputeDispatchRun(t, ctx, pool, orgID, scope.ProjectID, scope.EnvironmentID)
	quietQueue := seedComputeDispatchRun(t, ctx, pool, orgID, scope.ProjectID, scope.EnvironmentID)
	siblingNoisy := seedComputeDispatchRun(t, ctx, pool, orgID, scope.ProjectID, siblingEnvironment.ID)
	setRunDispatchFields(t, ctx, pool, orgID, lowPriority, "noisy", 0, base)
	setRunDispatchFields(t, ctx, pool, orgID, highPriorityLater, "noisy", 10, base.Add(2*time.Minute))
	setRunDispatchFields(t, ctx, pool, orgID, highPriorityEarlier, "noisy", 10, base.Add(time.Minute))
	setRunDispatchFields(t, ctx, pool, orgID, quietQueue, "quiet", 0, base)
	setRunDispatchFields(t, ctx, pool, orgID, siblingNoisy, "noisy", 100, base.Add(-time.Minute))

	noisy, err := queries.ListQueuedRunQueueItemCandidatesForScope(ctx, db.ListQueuedRunQueueItemCandidatesForScopeParams{
		OrgID:         orgID,
		ProjectID:     scope.ProjectID,
		EnvironmentID: scope.EnvironmentID,
		QueueName:     "noisy",
		RowLimit:      3,
	})
	if err != nil {
		t.Fatal(err)
	}
	// Keep the lower-priority local item in the page so this test proves ordering
	// without hiding same-scope candidates behind the row limit.
	if len(noisy) != 3 || noisy[0].RunID != highPriorityEarlier || noisy[1].RunID != highPriorityLater || noisy[2].RunID != lowPriority {
		t.Fatalf("noisy candidates = %+v", noisy)
	}
	quiet, err := queries.ListQueuedRunQueueItemCandidatesForScope(ctx, db.ListQueuedRunQueueItemCandidatesForScopeParams{
		OrgID:         orgID,
		ProjectID:     scope.ProjectID,
		EnvironmentID: scope.EnvironmentID,
		QueueName:     "quiet",
		RowLimit:      1,
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(quiet) != 1 || quiet[0].RunID != quietQueue {
		t.Fatalf("quiet candidates = %+v", quiet)
	}
}

func TestRuntimeReleaseTupleIsImmutable(t *testing.T) {
	ctx := context.Background()
	queries, pool := newPostgresTestDB(t, ctx)
	workerGroup := defaultPostgresTestWorkerGroup(t, ctx, queries)

	original := upsertRuntimeWorker(t, ctx, queries, "worker-a", runtimeReleaseFields{
		id:              "sha256:runtime",
		arch:            "x86_64",
		abi:             "helmr.firecracker.snapshot.v0",
		kernelDigest:    "sha256:kernel",
		initramfsDigest: "sha256:initramfs",
		rootfsDigest:    "sha256:rootfs",
		cniProfile:      "helmr/v0",
	})

	_, err := queries.UpsertWorkerInstanceHeartbeat(ctx, workerHeartbeatParams("worker-b", runtimeReleaseFields{
		id:              "sha256:runtime",
		arch:            "x86_64",
		abi:             "helmr.firecracker.snapshot.v0",
		kernelDigest:    "sha256:other-kernel",
		initramfsDigest: "sha256:initramfs",
		rootfsDigest:    "sha256:rootfs",
		cniProfile:      "helmr/v0",
	}, workerGroup.ID))
	if !errors.Is(err, pgx.ErrNoRows) {
		t.Fatalf("runtime tuple rewrite error = %v, want no rows", err)
	}
	var mutatedWorkers int
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM worker_instances WHERE resource_id = 'worker-b'`).Scan(&mutatedWorkers); err != nil {
		t.Fatal(err)
	}
	if mutatedWorkers != 0 {
		t.Fatalf("worker row was written after runtime tuple rejection")
	}

	invalidExistingWorker := workerHeartbeatParams("worker-a", runtimeReleaseFields{
		id:              "sha256:runtime",
		arch:            "x86_64",
		abi:             "helmr.firecracker.snapshot.v0",
		kernelDigest:    "sha256:other-kernel",
		initramfsDigest: "sha256:initramfs",
		rootfsDigest:    "sha256:rootfs",
		cniProfile:      "helmr/v0",
	}, workerGroup.ID)
	invalidExistingWorker.AvailableExecutionSlots = 0
	invalidExistingWorker.AvailableMilliCpu = 0
	invalidExistingWorker.Heartbeat = []byte(`{"invalid":true}`)
	_, err = queries.UpsertWorkerInstanceHeartbeat(ctx, invalidExistingWorker)
	if !errors.Is(err, pgx.ErrNoRows) {
		t.Fatalf("existing worker runtime tuple rewrite error = %v, want no rows", err)
	}
	var storedRuntimeID, storedKernelDigest string
	var storedAvailableSlots int32
	var storedAvailableMilliCPU int64
	var storedHeartbeat []byte
	if err := pool.QueryRow(ctx, `
SELECT runtime_id, kernel_digest, available_execution_slots, available_milli_cpu, heartbeat
  FROM worker_instances
 WHERE resource_id = 'worker-a'
`).Scan(&storedRuntimeID, &storedKernelDigest, &storedAvailableSlots, &storedAvailableMilliCPU, &storedHeartbeat); err != nil {
		t.Fatal(err)
	}
	if storedRuntimeID != original.RuntimeID ||
		storedKernelDigest != original.KernelDigest ||
		storedAvailableSlots != original.AvailableExecutionSlots ||
		storedAvailableMilliCPU != original.AvailableMilliCpu ||
		string(storedHeartbeat) != string(original.Heartbeat) {
		t.Fatalf("existing worker mutated after runtime tuple rejection: runtime=%s kernel=%s slots=%d cpu=%d heartbeat=%s", storedRuntimeID, storedKernelDigest, storedAvailableSlots, storedAvailableMilliCPU, storedHeartbeat)
	}
}

func TestRuntimeReleaseSelectionControlsPreparedRequirements(t *testing.T) {
	ctx := context.Background()
	queries, pool := newPostgresTestDB(t, ctx)
	orgID := ids.ToPG(ids.DefaultOrgID)

	scope := seedPostgresTestConfiguredScope(t, ctx, pool, queries, orgID)
	firstRuntime := runtimeReleaseFields{
		id:              "sha256:runtime-a",
		arch:            "x86_64",
		abi:             "helmr.firecracker.snapshot.v0",
		kernelDigest:    "sha256:kernel-a",
		initramfsDigest: "sha256:initramfs",
		rootfsDigest:    "sha256:rootfs",
		cniProfile:      "helmr/v0",
	}
	secondRuntime := runtimeReleaseFields{
		id:              "sha256:runtime-b",
		arch:            "x86_64",
		abi:             "helmr.firecracker.snapshot.v0",
		kernelDigest:    "sha256:kernel-b",
		initramfsDigest: "sha256:initramfs",
		rootfsDigest:    "sha256:rootfs",
		cniProfile:      "helmr/v0",
	}
	upsertRuntimeWorker(t, ctx, queries, "worker-a", firstRuntime)
	if err := queries.EnsureRuntimeReleaseSelection(ctx, firstRuntime.id); err != nil {
		t.Fatal(err)
	}
	upsertRuntimeWorker(t, ctx, queries, "worker-b", secondRuntime)
	if err := queries.EnsureRuntimeReleaseSelection(ctx, secondRuntime.id); err != nil {
		t.Fatal(err)
	}

	runID := seedComputeDispatchRunWithResources(t, ctx, pool, orgID, scope.ProjectID, scope.EnvironmentID, 3000, 4096, 32768)
	prepared, err := queries.PrepareQueuedRunQueueItem(ctx, db.PrepareQueuedRunQueueItemParams{
		OrgID: orgID,
		RunID: runID,
	})
	if err != nil {
		t.Fatal(err)
	}
	if prepared.RuntimeID != secondRuntime.id || prepared.KernelDigest != secondRuntime.kernelDigest {
		t.Fatalf("prepared runtime = id:%s kernel:%s, want id:%s kernel:%s", prepared.RuntimeID, prepared.KernelDigest, secondRuntime.id, secondRuntime.kernelDigest)
	}
}

func TestPrepareQueuedRunQueueItemRequiresRuntimeReleaseSelection(t *testing.T) {
	ctx := context.Background()
	queries, pool := newPostgresTestDB(t, ctx)
	orgID := ids.ToPG(ids.DefaultOrgID)

	scope := seedPostgresTestConfiguredScope(t, ctx, pool, queries, orgID)
	runID := seedComputeDispatchRunWithResources(t, ctx, pool, orgID, scope.ProjectID, scope.EnvironmentID, 3000, 4096, 32768)
	_, err := queries.PrepareQueuedRunQueueItem(ctx, db.PrepareQueuedRunQueueItemParams{
		OrgID: orgID,
		RunID: runID,
	})
	if !errors.Is(err, pgx.ErrNoRows) {
		t.Fatalf("prepare without current runtime error = %v, want no rows", err)
	}
}

func TestQueuedRunQueueItemWithMessageIDCanBeReenqueued(t *testing.T) {
	ctx := context.Background()
	queries, pool := newPostgresTestDB(t, ctx)
	orgID := ids.ToPG(ids.DefaultOrgID)

	scope := seedPostgresTestConfiguredScope(t, ctx, pool, queries, orgID)
	runID := seedComputeDispatchRunWithResources(t, ctx, pool, orgID, scope.ProjectID, scope.EnvironmentID, 3000, 4096)
	instance := upsertTestWorkerInstance(t, ctx, queries, "instance-redis-loss")

	prepared, err := queries.PrepareQueuedRunQueueItem(ctx, db.PrepareQueuedRunQueueItemParams{
		OrgID: orgID,
		RunID: runID,
	})
	if err != nil {
		t.Fatal(err)
	}
	marked, err := queries.MarkRunQueueItemEnqueued(ctx, db.MarkRunQueueItemEnqueuedParams{
		OrgID:                      orgID,
		RunID:                      runID,
		DispatchMessageID:          pgText("redis-message-before-loss"),
		ExpectedDispatchGeneration: prepared.DispatchGeneration,
	})
	if err != nil {
		t.Fatal(err)
	}
	if marked.Status != db.RunQueueStatusPublished {
		t.Fatalf("marked status = %s", marked.Status)
	}
	if _, err := pool.Exec(ctx, `UPDATE run_queue_items SET enqueued_at = now() - interval '2 minutes' WHERE org_id = $1 AND run_id = $2`, orgID, runID); err != nil {
		t.Fatal(err)
	}

	candidates, err := queries.ListQueuedRunQueueItemCandidatesForScope(ctx, db.ListQueuedRunQueueItemCandidatesForScopeParams{
		OrgID:         orgID,
		ProjectID:     scope.ProjectID,
		EnvironmentID: scope.EnvironmentID,
		QueueName:     "task/deploy",
		RowLimit:      10,
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(candidates) != 1 || candidates[0].RunID != runID || candidates[0].DispatchMessageID != "redis-message-before-loss" {
		t.Fatalf("candidates = %+v", candidates)
	}

	if _, err := queries.ReserveRunQueueItem(ctx, db.ReserveRunQueueItemParams{
		OrgID:                orgID,
		RunID:                runID,
		WorkerInstanceID:     instance.ID,
		DispatchMessageID:    pgText("redis-message-before-loss"),
		ReservationExpiresAt: pgTime(time.Now().Add(time.Minute)),
	}); err != nil {
		t.Fatal(err)
	}
	requeued, err := queries.RequeueRunQueueItem(ctx, db.RequeueRunQueueItemParams{
		OrgID:             orgID,
		RunID:             runID,
		WorkerInstanceID:  instance.ID,
		DispatchMessageID: pgText("redis-message-before-loss"),
		LastError:         "redis lease lost",
	})
	if err != nil {
		t.Fatal(err)
	}
	if requeued.Status != db.RunQueueStatusQueued || requeued.DispatchMessageID.Valid || requeued.LastError != "redis lease lost" {
		t.Fatalf("requeued = %+v", requeued)
	}
}

func TestListQueueScopesReturnsEveryQueueForEnvironment(t *testing.T) {
	ctx := context.Background()
	queries, pool := newPostgresTestDB(t, ctx)
	orgID := ids.ToPG(ids.DefaultOrgID)

	scope := seedPostgresTestConfiguredScope(t, ctx, pool, queries, orgID)
	siblingEnvironment, err := queries.CreateEnvironment(ctx, db.CreateEnvironmentParams{
		ID:        ids.ToPG(ids.New()),
		OrgID:     orgID,
		ProjectID: scope.ProjectID,
		Slug:      "sibling",
		Name:      "Sibling",
		ColorHex:  "#4F46E5",
	})
	if err != nil {
		t.Fatal(err)
	}
	runtime := runtimeReleaseFields{
		id:              "sha256:runtime",
		arch:            "x86_64",
		abi:             "helmr.firecracker.snapshot.v0",
		kernelDigest:    "sha256:kernel",
		initramfsDigest: "sha256:initramfs",
		rootfsDigest:    "sha256:rootfs",
		cniProfile:      "helmr/v0",
	}
	upsertRuntimeWorker(t, ctx, queries, "queue-scope-runtime", runtime)
	runA := seedComputeDispatchRunWithResources(t, ctx, pool, orgID, scope.ProjectID, scope.EnvironmentID, 1000, 1024)
	runB := seedComputeDispatchRunWithResources(t, ctx, pool, orgID, scope.ProjectID, scope.EnvironmentID, 1000, 1024)
	runC := seedComputeDispatchRunWithResources(t, ctx, pool, orgID, scope.ProjectID, siblingEnvironment.ID, 1000, 1024)
	runD := seedComputeDispatchRunWithResources(t, ctx, pool, orgID, scope.ProjectID, scope.EnvironmentID, 1000, 1024)
	defaultWorkerGroup := defaultPostgresTestWorkerGroup(t, ctx, queries)
	otherWorkerGroup := createPostgresTestWorkerGroup(t, ctx, pool, "other-queue-scope")
	for _, row := range []struct {
		runID         pgtype.UUID
		queue         string
		workerGroupID pgtype.UUID
	}{
		{runID: runA, queue: "queue-a", workerGroupID: defaultWorkerGroup.ID},
		{runID: runB, queue: "queue-b", workerGroupID: defaultWorkerGroup.ID},
		{runID: runC, queue: "queue-a", workerGroupID: defaultWorkerGroup.ID},
		{runID: runD, queue: "queue-other", workerGroupID: otherWorkerGroup},
	} {
		if _, err := queries.UpsertRunRuntimeRequirements(ctx, db.UpsertRunRuntimeRequirementsParams{
			RunID:                   row.runID,
			OrgID:                   orgID,
			RequestedMilliCpu:       1000,
			RequestedMemoryMib:      1024,
			RequestedDiskMib:        0,
			RequestedExecutionSlots: 1,
			RuntimeID:               runtime.id,
			RuntimeArch:             runtime.arch,
			RuntimeABI:              runtime.abi,
			KernelDigest:            runtime.kernelDigest,
			InitramfsDigest:         runtime.initramfsDigest,
			RootfsDigest:            runtime.rootfsDigest,
			CniProfile:              runtime.cniProfile,
			NetworkPolicy:           []byte(`{}`),
			Placement:               []byte(`{}`),
			WorkerGroupID:           row.workerGroupID,
		}); err != nil {
			t.Fatal(err)
		}
		if _, err := queries.UpsertRunQueueItemQueued(ctx, db.UpsertRunQueueItemQueuedParams{
			RunID:             row.runID,
			OrgID:             orgID,
			Priority:          1,
			QueueName:         row.queue,
			QueueTimestamp:    pgTime(time.Now()),
			DispatchMessageID: pgText("message-" + row.queue),
		}); err != nil {
			t.Fatal(err)
		}
	}

	scopes, err := queries.ListQueueScopes(ctx, db.ListQueueScopesParams{
		WorkerGroupID: defaultWorkerGroup.ID,
		ScanSeed:      "test",
		RowLimit:      10,
	})
	if err != nil {
		t.Fatal(err)
	}
	seen := map[string]bool{}
	for _, scope := range scopes {
		if scope.OrgID == orgID {
			seen[ids.MustFromPG(scope.EnvironmentID).String()+":"+scope.QueueName] = true
		}
	}
	if !seen[ids.MustFromPG(scope.EnvironmentID).String()+":queue-a"] ||
		!seen[ids.MustFromPG(scope.EnvironmentID).String()+":queue-b"] ||
		!seen[ids.MustFromPG(siblingEnvironment.ID).String()+":queue-a"] {
		t.Fatalf("queue scopes = %+v", scopes)
	}
	if seen["queue-other"] {
		t.Fatalf("queue scopes included other worker group: %+v", scopes)
	}
}

func TestRunQueueItemFencesStaleEnqueueAndRecoversExpiredLease(t *testing.T) {
	ctx := context.Background()
	queries, pool := newPostgresTestDB(t, ctx)
	orgID := ids.ToPG(ids.DefaultOrgID)

	scope := seedPostgresTestConfiguredScope(t, ctx, pool, queries, orgID)
	runID := seedComputeDispatchRunWithResources(t, ctx, pool, orgID, scope.ProjectID, scope.EnvironmentID, 1000, 1024)
	instance := upsertTestWorkerInstance(t, ctx, queries, "instance-expired-lease")

	prepared, err := queries.PrepareQueuedRunQueueItem(ctx, db.PrepareQueuedRunQueueItemParams{
		OrgID: orgID,
		RunID: runID,
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := queries.MarkRunQueueItemEnqueued(ctx, db.MarkRunQueueItemEnqueuedParams{
		OrgID:                      orgID,
		RunID:                      runID,
		DispatchMessageID:          pgText("message-a"),
		ExpectedDispatchGeneration: prepared.DispatchGeneration + 1,
	}); !errors.Is(err, pgx.ErrNoRows) {
		t.Fatalf("stale enqueue error = %v, want no rows", err)
	}
	_, err = queries.MarkRunQueueItemEnqueued(ctx, db.MarkRunQueueItemEnqueuedParams{
		OrgID:                      orgID,
		RunID:                      runID,
		DispatchMessageID:          pgText("message-a"),
		ExpectedDispatchGeneration: prepared.DispatchGeneration,
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := queries.ReserveRunQueueItem(ctx, db.ReserveRunQueueItemParams{
		OrgID:                orgID,
		RunID:                runID,
		WorkerInstanceID:     instance.ID,
		DispatchMessageID:    pgText("message-a"),
		ReservationExpiresAt: pgTime(time.Now().Add(time.Minute)),
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `UPDATE run_queue_items SET reservation_expires_at = now() - interval '1 second' WHERE run_id = $1`, runID); err != nil {
		t.Fatal(err)
	}
	if _, err := queries.CompleteRunQueueItem(ctx, db.CompleteRunQueueItemParams{
		OrgID:             orgID,
		RunID:             runID,
		WorkerInstanceID:  instance.ID,
		DispatchMessageID: pgText("message-a"),
	}); err == nil {
		t.Fatal("expected expired queue lease ack to fail")
	}
	if _, err := queries.PrepareQueuedRunQueueItem(ctx, db.PrepareQueuedRunQueueItemParams{
		OrgID: orgID,
		RunID: runID,
	}); err != nil {
		t.Fatal(err)
	}
}

func upsertTestWorkerInstance(t *testing.T, ctx context.Context, queries *db.Queries, instanceID string) db.UpsertWorkerInstanceHeartbeatRow {
	t.Helper()
	return upsertTestWorkerInstanceWithRuntime(t, ctx, queries, instanceID, "", []byte(`{}`), []byte(`{
		"runtime_id":"sha256:runtime",
		"runtime_arch":"x86_64",
		"runtime_abi":"helmr.firecracker.snapshot.v0",
		"kernel_digest":"sha256:kernel",
		"initramfs_digest":"sha256:initramfs",
		"rootfs_digest":"sha256:rootfs",
		"cni_profile":"helmr/v0"
	}`))
}

func upsertTestWorkerInstanceInGroup(t *testing.T, ctx context.Context, queries *db.Queries, instanceID string, workerGroupID pgtype.UUID) db.UpsertWorkerInstanceHeartbeatRow {
	t.Helper()
	instance, err := queries.UpsertWorkerInstanceHeartbeat(ctx, workerHeartbeatParams(instanceID, runtimeReleaseFields{
		id:              "sha256:runtime",
		arch:            "x86_64",
		abi:             "helmr.firecracker.snapshot.v0",
		kernelDigest:    "sha256:kernel",
		initramfsDigest: "sha256:initramfs",
		rootfsDigest:    "sha256:rootfs",
		cniProfile:      "helmr/v0",
	}, workerGroupID))
	if err != nil {
		t.Fatal(err)
	}
	if err := queries.EnsureRuntimeReleaseSelection(ctx, instance.RuntimeID); err != nil {
		t.Fatal(err)
	}
	return instance
}

func upsertTestWorkerInstanceWithRuntime(t *testing.T, ctx context.Context, queries *db.Queries, instanceID, region string, labels, heartbeat []byte) db.UpsertWorkerInstanceHeartbeatRow {
	t.Helper()
	workerGroup := defaultPostgresTestWorkerGroup(t, ctx, queries)
	instance, err := queries.UpsertWorkerInstanceHeartbeat(ctx, workerHeartbeatParams(instanceID, runtimeReleaseFields{
		id:              "sha256:runtime",
		arch:            "x86_64",
		abi:             "helmr.firecracker.snapshot.v0",
		kernelDigest:    "sha256:kernel",
		initramfsDigest: "sha256:initramfs",
		rootfsDigest:    "sha256:rootfs",
		cniProfile:      "helmr/v0",
	}, workerGroup.ID))
	if err != nil {
		t.Fatal(err)
	}
	if err := queries.EnsureRuntimeReleaseSelection(ctx, instance.RuntimeID); err != nil {
		t.Fatal(err)
	}
	return instance
}

type runtimeReleaseFields struct {
	id              string
	arch            string
	abi             string
	kernelDigest    string
	initramfsDigest string
	rootfsDigest    string
	cniProfile      string
}

func upsertRuntimeWorker(t *testing.T, ctx context.Context, queries *db.Queries, instanceID string, runtime runtimeReleaseFields) db.UpsertWorkerInstanceHeartbeatRow {
	t.Helper()
	workerGroup := defaultPostgresTestWorkerGroup(t, ctx, queries)
	instance, err := queries.UpsertWorkerInstanceHeartbeat(ctx, workerHeartbeatParams(instanceID, runtime, workerGroup.ID))
	if err != nil {
		t.Fatal(err)
	}
	return instance
}

func workerHeartbeatParams(instanceID string, runtime runtimeReleaseFields, workerGroupID pgtype.UUID) db.UpsertWorkerInstanceHeartbeatParams {
	return db.UpsertWorkerInstanceHeartbeatParams{
		ID:                        ids.ToPG(ids.New()),
		WorkerGroupID:             workerGroupID,
		ResourceID:                instanceID,
		Region:                    "",
		TotalMilliCpu:             4000,
		TotalMemoryMib:            8192,
		TotalDiskMib:              20480,
		TotalExecutionSlots:       4,
		AvailableMilliCpu:         4000,
		AvailableMemoryMib:        8192,
		AvailableDiskMib:          20480,
		AvailableExecutionSlots:   4,
		Labels:                    []byte(`{}`),
		Heartbeat:                 []byte(`{}`),
		ProtocolVersion:           api.CurrentWorkerProtocolVersion,
		SupportedProtocolVersions: []byte(`["` + api.CurrentWorkerProtocolVersion + `"]`),
		RuntimeID:                 runtime.id,
		RuntimeArch:               runtime.arch,
		RuntimeABI:                runtime.abi,
		KernelDigest:              runtime.kernelDigest,
		InitramfsDigest:           runtime.initramfsDigest,
		RootfsDigest:              runtime.rootfsDigest,
		CniProfile:                runtime.cniProfile,
	}
}

func publishTestRunQueueItem(t *testing.T, ctx context.Context, queries *db.Queries, orgID, runID pgtype.UUID, entry db.RunQueueItem, queueMessageID string) db.RunQueueItem {
	t.Helper()
	published, err := queries.MarkRunQueueItemEnqueued(ctx, db.MarkRunQueueItemEnqueuedParams{
		OrgID:                      orgID,
		RunID:                      runID,
		DispatchMessageID:          pgText(queueMessageID),
		ExpectedDispatchGeneration: entry.DispatchGeneration,
	})
	if err != nil {
		t.Fatal(err)
	}
	return published
}

func seedComputeDispatchRun(t *testing.T, ctx context.Context, pool *pgxpool.Pool, orgID, projectID, environmentID pgtype.UUID) pgtype.UUID {
	t.Helper()
	return seedComputeDispatchRunWithResources(t, ctx, pool, orgID, projectID, environmentID, 1000, 1024, 0)
}

func seedComputeDispatchRunWithResources(t *testing.T, ctx context.Context, pool *pgxpool.Pool, orgID, projectID, environmentID pgtype.UUID, requestedMilliCPU, requestedMemoryMiB int64, requestedDiskMiB ...int64) pgtype.UUID {
	t.Helper()
	diskMiB := int64(0)
	if len(requestedDiskMiB) > 0 {
		diskMiB = requestedDiskMiB[0]
	}
	deploymentID, deploymentTaskID := ensureComputeDispatchDeploymentTask(t, ctx, pool, orgID, projectID, environmentID, requestedMilliCPU, requestedMemoryMiB, diskMiB)
	runID := ids.ToPG(ids.New())
	attemptID := ids.ToPG(ids.New())
	traceID, err := tracing.NewTraceID()
	if err != nil {
		t.Fatal(err)
	}
	rootSpanID, err := tracing.NewSpanID()
	if err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `
		INSERT INTO runs (
    id,
    org_id,
    project_id,
    environment_id,
    deployment_id,
    deployment_task_id,
    task_id,
    status,
    payload,
    queue_name,
    priority,
    queue_timestamp,
    max_duration_seconds,
    trace_id,
    root_span_id
) VALUES ($1, $2, $3, $4, $5, $6, 'deploy', 'queued', '{}', 'task/deploy', 0, now(), 300, $7, $8)
	`, runID, orgID, projectID, environmentID, deploymentID, deploymentTaskID, traceID, rootSpanID); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `
	INSERT INTO run_attempts (id, org_id, run_id, attempt_number, status)
	VALUES ($1, $2, $3, 1, 'queued')
	`, attemptID, orgID, runID); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `
	UPDATE runs
	   SET current_attempt_id = $3,
	       current_attempt_number = 1
	 WHERE org_id = $1
	   AND id = $2
	`, orgID, runID, attemptID); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `
	INSERT INTO run_snapshots (org_id, run_id, version, status, attempt_id, transition, reason)
	VALUES ($1, $2, 1, 'queued', $3, 'run.created', '{}'::jsonb)
	`, orgID, runID, attemptID); err != nil {
		t.Fatal(err)
	}
	return runID
}

func setRunDispatchFields(t *testing.T, ctx context.Context, pool *pgxpool.Pool, orgID, runID pgtype.UUID, queueName string, priority int32, queueTimestamp time.Time) {
	t.Helper()
	if _, err := pool.Exec(ctx, `
UPDATE runs
   SET queue_name = $3,
       priority = $4,
       queue_timestamp = $5
 WHERE org_id = $1
   AND id = $2
`, orgID, runID, queueName, priority, queueTimestamp); err != nil {
		t.Fatal(err)
	}
}

func ensureComputeDispatchDeploymentTask(t *testing.T, ctx context.Context, pool *pgxpool.Pool, orgID, projectID, environmentID pgtype.UUID, requestedMilliCPU, requestedMemoryMiB, requestedDiskMiB int64) (pgtype.UUID, pgtype.UUID) {
	t.Helper()
	queries := db.New(pool)
	workerGroup := defaultPostgresTestWorkerGroup(t, ctx, queries)
	deploymentID := ids.ToPG(ids.New())
	deploymentTaskID := ids.ToPG(ids.New())
	sourceArtifactID := ids.ToPG(ids.New())
	buildManifestArtifactID := ids.ToPG(ids.New())
	deploymentManifestArtifactID := ids.ToPG(ids.New())
	bundleArtifactID := ids.ToPG(ids.New())
	sourceDigest := "sha256:" + ids.New().String()
	if _, err := pool.Exec(ctx, `
INSERT INTO cas_objects (digest, size_bytes, media_type)
VALUES ($1, 1, 'application/vnd.helmr.bundle')
ON CONFLICT (digest) DO NOTHING
`, sourceDigest); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `
INSERT INTO artifacts (id, org_id, project_id, environment_id, digest, kind, size_bytes, media_type)
VALUES
    ($1, $5, $6, $7, $8, 'deployment_source', 1, 'application/vnd.helmr.bundle'),
    ($2, $5, $6, $7, $8, 'build_manifest', 1, 'application/vnd.helmr.bundle'),
    ($3, $5, $6, $7, $8, 'deployment_manifest', 1, 'application/vnd.helmr.bundle'),
    ($4, $5, $6, $7, $8, 'task_bundle', 1, 'application/vnd.helmr.bundle')
`, sourceArtifactID, buildManifestArtifactID, deploymentManifestArtifactID, bundleArtifactID, orgID, projectID, environmentID, sourceDigest); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `
INSERT INTO deployments (id, org_id, project_id, environment_id, version, worker_group_id, content_hash, deployment_source_artifact_id, build_manifest_artifact_id, deployment_manifest_artifact_id, status, building_at, built_at, deployed_at)
VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, 'deployed', now(), now(), now())
`, deploymentID, orgID, projectID, environmentID, "test-"+ids.MustFromPG(deploymentID).String(), workerGroup.ID, sourceDigest, sourceArtifactID, buildManifestArtifactID, deploymentManifestArtifactID); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `
INSERT INTO deployment_tasks (
    id,
    org_id,
    project_id,
    environment_id,
    deployment_id,
    task_id,
    file_path,
    export_name,
    handler_entrypoint,
    bundle_artifact_id,
    requested_milli_cpu,
    requested_memory_mib,
    requested_disk_mib,
	    secret_declarations,
	    resource_requirements,
	    queue_name,
	    max_duration_seconds
	) VALUES ($1, $2, $3, $4, $5, 'deploy', 'src/task.ts', 'deploy', 'src/task.ts#deploy', $9, $6, $7, $8, '[]', '{}', 'task/deploy', 300)
`, deploymentTaskID, orgID, projectID, environmentID, deploymentID, requestedMilliCPU, requestedMemoryMiB, requestedDiskMiB, bundleArtifactID); err != nil {
		t.Fatal(err)
	}
	return deploymentID, deploymentTaskID
}
