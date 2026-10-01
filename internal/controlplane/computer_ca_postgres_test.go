package controlplane

import (
	"bytes"
	"net/http"
	"testing"
	"time"
	"uuid"

	"github.com/helmrdotdev/helmr/internal/db/dbtest"
	"github.com/helmrdotdev/helmr/internal/secretbinding"
	"github.com/helmrdotdev/helmr/internal/workerapi"

	"github.com/helmrdotdev/helmr/internal/db"
	"github.com/helmrdotdev/helmr/internal/pgvalue"
	"github.com/helmrdotdev/helmr/internal/secret"
	"github.com/jackc/pgx/v5/pgxpool"
)

func testComputerCAStore(t *testing.T, pool *pgxpool.Pool) *secret.Store {
	t.Helper()
	store, err := secret.New(db.New(pool), pool, bytes.Repeat([]byte{71}, 32))
	if err != nil {
		t.Fatal(err)
	}
	return store
}

// Direct instance fixtures deliberately attach protected bindings after their raw
// Computer insert. Production creation performs this write inside its insert tx.
func createTestComputerCA(t *testing.T, pool *pgxpool.Pool, store *secret.Store, environmentID, computerID uuid.UUID) secret.ProxyTrust {
	t.Helper()
	var created time.Time
	if err := pool.QueryRow(t.Context(), "SELECT created_at FROM computers WHERE id=$1", computerID).Scan(&created); err != nil {
		t.Fatal(err)
	}
	trust, err := store.GenerateProxyTrust(environmentID, computerID, created)
	if err != nil {
		t.Fatal(err)
	}
	n, err := db.New(pool).InitializeComputerSecretCA(t.Context(), db.InitializeComputerSecretCAParams{
		EnvironmentID: pgvalue.UUID(environmentID), ComputerID: pgvalue.UUID(computerID), Certificate: trust.Certificate,
		PrivateKeyNonce: trust.PrivateKeyNonce, PrivateKeyCiphertext: trust.PrivateKeyCiphertext, NotAfter: pgvalue.Timestamptz(trust.NotAfter),
	})
	if err != nil || n != 1 {
		t.Fatalf("initialize fixture CA: %d %v", n, err)
	}
	return trust
}

func TestComputerCAMalformedPreparationFailsWithoutWrites(t *testing.T) {
	for _, mutation := range []string{"missing", "certificate", "nonce", "ciphertext"} {
		t.Run(mutation, func(t *testing.T) {
			f := newSnapshotFixture(t, 1, true)
			switch mutation {
			case "missing":
				dbtest.MustExec(t, t.Context(), f.fixture.Pool, "UPDATE computers SET secret_ca_certificate=NULL,secret_ca_not_after=NULL,secret_ca_private_key_nonce=NULL,secret_ca_private_key_ciphertext=NULL WHERE id=$1", f.computer)
			case "certificate":
				dbtest.MustExec(t, t.Context(), f.fixture.Pool, "UPDATE computers SET secret_ca_certificate=$2 WHERE id=$1", f.computer, []byte("invalid"))
			case "nonce":
				dbtest.MustExec(t, t.Context(), f.fixture.Pool, "UPDATE computers SET secret_ca_private_key_nonce=$2 WHERE id=$1", f.computer, []byte("invalid"))
			case "ciphertext":
				dbtest.MustExec(t, t.Context(), f.fixture.Pool, "UPDATE computers SET secret_ca_private_key_ciphertext=$2 WHERE id=$1", f.computer, []byte("invalid"))
			}
			var before, after string
			if err := f.fixture.Pool.QueryRow(t.Context(), "SELECT xmin::text FROM computers WHERE id=$1", f.computer).Scan(&before); err != nil {
				t.Fatal(err)
			}
			for range 2 {
				if r := f.invoke(t.Context(), false); r.Code == 200 {
					t.Fatal("malformed CA prepared")
				}
			}
			if err := f.fixture.Pool.QueryRow(t.Context(), "SELECT xmin::text FROM computers WHERE id=$1", f.computer).Scan(&after); err != nil {
				t.Fatal(err)
			}
			if before != after {
				t.Fatal("preparation repaired CA")
			}
		})
	}
	f := newSnapshotFixture(t, 1, false)
	for i, statement := range []string{
		"UPDATE computers SET secret_ca_certificate=$2 WHERE id=$1",
		"UPDATE computers SET secret_ca_certificate=$2,secret_ca_private_key_nonce=$2,secret_ca_private_key_ciphertext=$2,secret_ca_not_after=now() WHERE id=$1",
	} {
		material := []byte("nonempty partial material")
		if i == 1 {
			material = []byte{}
		}
		if _, err := f.fixture.Pool.Exec(t.Context(), statement, f.computer, material); err == nil {
			t.Fatal("partial/empty material admitted")
		}
	}
}

func TestComputerCAGuestCreateIngress(t *testing.T) {
	for _, mode := range []string{"protected", "mixed", "raw", "none"} {
		t.Run(mode, func(t *testing.T) {
			f := newSnapshotFixture(t, 1, true)
			dbtest.MustExec(t, t.Context(), f.fixture.Pool, "UPDATE runs SET status='running',started_at=now(),active_started_at=now() WHERE id=$1", f.run.RunID)
			dbtest.MustExec(t, t.Context(), f.fixture.Pool, "UPDATE run_leases SET status='running',started_at=now() WHERE id=$1", f.run.LeaseID)
			dbtest.MustExec(t, t.Context(), f.fixture.Pool, "UPDATE run_attempts SET entrypoint_entered_at=now() WHERE run_id=$1", f.run.RunID)
			// Raw source authority permits the requested protected/raw subsets.
			dbtest.MustExec(t, t.Context(), f.fixture.Pool, "UPDATE computer_secrets SET mode='raw',placeholder='',allowed_origins='{}' WHERE computer_id=$1", f.computer)
			bindings := []secretbinding.Binding{{Name: "token-a", Env: &secretbinding.Env{Name: "TOKEN", Mode: "protected", AllowedOrigins: []string{"https://example.com"}}}}
			switch mode {
			case "none":
				bindings = nil
			case "raw":
				bindings = []secretbinding.Binding{{Name: "token-a", Env: &secretbinding.Env{Name: "RAW", Mode: "raw"}}}
			case "mixed":
				bindings = append(bindings, secretbinding.Binding{Name: "token-a", Env: &secretbinding.Env{Name: "RAW", Mode: "raw"}}, secretbinding.Binding{Name: "token-a", File: &secretbinding.File{Path: "/run/secrets/key"}})
			}
			handler := newPostgresServer(t, f.fixture.Pool, func(cfg *ServerConfig) { cfg.SecretProxy = f.store })
			worker := newWorkerHTTPClient(t, handler, f.fixture.Pool, f.fixture.WorkerID)
			request := workerapi.CreateComputerRequest{Lease: workerapi.RunLeaseFence{ID: f.run.LeaseID.String(), LeaseSequence: 1}, CorrelationID: uuid.NewV7().String(), SandboxDeclaredID: "test-computer", Secrets: bindings, IdempotencyKey: "guest-ca"}
			var first string
			for range 2 {
				var result workerapi.CreateComputerResponse
				worker.post(t, "/worker/v1/run/computers/create", request, http.StatusOK, &result)
				if result.Completed == nil {
					t.Fatalf("guest create: %+v", result)
				}
				if first == "" {
					first = result.Completed.ComputerID
				}
				if first != result.Completed.ComputerID {
					t.Fatal("guest replay replaced Computer")
				}
			}
			ca, err := f.q.GetComputerSecretCAPublic(t.Context(), db.GetComputerSecretCAPublicParams{EnvironmentID: pgvalue.UUID(f.fixture.EnvironmentID), ComputerID: pgvalue.UUID(uuid.MustParse(first))})
			if err != nil {
				t.Fatal(err)
			}
			if ca.NotAfter.Valid != (mode == "protected" || mode == "mixed") {
				t.Fatal("guest CA presence differs")
			}
		})
	}
}
