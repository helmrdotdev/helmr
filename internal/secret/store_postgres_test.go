package secret

import (
	"errors"
	"github.com/helmrdotdev/helmr/internal/auditpolicy"
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
	store, err := New(db.New(database.Pool), database.Pool, make([]byte, 32), auditpolicy.Policy{RetentionSeconds: 3600})
	if err != nil {
		t.Fatal(err)
	}

	created, err := store.Create(
		t.Context(), dbtest.SecretManager(t, database.Pool, environmentID), environmentID,
		"API_TOKEN",
		[]byte("first-value"),
		"create-1",
	)
	if err != nil {
		t.Fatal(err)
	}
	replayed, err := store.Create(
		t.Context(), dbtest.SecretManager(t, database.Pool, environmentID), environmentID,
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
		t.Context(), dbtest.SecretManager(t, database.Pool, environmentID), environmentID,
		"API_TOKEN",
		[]byte("different-value"),
		"create-1",
	)
	if !errors.Is(err, ErrMutationConflict) {
		t.Fatalf("different replay error = %v", err)
	}

	secretID := pgvalue.MustUUIDValue(created.ID)
	rotated, err := store.Rotate(
		t.Context(), dbtest.SecretManager(t, database.Pool, environmentID), environmentID,
		secretID,
		[]byte("second-value"),
		"rotate-1",
	)
	if err != nil {
		t.Fatal(err)
	}
	replayedRotation, err := store.Rotate(
		t.Context(), dbtest.SecretManager(t, database.Pool, environmentID), environmentID,
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

	if _, err = store.Rotate(t.Context(), dbtest.SecretManager(t, database.Pool, environmentID), environmentID, secretID, []byte("third-value"), "rotate-2"); err != nil {
		t.Fatal(err)
	}
	// Original retry identities must compare against their original encrypted value,
	// not the newest value, and must never roll the Secret back to that version.
	latest := currentSecretVersion(t, database.Pool, environmentID, created.ID)
	if _, err = store.Rotate(t.Context(), dbtest.SecretManager(t, database.Pool, environmentID), environmentID, secretID, []byte("second-value"), "rotate-1"); err != nil {
		t.Fatal(err)
	}
	if got := currentSecretVersion(t, database.Pool, environmentID, created.ID); got != latest {
		t.Fatal("replay rolled back current version")
	}
	if _, err = store.Rotate(t.Context(), dbtest.SecretManager(t, database.Pool, environmentID), environmentID, secretID, []byte("third-value"), "rotate-1"); !errors.Is(err, ErrMutationConflict) {
		t.Fatalf("changed rotation replay: %v", err)
	}
	if other, err := store.Create(t.Context(), dbtest.SecretManager(t, database.Pool, environmentID), environmentID, "OTHER", []byte("first-value"), "create-1"); err != nil || other.ID == created.ID {
		t.Fatalf("name-scoped create key: %v", err)
	}
	if _, err = store.Revoke(t.Context(), dbtest.SecretManager(t, database.Pool, environmentID), environmentID, secretID, "revoke-1"); err != nil {
		t.Fatal(err)
	}
	if _, err = store.Revoke(t.Context(), dbtest.SecretManager(t, database.Pool, environmentID), environmentID, secretID, ""); err != nil {
		t.Fatal(err)
	}
	replayed, err = store.Create(t.Context(), dbtest.SecretManager(t, database.Pool, environmentID), environmentID, "API_TOKEN", []byte("first-value"), "create-1")
	if err != nil || !replayed.RevokedAt.Valid {
		t.Fatalf("revoked create replay: %+v %v", replayed, err)
	}
	replayed, err = store.Rotate(t.Context(), dbtest.SecretManager(t, database.Pool, environmentID), environmentID, secretID, []byte("second-value"), "rotate-1")
	if err != nil || !replayed.RevokedAt.Valid {
		t.Fatalf("revoked rotate replay reactivated: %+v %v", replayed, err)
	}
	var versions int
	var revoked bool
	if err = database.Pool.QueryRow(t.Context(), `SELECT (SELECT count(*) FROM secret_versions WHERE secret_id=$1),current_version_id=$2 AND revoked_at IS NOT NULL FROM secrets WHERE id=$1`, secretID, latest).Scan(&versions, &revoked); err != nil || versions != 3 || !revoked {
		t.Fatalf("revocation kept identity: versions=%d revoked=%t: %v", versions, revoked, err)
	}
}

// A Secret's identity is permanent. Rotating a revoked Secret reactivates it
// with the next version; Create only makes new names.
func TestSecretRotationReactivatesRevokedSecret(t *testing.T) {
	database := dbtest.Open(t)
	if err := schema.Up(t.Context(), database.DSN); err != nil {
		t.Fatal(err)
	}
	env := seedSecretEnvironment(t, database.Pool)
	store, err := New(db.New(database.Pool), database.Pool, make([]byte, 32), auditpolicy.Policy{RetentionSeconds: 3600})
	if err != nil {
		t.Fatal(err)
	}
	manager := dbtest.SecretManager(t, database.Pool, env)
	created, err := store.Create(t.Context(), manager, env, "TOKEN", []byte("first"), "")
	if err != nil {
		t.Fatal(err)
	}
	id := pgvalue.MustUUIDValue(created.ID)
	if _, err = store.Create(t.Context(), manager, env, "TOKEN", []byte("other"), ""); !errors.Is(err, ErrNameExists) {
		t.Fatalf("create on an active name: %v", err)
	}
	if _, err = store.Rotate(t.Context(), manager, env, id, []byte("second"), ""); err != nil {
		t.Fatalf("unkeyed rotation: %v", err)
	}
	revoked, err := store.Revoke(t.Context(), manager, env, id, "revoke-1")
	if err != nil || !revoked.RevokedAt.Valid {
		t.Fatalf("revoke: %+v %v", revoked, err)
	}
	if _, err = store.Create(t.Context(), manager, env, "TOKEN", []byte("other"), "create-again"); !errors.Is(err, ErrNameRevoked) {
		t.Fatalf("create on a revoked name: %v", err)
	}
	reactivated, err := store.Rotate(t.Context(), manager, env, id, []byte("third"), "")
	if err != nil || reactivated.ID != created.ID || reactivated.RevokedAt.Valid || !reactivated.RotatedAt.Valid {
		t.Fatalf("reactivation: %+v %v", reactivated, err)
	}
	var version int64
	if err = database.Pool.QueryRow(t.Context(), `SELECT v.version FROM secrets s JOIN secret_versions v ON v.id=s.current_version_id WHERE s.id=$1 AND s.revoked_at IS NULL`, id).Scan(&version); err != nil || version != 3 {
		t.Fatalf("reactivated version %d: %v", version, err)
	}
	// A keyed revoke replayed after reactivation returns its original receipt
	// and never revokes the Secret again.
	replayed, err := store.Revoke(t.Context(), manager, env, id, "revoke-1")
	if err != nil || replayed.ID != revoked.ID || !replayed.RevokedAt.Valid || !replayed.RevokedAt.Time.Equal(revoked.RevokedAt.Time) {
		t.Fatalf("revoke replay: %+v %v", replayed, err)
	}
	var active bool
	if err = database.Pool.QueryRow(t.Context(), `SELECT revoked_at IS NULL FROM secrets WHERE id=$1`, id).Scan(&active); err != nil || !active {
		t.Fatalf("revoke replay re-revoked the reactivated Secret: %v %v", active, err)
	}
	var revocations int
	if err = database.Pool.QueryRow(t.Context(), `SELECT count(*) FROM audit_events WHERE environment_id=$1 AND target_id=$2 AND action='secret.revoked'`, env, id).Scan(&revocations); err != nil || revocations != 1 {
		t.Fatalf("revocation audit %d: %v", revocations, err)
	}
}

func TestSecretMutationConcurrentRetriesAndEnvironmentScope(t *testing.T) {
	database := dbtest.Open(t)
	if err := schema.Up(t.Context(), database.DSN); err != nil {
		t.Fatal(err)
	}
	env := seedSecretEnvironment(t, database.Pool)
	store, err := New(db.New(database.Pool), database.Pool, make([]byte, 32), auditpolicy.Policy{RetentionSeconds: 3600})
	if err != nil {
		t.Fatal(err)
	}
	const clients = 12
	created := make(chan db.GetSecretSnapshotRow, clients)
	errs := make(chan error, clients)
	var wg sync.WaitGroup
	for range clients {
		wg.Go(func() {
			row, err := store.Create(t.Context(), dbtest.SecretManager(t, database.Pool, env), env, "TOKEN", []byte("first"), "same-create")
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
			_, err := store.Rotate(t.Context(), dbtest.SecretManager(t, database.Pool, env), env, pgvalue.MustUUIDValue(id), []byte("second"), "same-rotate")
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
	if _, err = store.Rotate(t.Context(), dbtest.SecretManager(t, database.Pool, other), other, pgvalue.MustUUIDValue(id), []byte("second"), "same-rotate"); err == nil {
		t.Fatal("foreign rotation accepted")
	}
	if _, err = store.Revoke(t.Context(), dbtest.SecretManager(t, database.Pool, other), other, pgvalue.MustUUIDValue(id), "revoke"); err == nil {
		t.Fatal("foreign revocation accepted")
	}
	if _, err = store.Create(t.Context(), dbtest.SecretManager(t, database.Pool, other), other, "TOKEN", []byte("other"), "same-create"); err != nil {
		t.Fatalf("retry identity leaked across Environments: %v", err)
	}
	for _, key := range []string{"bad\x00key", string([]byte{0xff})} {
		if _, err = store.Rotate(t.Context(), dbtest.SecretManager(t, database.Pool, env), env, pgvalue.MustUUIDValue(id), []byte("bad"), key); !errors.Is(err, ErrInvalidMutation) {
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
		INSERT INTO environments (id, org_id, project_id, slug, name, color_hex,max_outstanding_admissions,max_causal_depth,max_resident_computers,max_cpu_millis,max_memory_bytes,max_reserved_storage_bytes,preparation_timeout_ms,admission_rate_per_second,admission_burst,admission_tokens,admission_refilled_at)
		VALUES ($1, $2, $3, 'production', 'Production', '#000000',1024,32,1024,1024000,1099511627776,1099511627776,600000,1000000,1000000,1000000,clock_timestamp())
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
	store, err := New(db.New(database.Pool), database.Pool, make([]byte, 32), auditpolicy.Policy{RetentionSeconds: 3600})
	if err != nil {
		t.Fatal(err)
	}
	created, err := store.Create(t.Context(), dbtest.SecretManager(t, database.Pool, env), env, "TOKEN", []byte("first"), "create")
	if err != nil {
		t.Fatal(err)
	}
	id := pgvalue.MustUUIDValue(created.ID)
	results := make(chan error, 8)
	for range 8 {
		key := uuid.NewV7().String()
		go func() {
			_, err := store.Rotate(t.Context(), dbtest.SecretManager(t, database.Pool, env), env, id, []byte("rotated"), key)
			results <- err
		}()
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
