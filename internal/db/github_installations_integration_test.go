package db_test

import (
	"context"
	"testing"

	"github.com/helmrdotdev/helmr/internal/db"
	"github.com/helmrdotdev/helmr/internal/ids"
	"github.com/jackc/pgx/v5/pgtype"
)

func TestListGitHubInstallationsExcludesDeleted(t *testing.T) {
	ctx := context.Background()
	queries, pool := newPostgresTestDB(t, ctx)
	orgID := ids.ToPG(ids.New())
	seedPostgresTestOrganization(t, ctx, pool, orgID)

	if _, err := queries.UpsertGitHubInstallation(ctx, db.UpsertGitHubInstallationParams{
		ID:                  ids.ToPG(ids.New()),
		OrgID:               orgID,
		InstallationID:      123,
		AccountLogin:        "active-org",
		AccountType:         "Organization",
		RepositorySelection: pgtype.Text{String: "selected", Valid: true},
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := queries.UpsertGitHubInstallation(ctx, db.UpsertGitHubInstallationParams{
		ID:                  ids.ToPG(ids.New()),
		OrgID:               orgID,
		InstallationID:      456,
		AccountLogin:        "deleted-org",
		AccountType:         "Organization",
		RepositorySelection: pgtype.Text{String: "selected", Valid: true},
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := queries.DeleteGitHubInstallation(ctx, db.DeleteGitHubInstallationParams{
		OrgID:          orgID,
		InstallationID: 456,
	}); err != nil {
		t.Fatal(err)
	}

	installations, err := queries.ListGitHubInstallations(ctx, orgID)
	if err != nil {
		t.Fatal(err)
	}
	if len(installations) != 1 {
		t.Fatalf("installations length = %d, want 1: %+v", len(installations), installations)
	}
	if installations[0].InstallationID != 123 {
		t.Fatalf("installation id = %d, want 123", installations[0].InstallationID)
	}
}
