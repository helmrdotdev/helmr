package db_test

import (
	"context"
	"testing"

	"github.com/helmrdotdev/helmr/internal/db"
	"github.com/helmrdotdev/helmr/internal/db/dbtest"
	"github.com/helmrdotdev/helmr/internal/ids"
)

func TestCreateWorkerInstanceCredentialFromBootstrapRotatesExistingHostCredential(t *testing.T) {
	ctx := context.Background()
	queries, pool := newPostgresTestDB(t, ctx)
	orgID := ids.ToPG(dbtest.DefaultOrgID)
	tokenHash := []byte("bootstrap-token-hash")
	workerInstanceID := ids.ToPG(ids.New())

	seedPostgresTestWorkerBootstrapToken(t, ctx, pool, queries, orgID, tokenHash)

	first, err := queries.CreateWorkerInstanceCredentialFromBootstrap(ctx, db.CreateWorkerInstanceCredentialFromBootstrapParams{
		BootstrapTokenHash: tokenHash,
		CredentialID:       ids.ToPG(ids.New()),
		WorkerInstanceID:   workerInstanceID,
		ResourceID:         "instance-a",
		KeyPrefix:          "first",
		SecretHash:         []byte("first-secret-hash"),
	})
	if err != nil {
		t.Fatal(err)
	}
	second, err := queries.CreateWorkerInstanceCredentialFromBootstrap(ctx, db.CreateWorkerInstanceCredentialFromBootstrapParams{
		BootstrapTokenHash: tokenHash,
		CredentialID:       ids.ToPG(ids.New()),
		WorkerInstanceID:   ids.ToPG(ids.New()),
		ResourceID:         "instance-a",
		KeyPrefix:          "second",
		SecretHash:         []byte("second-secret-hash"),
	})
	if err != nil {
		t.Fatal(err)
	}
	if second.WorkerInstanceID != first.WorkerInstanceID {
		t.Fatalf("rotated worker instance id = %s, want %s", second.WorkerInstanceID, first.WorkerInstanceID)
	}

	var activeCount int
	if err := pool.QueryRow(ctx, `
SELECT count(*)
  FROM worker_instance_credentials
 WHERE worker_instance_id = $1
   AND revoked_at IS NULL
`, first.WorkerInstanceID).Scan(&activeCount); err != nil {
		t.Fatal(err)
	}
	if activeCount != 1 {
		t.Fatalf("active credential count = %d, want 1", activeCount)
	}

	authenticated, err := queries.AuthenticateWorkerInstanceCredential(ctx, db.AuthenticateWorkerInstanceCredentialParams{
		WorkerInstanceID: first.WorkerInstanceID,
		SecretHash:       []byte("second-secret-hash"),
	})
	if err != nil {
		t.Fatal(err)
	}
	if authenticated.ID != second.ID {
		t.Fatalf("authenticated credential = %s, want %s", ids.MustFromPG(authenticated.ID), ids.MustFromPG(second.ID))
	}
}

func TestWorkerInstanceResourceIdentifierIsScopedByWorkerGroup(t *testing.T) {
	ctx := context.Background()
	queries, pool := newPostgresTestDB(t, ctx)
	orgID := ids.ToPG(dbtest.DefaultOrgID)
	firstTokenHash := []byte("first-group-bootstrap-token-hash")
	secondTokenHash := []byte("second-group-bootstrap-token-hash")

	seedPostgresTestConfiguredScope(t, ctx, pool, queries, orgID)
	defaultGroup, err := queries.GetDefaultWorkerGroup(ctx)
	if err != nil {
		t.Fatal(err)
	}
	secondGroupID := createPostgresTestWorkerGroup(t, ctx, pool, "secondary")
	if _, err := queries.UpsertWorkerBootstrapToken(ctx, db.UpsertWorkerBootstrapTokenParams{
		ID:            ids.ToPG(ids.New()),
		TokenHash:     firstTokenHash,
		WorkerGroupID: defaultGroup.ID,
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := queries.UpsertWorkerBootstrapToken(ctx, db.UpsertWorkerBootstrapTokenParams{
		ID:            ids.ToPG(ids.New()),
		TokenHash:     secondTokenHash,
		WorkerGroupID: secondGroupID,
	}); err != nil {
		t.Fatal(err)
	}

	first, err := queries.CreateWorkerInstanceCredentialFromBootstrap(ctx, db.CreateWorkerInstanceCredentialFromBootstrapParams{
		BootstrapTokenHash: firstTokenHash,
		CredentialID:       ids.ToPG(ids.New()),
		WorkerInstanceID:   ids.ToPG(ids.New()),
		ResourceID:         "shared-resource",
		KeyPrefix:          "first",
		SecretHash:         []byte("first-group-secret-hash"),
	})
	if err != nil {
		t.Fatal(err)
	}
	second, err := queries.CreateWorkerInstanceCredentialFromBootstrap(ctx, db.CreateWorkerInstanceCredentialFromBootstrapParams{
		BootstrapTokenHash: secondTokenHash,
		CredentialID:       ids.ToPG(ids.New()),
		WorkerInstanceID:   ids.ToPG(ids.New()),
		ResourceID:         "shared-resource",
		KeyPrefix:          "second",
		SecretHash:         []byte("second-group-secret-hash"),
	})
	if err != nil {
		t.Fatal(err)
	}
	if first.WorkerInstanceID == second.WorkerInstanceID {
		t.Fatalf("worker instance id reused across groups: %s", ids.MustFromPG(first.WorkerInstanceID))
	}
	if first.WorkerGroupID != defaultGroup.ID {
		t.Fatalf("first worker group = %s, want %s", ids.MustFromPG(first.WorkerGroupID), ids.MustFromPG(defaultGroup.ID))
	}
	if second.WorkerGroupID != secondGroupID {
		t.Fatalf("second worker group = %s, want %s", ids.MustFromPG(second.WorkerGroupID), ids.MustFromPG(secondGroupID))
	}
}

func TestWorkerBootstrapTokenConflictIsIdempotent(t *testing.T) {
	ctx := context.Background()
	queries, pool := newPostgresTestDB(t, ctx)
	orgID := ids.ToPG(dbtest.DefaultOrgID)
	tokenHash := []byte("stable-bootstrap-token-hash")

	seedPostgresTestConfiguredScope(t, ctx, pool, queries, orgID)
	workerGroup, err := queries.GetDefaultWorkerGroup(ctx)
	if err != nil {
		t.Fatal(err)
	}

	created, err := queries.UpsertWorkerBootstrapToken(ctx, db.UpsertWorkerBootstrapTokenParams{
		ID:            ids.ToPG(ids.New()),
		TokenHash:     tokenHash,
		WorkerGroupID: workerGroup.ID,
	})
	if err != nil {
		t.Fatal(err)
	}

	reused, err := queries.UpsertWorkerBootstrapToken(ctx, db.UpsertWorkerBootstrapTokenParams{
		ID:            ids.ToPG(ids.New()),
		TokenHash:     tokenHash,
		WorkerGroupID: workerGroup.ID,
	})
	if err != nil {
		t.Fatal(err)
	}
	if reused.ID != created.ID {
		t.Fatalf("reused token id = %v, want %v", reused.ID, created.ID)
	}
	if reused.WorkerGroupID != workerGroup.ID {
		t.Fatalf("reused worker group = %s, want %s", ids.MustFromPG(reused.WorkerGroupID), ids.MustFromPG(workerGroup.ID))
	}
	secondWorkerGroupID := createPostgresTestWorkerGroup(t, ctx, pool, "token-secondary")
	again, err := queries.UpsertWorkerBootstrapToken(ctx, db.UpsertWorkerBootstrapTokenParams{
		ID:            ids.ToPG(ids.New()),
		TokenHash:     tokenHash,
		WorkerGroupID: secondWorkerGroupID,
	})
	if err != nil {
		t.Fatal(err)
	}
	if again.ID != created.ID {
		t.Fatalf("reused token id after conflicting group = %v, want %v", again.ID, created.ID)
	}
	if again.WorkerGroupID != workerGroup.ID {
		t.Fatalf("reused worker group after conflicting group = %s, want preserved %s", ids.MustFromPG(again.WorkerGroupID), ids.MustFromPG(workerGroup.ID))
	}
}
