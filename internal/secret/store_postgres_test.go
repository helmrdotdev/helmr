package secret

import (
	"errors"
	"strings"
	"testing"

	"github.com/google/uuid"
	"github.com/helmrdotdev/helmr/internal/db"
	"github.com/helmrdotdev/helmr/internal/db/dbtest"
	"github.com/helmrdotdev/helmr/internal/db/schema"
	"github.com/helmrdotdev/helmr/internal/idempotency"
	"github.com/helmrdotdev/helmr/internal/pgvalue"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/jackc/pgx/v5/pgxpool"
)

func TestSecretMutationReplayComparesExactEncryptedVersion(t *testing.T) {
	database := dbtest.Open(t)
	if err := schema.Up(t.Context(), database.DSN); err != nil {
		t.Fatal(err)
	}
	environmentID := seedSecretEnvironment(t, database.Pool)
	store, err := New(db.New(database.Pool), database.Pool, make([]byte, 32))
	if err != nil {
		t.Fatal(err)
	}

	created, err := store.Create(
		t.Context(),
		environmentID,
		"API_TOKEN",
		[]byte("first-value"),
		"create-1",
	)
	if err != nil {
		t.Fatal(err)
	}
	replayed, err := store.Create(
		t.Context(),
		environmentID,
		"API_TOKEN",
		[]byte("first-value"),
		"create-1",
	)
	if err != nil {
		t.Fatal(err)
	}
	createdVersionID := currentSecretVersion(t, database.Pool, environmentID, created.ID)
	replayedVersionID := currentSecretVersion(t, database.Pool, environmentID, replayed.ID)
	if replayed.ID != created.ID || replayedVersionID != createdVersionID {
		t.Fatalf("create replay changed authority: first=%+v replay=%+v", created, replayed)
	}
	_, err = store.Create(
		t.Context(),
		environmentID,
		"API_TOKEN",
		[]byte("different-value"),
		"create-1",
	)
	var conflict idempotency.ConflictError
	if !errors.As(err, &conflict) {
		t.Fatalf("different replay error = %v", err)
	}

	secretID := pgvalue.MustUUIDValue(created.ID)
	rotated, err := store.Rotate(
		t.Context(),
		environmentID,
		secretID,
		[]byte("second-value"),
		"rotate-1",
	)
	if err != nil {
		t.Fatal(err)
	}
	replayedRotation, err := store.Rotate(
		t.Context(),
		environmentID,
		secretID,
		[]byte("second-value"),
		"rotate-1",
	)
	if err != nil {
		t.Fatal(err)
	}
	rotatedVersionID := currentSecretVersion(t, database.Pool, environmentID, rotated.ID)
	replayedRotationVersionID := currentSecretVersion(
		t,
		database.Pool,
		environmentID,
		replayedRotation.ID,
	)
	if replayedRotationVersionID != rotatedVersionID {
		t.Fatalf("rotation replay changed version: first=%+v replay=%+v", rotated, replayedRotation)
	}

	var receipt string
	if err := database.Pool.QueryRow(t.Context(), `
		SELECT receipt::text
		  FROM idempotency_claims
		 WHERE environment_id = $1
		   AND operation = 'secret.rotate'
	`, environmentID).Scan(&receipt); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(receipt, pgvalue.MustUUIDValue(rotatedVersionID).String()) {
		t.Fatalf("receipt does not pin the Secret version: %s", receipt)
	}
	if strings.Contains(receipt, "second-value") {
		t.Fatalf("receipt contains Secret plaintext: %s", receipt)
	}
}

func seedSecretEnvironment(t *testing.T, pool *pgxpool.Pool) uuid.UUID {
	t.Helper()
	orgID := uuid.Must(uuid.NewV7())
	projectID := uuid.Must(uuid.NewV7())
	environmentID := uuid.Must(uuid.NewV7())
	regionID := "secret-" + environmentID.String()
	if _, err := pool.Exec(t.Context(), `
		INSERT INTO organizations (id, name, slug)
		VALUES ($1, 'Secrets', $2)
	`, orgID, "secrets-"+orgID.String()); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(t.Context(), `
		INSERT INTO regions (id, provider, provider_region, display_name)
		VALUES ($1, 'test', $1, 'Secrets')
	`, regionID); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(t.Context(), `
		INSERT INTO projects (id, org_id, default_region_id, slug, name)
		VALUES ($1, $2, $3, $4, 'Secrets')
	`,
		projectID,
		orgID,
		regionID,
		"secrets-"+projectID.String(),
	); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(t.Context(), `
		INSERT INTO environments (id, org_id, project_id, slug, name, color_hex)
		VALUES ($1, $2, $3, 'production', 'Production', '#000000')
	`, environmentID, orgID, projectID); err != nil {
		t.Fatal(err)
	}
	return environmentID
}

func currentSecretVersion(
	t *testing.T,
	pool *pgxpool.Pool,
	environmentID uuid.UUID,
	secretID pgtype.UUID,
) pgtype.UUID {
	t.Helper()
	var versionID pgtype.UUID
	if err := pool.QueryRow(t.Context(), `
		SELECT current_version_id
		  FROM secrets
		 WHERE environment_id = $1
		   AND id = $2
	`, environmentID, secretID).Scan(&versionID); err != nil {
		t.Fatal(err)
	}
	return versionID
}
