package db_test

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/helmrdotdev/helmr/internal/db"
	"github.com/helmrdotdev/helmr/internal/ids"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
)

func TestDeploymentsPromoteCurrentBundleWithoutArchivingHistory(t *testing.T) {
	ctx := context.Background()
	queries, pool := newPostgresTestDB(t, ctx)
	orgID := ids.ToPG(ids.DefaultOrgID)
	scope := seedPostgresTestDefaultScope(t, ctx, pool, queries, orgID)

	firstDeploymentID := createTestDeployment(t, ctx, queries, orgID, scope.ProjectID, scope.EnvironmentID, "sha256:"+strings.Repeat("1", 64), "hello-world")
	if _, err := queries.GetCurrentDeploymentTask(ctx, db.GetCurrentDeploymentTaskParams{
		OrgID:         orgID,
		ProjectID:     scope.ProjectID,
		EnvironmentID: scope.EnvironmentID,
		TaskID:        "hello-world",
	}); err != nil {
		t.Fatal(err)
	}

	secondDeploymentID := createTestDeployment(t, ctx, queries, orgID, scope.ProjectID, scope.EnvironmentID, "sha256:"+strings.Repeat("2", 64), "cli-tooling")
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

func createTestDeployment(t *testing.T, ctx context.Context, queries *db.Queries, orgID, projectID, environmentID pgtype.UUID, digest, taskID string) pgtype.UUID {
	t.Helper()
	if _, err := queries.UpsertCasObject(ctx, db.UpsertCasObjectParams{
		Digest:    digest,
		SizeBytes: 1,
		MediaType: "application/vnd.helmr.task-source.v1.tar",
	}); err != nil {
		t.Fatal(err)
	}
	deploymentID := ids.ToPG(ids.New())
	if _, err := queries.CreateDeployment(ctx, db.CreateDeploymentParams{
		ID:            deploymentID,
		OrgID:         orgID,
		ProjectID:     projectID,
		EnvironmentID: environmentID,
		SourceDigest:  digest,
		Status:        db.DeploymentStatusCreating,
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := queries.CreateDeploymentTask(ctx, db.CreateDeploymentTaskParams{
		ID:                 ids.ToPG(ids.New()),
		OrgID:              orgID,
		ProjectID:          projectID,
		EnvironmentID:      environmentID,
		DeploymentID:       deploymentID,
		TaskID:             taskID,
		ModulePath:         "tasks/" + taskID + ".ts",
		ExportName:         "task",
		RequestedMilliCpu:  2000,
		RequestedMemoryMib: 2048,
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := queries.MarkDeploymentDeployed(ctx, db.MarkDeploymentDeployedParams{
		OrgID:         orgID,
		ProjectID:     projectID,
		EnvironmentID: environmentID,
		ID:            deploymentID,
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := queries.AssignDeploymentLabel(ctx, db.AssignDeploymentLabelParams{
		OrgID:         orgID,
		ProjectID:     projectID,
		EnvironmentID: environmentID,
		Label:         "current",
		DeploymentID:  deploymentID,
	}); err != nil {
		t.Fatal(err)
	}
	return deploymentID
}
