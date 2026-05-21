package db_test

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/helmrdotdev/helmr/internal/db"
	"github.com/helmrdotdev/helmr/internal/ids"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/jackc/pgx/v5/pgxpool"
)

func TestComputeDispatchGroupBoundaries(t *testing.T) {
	ctx := context.Background()
	queries, pool := newPostgresTestDB(t, ctx)
	orgID := ids.ToPG(ids.DefaultOrgID)

	scope := seedPostgresTestDefaultScope(t, ctx, pool, queries, orgID)

	poolA := createTestWorkerPool(t, ctx, queries, orgID, "pool-a", "queue-a")
	poolB := createTestWorkerPool(t, ctx, queries, orgID, "pool-b", "queue-b")
	hostA := upsertTestWorkerHost(t, ctx, queries, orgID, poolA.ID, "host-a")
	hostB := upsertTestWorkerHost(t, ctx, queries, orgID, poolB.ID, "host-b")

	runID := seedComputeDispatchRun(t, ctx, pool, orgID, scope.ProjectID, scope.EnvironmentID)
	if _, err := queries.UpsertRunRequirements(ctx, db.UpsertRunRequirementsParams{
		RunID:                   runID,
		OrgID:                   orgID,
		WorkerPoolID:            poolA.ID,
		RequestedMilliCpu:       1000,
		RequestedMemoryMib:      1024,
		RequestedDiskMib:        2048,
		RequestedExecutionSlots: 1,
		RuntimeArch:             "x86_64",
		RuntimeABI:              "linux",
		NetworkPolicy:           []byte(`{}`),
		Placement:               []byte(`{}`),
	}); err != nil {
		t.Fatal(err)
	}

	if _, err := queries.UpsertRunQueueEntryQueued(ctx, db.UpsertRunQueueEntryQueuedParams{
		RunID:          runID,
		OrgID:          orgID,
		WorkerPoolID:   poolA.ID,
		Priority:       0,
		QueueName:      poolB.QueueName,
		QueueMessageID: pgText("redis-wrong-queue"),
	}); err == nil {
		t.Fatal("expected dispatch row with mismatched worker pool queue to fail")
	}

	dispatch, err := queries.UpsertRunQueueEntryQueued(ctx, db.UpsertRunQueueEntryQueuedParams{
		RunID:          runID,
		OrgID:          orgID,
		WorkerPoolID:   poolA.ID,
		Priority:       10,
		QueueName:      poolA.QueueName,
		QueueMessageID: pgText("redis-message-1"),
	})
	if err != nil {
		t.Fatal(err)
	}
	if dispatch.Status != db.RunQueueStatusQueued {
		t.Fatalf("dispatch status = %s, want queued", dispatch.Status)
	}
	publishTestRunQueueEntry(t, ctx, queries, orgID, runID, dispatch, "redis-message-1")

	if _, err := queries.ReserveRunQueueEntry(ctx, db.ReserveRunQueueEntryParams{
		OrgID:                orgID,
		RunID:                runID,
		WorkerPoolID:         poolA.ID,
		WorkerHostID:         hostB.ID,
		QueueMessageID:       pgText("redis-message-1"),
		ReservationExpiresAt: pgtype.Timestamptz{Time: time.Now().Add(time.Minute), Valid: true},
	}); err == nil {
		t.Fatal("expected queue lease to a host from another worker pool to fail")
	}

	if _, err := queries.ReserveRunQueueEntry(ctx, db.ReserveRunQueueEntryParams{
		OrgID:                orgID,
		RunID:                runID,
		WorkerPoolID:         poolA.ID,
		WorkerHostID:         hostA.ID,
		QueueMessageID:       pgText("redis-message-stale"),
		ReservationExpiresAt: pgtype.Timestamptz{Time: time.Now().Add(time.Minute), Valid: true},
	}); !errors.Is(err, pgx.ErrNoRows) {
		t.Fatalf("stale redis message lease err = %v, want no rows", err)
	}

	leased, err := queries.ReserveRunQueueEntry(ctx, db.ReserveRunQueueEntryParams{
		OrgID:                orgID,
		RunID:                runID,
		WorkerPoolID:         poolA.ID,
		WorkerHostID:         hostA.ID,
		QueueMessageID:       pgText("redis-message-1"),
		ReservationExpiresAt: pgtype.Timestamptz{Time: time.Now().Add(time.Minute), Valid: true},
	})
	if err != nil {
		t.Fatal(err)
	}
	if leased.Status != db.RunQueueStatusReserved || leased.ReservedByWorkerHostID != hostA.ID {
		t.Fatalf("leased dispatch = %+v, host = %+v", leased, hostA)
	}

	if _, err := pool.Exec(ctx, `UPDATE run_queue_entries SET reservation_expires_at = now() - interval '1 second' WHERE run_id = $1`, runID); err != nil {
		t.Fatal(err)
	}
	if _, err := queries.RenewRunQueueReservation(ctx, db.RenewRunQueueReservationParams{
		OrgID:                orgID,
		RunID:                runID,
		WorkerPoolID:         poolA.ID,
		WorkerHostID:         hostA.ID,
		QueueMessageID:       pgText("redis-message-1"),
		ReservationExpiresAt: pgtype.Timestamptz{Time: time.Now().Add(time.Minute), Valid: true},
	}); err == nil {
		t.Fatal("expected expired queue lease renew to fail")
	}
	if _, err := queries.CompleteRunQueueEntry(ctx, db.CompleteRunQueueEntryParams{
		OrgID:          orgID,
		RunID:          runID,
		WorkerPoolID:   poolA.ID,
		WorkerHostID:   hostA.ID,
		QueueMessageID: pgText("redis-message-1"),
	}); err == nil {
		t.Fatal("expected expired queue lease ack to fail")
	}
	if _, err := queries.RequeueRunQueueEntry(ctx, db.RequeueRunQueueEntryParams{
		OrgID:          orgID,
		RunID:          runID,
		WorkerPoolID:   poolA.ID,
		WorkerHostID:   hostA.ID,
		QueueMessageID: pgText("redis-message-1"),
		LastError:      "expired",
	}); err == nil {
		t.Fatal("expected expired queue lease nack to fail")
	}
}

func TestArchiveWorkerPoolAllowsSlugAndQueueReuse(t *testing.T) {
	ctx := context.Background()
	queries, pool := newPostgresTestDB(t, ctx)
	orgID := ids.ToPG(ids.DefaultOrgID)

	seedPostgresTestDefaultScope(t, ctx, pool, queries, orgID)

	workerPool := createTestWorkerPool(t, ctx, queries, orgID, "reuse", "queue-reuse")
	if _, err := queries.ArchiveWorkerPool(ctx, db.ArchiveWorkerPoolParams{
		OrgID: orgID,
		ID:    workerPool.ID,
	}); err != nil {
		t.Fatal(err)
	}
	replacement := createTestWorkerPool(t, ctx, queries, orgID, "reuse", "queue-reuse")
	if replacement.ID == workerPool.ID {
		t.Fatalf("replacement reused archived worker pool id: %v", replacement.ID)
	}
}

func TestPrepareQueuedRunQueueEntryBuildsRequirementsFromDeploymentTask(t *testing.T) {
	ctx := context.Background()
	queries, pool := newPostgresTestDB(t, ctx)
	orgID := ids.ToPG(ids.DefaultOrgID)

	scope := seedPostgresTestDefaultScope(t, ctx, pool, queries, orgID)
	runID := seedComputeDispatchRunWithResources(t, ctx, pool, orgID, scope.ProjectID, scope.EnvironmentID, 3000, 4096)

	prepared, err := queries.PrepareQueuedRunQueueEntry(ctx, db.PrepareQueuedRunQueueEntryParams{
		OrgID:    orgID,
		RunID:    runID,
		Priority: 10,
	})
	if err != nil {
		t.Fatal(err)
	}
	if !prepared.WorkerPoolID.Valid || prepared.QueueName != "default" || prepared.Priority != 10 {
		t.Fatalf("prepared dispatch = %+v", prepared)
	}
	if prepared.RequestedMilliCpu != 3000 || prepared.RequestedMemoryMib != 4096 || prepared.RequestedDiskMib != 0 || prepared.RequestedExecutionSlots != 1 {
		t.Fatalf("prepared requirements = %+v", prepared)
	}

	marked, err := queries.MarkRunQueueEntryEnqueued(ctx, db.MarkRunQueueEntryEnqueuedParams{
		OrgID:                      orgID,
		RunID:                      runID,
		QueueMessageID:             pgText("redis-message-1"),
		ExpectedDispatchGeneration: prepared.DispatchGeneration,
	})
	if err != nil {
		t.Fatal(err)
	}
	if marked.QueueMessageID.String != "redis-message-1" || marked.LastError != "" {
		t.Fatalf("marked dispatch = %+v", marked)
	}

	candidates, err := queries.ListQueuedRunQueueEntryCandidates(ctx, db.ListQueuedRunQueueEntryCandidatesParams{
		OrgID:    orgID,
		RowLimit: 10,
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(candidates) != 0 {
		t.Fatalf("candidates = %+v", candidates)
	}
}

func TestQueuedRunQueueEntryWithMessageIDCanBeReenqueued(t *testing.T) {
	ctx := context.Background()
	queries, pool := newPostgresTestDB(t, ctx)
	orgID := ids.ToPG(ids.DefaultOrgID)

	scope := seedPostgresTestDefaultScope(t, ctx, pool, queries, orgID)
	runID := seedComputeDispatchRunWithResources(t, ctx, pool, orgID, scope.ProjectID, scope.EnvironmentID, 3000, 4096)

	prepared, err := queries.PrepareQueuedRunQueueEntry(ctx, db.PrepareQueuedRunQueueEntryParams{
		OrgID: orgID,
		RunID: runID,
	})
	if err != nil {
		t.Fatal(err)
	}
	host := upsertTestWorkerHost(t, ctx, queries, orgID, prepared.WorkerPoolID, "host-redis-loss")

	marked, err := queries.MarkRunQueueEntryEnqueued(ctx, db.MarkRunQueueEntryEnqueuedParams{
		OrgID:                      orgID,
		RunID:                      runID,
		QueueMessageID:             pgText("redis-message-before-loss"),
		ExpectedDispatchGeneration: prepared.DispatchGeneration,
	})
	if err != nil {
		t.Fatal(err)
	}

	candidates, err := queries.ListQueuedRunQueueEntryCandidates(ctx, db.ListQueuedRunQueueEntryCandidatesParams{
		OrgID:    orgID,
		RowLimit: 10,
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(candidates) != 0 {
		t.Fatalf("fresh candidates = %+v", candidates)
	}

	if _, err := pool.Exec(ctx, `
UPDATE run_queue_entries
   SET enqueued_at = now() - interval '2 minutes'
 WHERE org_id = $1
   AND run_id = $2
`, orgID, runID); err != nil {
		t.Fatal(err)
	}

	candidates, err = queries.ListQueuedRunQueueEntryCandidates(ctx, db.ListQueuedRunQueueEntryCandidatesParams{
		OrgID:    orgID,
		RowLimit: 10,
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(candidates) != 1 || candidates[0].RunID != runID {
		t.Fatalf("stale candidates = %+v", candidates)
	}

	refreshed, err := queries.PrepareQueuedRunQueueEntry(ctx, db.PrepareQueuedRunQueueEntryParams{
		OrgID: orgID,
		RunID: runID,
	})
	if err != nil {
		t.Fatal(err)
	}
	if refreshed.DispatchGeneration <= marked.DispatchGeneration {
		t.Fatalf("refreshed queue version = %d, want > %d", refreshed.DispatchGeneration, marked.DispatchGeneration)
	}
	var currentMessageID pgtype.Text
	if err := pool.QueryRow(ctx, `
	SELECT queue_message_id
	  FROM run_queue_entries
 WHERE org_id = $1
   AND run_id = $2
	`, orgID, runID).Scan(&currentMessageID); err != nil {
		t.Fatal(err)
	}
	if currentMessageID.Valid {
		t.Fatalf("queue_message_id after refresh = %q, want null", currentMessageID.String)
	}

	if _, err := queries.ReserveRunQueueEntry(ctx, db.ReserveRunQueueEntryParams{
		OrgID:                orgID,
		RunID:                runID,
		WorkerPoolID:         prepared.WorkerPoolID,
		WorkerHostID:         host.ID,
		QueueMessageID:       pgText("redis-message-before-loss"),
		ReservationExpiresAt: pgtype.Timestamptz{Time: time.Now().Add(time.Minute), Valid: true},
	}); !errors.Is(err, pgx.ErrNoRows) {
		t.Fatalf("stale redis message lease err = %v, want no rows", err)
	}

	reenqueued, err := queries.MarkRunQueueEntryEnqueued(ctx, db.MarkRunQueueEntryEnqueuedParams{
		OrgID:                      orgID,
		RunID:                      runID,
		QueueMessageID:             pgText("redis-message-after-loss"),
		ExpectedDispatchGeneration: refreshed.DispatchGeneration,
	})
	if err != nil {
		t.Fatal(err)
	}
	if reenqueued.QueueMessageID.String != "redis-message-after-loss" {
		t.Fatalf("reenqueued dispatch = %+v", reenqueued)
	}

	leased, err := queries.ReserveRunQueueEntry(ctx, db.ReserveRunQueueEntryParams{
		OrgID:                orgID,
		RunID:                runID,
		WorkerPoolID:         prepared.WorkerPoolID,
		WorkerHostID:         host.ID,
		QueueMessageID:       pgText("redis-message-after-loss"),
		ReservationExpiresAt: pgtype.Timestamptz{Time: time.Now().Add(time.Minute), Valid: true},
	})
	if err != nil {
		t.Fatal(err)
	}
	if leased.Status != db.RunQueueStatusReserved || leased.QueueMessageID.String != "redis-message-after-loss" {
		t.Fatalf("leased dispatch = %+v", leased)
	}
}

func TestRunQueueEntryFencesStaleEnqueueAndRecoversExpiredLease(t *testing.T) {
	ctx := context.Background()
	queries, pool := newPostgresTestDB(t, ctx)
	orgID := ids.ToPG(ids.DefaultOrgID)

	scope := seedPostgresTestDefaultScope(t, ctx, pool, queries, orgID)
	workerPool := createTestWorkerPool(t, ctx, queries, orgID, "recover", "queue-recover")
	hostA := upsertTestWorkerHost(t, ctx, queries, orgID, workerPool.ID, "host-a")
	hostB := upsertTestWorkerHost(t, ctx, queries, orgID, workerPool.ID, "host-b")
	runID := seedComputeDispatchRun(t, ctx, pool, orgID, scope.ProjectID, scope.EnvironmentID)
	if _, err := queries.UpsertRunRequirements(ctx, db.UpsertRunRequirementsParams{
		RunID:                   runID,
		OrgID:                   orgID,
		WorkerPoolID:            workerPool.ID,
		RequestedMilliCpu:       1000,
		RequestedMemoryMib:      1024,
		RequestedDiskMib:        2048,
		RequestedExecutionSlots: 1,
		NetworkPolicy:           []byte(`{}`),
		Placement:               []byte(`{}`),
	}); err != nil {
		t.Fatal(err)
	}

	prepared, err := queries.PrepareQueuedRunQueueEntry(ctx, db.PrepareQueuedRunQueueEntryParams{
		OrgID: orgID,
		RunID: runID,
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := queries.PrepareQueuedRunQueueEntry(ctx, db.PrepareQueuedRunQueueEntryParams{
		OrgID: orgID,
		RunID: runID,
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := queries.MarkRunQueueEntryEnqueued(ctx, db.MarkRunQueueEntryEnqueuedParams{
		OrgID:                      orgID,
		RunID:                      runID,
		QueueMessageID:             pgText("redis-message-stale"),
		ExpectedDispatchGeneration: prepared.DispatchGeneration,
	}); !errors.Is(err, pgx.ErrNoRows) {
		t.Fatalf("stale enqueue mark err = %v, want no rows", err)
	}

	refreshed, err := queries.PrepareQueuedRunQueueEntry(ctx, db.PrepareQueuedRunQueueEntryParams{
		OrgID: orgID,
		RunID: runID,
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := queries.MarkRunQueueEntryEnqueued(ctx, db.MarkRunQueueEntryEnqueuedParams{
		OrgID:                      orgID,
		RunID:                      runID,
		QueueMessageID:             pgText("redis-message-current"),
		ExpectedDispatchGeneration: refreshed.DispatchGeneration,
	}); err != nil {
		t.Fatal(err)
	}

	if _, err := queries.ReserveRunQueueEntry(ctx, db.ReserveRunQueueEntryParams{
		OrgID:                orgID,
		RunID:                runID,
		WorkerPoolID:         workerPool.ID,
		WorkerHostID:         hostA.ID,
		QueueMessageID:       pgText("redis-message-current"),
		ReservationExpiresAt: pgtype.Timestamptz{Time: time.Now().Add(time.Minute), Valid: true},
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := queries.ReserveRunQueueEntry(ctx, db.ReserveRunQueueEntryParams{
		OrgID:                orgID,
		RunID:                runID,
		WorkerPoolID:         workerPool.ID,
		WorkerHostID:         hostB.ID,
		QueueMessageID:       pgText("redis-message-current"),
		ReservationExpiresAt: pgtype.Timestamptz{Time: time.Now().Add(time.Minute), Valid: true},
	}); !errors.Is(err, pgx.ErrNoRows) {
		t.Fatalf("active lease takeover err = %v, want no rows", err)
	}
	if _, err := pool.Exec(ctx, `UPDATE run_queue_entries SET reservation_expires_at = now() - interval '1 second' WHERE run_id = $1`, runID); err != nil {
		t.Fatal(err)
	}
	takenOver, err := queries.ReserveRunQueueEntry(ctx, db.ReserveRunQueueEntryParams{
		OrgID:                orgID,
		RunID:                runID,
		WorkerPoolID:         workerPool.ID,
		WorkerHostID:         hostB.ID,
		QueueMessageID:       pgText("redis-message-current"),
		ReservationExpiresAt: pgtype.Timestamptz{Time: time.Now().Add(time.Minute), Valid: true},
	})
	if err != nil {
		t.Fatal(err)
	}
	if takenOver.ReservedByWorkerHostID != hostB.ID || takenOver.Status != db.RunQueueStatusReserved {
		t.Fatalf("taken over dispatch = %+v", takenOver)
	}
}

func TestPrepareQueuedRunQueueEntryRequiresActiveWorkerPool(t *testing.T) {
	ctx := context.Background()
	queries, pool := newPostgresTestDB(t, ctx)
	orgID := ids.ToPG(ids.DefaultOrgID)

	scope := seedPostgresTestDefaultScope(t, ctx, pool, queries, orgID)
	pools, err := queries.ListWorkerPools(ctx, db.ListWorkerPoolsParams{
		OrgID:    orgID,
		RowLimit: 100,
	})
	if err != nil {
		t.Fatal(err)
	}
	for _, workerPool := range pools {
		if _, err := queries.ArchiveWorkerPool(ctx, db.ArchiveWorkerPoolParams{OrgID: orgID, ID: workerPool.ID}); err != nil {
			t.Fatal(err)
		}
	}
	runID := seedComputeDispatchRun(t, ctx, pool, orgID, scope.ProjectID, scope.EnvironmentID)

	_, err = queries.PrepareQueuedRunQueueEntry(ctx, db.PrepareQueuedRunQueueEntryParams{
		OrgID: orgID,
		RunID: runID,
	})
	if !errors.Is(err, pgx.ErrNoRows) {
		t.Fatalf("prepare error = %v, want no rows", err)
	}
}

func TestProjectEnvironmentCreationUsesSharedOrgWorkerPool(t *testing.T) {
	ctx := context.Background()
	queries, pool := newPostgresTestDB(t, ctx)
	orgID := ids.ToPG(ids.DefaultOrgID)

	seedPostgresTestDefaultScope(t, ctx, pool, queries, orgID)
	requireCustomerManagedDefaultWorkerPool(t, ctx, queries, orgID, "default")

	project, err := queries.CreateProjectWithDefaultEnvironment(ctx, db.CreateProjectWithDefaultEnvironmentParams{
		ID:            ids.ToPG(ids.New()),
		OrgID:         orgID,
		Slug:          "project-a",
		Name:          "Project A",
		EnvironmentID: ids.ToPG(ids.New()),
	})
	if err != nil {
		t.Fatal(err)
	}
	environments, err := queries.ListEnvironments(ctx, db.ListEnvironmentsParams{
		OrgID:     orgID,
		ProjectID: project.ID,
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(environments) != 1 {
		t.Fatalf("environments = %+v", environments)
	}
	requireWorkerPoolCount(t, ctx, queries, orgID, 1)

	runID := seedComputeDispatchRun(t, ctx, pool, orgID, project.ID, environments[0].ID)
	prepared, err := queries.PrepareQueuedRunQueueEntry(ctx, db.PrepareQueuedRunQueueEntryParams{
		OrgID: orgID,
		RunID: runID,
	})
	if err != nil {
		t.Fatal(err)
	}
	if prepared.QueueName != "default" {
		t.Fatalf("prepared queue = %q", prepared.QueueName)
	}
	managedPool := createTestWorkerPool(t, ctx, queries, orgID, "managed", "project-a/managed")
	runID = seedComputeDispatchRun(t, ctx, pool, orgID, project.ID, environments[0].ID)
	prepared, err = queries.PrepareQueuedRunQueueEntry(ctx, db.PrepareQueuedRunQueueEntryParams{
		OrgID: orgID,
		RunID: runID,
	})
	if err != nil {
		t.Fatal(err)
	}
	if prepared.WorkerPoolID != managedPool.ID || prepared.QueueName != "project-a/managed" {
		t.Fatalf("prepared worker pool = %v queue = %q, want %v project-a/managed", prepared.WorkerPoolID, prepared.QueueName, managedPool.ID)
	}

	environment, err := queries.CreateEnvironment(ctx, db.CreateEnvironmentParams{
		ID:        ids.ToPG(ids.New()),
		OrgID:     orgID,
		ProjectID: project.ID,
		Slug:      "preview",
		Name:      "Preview",
		IsDefault: false,
	})
	if err != nil {
		t.Fatal(err)
	}
	requireWorkerPoolCount(t, ctx, queries, orgID, 2)

	runID = seedComputeDispatchRun(t, ctx, pool, orgID, project.ID, environment.ID)
	prepared, err = queries.PrepareQueuedRunQueueEntry(ctx, db.PrepareQueuedRunQueueEntryParams{
		OrgID: orgID,
		RunID: runID,
	})
	if err != nil {
		t.Fatal(err)
	}
	if prepared.QueueName != "project-a/managed" {
		t.Fatalf("prepared queue = %q", prepared.QueueName)
	}
}

func TestCreateProjectWithDefaultEnvironmentDoesNotCreateWorkerPool(t *testing.T) {
	ctx := context.Background()
	queries, pool := newPostgresTestDB(t, ctx)
	orgID := ids.ToPG(ids.DefaultOrgID)
	seedPostgresTestOrganization(t, ctx, pool, orgID)

	if _, err := queries.CreateProjectWithDefaultEnvironment(ctx, db.CreateProjectWithDefaultEnvironmentParams{
		ID:            ids.ToPG(ids.New()),
		OrgID:         orgID,
		Slug:          "project-a",
		Name:          "Project A",
		EnvironmentID: ids.ToPG(ids.New()),
	}); err != nil {
		t.Fatal(err)
	}
	requireWorkerPoolCount(t, ctx, queries, orgID, 0)
}

func requireWorkerPoolCount(t *testing.T, ctx context.Context, queries *db.Queries, orgID pgtype.UUID, count int) {
	t.Helper()
	pools, err := queries.ListWorkerPools(ctx, db.ListWorkerPoolsParams{
		OrgID:    orgID,
		RowLimit: 100,
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(pools) != count {
		t.Fatalf("worker pools = %+v, want count %d", pools, count)
	}
}

func requireCustomerManagedDefaultWorkerPool(t *testing.T, ctx context.Context, queries *db.Queries, orgID pgtype.UUID, queueName string) db.WorkerPool {
	t.Helper()
	pools, err := queries.ListWorkerPools(ctx, db.ListWorkerPoolsParams{
		OrgID:    orgID,
		RowLimit: 100,
	})
	if err != nil {
		t.Fatal(err)
	}
	for _, workerPool := range pools {
		if workerPool.Slug != "default" {
			continue
		}
		if workerPool.ProvisioningMode != db.WorkerPoolProvisioningModeCustomerManaged {
			t.Fatalf("default worker pool provisioning mode = %q, want %q", workerPool.ProvisioningMode, db.WorkerPoolProvisioningModeCustomerManaged)
		}
		if workerPool.QueueName != queueName {
			t.Fatalf("default worker pool queue = %q, want %q", workerPool.QueueName, queueName)
		}
		return workerPool
	}
	t.Fatalf("default worker pool not found in pools = %+v", pools)
	return db.WorkerPool{}
}

func createTestWorkerPool(t *testing.T, ctx context.Context, queries *db.Queries, orgID pgtype.UUID, slug, queueName string) db.WorkerPool {
	t.Helper()
	workerPool, err := queries.CreateWorkerPool(ctx, db.CreateWorkerPoolParams{
		ID:               ids.ToPG(ids.New()),
		OrgID:            orgID,
		Slug:             slug,
		Name:             slug,
		ProvisioningMode: db.WorkerPoolProvisioningModeHelmrManaged,
		QueueName:        queueName,
		Region:           "us-east-1",
		Capabilities:     []byte(`{}`),
		Metadata:         []byte(`{}`),
	})
	if err != nil {
		t.Fatal(err)
	}
	return workerPool
}

func upsertTestWorkerHost(t *testing.T, ctx context.Context, queries *db.Queries, orgID, workerPoolID pgtype.UUID, externalID string) db.WorkerHost {
	t.Helper()
	return upsertTestWorkerHostWithRuntime(
		t,
		ctx,
		queries,
		orgID,
		workerPoolID,
		externalID,
		"us-east-1",
		[]byte(`{"host":"test","pool":"standard","dedicated_key":"tenant-a","snapshot_key":"snapshot-a"}`),
		[]byte(`{"runtime_arch":"x86_64","runtime_abi":"helmr.firecracker.snapshot.v0","kernel_digest":"sha256:kernel","rootfs_digest":"sha256:rootfs","cni_profile":"helmr/v1"}`),
	)
}

func upsertTestWorkerHostWithRuntime(t *testing.T, ctx context.Context, queries *db.Queries, orgID, workerPoolID pgtype.UUID, externalID, region string, labels, heartbeat []byte) db.WorkerHost {
	t.Helper()
	host, err := queries.UpsertWorkerHostHeartbeat(ctx, db.UpsertWorkerHostHeartbeatParams{
		ID:                      ids.ToPG(ids.New()),
		OrgID:                   orgID,
		WorkerPoolID:            workerPoolID,
		ExternalID:              externalID,
		Region:                  region,
		TotalMilliCpu:           4000,
		TotalMemoryMib:          8192,
		TotalDiskMib:            20480,
		TotalExecutionSlots:     4,
		AvailableMilliCpu:       4000,
		AvailableMemoryMib:      8192,
		AvailableDiskMib:        20480,
		AvailableExecutionSlots: 4,
		Labels:                  labels,
		Heartbeat:               heartbeat,
	})
	if err != nil {
		t.Fatal(err)
	}
	return host
}

func publishTestRunQueueEntry(t *testing.T, ctx context.Context, queries *db.Queries, orgID, runID pgtype.UUID, entry db.RunQueueEntry, queueMessageID string) db.RunQueueEntry {
	t.Helper()
	published, err := queries.MarkRunQueueEntryEnqueued(ctx, db.MarkRunQueueEntryEnqueuedParams{
		OrgID:                      orgID,
		RunID:                      runID,
		QueueMessageID:             pgText(queueMessageID),
		ExpectedDispatchGeneration: entry.DispatchGeneration,
	})
	if err != nil {
		t.Fatal(err)
	}
	return published
}

func seedComputeDispatchRun(t *testing.T, ctx context.Context, pool *pgxpool.Pool, orgID, projectID, environmentID pgtype.UUID) pgtype.UUID {
	t.Helper()
	return seedComputeDispatchRunWithResources(t, ctx, pool, orgID, projectID, environmentID, 2000, 2048)
}

func seedComputeDispatchRunWithResources(t *testing.T, ctx context.Context, pool *pgxpool.Pool, orgID, projectID, environmentID pgtype.UUID, requestedMilliCPU, requestedMemoryMiB int64) pgtype.UUID {
	t.Helper()
	runID := ids.ToPG(ids.New())
	const installationID int64 = 4242
	const repositoryID int64 = 100200

	if _, err := pool.Exec(ctx, `
INSERT INTO github_app_installations (id, org_id, installation_id, account_login, account_type)
VALUES ($1, $2, $3, 'octocat', 'User')
ON CONFLICT (org_id, installation_id) DO NOTHING
`, ids.ToPG(ids.New()), orgID, installationID); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `
INSERT INTO github_repositories (id, org_id, installation_id, github_repository_id, owner_login, name, full_name, private, archived)
VALUES ($1, $2, $3, $4, 'octocat', 'hello', 'octocat/hello', false, false)
ON CONFLICT (org_id, github_repository_id) DO NOTHING
`, ids.ToPG(ids.New()), orgID, installationID, repositoryID); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `
INSERT INTO cas_objects (digest, size_bytes, media_type)
VALUES ('sha256:compute-dispatch-test', 1, 'application/vnd.helmr.source')
ON CONFLICT (digest) DO NOTHING
`); err != nil {
		t.Fatal(err)
	}
	deploymentID, deploymentTaskID := ensureComputeDispatchDeploymentTask(t, ctx, pool, orgID, projectID, environmentID, requestedMilliCPU, requestedMemoryMiB)
	if _, err := pool.Exec(ctx, `
INSERT INTO runs (
    id,
    org_id,
    project_id,
    environment_id,
    deployment_id,
    deployment_task_id,
    task_id,
    payload,
    secret_bindings,
    workspace_repository,
    workspace_installation_id,
    workspace_github_repository_id,
    workspace_ref,
    workspace_sha,
    max_duration_seconds
) VALUES ($1, $2, $3, $4, $5, $6, 'test.task', '{}', '{}', 'octocat/hello', $7, $8, 'refs/heads/main', 'abc123', 300)
`, runID, orgID, projectID, environmentID, deploymentID, deploymentTaskID, installationID, repositoryID); err != nil {
		t.Fatal(err)
	}
	return runID
}

func ensureComputeDispatchDeploymentTask(t *testing.T, ctx context.Context, pool *pgxpool.Pool, orgID, projectID, environmentID pgtype.UUID, requestedMilliCPU, requestedMemoryMiB int64) (pgtype.UUID, pgtype.UUID) {
	t.Helper()
	var deploymentID pgtype.UUID
	var deploymentTaskID pgtype.UUID
	err := pool.QueryRow(ctx, `
SELECT deployments.id, deployment_tasks.id
  FROM deployments
  JOIN deployment_tasks ON deployment_tasks.org_id = deployments.org_id
                     AND deployment_tasks.project_id = deployments.project_id
                     AND deployment_tasks.environment_id = deployments.environment_id
                     AND deployment_tasks.deployment_id = deployments.id
 WHERE deployments.org_id = $1
   AND deployments.project_id = $2
   AND deployments.environment_id = $3
   AND deployments.status = 'deployed'
   AND deployment_tasks.task_id = 'test.task'
`, orgID, projectID, environmentID).Scan(&deploymentID, &deploymentTaskID)
	if err == nil {
		return deploymentID, deploymentTaskID
	}
	if !errors.Is(err, pgx.ErrNoRows) {
		t.Fatal(err)
	}
	err = pool.QueryRow(ctx, `
SELECT id
  FROM deployments
 WHERE org_id = $1
   AND project_id = $2
   AND environment_id = $3
   AND status = 'deployed'
`, orgID, projectID, environmentID).Scan(&deploymentID)
	if errors.Is(err, pgx.ErrNoRows) {
		deploymentID = ids.ToPG(ids.New())
		if _, err := pool.Exec(ctx, `
INSERT INTO deployments (id, org_id, project_id, environment_id, source_digest, status)
VALUES ($1, $2, $3, $4, 'sha256:compute-dispatch-test', 'deployed')
`, deploymentID, orgID, projectID, environmentID); err != nil {
			t.Fatal(err)
		}
	} else if err != nil {
		t.Fatal(err)
	}
	deploymentTaskID = ids.ToPG(ids.New())
	if _, err := pool.Exec(ctx, `
INSERT INTO deployment_tasks (id, org_id, project_id, environment_id, deployment_id, task_id, requested_milli_cpu, requested_memory_mib)
VALUES ($1, $2, $3, $4, $5, 'test.task', $6, $7)
`, deploymentTaskID, orgID, projectID, environmentID, deploymentID, requestedMilliCPU, requestedMemoryMiB); err != nil {
		t.Fatal(err)
	}
	return deploymentID, deploymentTaskID
}
