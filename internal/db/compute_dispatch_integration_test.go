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

func TestPrepareQueuedRunQueueItemBuildsRequirementsFromDeploymentTask(t *testing.T) {
	ctx := context.Background()
	queries, pool := newPostgresTestDB(t, ctx)
	orgID := ids.ToPG(ids.DefaultOrgID)

	scope := seedPostgresTestDefaultScope(t, ctx, pool, queries, orgID)
	runID := seedComputeDispatchRunWithResources(t, ctx, pool, orgID, scope.ProjectID, scope.EnvironmentID, 3000, 4096)

	prepared, err := queries.PrepareQueuedRunQueueItem(ctx, db.PrepareQueuedRunQueueItemParams{
		OrgID:    orgID,
		RunID:    runID,
		Priority: 10,
	})
	if err != nil {
		t.Fatal(err)
	}
	if prepared.QueueName != "default" || prepared.Priority != 10 {
		t.Fatalf("prepared dispatch = %+v", prepared)
	}
	if prepared.RequestedMilliCpu != 3000 || prepared.RequestedMemoryMib != 4096 || prepared.RequestedDiskMib != 0 || prepared.RequestedExecutionSlots != 1 {
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

	candidates, err := queries.ListQueuedRunQueueItemCandidates(ctx, db.ListQueuedRunQueueItemCandidatesParams{
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

func TestQueuedRunQueueItemWithMessageIDCanBeReenqueued(t *testing.T) {
	ctx := context.Background()
	queries, pool := newPostgresTestDB(t, ctx)
	orgID := ids.ToPG(ids.DefaultOrgID)

	scope := seedPostgresTestDefaultScope(t, ctx, pool, queries, orgID)
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

	candidates, err := queries.ListQueuedRunQueueItemCandidates(ctx, db.ListQueuedRunQueueItemCandidatesParams{
		OrgID:    orgID,
		RowLimit: 10,
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

func TestListQueueScopesReturnsEveryQueueForOrg(t *testing.T) {
	ctx := context.Background()
	queries, pool := newPostgresTestDB(t, ctx)
	orgID := ids.ToPG(ids.DefaultOrgID)

	scope := seedPostgresTestDefaultScope(t, ctx, pool, queries, orgID)
	runA := seedComputeDispatchRunWithResources(t, ctx, pool, orgID, scope.ProjectID, scope.EnvironmentID, 1000, 1024)
	runB := seedComputeDispatchRunWithResources(t, ctx, pool, orgID, scope.ProjectID, scope.EnvironmentID, 1000, 1024)
	for _, row := range []struct {
		runID pgtype.UUID
		queue string
	}{
		{runID: runA, queue: "queue-a"},
		{runID: runB, queue: "queue-b"},
	} {
		if _, err := queries.UpsertRunRuntimeRequirements(ctx, db.UpsertRunRuntimeRequirementsParams{
			RunID:                   row.runID,
			OrgID:                   orgID,
			RequestedMilliCpu:       1000,
			RequestedMemoryMib:      1024,
			RequestedDiskMib:        0,
			RequestedExecutionSlots: 1,
			NetworkPolicy:           []byte(`{}`),
			Placement:               []byte(`{}`),
		}); err != nil {
			t.Fatal(err)
		}
		if _, err := queries.UpsertRunQueueItemQueued(ctx, db.UpsertRunQueueItemQueuedParams{
			RunID:             row.runID,
			OrgID:             orgID,
			Priority:          1,
			QueueName:         row.queue,
			DispatchMessageID: pgText("message-" + row.queue),
		}); err != nil {
			t.Fatal(err)
		}
	}

	scopes, err := queries.ListQueueScopes(ctx, db.ListQueueScopesParams{
		ScanSeed: "test",
		RowLimit: 10,
	})
	if err != nil {
		t.Fatal(err)
	}
	seen := map[string]bool{}
	for _, scope := range scopes {
		if scope.OrgID == orgID {
			seen[scope.QueueName] = true
		}
	}
	if !seen["queue-a"] || !seen["queue-b"] {
		t.Fatalf("queue scopes = %+v", scopes)
	}
}

func TestRunQueueItemFencesStaleEnqueueAndRecoversExpiredLease(t *testing.T) {
	ctx := context.Background()
	queries, pool := newPostgresTestDB(t, ctx)
	orgID := ids.ToPG(ids.DefaultOrgID)

	scope := seedPostgresTestDefaultScope(t, ctx, pool, queries, orgID)
	runID := seedComputeDispatchRunWithResources(t, ctx, pool, orgID, scope.ProjectID, scope.EnvironmentID, 1000, 1024)
	instance := upsertTestWorkerInstance(t, ctx, queries, "instance-expired-lease")

	prepared, err := queries.PrepareQueuedRunQueueItem(ctx, db.PrepareQueuedRunQueueItemParams{
		OrgID:    orgID,
		RunID:    runID,
		Priority: 1,
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
	entry, err := queries.MarkRunQueueItemEnqueued(ctx, db.MarkRunQueueItemEnqueuedParams{
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
		OrgID:    orgID,
		RunID:    runID,
		Priority: entry.Priority,
	}); err != nil {
		t.Fatal(err)
	}
}

func upsertTestWorkerInstance(t *testing.T, ctx context.Context, queries *db.Queries, instanceID string) db.WorkerInstance {
	t.Helper()
	return upsertTestWorkerInstanceWithRuntime(t, ctx, queries, instanceID, "", []byte(`{}`), []byte(`{
		"runtime_arch":"x86_64",
		"runtime_abi":"helmr.firecracker.snapshot.v0",
		"kernel_digest":"sha256:kernel",
		"rootfs_digest":"sha256:rootfs",
		"cni_profile":"helmr/v0"
	}`))
}

func upsertTestWorkerInstanceWithRuntime(t *testing.T, ctx context.Context, queries *db.Queries, instanceID, region string, labels, heartbeat []byte) db.WorkerInstance {
	t.Helper()
	instance, err := queries.UpsertWorkerInstanceHeartbeat(ctx, db.UpsertWorkerInstanceHeartbeatParams{
		ID:                      ids.ToPG(ids.New()),
		ResourceID:              instanceID,
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
	return instance
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
	return seedComputeDispatchRunWithResources(t, ctx, pool, orgID, projectID, environmentID, 1000, 1024)
}

func seedComputeDispatchRunWithResources(t *testing.T, ctx context.Context, pool *pgxpool.Pool, orgID, projectID, environmentID pgtype.UUID, requestedMilliCPU, requestedMemoryMiB int64) pgtype.UUID {
	t.Helper()
	deploymentID, deploymentTaskID := ensureComputeDispatchDeploymentTask(t, ctx, pool, orgID, projectID, environmentID, requestedMilliCPU, requestedMemoryMiB)
	seedComputeDispatchGitHubSource(t, ctx, db.New(pool), orgID, projectID)
	runID := ids.ToPG(ids.New())
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
    secret_bindings,
    workspace_repository,
    workspace_installation_id,
    workspace_github_repository_id,
    workspace_ref,
    workspace_sha,
    workspace_subpath,
    workspace_ref_kind,
    workspace_ref_name,
    workspace_full_ref,
    workspace_default_branch,
    workspace_pr_number,
    workspace_pr_base_ref,
    workspace_pr_base_sha,
    workspace_pr_head_ref,
    workspace_pr_head_sha,
    max_duration_seconds
) VALUES ($1, $2, $3, $4, $5, $6, 'deploy', 'queued', '{}', '{}', 'helmrdotdev/helmr', 1, 1, 'main', 'abc123', '', '', '', '', '', NULL, '', '', '', '', 300)
`, runID, orgID, projectID, environmentID, deploymentID, deploymentTaskID); err != nil {
		t.Fatal(err)
	}
	return runID
}

func seedComputeDispatchGitHubSource(t *testing.T, ctx context.Context, queries *db.Queries, orgID, projectID pgtype.UUID) {
	t.Helper()
	if _, err := queries.UpsertGitHubInstallation(ctx, db.UpsertGitHubInstallationParams{
		ID:                  ids.ToPG(ids.New()),
		OrgID:               orgID,
		InstallationID:      1,
		AccountLogin:        "helmrdotdev",
		AccountType:         "Organization",
		RepositorySelection: pgtype.Text{String: "selected", Valid: true},
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := queries.UpsertGitHubRepository(ctx, db.UpsertGitHubRepositoryParams{
		ID:                 ids.ToPG(ids.New()),
		OrgID:              orgID,
		InstallationID:     1,
		GithubRepositoryID: 1,
		OwnerLogin:         "helmrdotdev",
		Name:               "helmr",
		FullName:           "helmrdotdev/helmr",
		DefaultBranch:      pgtype.Text{String: "main", Valid: true},
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := queries.ConnectProjectGitHubRepository(ctx, db.ConnectProjectGitHubRepositoryParams{
		ID:                 ids.ToPG(ids.New()),
		OrgID:              orgID,
		ProjectID:          projectID,
		GithubRepositoryID: 1,
	}); err != nil {
		t.Fatal(err)
	}
}

func ensureComputeDispatchDeploymentTask(t *testing.T, ctx context.Context, pool *pgxpool.Pool, orgID, projectID, environmentID pgtype.UUID, requestedMilliCPU, requestedMemoryMiB int64) (pgtype.UUID, pgtype.UUID) {
	t.Helper()
	deploymentID := ids.ToPG(ids.New())
	deploymentTaskID := ids.ToPG(ids.New())
	sourceDigest := "sha256:" + ids.New().String()
	if _, err := pool.Exec(ctx, `
INSERT INTO cas_objects (digest, size_bytes, media_type)
VALUES ($1, 1, 'application/vnd.helmr.bundle')
ON CONFLICT (digest) DO NOTHING
`, sourceDigest); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `
INSERT INTO deployments (id, org_id, project_id, environment_id, content_hash, deployment_source_digest, build_manifest_digest, deployment_manifest_digest, status, building_at, built_at, deployed_at)
VALUES ($1, $2, $3, $4, $5, $5, $5, $5, 'deployed', now(), now(), now())
`, deploymentID, orgID, projectID, environmentID, sourceDigest); err != nil {
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
    bundle_digest,
    requested_milli_cpu,
    requested_memory_mib,
    secrets_json,
    resources_json,
    max_duration_seconds
) VALUES ($1, $2, $3, $4, $5, 'deploy', 'src/task.ts', 'deploy', 'src/task.ts#deploy', $8, $6, $7, '[]', '{}', 300)
`, deploymentTaskID, orgID, projectID, environmentID, deploymentID, requestedMilliCPU, requestedMemoryMiB, sourceDigest); err != nil {
		t.Fatal(err)
	}
	return deploymentID, deploymentTaskID
}
