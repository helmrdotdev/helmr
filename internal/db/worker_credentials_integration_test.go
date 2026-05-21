package db_test

import (
	"context"
	"testing"

	"github.com/helmrdotdev/helmr/internal/db"
	"github.com/helmrdotdev/helmr/internal/ids"
	"github.com/jackc/pgx/v5/pgtype"
)

func TestCreateWorkerCredentialFromRegistrationRotatesExistingHostCredential(t *testing.T) {
	ctx := context.Background()
	queries, pool := newPostgresTestDB(t, ctx)
	orgID := ids.ToPG(ids.DefaultOrgID)
	tokenHash := []byte("registration-token-hash")
	workerHostID := ids.ToPG(ids.New())

	seedPostgresTestWorkerRegistrationToken(t, ctx, pool, queries, orgID, tokenHash)

	first, err := queries.CreateWorkerCredentialFromRegistration(ctx, db.CreateWorkerCredentialFromRegistrationParams{
		RegistrationTokenHash: tokenHash,
		CredentialID:          ids.ToPG(ids.New()),
		WorkerHostID:          workerHostID,
		ExternalID:            "host-a",
		KeyPrefix:             "first",
		SecretHash:            []byte("first-secret-hash"),
	})
	if err != nil {
		t.Fatal(err)
	}
	second, err := queries.CreateWorkerCredentialFromRegistration(ctx, db.CreateWorkerCredentialFromRegistrationParams{
		RegistrationTokenHash: tokenHash,
		CredentialID:          ids.ToPG(ids.New()),
		WorkerHostID:          ids.ToPG(ids.New()),
		ExternalID:            "host-a",
		KeyPrefix:             "second",
		SecretHash:            []byte("second-secret-hash"),
	})
	if err != nil {
		t.Fatal(err)
	}
	if second.WorkerHostID != first.WorkerHostID {
		t.Fatalf("rotated worker host id = %s, want %s", second.WorkerHostID, first.WorkerHostID)
	}

	var activeCount int
	if err := pool.QueryRow(ctx, `
SELECT count(*)
  FROM worker_credentials
 WHERE worker_host_id = $1
   AND revoked_at IS NULL
`, first.WorkerHostID).Scan(&activeCount); err != nil {
		t.Fatal(err)
	}
	if activeCount != 1 {
		t.Fatalf("active credential count = %d, want 1", activeCount)
	}

	authenticated, err := queries.AuthenticateWorkerCredential(ctx, db.AuthenticateWorkerCredentialParams{
		WorkerHostID: first.WorkerHostID,
		SecretHash:   []byte("second-secret-hash"),
	})
	if err != nil {
		t.Fatal(err)
	}
	if authenticated.ID != second.ID {
		t.Fatalf("authenticated credential = %s, want %s", ids.MustFromPG(authenticated.ID), ids.MustFromPG(second.ID))
	}
}

func TestWorkerRegistrationTokenConflictDoesNotRebindPool(t *testing.T) {
	ctx := context.Background()
	queries, pool := newPostgresTestDB(t, ctx)
	orgID := ids.ToPG(ids.DefaultOrgID)
	tokenHash := []byte("stable-registration-token-hash")

	seedPostgresTestDefaultScope(t, ctx, pool, queries, orgID)
	poolA := createTestWorkerPool(t, ctx, queries, orgID, "token-pool-a", "token-queue-a")
	poolB := createTestWorkerPool(t, ctx, queries, orgID, "token-pool-b", "token-queue-b")

	created, err := queries.UpsertWorkerRegistrationToken(ctx, db.UpsertWorkerRegistrationTokenParams{
		ID:           ids.ToPG(ids.New()),
		WorkerPoolID: poolA.ID,
		TokenHash:    tokenHash,
	})
	if err != nil {
		t.Fatal(err)
	}
	if created.WorkerPoolID != poolA.ID {
		t.Fatalf("created token pool = %v, want %v", created.WorkerPoolID, poolA.ID)
	}

	if _, err := queries.UpsertWorkerRegistrationToken(ctx, db.UpsertWorkerRegistrationTokenParams{
		ID:           ids.ToPG(ids.New()),
		WorkerPoolID: poolB.ID,
		TokenHash:    tokenHash,
	}); err == nil {
		t.Fatal("expected token hash conflict for another worker pool to return no row")
	}

	var workerPoolID pgtype.UUID
	if err := pool.QueryRow(ctx, `
SELECT worker_pool_id
  FROM worker_registration_tokens
 WHERE token_hash = $1
`, tokenHash).Scan(&workerPoolID); err != nil {
		t.Fatal(err)
	}
	if workerPoolID != poolA.ID {
		t.Fatalf("token worker pool = %v, want %v", workerPoolID, poolA.ID)
	}
}
