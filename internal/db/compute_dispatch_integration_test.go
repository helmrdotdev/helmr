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

	groupA := createTestWorkerGroup(t, ctx, queries, orgID, scope.ProjectID, scope.EnvironmentID, "group-a", "queue-a")
	groupB := createTestWorkerGroup(t, ctx, queries, orgID, scope.ProjectID, scope.EnvironmentID, "group-b", "queue-b")
	hostA := upsertTestWorkerHost(t, ctx, queries, orgID, groupA.ID, "host-a")
	hostB := upsertTestWorkerHost(t, ctx, queries, orgID, groupB.ID, "host-b")

	runID := seedComputeDispatchRun(t, ctx, pool, orgID, scope.ProjectID, scope.EnvironmentID)
	if _, err := queries.UpsertRunRequirements(ctx, db.UpsertRunRequirementsParams{
		RunID:                   runID,
		OrgID:                   orgID,
		WorkerGroupID:           groupA.ID,
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
		WorkerGroupID:  groupA.ID,
		Priority:       0,
		QueueName:      groupB.QueueName,
		QueueMessageID: pgText("redis-wrong-queue"),
	}); err == nil {
		t.Fatal("expected dispatch row with mismatched worker group queue to fail")
	}

	dispatch, err := queries.UpsertRunQueueEntryQueued(ctx, db.UpsertRunQueueEntryQueuedParams{
		RunID:          runID,
		OrgID:          orgID,
		WorkerGroupID:  groupA.ID,
		Priority:       10,
		QueueName:      groupA.QueueName,
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
		WorkerGroupID:        groupA.ID,
		WorkerHostID:         hostB.ID,
		QueueMessageID:       pgText("redis-message-1"),
		ReservationExpiresAt: pgtype.Timestamptz{Time: time.Now().Add(time.Minute), Valid: true},
	}); err == nil {
		t.Fatal("expected queue lease to a host from another worker group to fail")
	}

	if _, err := queries.ReserveRunQueueEntry(ctx, db.ReserveRunQueueEntryParams{
		OrgID:                orgID,
		RunID:                runID,
		WorkerGroupID:        groupA.ID,
		WorkerHostID:         hostA.ID,
		QueueMessageID:       pgText("redis-message-stale"),
		ReservationExpiresAt: pgtype.Timestamptz{Time: time.Now().Add(time.Minute), Valid: true},
	}); !errors.Is(err, pgx.ErrNoRows) {
		t.Fatalf("stale redis message lease err = %v, want no rows", err)
	}

	leased, err := queries.ReserveRunQueueEntry(ctx, db.ReserveRunQueueEntryParams{
		OrgID:                orgID,
		RunID:                runID,
		WorkerGroupID:        groupA.ID,
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
		WorkerGroupID:        groupA.ID,
		WorkerHostID:         hostA.ID,
		QueueMessageID:       pgText("redis-message-1"),
		ReservationExpiresAt: pgtype.Timestamptz{Time: time.Now().Add(time.Minute), Valid: true},
	}); err == nil {
		t.Fatal("expected expired queue lease renew to fail")
	}
	if _, err := queries.CompleteRunQueueEntry(ctx, db.CompleteRunQueueEntryParams{
		OrgID:          orgID,
		RunID:          runID,
		WorkerGroupID:  groupA.ID,
		WorkerHostID:   hostA.ID,
		QueueMessageID: pgText("redis-message-1"),
	}); err == nil {
		t.Fatal("expected expired queue lease ack to fail")
	}
	if _, err := queries.RequeueRunQueueEntry(ctx, db.RequeueRunQueueEntryParams{
		OrgID:          orgID,
		RunID:          runID,
		WorkerGroupID:  groupA.ID,
		WorkerHostID:   hostA.ID,
		QueueMessageID: pgText("redis-message-1"),
		LastError:      "expired",
	}); err == nil {
		t.Fatal("expected expired queue lease nack to fail")
	}
}

func TestArchiveWorkerGroupAllowsSlugAndQueueReuse(t *testing.T) {
	ctx := context.Background()
	queries, pool := newPostgresTestDB(t, ctx)
	orgID := ids.ToPG(ids.DefaultOrgID)

	scope := seedPostgresTestDefaultScope(t, ctx, pool, queries, orgID)

	group := createTestWorkerGroup(t, ctx, queries, orgID, scope.ProjectID, scope.EnvironmentID, "reuse", "queue-reuse")
	if _, err := queries.ArchiveWorkerGroup(ctx, db.ArchiveWorkerGroupParams{
		OrgID: orgID,
		ID:    group.ID,
	}); err != nil {
		t.Fatal(err)
	}
	replacement := createTestWorkerGroup(t, ctx, queries, orgID, scope.ProjectID, scope.EnvironmentID, "reuse", "queue-reuse")
	if replacement.ID == group.ID {
		t.Fatalf("replacement reused archived group id: %v", replacement.ID)
	}
}

func TestPrepareQueuedRunQueueEntryBuildsRequirementsFromDeployedTask(t *testing.T) {
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
	if !prepared.WorkerGroupID.Valid || prepared.QueueName != "main/production" || prepared.Priority != 10 {
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
	host := upsertTestWorkerHost(t, ctx, queries, orgID, prepared.WorkerGroupID, "host-redis-loss")

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
		WorkerGroupID:        prepared.WorkerGroupID,
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
		WorkerGroupID:        prepared.WorkerGroupID,
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
	group := createTestWorkerGroup(t, ctx, queries, orgID, scope.ProjectID, scope.EnvironmentID, "recover", "queue-recover")
	hostA := upsertTestWorkerHost(t, ctx, queries, orgID, group.ID, "host-a")
	hostB := upsertTestWorkerHost(t, ctx, queries, orgID, group.ID, "host-b")
	runID := seedComputeDispatchRun(t, ctx, pool, orgID, scope.ProjectID, scope.EnvironmentID)
	if _, err := queries.UpsertRunRequirements(ctx, db.UpsertRunRequirementsParams{
		RunID:                   runID,
		OrgID:                   orgID,
		WorkerGroupID:           group.ID,
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
		WorkerGroupID:        group.ID,
		WorkerHostID:         hostA.ID,
		QueueMessageID:       pgText("redis-message-current"),
		ReservationExpiresAt: pgtype.Timestamptz{Time: time.Now().Add(time.Minute), Valid: true},
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := queries.ReserveRunQueueEntry(ctx, db.ReserveRunQueueEntryParams{
		OrgID:                orgID,
		RunID:                runID,
		WorkerGroupID:        group.ID,
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
		WorkerGroupID:        group.ID,
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

func TestPrepareQueuedRunQueueEntryRequiresActiveWorkerGroup(t *testing.T) {
	ctx := context.Background()
	queries, pool := newPostgresTestDB(t, ctx)
	orgID := ids.ToPG(ids.DefaultOrgID)

	scope := seedPostgresTestDefaultScope(t, ctx, pool, queries, orgID)
	groups, err := queries.ListWorkerGroupsByScope(ctx, db.ListWorkerGroupsByScopeParams{
		OrgID:         orgID,
		ProjectID:     scope.ProjectID,
		EnvironmentID: scope.EnvironmentID,
		RowLimit:      100,
	})
	if err != nil {
		t.Fatal(err)
	}
	for _, group := range groups {
		if _, err := queries.ArchiveWorkerGroup(ctx, db.ArchiveWorkerGroupParams{OrgID: orgID, ID: group.ID}); err != nil {
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

func TestProjectEnvironmentCreationCreatesDefaultWorkerGroups(t *testing.T) {
	ctx := context.Background()
	queries, pool := newPostgresTestDB(t, ctx)
	orgID := ids.ToPG(ids.DefaultOrgID)

	defaultScope := seedPostgresTestDefaultScope(t, ctx, pool, queries, orgID)
	requireCustomerManagedDefaultWorkerGroup(t, ctx, queries, orgID, defaultScope.ProjectID, defaultScope.EnvironmentID, "main/production")

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
	requireCustomerManagedDefaultWorkerGroup(t, ctx, queries, orgID, project.ID, environments[0].ID, "project-a/production")

	runID := seedComputeDispatchRun(t, ctx, pool, orgID, project.ID, environments[0].ID)
	prepared, err := queries.PrepareQueuedRunQueueEntry(ctx, db.PrepareQueuedRunQueueEntryParams{
		OrgID: orgID,
		RunID: runID,
	})
	if err != nil {
		t.Fatal(err)
	}
	if prepared.QueueName != "project-a/production" {
		t.Fatalf("prepared queue = %q", prepared.QueueName)
	}
	managedGroup := createTestWorkerGroup(t, ctx, queries, orgID, project.ID, environments[0].ID, "managed", "project-a/managed")
	runID = seedComputeDispatchRun(t, ctx, pool, orgID, project.ID, environments[0].ID)
	prepared, err = queries.PrepareQueuedRunQueueEntry(ctx, db.PrepareQueuedRunQueueEntryParams{
		OrgID: orgID,
		RunID: runID,
	})
	if err != nil {
		t.Fatal(err)
	}
	if prepared.WorkerGroupID != managedGroup.ID || prepared.QueueName != "project-a/managed" {
		t.Fatalf("prepared worker group = %v queue = %q, want %v project-a/managed", prepared.WorkerGroupID, prepared.QueueName, managedGroup.ID)
	}

	environment, err := queries.CreateEnvironmentWithDefaultWorkerGroup(ctx, db.CreateEnvironmentWithDefaultWorkerGroupParams{
		ID:        ids.ToPG(ids.New()),
		OrgID:     orgID,
		ProjectID: project.ID,
		Slug:      "preview",
		Name:      "Preview",
	})
	if err != nil {
		t.Fatal(err)
	}
	requireCustomerManagedDefaultWorkerGroup(t, ctx, queries, orgID, project.ID, environment.ID, "project-a/preview")

	runID = seedComputeDispatchRun(t, ctx, pool, orgID, project.ID, environment.ID)
	prepared, err = queries.PrepareQueuedRunQueueEntry(ctx, db.PrepareQueuedRunQueueEntryParams{
		OrgID: orgID,
		RunID: runID,
	})
	if err != nil {
		t.Fatal(err)
	}
	if prepared.QueueName != "project-a/preview" {
		t.Fatalf("prepared queue = %q", prepared.QueueName)
	}
}

func requireCustomerManagedDefaultWorkerGroup(t *testing.T, ctx context.Context, queries *db.Queries, orgID, projectID, environmentID pgtype.UUID, queueName string) db.WorkerGroup {
	t.Helper()
	groups, err := queries.ListWorkerGroupsByScope(ctx, db.ListWorkerGroupsByScopeParams{
		OrgID:         orgID,
		ProjectID:     projectID,
		EnvironmentID: environmentID,
		RowLimit:      100,
	})
	if err != nil {
		t.Fatal(err)
	}
	for _, group := range groups {
		if group.Slug != "default" {
			continue
		}
		if group.ProvisioningMode != db.WorkerGroupProvisioningModeCustomerManaged {
			t.Fatalf("default worker group provisioning mode = %q, want %q", group.ProvisioningMode, db.WorkerGroupProvisioningModeCustomerManaged)
		}
		if group.QueueName != queueName {
			t.Fatalf("default worker group queue = %q, want %q", group.QueueName, queueName)
		}
		return group
	}
	t.Fatalf("default worker group not found in groups = %+v", groups)
	return db.WorkerGroup{}
}

func createTestWorkerGroup(t *testing.T, ctx context.Context, queries *db.Queries, orgID, projectID, environmentID pgtype.UUID, slug, queueName string) db.WorkerGroup {
	t.Helper()
	group, err := queries.CreateWorkerGroup(ctx, db.CreateWorkerGroupParams{
		ID:               ids.ToPG(ids.New()),
		OrgID:            orgID,
		ProjectID:        projectID,
		EnvironmentID:    environmentID,
		Slug:             slug,
		Name:             slug,
		ProvisioningMode: db.WorkerGroupProvisioningModeHelmrManaged,
		QueueName:        queueName,
		Region:           "us-east-1",
		Capabilities:     []byte(`{}`),
		Metadata:         []byte(`{}`),
	})
	if err != nil {
		t.Fatal(err)
	}
	return group
}

func upsertTestWorkerHost(t *testing.T, ctx context.Context, queries *db.Queries, orgID, workerGroupID pgtype.UUID, externalID string) db.WorkerHost {
	t.Helper()
	return upsertTestWorkerHostWithRuntime(
		t,
		ctx,
		queries,
		orgID,
		workerGroupID,
		externalID,
		"us-east-1",
		[]byte(`{"host":"test","pool":"standard","dedicated_key":"tenant-a","snapshot_key":"snapshot-a"}`),
		[]byte(`{"runtime_arch":"x86_64","runtime_abi":"helmr.firecracker.snapshot.v0","kernel_digest":"sha256:kernel","rootfs_digest":"sha256:rootfs","cni_profile":"helmr/v1"}`),
	)
}

func upsertTestWorkerHostWithRuntime(t *testing.T, ctx context.Context, queries *db.Queries, orgID, workerGroupID pgtype.UUID, externalID, region string, labels, heartbeat []byte) db.WorkerHost {
	t.Helper()
	host, err := queries.UpsertWorkerHostHeartbeat(ctx, db.UpsertWorkerHostHeartbeatParams{
		ID:                      ids.ToPG(ids.New()),
		OrgID:                   orgID,
		WorkerGroupID:           workerGroupID,
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
	taskDeploymentID, deployedTaskID := ensureComputeDispatchDeployedTask(t, ctx, pool, orgID, projectID, environmentID, requestedMilliCPU, requestedMemoryMiB)
	if _, err := pool.Exec(ctx, `
INSERT INTO runs (
    id,
    org_id,
    project_id,
    environment_id,
    task_deployment_id,
    deployed_task_id,
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
`, runID, orgID, projectID, environmentID, taskDeploymentID, deployedTaskID, installationID, repositoryID); err != nil {
		t.Fatal(err)
	}
	return runID
}

func ensureComputeDispatchDeployedTask(t *testing.T, ctx context.Context, pool *pgxpool.Pool, orgID, projectID, environmentID pgtype.UUID, requestedMilliCPU, requestedMemoryMiB int64) (pgtype.UUID, pgtype.UUID) {
	t.Helper()
	var taskDeploymentID pgtype.UUID
	var deployedTaskID pgtype.UUID
	err := pool.QueryRow(ctx, `
SELECT task_deployments.id, deployed_tasks.id
  FROM task_deployments
  JOIN deployed_tasks ON deployed_tasks.org_id = task_deployments.org_id
                     AND deployed_tasks.project_id = task_deployments.project_id
                     AND deployed_tasks.environment_id = task_deployments.environment_id
                     AND deployed_tasks.deployment_id = task_deployments.id
 WHERE task_deployments.org_id = $1
   AND task_deployments.project_id = $2
   AND task_deployments.environment_id = $3
   AND task_deployments.status = 'active'
   AND deployed_tasks.task_id = 'test.task'
`, orgID, projectID, environmentID).Scan(&taskDeploymentID, &deployedTaskID)
	if err == nil {
		return taskDeploymentID, deployedTaskID
	}
	if !errors.Is(err, pgx.ErrNoRows) {
		t.Fatal(err)
	}
	err = pool.QueryRow(ctx, `
SELECT id
  FROM task_deployments
 WHERE org_id = $1
   AND project_id = $2
   AND environment_id = $3
   AND status = 'active'
`, orgID, projectID, environmentID).Scan(&taskDeploymentID)
	if errors.Is(err, pgx.ErrNoRows) {
		taskDeploymentID = ids.ToPG(ids.New())
		if _, err := pool.Exec(ctx, `
INSERT INTO task_deployments (id, org_id, project_id, environment_id, source_digest, status)
VALUES ($1, $2, $3, $4, 'sha256:compute-dispatch-test', 'active')
`, taskDeploymentID, orgID, projectID, environmentID); err != nil {
			t.Fatal(err)
		}
	} else if err != nil {
		t.Fatal(err)
	}
	deployedTaskID = ids.ToPG(ids.New())
	if _, err := pool.Exec(ctx, `
INSERT INTO deployed_tasks (id, org_id, project_id, environment_id, deployment_id, task_id, requested_milli_cpu, requested_memory_mib)
VALUES ($1, $2, $3, $4, $5, 'test.task', $6, $7)
`, deployedTaskID, orgID, projectID, environmentID, taskDeploymentID, requestedMilliCPU, requestedMemoryMiB); err != nil {
		t.Fatal(err)
	}
	return taskDeploymentID, deployedTaskID
}
