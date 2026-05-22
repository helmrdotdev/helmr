package db_test

import (
	"context"
	"testing"

	"github.com/helmrdotdev/helmr/internal/db"
	"github.com/helmrdotdev/helmr/internal/ids"
)

func TestArchivedProjectSlugCanBeReused(t *testing.T) {
	ctx := context.Background()
	queries, pool := newPostgresTestDB(t, ctx)
	orgID := ids.ToPG(ids.DefaultOrgID)
	seedPostgresTestOrganization(t, ctx, pool, orgID)

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
