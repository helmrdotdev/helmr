package db_test

import (
	"context"
	"testing"

	"github.com/helmrdotdev/helmr/internal/db"
	"github.com/helmrdotdev/helmr/internal/ids"
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
 WHERE org_id = $1
   AND worker_host_id = $2
   AND revoked_at IS NULL
`, orgID, first.WorkerHostID).Scan(&activeCount); err != nil {
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
