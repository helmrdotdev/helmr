package db_test

import (
	"context"
	"testing"

	"github.com/helmrdotdev/helmr/internal/db"
	"github.com/helmrdotdev/helmr/internal/ids"
)

func TestCreateWorkerInstanceCredentialFromBootstrapRotatesExistingHostCredential(t *testing.T) {
	ctx := context.Background()
	queries, pool := newPostgresTestDB(t, ctx)
	orgID := ids.ToPG(ids.DefaultOrgID)
	tokenHash := []byte("registration-token-hash")
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

func TestWorkerBootstrapTokenConflictIsIdempotent(t *testing.T) {
	ctx := context.Background()
	queries, pool := newPostgresTestDB(t, ctx)
	orgID := ids.ToPG(ids.DefaultOrgID)
	tokenHash := []byte("stable-registration-token-hash")

	seedPostgresTestDefaultScope(t, ctx, pool, queries, orgID)

	created, err := queries.UpsertWorkerBootstrapToken(ctx, db.UpsertWorkerBootstrapTokenParams{
		ID:        ids.ToPG(ids.New()),
		TokenHash: tokenHash,
	})
	if err != nil {
		t.Fatal(err)
	}

	reused, err := queries.UpsertWorkerBootstrapToken(ctx, db.UpsertWorkerBootstrapTokenParams{
		ID:        ids.ToPG(ids.New()),
		TokenHash: tokenHash,
	})
	if err != nil {
		t.Fatal(err)
	}
	if reused.ID != created.ID {
		t.Fatalf("reused token id = %v, want %v", reused.ID, created.ID)
	}
}
