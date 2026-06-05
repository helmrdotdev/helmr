package db_test

import (
	"bytes"
	"context"
	"errors"
	"testing"

	"github.com/helmrdotdev/helmr/internal/db"
	"github.com/helmrdotdev/helmr/internal/ids"
	"github.com/helmrdotdev/helmr/internal/secret"
	"github.com/jackc/pgx/v5"
)

func TestSecretCanBeSetAfterDelete(t *testing.T) {
	ctx := context.Background()
	queries, pool := newPostgresTestDB(t, ctx)
	orgID := ids.ToPG(ids.DefaultOrgID)
	scope := seedPostgresTestDefaultScope(t, ctx, pool, queries, orgID)
	orgUUID := ids.MustFromPG(orgID)
	projectUUID := ids.MustFromPG(scope.ProjectID)
	environmentUUID := ids.MustFromPG(scope.EnvironmentID)

	store, err := secret.New(queries, secret.DefaultKeyID, bytes.Repeat([]byte{1}, 32))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.PutScoped(ctx, orgUUID, projectUUID, environmentUUID, "API_TOKEN", []byte("first")); err != nil {
		t.Fatal(err)
	}
	rows, err := queries.DeleteScopedSecret(ctx, db.DeleteScopedSecretParams{
		OrgID:         orgID,
		ProjectID:     scope.ProjectID,
		EnvironmentID: scope.EnvironmentID,
		Name:          "API_TOKEN",
	})
	if err != nil {
		t.Fatal(err)
	}
	if rows != 1 {
		t.Fatalf("deleted rows = %d, want 1", rows)
	}
	if _, err := queries.GetScopedSecretMetadataByName(ctx, db.GetScopedSecretMetadataByNameParams{
		OrgID:         orgID,
		ProjectID:     scope.ProjectID,
		EnvironmentID: scope.EnvironmentID,
		Name:          "API_TOKEN",
	}); !errors.Is(err, pgx.ErrNoRows) {
		t.Fatalf("metadata after delete err = %v, want pgx.ErrNoRows", err)
	}

	if _, err := store.PutScoped(ctx, orgUUID, projectUUID, environmentUUID, "API_TOKEN", []byte("second")); err != nil {
		t.Fatal(err)
	}
	resolved, err := store.ResolveScopedNames(ctx, orgUUID, projectUUID, environmentUUID, []string{"API_TOKEN"})
	if err != nil {
		t.Fatal(err)
	}
	if got := string(resolved["API_TOKEN"]); got != "second" {
		t.Fatalf("resolved secret = %q, want second", got)
	}
}
