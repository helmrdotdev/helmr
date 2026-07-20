package secret

import (
	"encoding/base32"
	"fmt"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/google/uuid"
	"github.com/helmrdotdev/helmr/internal/db"
	"github.com/helmrdotdev/helmr/internal/db/schema"
	"github.com/helmrdotdev/helmr/internal/keyedhash"
	"github.com/helmrdotdev/helmr/internal/pgvalue"
	"github.com/jackc/pgx/v5/pgxpool"
)

func TestStoreRevokeCommitsOneAuthorityTuple(t *testing.T) {
	pool := openSecretPostgres(t)
	orgID, projectID, environmentID := seedSecretEnvironment(t, pool)
	hashes := seedSecretHashAuthority(t, pool)
	encryption, err := NewKeyring(makeKey(1), nil)
	if err != nil {
		t.Fatal(err)
	}
	store, err := New(t.Context(), db.New(pool), pool, encryption, hashes)
	if err != nil {
		t.Fatal(err)
	}
	created, err := store.PutScoped(t.Context(), orgID, projectID, environmentID, "API_TOKEN", []byte("first"))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.PutScoped(t.Context(), orgID, projectID, environmentID, "API_TOKEN", []byte("second")); err != nil {
		t.Fatal(err)
	}
	secretID := pgvalue.MustUUIDValue(created.ID)
	first, err := store.Revoke(t.Context(), environmentID, secretID, "revoke-1")
	if err != nil {
		t.Fatal(err)
	}
	replayed, err := store.Revoke(t.Context(), environmentID, secretID, "revoke-1")
	if err != nil {
		t.Fatal(err)
	}
	if first.State != "revoked" || !first.RotatedAt.Valid || !first.RevokedAt.Valid {
		t.Fatalf("first snapshot = %+v", first)
	}
	if first.ID != replayed.ID || first.RotatedAt != replayed.RotatedAt || first.RevokedAt != replayed.RevokedAt {
		t.Fatalf("replay changed snapshot: first=%+v replay=%+v", first, replayed)
	}
	var state string
	var stateVersion int64
	var revocationGeneration int64
	if err := pool.QueryRow(t.Context(), `
		SELECT state, state_version, revocation_generation
		  FROM secrets
		 WHERE environment_id = $1
		   AND id = $2
	`, environmentID, secretID).Scan(&state, &stateVersion, &revocationGeneration); err != nil {
		t.Fatal(err)
	}
	if state != "revoked" || stateVersion != 3 || revocationGeneration != 1 {
		t.Fatalf("secret authority = %s/%d/%d", state, stateVersion, revocationGeneration)
	}
	assertSecretRevokeCounts(t, pool, environmentID, 1, 1)

	rollbackSecret, err := store.PutScoped(t.Context(), orgID, projectID, environmentID, "ROLLBACK_TOKEN", []byte("value"))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(t.Context(), `
		CREATE FUNCTION reject_secret_revoke_outbox() RETURNS trigger
		LANGUAGE plpgsql
		AS $$
		BEGIN
			IF NEW.topic = 'secret.revoked' THEN
				RAISE EXCEPTION 'reject secret revoke outbox';
			END IF;
			RETURN NEW;
		END;
		$$;
	`); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(t.Context(), `
		CREATE TRIGGER reject_secret_revoke_outbox
		BEFORE INSERT ON outbox_messages
		FOR EACH ROW EXECUTE FUNCTION reject_secret_revoke_outbox();
	`); err != nil {
		t.Fatal(err)
	}
	rollbackID := pgvalue.MustUUIDValue(rollbackSecret.ID)
	if _, err := store.Revoke(t.Context(), environmentID, rollbackID, "revoke-rollback"); err == nil {
		t.Fatal("expected revocation to roll back")
	}
	var rollbackState string
	var rollbackGeneration int64
	if err := pool.QueryRow(t.Context(), `
		SELECT state, revocation_generation
		  FROM secrets
		 WHERE environment_id = $1
		   AND id = $2
	`, environmentID, rollbackID).Scan(&rollbackState, &rollbackGeneration); err != nil {
		t.Fatal(err)
	}
	if rollbackState != "active" || rollbackGeneration != 0 {
		t.Fatalf("rolled-back secret = %s/%d", rollbackState, rollbackGeneration)
	}
	assertSecretRevokeCounts(t, pool, environmentID, 1, 1)
}

func assertSecretRevokeCounts(t *testing.T, pool *pgxpool.Pool, environmentID uuid.UUID, claims int, outbox int) {
	t.Helper()
	var claimCount int
	if err := pool.QueryRow(t.Context(), `
		SELECT count(*)
		  FROM idempotency_claims
		 WHERE environment_id = $1
		   AND operation = 'secret.revoke'
		   AND state = 'completed'
	`, environmentID).Scan(&claimCount); err != nil {
		t.Fatal(err)
	}
	var outboxCount int
	if err := pool.QueryRow(t.Context(), `
		SELECT count(*)
		  FROM outbox_messages
		 WHERE topic = 'secret.revoked'
	`).Scan(&outboxCount); err != nil {
		t.Fatal(err)
	}
	if claimCount != claims || outboxCount != outbox {
		t.Fatalf("claims/outbox = %d/%d, want %d/%d", claimCount, outboxCount, claims, outbox)
	}
}

func seedSecretHashAuthority(t *testing.T, pool *pgxpool.Pool) keyedhash.Keyring {
	t.Helper()
	hashes, err := keyedhash.New(map[int32][]byte{1: makeKey(10)})
	if err != nil {
		t.Fatal(err)
	}
	fingerprint, err := hashes.Fingerprint(1)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(t.Context(), `
		INSERT INTO lookup_hmac_versions (version, key_fingerprint, is_current)
		VALUES (1, $1, true)
	`, fingerprint[:]); err != nil {
		t.Fatal(err)
	}
	return hashes
}

func seedSecretEnvironment(t *testing.T, pool *pgxpool.Pool) (uuid.UUID, uuid.UUID, uuid.UUID) {
	t.Helper()
	orgID := uuid.Must(uuid.NewV7())
	projectID := uuid.Must(uuid.NewV7())
	environmentID := uuid.Must(uuid.NewV7())
	regionID := "secret-" + environmentID.String()
	if _, err := pool.Exec(t.Context(), `
		INSERT INTO organizations (id, public_id, name, slug)
		VALUES ($1, $2, 'Secrets', $3)
	`, orgID, secretPublicID("org_", orgID), "secrets-"+orgID.String()); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(t.Context(), `
		INSERT INTO regions (id, provider, provider_region, display_name)
		VALUES ($1, 'test', $1, 'Secrets')
	`, regionID); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(t.Context(), `
		INSERT INTO projects (id, public_id, org_id, default_region_id, slug, name)
		VALUES ($1, $2, $3, $4, $5, 'Secrets')
	`, projectID, secretPublicID("prj_", projectID), orgID, regionID, "secrets-"+projectID.String()); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(t.Context(), `
		INSERT INTO environments (id, public_id, org_id, project_id, slug, name, color_hex)
		VALUES ($1, $2, $3, $4, 'production', 'Production', '#000000')
	`, environmentID, secretPublicID("env_", environmentID), orgID, projectID); err != nil {
		t.Fatal(err)
	}
	return orgID, projectID, environmentID
}

func secretPublicID(prefix string, id uuid.UUID) string {
	return prefix + strings.ToLower(base32.StdEncoding.WithPadding(base32.NoPadding).EncodeToString(id[:]))
}

func openSecretPostgres(t *testing.T) *pgxpool.Pool {
	t.Helper()
	for _, name := range []string{"initdb", "pg_ctl", "postgres"} {
		if _, err := exec.LookPath(name); err != nil {
			t.Skipf("%s not found; skipping PostgreSQL Secret test", name)
		}
	}
	tmp := t.TempDir()
	dataDir := filepath.Join(tmp, "data")
	if output, err := exec.Command("initdb", "-D", dataDir, "-A", "trust").CombinedOutput(); err != nil {
		t.Fatalf("initdb: %v\n%s", err, output)
	}
	port := freeSecretPostgresPort(t)
	logPath := filepath.Join(tmp, "postgres.log")
	command := exec.Command("pg_ctl", "-D", dataDir, "-l", logPath, "-o", fmt.Sprintf("-p %d -c listen_addresses=127.0.0.1", port), "-w", "start")
	if output, err := command.CombinedOutput(); err != nil {
		t.Fatalf("pg_ctl start: %v\n%s", err, output)
	}
	t.Cleanup(func() {
		_ = exec.Command("pg_ctl", "-D", dataDir, "-m", "fast", "-w", "stop").Run()
	})
	dsn := fmt.Sprintf("postgres://%s@127.0.0.1:%d/postgres?sslmode=disable", os.Getenv("USER"), port)
	if err := schema.Up(t.Context(), dsn); err != nil {
		t.Fatal(err)
	}
	pool, err := pgxpool.New(t.Context(), dsn)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(pool.Close)
	return pool
}

func freeSecretPostgresPort(t *testing.T) int {
	t.Helper()
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer listener.Close()
	return listener.Addr().(*net.TCPAddr).Port
}
