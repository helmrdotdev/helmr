package secret

import (
	"errors"
	"sync"
	"testing"
	"uuid"

	"github.com/helmrdotdev/helmr/internal/db"
	"github.com/helmrdotdev/helmr/internal/db/dbtest"
	"github.com/helmrdotdev/helmr/internal/db/schema"
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
	if !errors.Is(err, ErrMutationConflict) {
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

	if _, err = store.Rotate(t.Context(), environmentID, secretID, []byte("third-value"), "rotate-2"); err != nil {
		t.Fatal(err)
	}
	// Original retry identities must compare against their original encrypted value,
	// not the newest value, and must never roll the Secret back to that version.
	latest := currentSecretVersion(t, database.Pool, environmentID, created.ID)
	if _, err = store.Rotate(t.Context(), environmentID, secretID, []byte("second-value"), "rotate-1"); err != nil {
		t.Fatal(err)
	}
	if got := currentSecretVersion(t, database.Pool, environmentID, created.ID); got != latest {
		t.Fatal("replay rolled back current version")
	}
	if _, err = store.Rotate(t.Context(), environmentID, secretID, []byte("third-value"), "rotate-1"); !errors.Is(err, ErrMutationConflict) {
		t.Fatalf("changed rotation replay: %v", err)
	}
	if other, err := store.Create(t.Context(), environmentID, "OTHER", []byte("first-value"), "create-1"); err != nil || other.ID == created.ID {
		t.Fatalf("name-scoped create key: %v", err)
	}
	if _, err = store.Revoke(t.Context(), environmentID, secretID, "revoke-1"); err != nil {
		t.Fatal(err)
	}
	if _, err = store.Revoke(t.Context(), environmentID, secretID, ""); err != nil {
		t.Fatal(err)
	}
	var notices int
	if err := database.Pool.QueryRow(t.Context(), `SELECT count(*) FROM control_outbox WHERE topic='secret.revoked' AND payload->>'environmentId'=$1 AND payload->>'secretId'=$2 AND payload->>'revocationGeneration'='1'`, environmentID.String(), secretID.String()).Scan(&notices); err != nil || notices != 1 {
		t.Fatalf("revocation notices=%d: %v", notices, err)
	}

	replayed, err = store.Create(t.Context(), environmentID, "API_TOKEN", []byte("first-value"), "create-1")
	if err != nil || replayed.Status != "revoked" {
		t.Fatalf("revoked create replay: %+v %v", replayed, err)
	}
	replayed, err = store.Rotate(t.Context(), environmentID, secretID, []byte("second-value"), "rotate-1")
	if err != nil || replayed.Status != "revoked" {
		t.Fatalf("revoked rotate replay: %+v %v", replayed, err)
	}
	if _, err = store.Rotate(t.Context(), environmentID, secretID, []byte("new"), "rotate-3"); !IsUnavailable(err) {
		t.Fatalf("revoked rotation: %v", err)
	}
	var versions int
	var generation int64
	if err = database.Pool.QueryRow(t.Context(), `SELECT (SELECT count(*) FROM secret_versions WHERE secret_id=$1),revocation_generation FROM secrets WHERE id=$1`, secretID).Scan(&versions, &generation); err != nil || versions != 3 || generation != 1 {
		t.Fatalf("versions=%d revocations=%d: %v", versions, generation, err)
	}
}

func TestSecretMutationConcurrentRetriesAndEnvironmentScope(t *testing.T) {
	database := dbtest.Open(t)
	if err := schema.Up(t.Context(), database.DSN); err != nil {
		t.Fatal(err)
	}
	env := seedSecretEnvironment(t, database.Pool)
	store, err := New(db.New(database.Pool), database.Pool, make([]byte, 32))
	if err != nil {
		t.Fatal(err)
	}
	const clients = 12
	created := make(chan db.GetSecretSnapshotRow, clients)
	errs := make(chan error, clients)
	var wg sync.WaitGroup
	for range clients {
		wg.Go(func() {
			row, err := store.Create(t.Context(), env, "TOKEN", []byte("first"), "same-create")
			created <- row
			errs <- err
		})
	}
	wg.Wait()
	close(created)
	close(errs)
	for err := range errs {
		if err != nil {
			t.Fatal(err)
		}
	}
	var id pgtype.UUID
	for row := range created {
		if id.Valid && id != row.ID {
			t.Fatal("create retries forked Secret")
		}
		id = row.ID
	}
	errs = make(chan error, clients)
	for range clients {
		wg.Go(func() {
			_, err := store.Rotate(t.Context(), env, pgvalue.MustUUIDValue(id), []byte("second"), "same-rotate")
			errs <- err
		})
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		if err != nil {
			t.Fatal(err)
		}
	}
	var count int
	if err = database.Pool.QueryRow(t.Context(), `SELECT count(*) FROM secret_versions WHERE secret_id=$1`, id).Scan(&count); err != nil || count != 2 {
		t.Fatalf("versions=%d: %v", count, err)
	}
	other := seedSecretEnvironment(t, database.Pool)
	if _, err = store.Rotate(t.Context(), other, pgvalue.MustUUIDValue(id), []byte("second"), "same-rotate"); err == nil {
		t.Fatal("foreign rotation accepted")
	}
	if _, err = store.Revoke(t.Context(), other, pgvalue.MustUUIDValue(id), "revoke"); err == nil {
		t.Fatal("foreign revocation accepted")
	}
	if _, err = store.Create(t.Context(), other, "TOKEN", []byte("other"), "same-create"); err != nil {
		t.Fatalf("retry identity leaked across Environments: %v", err)
	}
	for _, key := range []string{"", "bad\x00key", string([]byte{0xff})} {
		if _, err = store.Rotate(t.Context(), env, pgvalue.MustUUIDValue(id), []byte("bad"), key); !errors.Is(err, ErrInvalidMutation) {
			t.Fatalf("invalid key accepted %v", err)
		}
	}
}

func seedSecretEnvironment(t *testing.T, pool *pgxpool.Pool) uuid.UUID {
	t.Helper()
	orgID := uuid.NewV7()
	projectID := uuid.NewV7()
	environmentID := uuid.NewV7()
	regionID := "secret-" + environmentID.String()
	if _, err := pool.Exec(t.Context(), `
		INSERT INTO organizations (id, name, slug)
		VALUES ($1, 'Secrets', $2)
	`, orgID, "secrets-"+orgID.String()); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(t.Context(), `
		INSERT INTO regions (id, display_name)
		VALUES ($1, 'Secrets')
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
		INSERT INTO environments (history_retention_mode,id, org_id, project_id, slug, name, color_hex)
		VALUES ('until_environment_deletion',$1, $2, $3, 'production', 'Production', '#000000')
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

func TestSecretDistinctConcurrentMutationsDoNotUpgradeForeignKeyLocks(t *testing.T) {
	database := dbtest.Open(t)
	if err := schema.Up(t.Context(), database.DSN); err != nil {
		t.Fatal(err)
	}
	env := seedSecretEnvironment(t, database.Pool)
	store, err := New(db.New(database.Pool), database.Pool, make([]byte, 32))
	if err != nil {
		t.Fatal(err)
	}
	created, err := store.Create(t.Context(), env, "TOKEN", []byte("first"), "create")
	if err != nil {
		t.Fatal(err)
	}
	id := pgvalue.MustUUIDValue(created.ID)
	results := make(chan error, 8)
	for range 8 {
		key := uuid.NewV7().String()
		go func() { _, err := store.Rotate(t.Context(), env, id, []byte("rotated"), key); results <- err }()
	}
	for range 8 {
		if err := <-results; err != nil {
			t.Fatal(err)
		}
	}
	var count int
	if err = database.Pool.QueryRow(t.Context(), `SELECT count(*) FROM secret_versions WHERE secret_id=$1`, id).Scan(&count); err != nil || count != 9 {
		t.Fatalf("distinct rotations %d: %v", count, err)
	}
}

func TestSecretMutationRollsBackWithoutRevocationNotice(t *testing.T) {
	database := dbtest.Open(t)
	if err := schema.Up(t.Context(), database.DSN); err != nil {
		t.Fatal(err)
	}
	env := seedSecretEnvironment(t, database.Pool)
	store, err := New(db.New(database.Pool), database.Pool, make([]byte, 32))
	if err != nil {
		t.Fatal(err)
	}
	created, err := store.Create(t.Context(), env, "TOKEN", []byte("test"), "create")
	if err != nil {
		t.Fatal(err)
	}
	id := pgvalue.MustUUIDValue(created.ID)
	dbtest.MustExec(t, t.Context(), database.Pool, `ALTER TABLE control_outbox ADD CONSTRAINT injected_notice_failure CHECK (topic<>'secret.revoked')`)
	if _, err = store.Revoke(t.Context(), env, id, "revoke"); err == nil {
		t.Fatal("missing durable notice accepted")
	}
	var active bool
	if err = database.Pool.QueryRow(t.Context(), `SELECT status='active' AND revocation_generation=0 AND current_version_id IS NOT NULL FROM secrets WHERE environment_id=$1 AND id=$2`, env, id).Scan(&active); err != nil || !active {
		t.Fatalf("revocation leaked: %v %v", active, err)
	}
	dbtest.MustExec(t, t.Context(), database.Pool, `ALTER TABLE control_outbox DROP CONSTRAINT injected_notice_failure`)
	if _, err = store.Revoke(t.Context(), env, id, "revoke"); err != nil {
		t.Fatal(err)
	}
}
