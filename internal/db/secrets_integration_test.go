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

	keyring, err := secret.NewKeyring(bytes.Repeat([]byte{1}, 32), nil)
	if err != nil {
		t.Fatal(err)
	}
	store, err := secret.New(queries, keyring)
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

func TestSecretUpsertVersionConflictReturnsNoRows(t *testing.T) {
	ctx := context.Background()
	queries, pool := newPostgresTestDB(t, ctx)
	orgID := ids.ToPG(ids.DefaultOrgID)
	scope := seedPostgresTestDefaultScope(t, ctx, pool, queries, orgID)

	inserted, err := queries.UpsertScopedSecret(ctx, db.UpsertScopedSecretParams{
		ID:              ids.ToPG(ids.New()),
		OrgID:           orgID,
		ProjectID:       scope.ProjectID,
		EnvironmentID:   scope.EnvironmentID,
		Name:            "API_TOKEN",
		Version:         1,
		KeyID:           "key-a",
		Nonce:           []byte("nonce-a"),
		Ciphertext:      []byte("ciphertext-a"),
		PreviousVersion: 0,
	})
	if err != nil {
		t.Fatal(err)
	}
	if inserted.Version != 1 {
		t.Fatalf("inserted version = %d, want 1", inserted.Version)
	}
	_, err = queries.UpsertScopedSecret(ctx, db.UpsertScopedSecretParams{
		ID:              ids.ToPG(ids.New()),
		OrgID:           orgID,
		ProjectID:       scope.ProjectID,
		EnvironmentID:   scope.EnvironmentID,
		Name:            "API_TOKEN",
		Version:         2,
		KeyID:           "key-b",
		Nonce:           []byte("nonce-b"),
		Ciphertext:      []byte("ciphertext-b"),
		PreviousVersion: 0,
	})
	if !errors.Is(err, pgx.ErrNoRows) {
		t.Fatalf("stale upsert err = %v, want pgx.ErrNoRows", err)
	}
}

func TestSecretReencryptBatchRoundTrip(t *testing.T) {
	ctx := context.Background()
	queries, pool := newPostgresTestDB(t, ctx)
	orgID := ids.ToPG(ids.DefaultOrgID)
	scope := seedPostgresTestDefaultScope(t, ctx, pool, queries, orgID)
	orgUUID := ids.MustFromPG(orgID)
	projectUUID := ids.MustFromPG(scope.ProjectID)
	environmentUUID := ids.MustFromPG(scope.EnvironmentID)

	oldKey := bytes.Repeat([]byte{1}, 32)
	currentKey := bytes.Repeat([]byte{2}, 32)
	oldKeyring, err := secret.NewKeyring(oldKey, nil)
	if err != nil {
		t.Fatal(err)
	}
	oldStore, err := secret.New(queries, oldKeyring)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := oldStore.PutScoped(ctx, orgUUID, projectUUID, environmentUUID, "API_TOKEN", []byte("secret-value")); err != nil {
		t.Fatal(err)
	}

	rotatingKeyring, err := secret.NewKeyring(currentKey, oldKey)
	if err != nil {
		t.Fatal(err)
	}
	rotatingStore, err := secret.New(queries, rotatingKeyring)
	if err != nil {
		t.Fatal(err)
	}
	oldKeyID, ok := rotatingKeyring.OldKeyID()
	if !ok {
		t.Fatal("old key id missing")
	}
	result, err := rotatingStore.ReencryptBatch(ctx, oldKeyID, 10)
	if err != nil {
		t.Fatal(err)
	}
	if result.Scanned != 1 || result.Reencrypted != 1 || result.Skipped != 0 || result.Failed != 0 {
		t.Fatalf("reencrypt result = %+v", result)
	}
	count, err := rotatingStore.CountByKeyID(ctx, oldKeyID)
	if err != nil {
		t.Fatal(err)
	}
	if count != 0 {
		t.Fatalf("old key count = %d, want 0", count)
	}
	record, err := queries.GetScopedSecretByName(ctx, db.GetScopedSecretByNameParams{
		OrgID:         orgID,
		ProjectID:     scope.ProjectID,
		EnvironmentID: scope.EnvironmentID,
		Name:          "API_TOKEN",
	})
	if err != nil {
		t.Fatal(err)
	}
	if record.KeyID != rotatingKeyring.CurrentKeyID() {
		t.Fatalf("key id = %q, want current %q", record.KeyID, rotatingKeyring.CurrentKeyID())
	}
	if record.Version != 2 {
		t.Fatalf("version = %d, want 2", record.Version)
	}
	if !record.RotatedAt.Valid {
		t.Fatal("rotated_at was not set")
	}
	resolved, err := rotatingStore.ResolveScopedNames(ctx, orgUUID, projectUUID, environmentUUID, []string{"API_TOKEN"})
	if err != nil {
		t.Fatal(err)
	}
	if got := string(resolved["API_TOKEN"]); got != "secret-value" {
		t.Fatalf("resolved secret = %q, want secret-value", got)
	}
}
