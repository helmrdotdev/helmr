package controlplane

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"
	"uuid"

	"github.com/helmrdotdev/helmr/internal/db/dbtest"
	"github.com/helmrdotdev/helmr/internal/secretbinding"
	"github.com/helmrdotdev/helmr/internal/workerapi"
	"github.com/jackc/pgx/v5"

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

// Direct runtime fixtures deliberately attach protected bindings after their raw
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

func TestComputerCACreationRoutesAndRollback(t *testing.T) {
	for _, pinned := range []bool{false, true} {
		for _, mode := range []string{"protected", "mixed", "raw", "none", "missing-store", "generation-failure", "after-generation-failure"} {
			t.Run(fmt.Sprintf("pinned=%v/%s", pinned, mode), func(t *testing.T) {
				f := newActorStartPostgresFixture(t, 1)
				f.server.secretProxy = testComputerCAStore(t, f.pool)
				request := computerCreateRequest{OrgID: f.orgID, ProjectID: f.projectID, EnvironmentID: f.environmentID,
					Declaration: computerDeclarationSelector{Kind: computerDeclarationPromoted}, DeclaredID: "computer.v1", IdempotencyKey: "ca-create"}
				if pinned {
					started, err := f.server.startActor(t.Context(), f.request(0, nil, "ca-parent"))
					if err != nil {
						t.Fatal(err)
					}
					request.Declaration = computerDeclarationSelector{Kind: computerDeclarationRunPinned, RunID: started.BootRunID}
					request.Authorize = func(context.Context, pgx.Tx) error { return nil }
				}
				protected := secretbinding.Binding{Name: "API_TOKEN", Env: &secretbinding.Env{Name: "TOKEN", Mode: "protected", AllowedOrigins: []string{"https://example.com"}}}
				raw := secretbinding.Binding{Name: "API_TOKEN", Env: &secretbinding.Env{Name: "RAW", Mode: "raw"}}
				switch mode {
				case "none":
				case "raw":
					request.Secrets = []secretbinding.Binding{raw}
				case "mixed":
					request.Secrets = []secretbinding.Binding{protected, raw, {Name: "API_TOKEN", File: &secretbinding.File{Path: "/run/secrets/key"}}}
				default:
					request.Secrets = []secretbinding.Binding{protected}
				}
				if mode == "missing-store" {
					f.server.secretProxy = nil
				}
				if mode == "generation-failure" {
					f.server.secretProxy = &secret.Store{}
				}
				if mode == "after-generation-failure" {
					dbtest.MustExec(t, t.Context(), f.pool, `CREATE FUNCTION reject_ca_binding() RETURNS trigger LANGUAGE plpgsql AS $$ BEGIN
        IF NOT EXISTS (SELECT 1 FROM computers WHERE id=NEW.computer_id AND secret_ca_certificate IS NOT NULL) THEN RAISE EXCEPTION 'CA not generated'; END IF;
        RAISE EXCEPTION 'synthetic post-generation failure'; END $$;
        CREATE TRIGGER reject_ca_binding BEFORE INSERT ON computer_secrets FOR EACH ROW EXECUTE FUNCTION reject_ca_binding();`)
				}
				var before int
				if err := f.pool.QueryRow(t.Context(), "SELECT count(*) FROM computers").Scan(&before); err != nil {
					t.Fatal(err)
				}
				result, err := f.server.createComputer(t.Context(), request)
				fails := strings.Contains(mode, "failure") || mode == "missing-store"
				if fails {
					if err == nil {
						t.Fatal("creation unexpectedly succeeded")
					}
					if mode == "after-generation-failure" && !strings.Contains(err.Error(), "synthetic post-generation failure") {
						t.Fatal(err)
					}
					var after int
					if err := f.pool.QueryRow(t.Context(), "SELECT count(*) FROM computers").Scan(&after); err != nil {
						t.Fatal(err)
					}
					if before != after {
						t.Fatal("failed transaction retained Computer")
					}
					if mode == "after-generation-failure" {
						dbtest.MustExec(t, t.Context(), f.pool, "DROP TRIGGER reject_ca_binding ON computer_secrets")
					}
					f.server.secretProxy = testComputerCAStore(t, f.pool)
					result, err = f.server.createComputer(t.Context(), request)
					if err != nil {
						t.Fatalf("rolled-back receipt blocked retry: %v", err)
					}
				} else if err != nil {
					t.Fatal(err)
				}
				ca, err := db.New(f.pool).GetComputerSecretCAPublic(t.Context(), db.GetComputerSecretCAPublicParams{EnvironmentID: pgvalue.UUID(f.environmentID), ComputerID: pgvalue.UUID(result.ComputerID)})
				if err != nil {
					t.Fatal(err)
				}
				hasCA := mode != "raw" && mode != "none"
				if (len(ca.Certificate) > 0) != hasCA || ca.NotAfter.Valid != hasCA {
					t.Fatal("CA presence differs from bindings")
				}
				if hasCA {
					if !ca.NotAfter.Time.Equal(result.Snapshot.CreatedAt.AddDate(10, 0, 0).Truncate(time.Second)) {
						t.Fatal("expiry not anchored to inserted timestamp")
					}
					if err := secret.ValidateProxyTrust(ca.Certificate, ca.NotAfter.Time, time.Now()); err != nil {
						t.Fatal(err)
					}
				}
				f.server.secretProxy = nil
				replay, err := f.server.createComputer(t.Context(), request)
				if err != nil || !replay.Replayed || replay.ComputerID != result.ComputerID {
					t.Fatalf("replay: %+v %v", replay, err)
				}
			})
		}
	}
}

func TestComputerCAConcurrentIdempotentCreation(t *testing.T) {
	f := newActorStartPostgresFixture(t, 1)
	f.server.secretProxy = testComputerCAStore(t, f.pool)
	request := computerCreateRequest{OrgID: f.orgID, ProjectID: f.projectID, EnvironmentID: f.environmentID, Declaration: computerDeclarationSelector{Kind: computerDeclarationPromoted}, DeclaredID: "computer.v1", IdempotencyKey: "same-ca", Secrets: []secretbinding.Binding{{Name: "API_TOKEN", Env: &secretbinding.Env{Name: "TOKEN", Mode: "protected", AllowedOrigins: []string{"https://example.com"}}}}}
	var wg sync.WaitGroup
	results := make(chan computerCreateResult, 8)
	errs := make(chan error, 8)
	for range 8 {
		wg.Go(func() { r, e := f.server.createComputer(t.Context(), request); results <- r; errs <- e })
	}
	wg.Wait()
	close(results)
	close(errs)
	for err := range errs {
		if err != nil {
			t.Fatal(err)
		}
	}
	var first uuid.UUID
	for r := range results {
		if first == uuid.Nil() {
			first = r.ComputerID
		}
		if r.ComputerID != first {
			t.Fatal("duplicate Computer")
		}
	}
	var count int
	if err := f.pool.QueryRow(t.Context(), "SELECT count(*) FROM computers WHERE secret_ca_certificate IS NOT NULL").Scan(&count); err != nil || count != 1 {
		t.Fatalf("CA count %d: %v", count, err)
	}
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
			request := workerapi.CreateComputerRequest{Lease: workerapi.RunLeaseFence{ID: f.run.LeaseID.String(), LeaseSequence: 1}, CorrelationID: uuid.NewV7().String(), SandboxDeclaredID: "test-computer", Secrets: bindings, IdempotencyKey: "guest-ca"}
			var first string
			for range 2 {
				body, err := json.Marshal(request)
				if err != nil {
					t.Fatal(err)
				}
				r := httptest.NewRequest("POST", "/", bytes.NewReader(body)).WithContext(context.WithValue(t.Context(), workerContextKey{}, f.worker))
				response := httptest.NewRecorder()
				f.server.workerCreateComputer(response, r)
				var result workerapi.CreateComputerResponse
				if err := json.Unmarshal(response.Body.Bytes(), &result); err != nil {
					t.Fatal(err)
				}
				if response.Code != 200 || result.Completed == nil {
					t.Fatalf("guest create: %d %s", response.Code, response.Body.String())
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
