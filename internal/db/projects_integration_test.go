package db_test

import (
	"context"
	"errors"
	"testing"

	"github.com/helmrdotdev/helmr/internal/db"
	"github.com/helmrdotdev/helmr/internal/ids"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
)

func TestDeletedProjectSlugCanBeReused(t *testing.T) {
	ctx := context.Background()
	queries, pool := newPostgresTestDB(t, ctx)
	orgID := ids.ToPG(ids.DefaultOrgID)
	seedPostgresTestConfiguredScope(t, ctx, pool, queries, orgID)

	projectID := ids.ToPG(ids.New())
	if _, err := queries.CreateProjectWithDefaultEnvironment(ctx, db.CreateProjectWithDefaultEnvironmentParams{
		ID:                   projectID,
		OrgID:                orgID,
		Slug:                 "reusable",
		Name:                 "Reusable",
		EnvironmentID:        ids.ToPG(ids.New()),
		StagingEnvironmentID: ids.ToPG(ids.New()),
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := queries.DeleteProject(ctx, db.DeleteProjectParams{
		OrgID: orgID,
		ID:    projectID,
	}); err != nil {
		t.Fatal(err)
	}
	recreated, err := queries.CreateProjectWithDefaultEnvironment(ctx, db.CreateProjectWithDefaultEnvironmentParams{
		ID:                   ids.ToPG(ids.New()),
		OrgID:                orgID,
		Slug:                 "reusable",
		Name:                 "Reusable Again",
		EnvironmentID:        ids.ToPG(ids.New()),
		StagingEnvironmentID: ids.ToPG(ids.New()),
	})
	if err != nil {
		t.Fatal(err)
	}
	if recreated.Slug != "reusable" {
		t.Fatalf("project slug = %q", recreated.Slug)
	}
}

func TestDeletedEnvironmentSlugCanBeReused(t *testing.T) {
	ctx := context.Background()
	queries, pool := newPostgresTestDB(t, ctx)
	orgID := ids.ToPG(ids.DefaultOrgID)
	scope := seedPostgresTestConfiguredScope(t, ctx, pool, queries, orgID)

	environmentID := ids.ToPG(ids.New())
	if _, err := queries.CreateEnvironment(ctx, db.CreateEnvironmentParams{
		ID:        environmentID,
		OrgID:     orgID,
		ProjectID: scope.ProjectID,
		Slug:      "qa",
		Name:      "QA",
		ColorHex:  "#F59E0B",
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := queries.DeleteEnvironment(ctx, db.DeleteEnvironmentParams{
		OrgID:     orgID,
		ProjectID: scope.ProjectID,
		ID:        environmentID,
	}); err != nil {
		t.Fatal(err)
	}
	recreated, err := queries.CreateEnvironment(ctx, db.CreateEnvironmentParams{
		ID:        ids.ToPG(ids.New()),
		OrgID:     orgID,
		ProjectID: scope.ProjectID,
		Slug:      "qa",
		Name:      "QA Again",
		ColorHex:  "#F59E0B",
	})
	if err != nil {
		t.Fatal(err)
	}
	if recreated.Slug != "qa" {
		t.Fatalf("environment slug = %q", recreated.Slug)
	}
}

func TestCreateProjectWithDefaultEnvironmentCreatesProductionAndStaging(t *testing.T) {
	ctx := context.Background()
	queries, pool := newPostgresTestDB(t, ctx)
	orgID := ids.ToPG(ids.DefaultOrgID)
	seedPostgresTestOrganization(t, ctx, pool, orgID)
	projectID := ids.ToPG(ids.New())
	productionID := ids.ToPG(ids.New())
	stagingID := ids.ToPG(ids.New())

	if _, err := queries.CreateProjectWithDefaultEnvironment(ctx, db.CreateProjectWithDefaultEnvironmentParams{
		ID:                   projectID,
		OrgID:                orgID,
		Slug:                 "dual-env",
		Name:                 "Dual Env",
		EnvironmentID:        productionID,
		StagingEnvironmentID: stagingID,
	}); err != nil {
		t.Fatal(err)
	}

	environments, err := queries.ListEnvironments(ctx, db.ListEnvironmentsParams{OrgID: orgID, ProjectID: projectID})
	if err != nil {
		t.Fatal(err)
	}
	if len(environments) != 2 {
		t.Fatalf("environment count = %d, want 2", len(environments))
	}
	if environments[0].Slug != "production" || !environments[0].IsDefault {
		t.Fatalf("first environment = %+v, want default production", environments[0])
	}
	if environments[0].ColorHex != "#315FCE" {
		t.Fatalf("production color = %q, want #315FCE", environments[0].ColorHex)
	}
	if environments[1].Slug != "staging" || environments[1].IsDefault {
		t.Fatalf("second environment = %+v, want non-default staging", environments[1])
	}
	if environments[1].ColorHex != "#F59E0B" {
		t.Fatalf("staging color = %q, want #F59E0B", environments[1].ColorHex)
	}
}

func TestDeleteProjectAllowsOnlyProjectInSQL(t *testing.T) {
	ctx := context.Background()
	queries, pool := newPostgresTestDB(t, ctx)
	orgID := ids.ToPG(ids.DefaultOrgID)
	seedPostgresTestOrganization(t, ctx, pool, orgID)

	projectID := ids.ToPG(ids.New())
	if _, err := queries.CreateProjectWithDefaultEnvironment(ctx, db.CreateProjectWithDefaultEnvironmentParams{
		ID:                   projectID,
		OrgID:                orgID,
		Slug:                 "only",
		Name:                 "Only",
		EnvironmentID:        ids.ToPG(ids.New()),
		StagingEnvironmentID: ids.ToPG(ids.New()),
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := queries.DeleteProject(ctx, db.DeleteProjectParams{OrgID: orgID, ID: projectID}); err != nil {
		t.Fatal(err)
	}
	_, err := queries.GetProject(ctx, db.GetProjectParams{OrgID: orgID, ID: projectID})
	if !errors.Is(err, pgx.ErrNoRows) {
		t.Fatalf("deleted project lookup error = %v, want no rows", err)
	}
}

func TestDeleteEnvironmentProtectsProductionAndStagingInSQL(t *testing.T) {
	ctx := context.Background()
	queries, pool := newPostgresTestDB(t, ctx)
	orgID := ids.ToPG(ids.DefaultOrgID)
	scope := seedPostgresTestConfiguredScope(t, ctx, pool, queries, orgID)

	_, err := queries.DeleteEnvironment(ctx, db.DeleteEnvironmentParams{OrgID: orgID, ProjectID: scope.ProjectID, ID: scope.EnvironmentID})
	if !errors.Is(err, pgx.ErrNoRows) {
		t.Fatalf("delete production environment error = %v, want no rows", err)
	}

	staging, err := queries.GetEnvironmentBySlug(ctx, db.GetEnvironmentBySlugParams{
		OrgID:     orgID,
		ProjectID: scope.ProjectID,
		Slug:      "staging",
	})
	if err != nil {
		t.Fatal(err)
	}
	_, err = queries.DeleteEnvironment(ctx, db.DeleteEnvironmentParams{OrgID: orgID, ProjectID: scope.ProjectID, ID: staging.ID})
	if !errors.Is(err, pgx.ErrNoRows) {
		t.Fatalf("delete staging environment error = %v, want no rows", err)
	}
}

func TestDeleteProjectCascadesDeploymentAndRunGraph(t *testing.T) {
	ctx := context.Background()
	queries, pool := newPostgresTestDB(t, ctx)
	orgID := ids.ToPG(ids.DefaultOrgID)
	scope := seedPostgresTestConfiguredScope(t, ctx, pool, queries, orgID)
	seedComputeDispatchRun(t, ctx, pool, orgID, scope.ProjectID, scope.EnvironmentID)
	scopedRows := seedScopedDeletionRows(t, ctx, queries, orgID, scope.ProjectID, scope.EnvironmentID, "project-delete")

	if _, err := queries.DeleteProject(ctx, db.DeleteProjectParams{OrgID: orgID, ID: scope.ProjectID}); err != nil {
		t.Fatal(err)
	}

	assertNoRowsForScope(t, ctx, pool, "projects", orgID, scope.ProjectID, pgtype.UUID{})
	assertNoRowsForScope(t, ctx, pool, "environments", orgID, scope.ProjectID, scope.EnvironmentID)
	assertNoRowsForScope(t, ctx, pool, "deployments", orgID, scope.ProjectID, scope.EnvironmentID)
	assertNoRowsForScope(t, ctx, pool, "runs", orgID, scope.ProjectID, scope.EnvironmentID)
	assertNoRowsForScope(t, ctx, pool, "secrets", orgID, scope.ProjectID, scope.EnvironmentID)
	assertNoRowsByID(t, ctx, pool, "task_schedules", scopedRows.scheduleID)
	assertNoRowsByID(t, ctx, pool, "api_key_grants", scopedRows.apiKeyGrantID)
}

func TestDeleteEnvironmentCascadesDeploymentAndRunGraph(t *testing.T) {
	ctx := context.Background()
	queries, pool := newPostgresTestDB(t, ctx)
	orgID := ids.ToPG(ids.DefaultOrgID)
	scope := seedPostgresTestConfiguredScope(t, ctx, pool, queries, orgID)
	environmentID := ids.ToPG(ids.New())
	if _, err := queries.CreateEnvironment(ctx, db.CreateEnvironmentParams{
		ID:        environmentID,
		OrgID:     orgID,
		ProjectID: scope.ProjectID,
		Slug:      "qa",
		Name:      "QA",
		ColorHex:  "#F59E0B",
	}); err != nil {
		t.Fatal(err)
	}
	seedComputeDispatchRun(t, ctx, pool, orgID, scope.ProjectID, environmentID)
	scopedRows := seedScopedDeletionRows(t, ctx, queries, orgID, scope.ProjectID, environmentID, "environment-delete")

	if _, err := queries.DeleteEnvironment(ctx, db.DeleteEnvironmentParams{
		OrgID:     orgID,
		ProjectID: scope.ProjectID,
		ID:        environmentID,
	}); err != nil {
		t.Fatal(err)
	}

	assertNoRowsForScope(t, ctx, pool, "environments", orgID, scope.ProjectID, environmentID)
	assertNoRowsForScope(t, ctx, pool, "deployments", orgID, scope.ProjectID, environmentID)
	assertNoRowsForScope(t, ctx, pool, "runs", orgID, scope.ProjectID, environmentID)
	assertNoRowsForScope(t, ctx, pool, "secrets", orgID, scope.ProjectID, environmentID)
	assertNoRowsByID(t, ctx, pool, "task_schedules", scopedRows.scheduleID)
	assertNoRowsByID(t, ctx, pool, "api_key_grants", scopedRows.apiKeyGrantID)
}

type scopedDeletionRows struct {
	scheduleID    pgtype.UUID
	apiKeyGrantID pgtype.UUID
}

func seedScopedDeletionRows(t *testing.T, ctx context.Context, queries *db.Queries, orgID, projectID, environmentID pgtype.UUID, suffix string) scopedDeletionRows {
	t.Helper()
	if _, err := queries.UpsertScopedSecret(ctx, db.UpsertScopedSecretParams{
		ID:              ids.ToPG(ids.New()),
		OrgID:           orgID,
		ProjectID:       projectID,
		EnvironmentID:   environmentID,
		Name:            "delete-" + suffix,
		Version:         1,
		KeyID:           "test-key",
		Nonce:           []byte("nonce"),
		Ciphertext:      []byte("ciphertext"),
		PreviousVersion: 0,
	}); err != nil {
		t.Fatal(err)
	}
	scheduleID := ids.ToPG(ids.New())
	if _, err := queries.CreateSchedule(ctx, db.CreateScheduleParams{
		ScheduleID:    scheduleID,
		OrgID:         orgID,
		ProjectID:     projectID,
		ScheduleType:  db.TaskScheduleTypeImperative,
		TaskID:        "delete-" + suffix,
		DedupKey:      "delete-" + suffix,
		Cron:          "0 1 * * *",
		Timezone:      "UTC",
		RunOptions:    []byte(`{}`),
		Active:        true,
		InstanceID:    ids.ToPG(ids.New()),
		EnvironmentID: environmentID,
	}); err != nil {
		t.Fatal(err)
	}
	apiKey, err := queries.IssueAPIKey(ctx, db.IssueAPIKeyParams{
		ID:            ids.ToPG(ids.New()),
		OrgID:         orgID,
		ProjectID:     projectID,
		EnvironmentID: environmentID,
		Role:          db.OrgMemberRoleDeveloper,
		Name:          "delete-" + suffix,
		KeyPrefix:     "helmr_test_" + suffix,
		TokenHash:     []byte("token-" + suffix),
	})
	if err != nil {
		t.Fatal(err)
	}
	grant, err := queries.CreateAPIKeyGrant(ctx, db.CreateAPIKeyGrantParams{
		ID:         ids.ToPG(ids.New()),
		OrgID:      orgID,
		ApiKeyID:   apiKey.ID,
		Permission: "runs:read",
	})
	if err != nil {
		t.Fatal(err)
	}
	return scopedDeletionRows{scheduleID: scheduleID, apiKeyGrantID: grant.ID}
}

func assertNoRowsForScope(t *testing.T, ctx context.Context, pool interface {
	QueryRow(context.Context, string, ...any) pgx.Row
}, table string, orgID, projectID, environmentID pgtype.UUID) {
	t.Helper()
	var count int
	var err error
	switch table {
	case "projects":
		err = pool.QueryRow(ctx, "SELECT count(*)::int FROM "+table+" WHERE org_id = $1 AND id = $2", orgID, projectID).Scan(&count)
	case "environments":
		err = pool.QueryRow(ctx, "SELECT count(*)::int FROM "+table+" WHERE org_id = $1 AND project_id = $2 AND id = $3", orgID, projectID, environmentID).Scan(&count)
	default:
		err = pool.QueryRow(ctx, "SELECT count(*)::int FROM "+table+" WHERE org_id = $1 AND project_id = $2 AND environment_id = $3", orgID, projectID, environmentID).Scan(&count)
	}
	if err != nil {
		t.Fatal(err)
	}
	if count != 0 {
		t.Fatalf("%s rows for deleted scope = %d", table, count)
	}
}

func assertNoRowsByID(t *testing.T, ctx context.Context, pool interface {
	QueryRow(context.Context, string, ...any) pgx.Row
}, table string, id pgtype.UUID) {
	t.Helper()
	var count int
	if err := pool.QueryRow(ctx, "SELECT count(*)::int FROM "+table+" WHERE id = $1", id).Scan(&count); err != nil {
		t.Fatal(err)
	}
	if count != 0 {
		t.Fatalf("%s rows for deleted id = %d", table, count)
	}
}
