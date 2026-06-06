package db_test

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/helmrdotdev/helmr/internal/api"
	"github.com/helmrdotdev/helmr/internal/db"
	"github.com/helmrdotdev/helmr/internal/ids"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/jackc/pgx/v5/pgxpool"
)

func TestDeploymentsPromoteCurrentBundleWithoutArchivingHistory(t *testing.T) {
	ctx := context.Background()
	queries, pool := newPostgresTestDB(t, ctx)
	orgID := ids.ToPG(ids.DefaultOrgID)
	scope := seedPostgresTestDefaultScope(t, ctx, pool, queries, orgID)

	firstDeploymentID := createTestDeployment(t, ctx, queries, pool, orgID, scope.ProjectID, scope.EnvironmentID, "sha256:"+strings.Repeat("1", 64), "hello-world")
	if _, err := queries.GetCurrentDeploymentTask(ctx, db.GetCurrentDeploymentTaskParams{
		OrgID:         orgID,
		ProjectID:     scope.ProjectID,
		EnvironmentID: scope.EnvironmentID,
		TaskID:        "hello-world",
	}); err != nil {
		t.Fatal(err)
	}

	secondDeploymentID := createTestDeployment(t, ctx, queries, pool, orgID, scope.ProjectID, scope.EnvironmentID, "sha256:"+strings.Repeat("2", 64), "cli-tooling")
	if _, err := queries.GetCurrentDeploymentTask(ctx, db.GetCurrentDeploymentTaskParams{
		OrgID:         orgID,
		ProjectID:     scope.ProjectID,
		EnvironmentID: scope.EnvironmentID,
		TaskID:        "hello-world",
	}); !errors.Is(err, pgx.ErrNoRows) {
		t.Fatalf("old task lookup error = %v, want no rows", err)
	}
	currentTask, err := queries.GetCurrentDeploymentTask(ctx, db.GetCurrentDeploymentTaskParams{
		OrgID:         orgID,
		ProjectID:     scope.ProjectID,
		EnvironmentID: scope.EnvironmentID,
		TaskID:        "cli-tooling",
	})
	if err != nil {
		t.Fatal(err)
	}
	if currentTask.DeploymentID != secondDeploymentID {
		t.Fatalf("current deployment = %v, want %v", currentTask.DeploymentID, secondDeploymentID)
	}

	firstDeployment, err := queries.GetDeployment(ctx, db.GetDeploymentParams{
		OrgID:         orgID,
		ProjectID:     scope.ProjectID,
		EnvironmentID: scope.EnvironmentID,
		ID:            firstDeploymentID,
	})
	if err != nil {
		t.Fatal(err)
	}
	if firstDeployment.Status != db.DeploymentStatusDeployed {
		t.Fatalf("first deployment status = %s, want deployed history", firstDeployment.Status)
	}
	currentDeployment, err := queries.GetCurrentDeployment(ctx, db.GetCurrentDeploymentParams{
		OrgID:         orgID,
		ProjectID:     scope.ProjectID,
		EnvironmentID: scope.EnvironmentID,
	})
	if err != nil {
		t.Fatal(err)
	}
	if currentDeployment.ID != secondDeploymentID {
		t.Fatalf("current deployment = %v, want %v", currentDeployment.ID, secondDeploymentID)
	}
}

func TestCreateDeploymentReusesReusableContentHashBuildKey(t *testing.T) {
	ctx := context.Background()
	queries, pool := newPostgresTestDB(t, ctx)
	orgID := ids.ToPG(ids.DefaultOrgID)
	scope := seedPostgresTestDefaultScope(t, ctx, pool, queries, orgID)
	digest := "sha256:" + strings.Repeat("3", 64)
	upsertTestDeploymentSource(t, ctx, queries, digest)

	firstID := ids.ToPG(ids.New())
	first, err := queries.CreateDeployment(ctx, db.CreateDeploymentParams{
		ID:                     firstID,
		OrgID:                  orgID,
		ProjectID:              scope.ProjectID,
		EnvironmentID:          scope.EnvironmentID,
		Version:                "20260101.1",
		ApiVersion:             api.CurrentAPIVersion,
		BundleFormatVersion:    api.CurrentBundleFormatVersion,
		WorkerProtocolVersion:  api.CurrentWorkerProtocolVersion,
		ContentHash:            digest,
		DeploymentSourceDigest: digest,
		Status:                 db.DeploymentStatusQueued,
	})
	if err != nil {
		t.Fatal(err)
	}
	second, err := queries.GetReusableDeploymentByContentHash(ctx, db.GetReusableDeploymentByContentHashParams{
		OrgID:         orgID,
		ProjectID:     scope.ProjectID,
		EnvironmentID: scope.EnvironmentID,
		ContentHash:   digest,
	})
	if err != nil {
		t.Fatal(err)
	}
	if second.ID != first.ID {
		t.Fatalf("second deployment = %v, want reused %v", second.ID, first.ID)
	}

	worker := upsertTestWorkerInstance(t, ctx, queries, "deployment-builder")
	lease, err := queries.LeaseQueuedDeploymentBuild(ctx, db.LeaseQueuedDeploymentBuildParams{
		BuildLeaseID:          pgtype.Text{String: "lease-1", Valid: true},
		BuildWorkerInstanceID: worker.ID,
		BuildLeaseExpiresAt:   pgtype.Timestamptz{Time: time.Now().Add(time.Hour), Valid: true},
	})
	if err != nil {
		t.Fatal(err)
	}
	if lease.ID != firstID {
		t.Fatalf("leased deployment = %v, want %v", lease.ID, firstID)
	}
	if _, err := queries.LeaseQueuedDeploymentBuild(ctx, db.LeaseQueuedDeploymentBuildParams{
		BuildLeaseID:          pgtype.Text{String: "lease-2", Valid: true},
		BuildWorkerInstanceID: worker.ID,
		BuildLeaseExpiresAt:   pgtype.Timestamptz{Time: time.Now().Add(time.Hour), Valid: true},
	}); !errors.Is(err, pgx.ErrNoRows) {
		t.Fatalf("second lease error = %v, want no rows", err)
	}
}

func TestCreateDeploymentRetriesFailedContentHashBuild(t *testing.T) {
	ctx := context.Background()
	queries, pool := newPostgresTestDB(t, ctx)
	orgID := ids.ToPG(ids.DefaultOrgID)
	scope := seedPostgresTestDefaultScope(t, ctx, pool, queries, orgID)
	digest := "sha256:" + strings.Repeat("4", 64)
	upsertTestDeploymentSource(t, ctx, queries, digest)

	failedID := ids.ToPG(ids.New())
	if _, err := queries.CreateDeployment(ctx, db.CreateDeploymentParams{
		ID:                     failedID,
		OrgID:                  orgID,
		ProjectID:              scope.ProjectID,
		EnvironmentID:          scope.EnvironmentID,
		Version:                "20260101.1",
		ApiVersion:             api.CurrentAPIVersion,
		BundleFormatVersion:    api.CurrentBundleFormatVersion,
		WorkerProtocolVersion:  api.CurrentWorkerProtocolVersion,
		ContentHash:            digest,
		DeploymentSourceDigest: digest,
		Status:                 db.DeploymentStatusQueued,
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `
UPDATE deployments
   SET status = 'failed',
       failure = '{"message":"boom"}'::jsonb,
       failed_at = now()
 WHERE org_id = $1
   AND project_id = $2
   AND environment_id = $3
   AND id = $4
`, orgID, scope.ProjectID, scope.EnvironmentID, failedID); err != nil {
		t.Fatal(err)
	}

	retryID := ids.ToPG(ids.New())
	retry, err := queries.CreateDeployment(ctx, db.CreateDeploymentParams{
		ID:                     retryID,
		OrgID:                  orgID,
		ProjectID:              scope.ProjectID,
		EnvironmentID:          scope.EnvironmentID,
		Version:                "20260101.2",
		ApiVersion:             api.CurrentAPIVersion,
		BundleFormatVersion:    api.CurrentBundleFormatVersion,
		WorkerProtocolVersion:  api.CurrentWorkerProtocolVersion,
		ContentHash:            digest,
		DeploymentSourceDigest: digest,
		Status:                 db.DeploymentStatusQueued,
	})
	if err != nil {
		t.Fatal(err)
	}
	if retry.ID != retryID {
		t.Fatalf("retry deployment = %v, want new queued %v", retry.ID, retryID)
	}
	if retry.Status != db.DeploymentStatusQueued {
		t.Fatalf("retry status = %s, want queued", retry.Status)
	}
}

func TestCreateDeploymentDoesNotReuseDeployedContentHashBuild(t *testing.T) {
	ctx := context.Background()
	queries, pool := newPostgresTestDB(t, ctx)
	orgID := ids.ToPG(ids.DefaultOrgID)
	scope := seedPostgresTestDefaultScope(t, ctx, pool, queries, orgID)
	digest := "sha256:" + strings.Repeat("5", 64)

	deployedID := createTestDeployment(t, ctx, queries, pool, orgID, scope.ProjectID, scope.EnvironmentID, digest, "ship")
	if _, err := queries.GetReusableDeploymentByContentHash(ctx, db.GetReusableDeploymentByContentHashParams{
		OrgID:         orgID,
		ProjectID:     scope.ProjectID,
		EnvironmentID: scope.EnvironmentID,
		ContentHash:   digest,
	}); !errors.Is(err, pgx.ErrNoRows) {
		t.Fatalf("reusable deployed deployment error = %v, want no rows for %v", err, deployedID)
	}
}

func createTestDeployment(t *testing.T, ctx context.Context, queries *db.Queries, pool *pgxpool.Pool, orgID, projectID, environmentID pgtype.UUID, digest, taskID string) pgtype.UUID {
	t.Helper()
	upsertTestDeploymentSource(t, ctx, queries, digest)
	deploymentID := ids.ToPG(ids.New())
	if _, err := queries.CreateDeployment(ctx, db.CreateDeploymentParams{
		ID:                     deploymentID,
		OrgID:                  orgID,
		ProjectID:              projectID,
		EnvironmentID:          environmentID,
		Version:                ids.MustFromPG(deploymentID).String(),
		ApiVersion:             api.CurrentAPIVersion,
		BundleFormatVersion:    api.CurrentBundleFormatVersion,
		WorkerProtocolVersion:  api.CurrentWorkerProtocolVersion,
		ContentHash:            digest,
		DeploymentSourceDigest: digest,
		Status:                 db.DeploymentStatusQueued,
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := queries.CreateDeploymentTask(ctx, db.CreateDeploymentTaskParams{
		ID:                   ids.ToPG(ids.New()),
		OrgID:                orgID,
		ProjectID:            projectID,
		EnvironmentID:        environmentID,
		DeploymentID:         deploymentID,
		TaskID:               taskID,
		FilePath:             "tasks/" + taskID + ".ts",
		ExportName:           "task",
		HandlerEntrypoint:    "tasks/" + taskID + ".ts#task",
		BundleDigest:         digest,
		BundleFormatVersion:  api.CurrentBundleFormatVersion,
		RequestedMilliCpu:    2000,
		RequestedMemoryMib:   2048,
		SecretDeclarations:   []byte("[]"),
		ResourceRequirements: []byte("{}"),
		QueueName:            "task/" + taskID,
		MaxDurationSeconds:   300,
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `
UPDATE deployments
   SET status = 'deployed',
       build_manifest_digest = $1,
       deployment_manifest_digest = $1,
       building_at = now(),
       built_at = now(),
       deployed_at = now()
 WHERE org_id = $2
   AND project_id = $3
   AND environment_id = $4
   AND id = $5
`, digest, orgID, projectID, environmentID, deploymentID); err != nil {
		t.Fatal(err)
	}
	if _, err := queries.PromoteDeployment(ctx, db.PromoteDeploymentParams{
		ID:            ids.ToPG(ids.New()),
		OrgID:         orgID,
		ProjectID:     projectID,
		EnvironmentID: environmentID,
		DeploymentID:  deploymentID,
		Reason:        "test",
	}); err != nil {
		t.Fatal(err)
	}
	return deploymentID
}

func upsertTestDeploymentSource(t *testing.T, ctx context.Context, queries *db.Queries, digest string) {
	t.Helper()
	if _, err := queries.UpsertCasObject(ctx, db.UpsertCasObjectParams{
		Digest:    digest,
		SizeBytes: 1,
		MediaType: "application/vnd.helmr.deployment-source.v0.tar",
	}); err != nil {
		t.Fatal(err)
	}
}
