package db_test

import (
	"context"
	"testing"

	"github.com/google/uuid"
	"github.com/helmrdotdev/helmr/internal/db"
	"github.com/helmrdotdev/helmr/internal/db/dbtest"
	"github.com/helmrdotdev/helmr/internal/pgvalue"
)

func TestScopedSecretQueriesRemainEnvironmentOwnedAcrossRouteMove(t *testing.T) {
	ctx := context.Background()
	pool := newIntegrationDB(t, ctx)
	ids := seedIntegration(t, ctx, pool)
	queries := db.New(pool)

	secretID := uuid.Must(uuid.NewV7())
	if _, err := queries.UpsertScopedSecret(ctx, db.UpsertScopedSecretParams{
		ID:              pgvalue.UUID(secretID),
		OrgID:           pgvalue.UUID(ids.orgID),
		ProjectID:       pgvalue.UUID(ids.projectID),
		EnvironmentID:   pgvalue.UUID(ids.environmentID),
		Name:            "API_KEY",
		Version:         1,
		KeyID:           "k_test",
		Nonce:           []byte{1},
		Ciphertext:      []byte{2},
		PreviousVersion: 0,
	}); err != nil {
		t.Fatal(err)
	}

	placeEnvironmentInOtherWorkerGroup(t, ctx, pool, ids)

	record, err := queries.GetScopedSecretByName(ctx, db.GetScopedSecretByNameParams{
		OrgID:         pgvalue.UUID(ids.orgID),
		ProjectID:     pgvalue.UUID(ids.projectID),
		EnvironmentID: pgvalue.UUID(ids.environmentID),
		Name:          "API_KEY",
	})
	if err != nil {
		t.Fatal(err)
	}
	if record.ID != pgvalue.UUID(secretID) {
		t.Fatalf("secret id = %s, want %s", pgvalue.UUIDString(record.ID), secretID)
	}
	rows, err := queries.ListScopedSecrets(ctx, db.ListScopedSecretsParams{
		OrgID:         pgvalue.UUID(ids.orgID),
		ProjectID:     pgvalue.UUID(ids.projectID),
		EnvironmentID: pgvalue.UUID(ids.environmentID),
		RowLimit:      100,
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 1 || rows[0].Name != "API_KEY" {
		t.Fatalf("secrets = %+v", rows)
	}
}

func TestScopedSecretQueriesDoNotDuplicateWhenWorkerGroupHasActiveAndDrainingRoutes(t *testing.T) {
	ctx := context.Background()
	pool := newIntegrationDB(t, ctx)
	ids := seedIntegration(t, ctx, pool)
	queries := db.New(pool)

	placeEnvironmentInOtherWorkerGroup(t, ctx, pool, ids)
	secretID := uuid.Must(uuid.NewV7())
	if _, err := queries.UpsertScopedSecret(ctx, db.UpsertScopedSecretParams{
		ID:              pgvalue.UUID(secretID),
		OrgID:           pgvalue.UUID(ids.orgID),
		ProjectID:       pgvalue.UUID(ids.projectID),
		EnvironmentID:   pgvalue.UUID(ids.environmentID),
		Name:            "DUPLICATE_GUARD",
		Version:         1,
		KeyID:           "k_test",
		Nonce:           []byte{1},
		Ciphertext:      []byte{2},
		PreviousVersion: 0,
	}); err != nil {
		t.Fatal(err)
	}

	rows, err := queries.ListScopedSecrets(ctx, db.ListScopedSecretsParams{
		OrgID:         pgvalue.UUID(ids.orgID),
		ProjectID:     pgvalue.UUID(ids.projectID),
		EnvironmentID: pgvalue.UUID(ids.environmentID),
		RowLimit:      100,
	})
	if err != nil {
		t.Fatal(err)
	}
	if got := secretNames(rows); len(got) != 1 || got[0] != "DUPLICATE_GUARD" {
		t.Fatalf("secret names = %+v, want single duplicate guard row", got)
	}
}

func TestScopedSecretListAndDeleteContinueOnStaleWorkerGroupHealth(t *testing.T) {
	ctx := context.Background()
	pool := newIntegrationDB(t, ctx)
	ids := seedIntegration(t, ctx, pool)
	queries := db.New(pool)

	if _, err := queries.UpsertScopedSecret(ctx, db.UpsertScopedSecretParams{
		ID:              pgvalue.UUID(uuid.Must(uuid.NewV7())),
		OrgID:           pgvalue.UUID(ids.orgID),
		ProjectID:       pgvalue.UUID(ids.projectID),
		EnvironmentID:   pgvalue.UUID(ids.environmentID),
		Name:            "STALE_HEALTH",
		Version:         1,
		KeyID:           "k_test",
		Nonce:           []byte{1},
		Ciphertext:      []byte{2},
		PreviousVersion: 0,
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `
		UPDATE worker_groups
		   SET health_state = 'unavailable',
		       routing_fresh_until = now() - interval '1 minute'
		 WHERE id = $1
	`, dbtest.DefaultWorkerGroupID); err != nil {
		t.Fatal(err)
	}

	rows, err := queries.ListScopedSecrets(ctx, db.ListScopedSecretsParams{
		OrgID:         pgvalue.UUID(ids.orgID),
		ProjectID:     pgvalue.UUID(ids.projectID),
		EnvironmentID: pgvalue.UUID(ids.environmentID),
		RowLimit:      100,
	})
	if err != nil {
		t.Fatal(err)
	}
	if got := secretNames(rows); len(got) != 1 || got[0] != "STALE_HEALTH" {
		t.Fatalf("secret names = %+v, want stale-health secret", got)
	}
	if affected, err := queries.DeleteScopedSecret(ctx, db.DeleteScopedSecretParams{
		OrgID:         pgvalue.UUID(ids.orgID),
		ProjectID:     pgvalue.UUID(ids.projectID),
		EnvironmentID: pgvalue.UUID(ids.environmentID),
		Name:          "STALE_HEALTH",
	}); err != nil {
		t.Fatal(err)
	} else if affected != 1 {
		t.Fatalf("deleted rows = %d, want 1", affected)
	}
}

func secretNames(rows []db.ListScopedSecretsRow) []string {
	names := make([]string, 0, len(rows))
	for _, row := range rows {
		names = append(names, row.Name)
	}
	return names
}
