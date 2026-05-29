package db_test

import (
	"context"
	"errors"
	"testing"

	"github.com/helmrdotdev/helmr/internal/db"
	"github.com/helmrdotdev/helmr/internal/ids"
	"github.com/jackc/pgx/v5"
)

func TestArchivedProjectSlugCanBeReused(t *testing.T) {
	ctx := context.Background()
	queries, pool := newPostgresTestDB(t, ctx)
	orgID := ids.ToPG(ids.DefaultOrgID)
	seedPostgresTestDefaultScope(t, ctx, pool, queries, orgID)

	projectID := ids.ToPG(ids.New())
	if _, err := queries.CreateProjectWithDefaultEnvironment(ctx, db.CreateProjectWithDefaultEnvironmentParams{
		ID:            projectID,
		OrgID:         orgID,
		Slug:          "reusable",
		Name:          "Reusable",
		EnvironmentID: ids.ToPG(ids.New()),
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := queries.ArchiveProjectWithEnvironments(ctx, db.ArchiveProjectWithEnvironmentsParams{
		OrgID: orgID,
		ID:    projectID,
	}); err != nil {
		t.Fatal(err)
	}
	recreated, err := queries.CreateProjectWithDefaultEnvironment(ctx, db.CreateProjectWithDefaultEnvironmentParams{
		ID:            ids.ToPG(ids.New()),
		OrgID:         orgID,
		Slug:          "reusable",
		Name:          "Reusable Again",
		EnvironmentID: ids.ToPG(ids.New()),
	})
	if err != nil {
		t.Fatal(err)
	}
	if recreated.Slug != "reusable" {
		t.Fatalf("project slug = %q", recreated.Slug)
	}
}

func TestArchivedEnvironmentSlugCanBeReused(t *testing.T) {
	ctx := context.Background()
	queries, pool := newPostgresTestDB(t, ctx)
	orgID := ids.ToPG(ids.DefaultOrgID)
	scope := seedPostgresTestDefaultScope(t, ctx, pool, queries, orgID)

	environmentID := ids.ToPG(ids.New())
	if _, err := queries.CreateEnvironment(ctx, db.CreateEnvironmentParams{
		ID:        environmentID,
		OrgID:     orgID,
		ProjectID: scope.ProjectID,
		Slug:      "qa",
		Name:      "QA",
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := queries.ArchiveEnvironment(ctx, db.ArchiveEnvironmentParams{
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
	})
	if err != nil {
		t.Fatal(err)
	}
	if recreated.Slug != "qa" {
		t.Fatalf("environment slug = %q", recreated.Slug)
	}
}

func TestArchiveProjectRequiresAnotherActiveProjectInSQL(t *testing.T) {
	ctx := context.Background()
	queries, pool := newPostgresTestDB(t, ctx)
	orgID := ids.ToPG(ids.DefaultOrgID)
	seedPostgresTestOrganization(t, ctx, pool, orgID)

	projectID := ids.ToPG(ids.New())
	if _, err := queries.CreateProjectWithDefaultEnvironment(ctx, db.CreateProjectWithDefaultEnvironmentParams{
		ID:            projectID,
		OrgID:         orgID,
		Slug:          "only",
		Name:          "Only",
		EnvironmentID: ids.ToPG(ids.New()),
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `UPDATE projects SET is_default = false WHERE org_id = $1 AND id = $2`, orgID, projectID); err != nil {
		t.Fatal(err)
	}
	_, err := queries.ArchiveProjectWithEnvironments(ctx, db.ArchiveProjectWithEnvironmentsParams{OrgID: orgID, ID: projectID})
	if !errors.Is(err, pgx.ErrNoRows) {
		t.Fatalf("archive only active project error = %v, want no rows", err)
	}
}

func TestArchiveEnvironmentRequiresAnotherActiveEnvironmentInSQL(t *testing.T) {
	ctx := context.Background()
	queries, pool := newPostgresTestDB(t, ctx)
	orgID := ids.ToPG(ids.DefaultOrgID)
	scope := seedPostgresTestDefaultScope(t, ctx, pool, queries, orgID)
	if _, err := pool.Exec(ctx, `UPDATE environments SET is_default = false WHERE org_id = $1 AND project_id = $2 AND id = $3`, orgID, scope.ProjectID, scope.EnvironmentID); err != nil {
		t.Fatal(err)
	}
	_, err := queries.ArchiveEnvironment(ctx, db.ArchiveEnvironmentParams{OrgID: orgID, ProjectID: scope.ProjectID, ID: scope.EnvironmentID})
	if !errors.Is(err, pgx.ErrNoRows) {
		t.Fatalf("archive only active environment error = %v, want no rows", err)
	}
}
