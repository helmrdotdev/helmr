package controlplane

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"github.com/helmrdotdev/helmr/internal/api"
	"github.com/helmrdotdev/helmr/internal/db/dbtest"
	"github.com/helmrdotdev/helmr/internal/workerapi"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"
	"uuid"

	"github.com/helmrdotdev/helmr/internal/db"
	"github.com/helmrdotdev/helmr/internal/pgvalue"
	"github.com/helmrdotdev/helmr/internal/secret"
	"github.com/jackc/pgx/v5/pgxpool"
)

func testWorkspaceCAStore(t *testing.T, pool *pgxpool.Pool) *secret.Store {
	t.Helper()
	store, err := secret.New(db.New(pool), pool, bytes.Repeat([]byte{71}, 32))
	if err != nil {
		t.Fatal(err)
	}
	return store
}

// Direct runtime fixtures deliberately attach protected bindings after their raw
// Workspace insert. Production creation performs this write inside its insert tx.
func createTestWorkspaceCA(t *testing.T, pool *pgxpool.Pool, store *secret.Store, environmentID, workspaceID uuid.UUID) secret.ProxyTrust {
	t.Helper()
	var created time.Time
	if err := pool.QueryRow(t.Context(), "SELECT created_at FROM workspaces WHERE id=$1", workspaceID).Scan(&created); err != nil {
		t.Fatal(err)
	}
	trust, err := store.GenerateProxyTrust(environmentID, workspaceID, created)
	if err != nil {
		t.Fatal(err)
	}
	n, err := db.New(pool).InitializeWorkspaceSecretCA(t.Context(), db.InitializeWorkspaceSecretCAParams{
		EnvironmentID: pgvalue.UUID(environmentID), WorkspaceID: pgvalue.UUID(workspaceID), Certificate: trust.Certificate,
		PrivateKeyNonce: trust.PrivateKeyNonce, PrivateKeyCiphertext: trust.PrivateKeyCiphertext, NotAfter: pgvalue.Timestamptz(trust.NotAfter),
	})
	if err != nil || n != 1 {
		t.Fatalf("initialize fixture CA: %d %v", n, err)
	}
	return trust
}

func TestWorkspaceCACreationRoutesAndRollback(t *testing.T) {
	for _, pinned := range []bool{false, true} {
		for _, mode := range []string{"protected", "mixed", "raw", "none", "missing-store", "generation-failure", "after-generation-failure"} {
			t.Run(fmt.Sprintf("pinned=%v/%s", pinned, mode), func(t *testing.T) {
				f := newActorStartPostgresFixture(t, 1)
				f.server.secretProxy = testWorkspaceCAStore(t, f.pool)
				request := workspaceCreateRequest{OrgID: f.orgID, ProjectID: f.projectID, EnvironmentID: f.environmentID,
					Declaration: workspaceDeclarationSelector{Kind: workspaceDeclarationPromoted}, DeclaredID: "workspace.v1", IdempotencyKey: "ca-create"}
				if pinned {
					started, err := f.server.startActor(t.Context(), f.request(0, nil, "ca-parent"))
					if err != nil {
						t.Fatal(err)
					}
					request.Declaration = workspaceDeclarationSelector{Kind: workspaceDeclarationRunPinned, RunID: started.BootRunID}
					request.Authorize = func(context.Context, db.Querier) error { return nil }
				}
				protected := api.WorkspaceSecret{Name: "API_TOKEN", Env: &api.SecretEnv{Name: "TOKEN", Mode: "protected", AllowedOrigins: []string{"https://example.com"}}}
				raw := api.WorkspaceSecret{Name: "API_TOKEN", Env: &api.SecretEnv{Name: "RAW", Mode: "raw"}}
				switch mode {
				case "none":
				case "raw":
					request.Secrets = []api.WorkspaceSecret{raw}
				case "mixed":
					request.Secrets = []api.WorkspaceSecret{protected, raw, {Name: "API_TOKEN", File: &api.SecretFile{Path: "/run/secrets/key"}}}
				default:
					request.Secrets = []api.WorkspaceSecret{protected}
				}
				if mode == "missing-store" {
					f.server.secretProxy = nil
				}
				if mode == "generation-failure" {
					f.server.secretProxy = &secret.Store{}
				}
				if mode == "after-generation-failure" {
					dbtest.MustExec(t, t.Context(), f.pool, `CREATE FUNCTION reject_ca_binding() RETURNS trigger LANGUAGE plpgsql AS $$ BEGIN
        IF NOT EXISTS (SELECT 1 FROM workspaces WHERE id=NEW.workspace_id AND secret_ca_certificate IS NOT NULL) THEN RAISE EXCEPTION 'CA not generated'; END IF;
        RAISE EXCEPTION 'synthetic post-generation failure'; END $$;
        CREATE TRIGGER reject_ca_binding BEFORE INSERT ON workspace_secrets FOR EACH ROW EXECUTE FUNCTION reject_ca_binding();`)
				}
				var before int
				if err := f.pool.QueryRow(t.Context(), "SELECT count(*) FROM workspaces").Scan(&before); err != nil {
					t.Fatal(err)
				}
				result, err := f.server.createWorkspace(t.Context(), request)
				fails := strings.Contains(mode, "failure") || mode == "missing-store"
				if fails {
					if err == nil {
						t.Fatal("creation unexpectedly succeeded")
					}
					if mode == "after-generation-failure" && !strings.Contains(err.Error(), "synthetic post-generation failure") {
						t.Fatal(err)
					}
					var after int
					if err := f.pool.QueryRow(t.Context(), "SELECT count(*) FROM workspaces").Scan(&after); err != nil {
						t.Fatal(err)
					}
					if before != after {
						t.Fatal("failed transaction retained Workspace")
					}
					if mode == "after-generation-failure" {
						dbtest.MustExec(t, t.Context(), f.pool, "DROP TRIGGER reject_ca_binding ON workspace_secrets")
					}
					f.server.secretProxy = testWorkspaceCAStore(t, f.pool)
					result, err = f.server.createWorkspace(t.Context(), request)
					if err != nil {
						t.Fatalf("rolled-back receipt blocked retry: %v", err)
					}
				} else if err != nil {
					t.Fatal(err)
				}
				ca, err := db.New(f.pool).GetWorkspaceSecretCAPublic(t.Context(), db.GetWorkspaceSecretCAPublicParams{EnvironmentID: pgvalue.UUID(f.environmentID), WorkspaceID: pgvalue.UUID(result.WorkspaceID)})
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
				replay, err := f.server.createWorkspace(t.Context(), request)
				if err != nil || !replay.Replayed || replay.WorkspaceID != result.WorkspaceID {
					t.Fatalf("replay: %+v %v", replay, err)
				}
			})
		}
	}
}

func TestWorkspaceCAConcurrentIdempotentCreation(t *testing.T) {
	f := newActorStartPostgresFixture(t, 1)
	f.server.secretProxy = testWorkspaceCAStore(t, f.pool)
	request := workspaceCreateRequest{OrgID: f.orgID, ProjectID: f.projectID, EnvironmentID: f.environmentID, Declaration: workspaceDeclarationSelector{Kind: workspaceDeclarationPromoted}, DeclaredID: "workspace.v1", IdempotencyKey: "same-ca", Secrets: []api.WorkspaceSecret{{Name: "API_TOKEN", Env: &api.SecretEnv{Name: "TOKEN", Mode: "protected", AllowedOrigins: []string{"https://example.com"}}}}}
	var wg sync.WaitGroup
	results := make(chan workspaceCreateResult, 8)
	errs := make(chan error, 8)
	for range 8 {
		wg.Go(func() { r, e := f.server.createWorkspace(t.Context(), request); results <- r; errs <- e })
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
			first = r.WorkspaceID
		}
		if r.WorkspaceID != first {
			t.Fatal("duplicate Workspace")
		}
	}
	var count int
	if err := f.pool.QueryRow(t.Context(), "SELECT count(*) FROM workspaces WHERE secret_ca_certificate IS NOT NULL").Scan(&count); err != nil || count != 1 {
		t.Fatalf("CA count %d: %v", count, err)
	}
}

func TestWorkspaceCAMalformedPreparationFailsWithoutWrites(t *testing.T) {
	for _, mutation := range []string{"missing", "certificate", "nonce", "ciphertext"} {
		t.Run(mutation, func(t *testing.T) {
			f := newSnapshotFixture(t, 1, true)
			switch mutation {
			case "missing":
				dbtest.MustExec(t, t.Context(), f.fixture.Pool, "UPDATE workspaces SET secret_ca_certificate=NULL,secret_ca_not_after=NULL,secret_ca_private_key_nonce=NULL,secret_ca_private_key_ciphertext=NULL WHERE id=$1", f.workspace)
			case "certificate":
				dbtest.MustExec(t, t.Context(), f.fixture.Pool, "UPDATE workspaces SET secret_ca_certificate=$2 WHERE id=$1", f.workspace, []byte("invalid"))
			case "nonce":
				dbtest.MustExec(t, t.Context(), f.fixture.Pool, "UPDATE workspaces SET secret_ca_private_key_nonce=$2 WHERE id=$1", f.workspace, []byte("invalid"))
			case "ciphertext":
				dbtest.MustExec(t, t.Context(), f.fixture.Pool, "UPDATE workspaces SET secret_ca_private_key_ciphertext=$2 WHERE id=$1", f.workspace, []byte("invalid"))
			}
			var before, after string
			if err := f.fixture.Pool.QueryRow(t.Context(), "SELECT xmin::text FROM workspaces WHERE id=$1", f.workspace).Scan(&before); err != nil {
				t.Fatal(err)
			}
			for range 2 {
				if r := f.invoke(t.Context(), false); r.Code == 200 {
					t.Fatal("malformed CA prepared")
				}
			}
			if err := f.fixture.Pool.QueryRow(t.Context(), "SELECT xmin::text FROM workspaces WHERE id=$1", f.workspace).Scan(&after); err != nil {
				t.Fatal(err)
			}
			if before != after {
				t.Fatal("preparation repaired CA")
			}
		})
	}
	f := newSnapshotFixture(t, 1, false)
	for i, statement := range []string{
		"UPDATE workspaces SET secret_ca_certificate=$2 WHERE id=$1",
		"UPDATE workspaces SET secret_ca_certificate=$2,secret_ca_private_key_nonce=$2,secret_ca_private_key_ciphertext=$2,secret_ca_not_after=now() WHERE id=$1",
	} {
		material := []byte("nonempty partial material")
		if i == 1 {
			material = []byte{}
		}
		if _, err := f.fixture.Pool.Exec(t.Context(), statement, f.workspace, material); err == nil {
			t.Fatal("partial/empty material admitted")
		}
	}
}

func TestWorkspaceCAGuestCreateIngress(t *testing.T) {
	for _, mode := range []string{"protected", "mixed", "raw", "none"} {
		t.Run(mode, func(t *testing.T) {
			f := newSnapshotFixture(t, 1, true)
			dbtest.MustExec(t, t.Context(), f.fixture.Pool, "UPDATE runs SET status='running',started_at=now(),active_started_at=now() WHERE id=$1", f.run.RunID)
			dbtest.MustExec(t, t.Context(), f.fixture.Pool, "UPDATE run_leases SET status='running',started_at=now() WHERE id=$1", f.run.LeaseID)
			dbtest.MustExec(t, t.Context(), f.fixture.Pool, "UPDATE run_attempts SET entrypoint_entered_at=now() WHERE run_id=$1", f.run.RunID)
			// Raw source authority permits the requested protected/raw subsets.
			dbtest.MustExec(t, t.Context(), f.fixture.Pool, "UPDATE workspace_secrets SET mode='raw',placeholder='',allowed_origins='{}' WHERE workspace_id=$1", f.workspace)
			bindings := []api.WorkspaceSecret{{Name: "token-a", Env: &api.SecretEnv{Name: "TOKEN", Mode: "protected", AllowedOrigins: []string{"https://example.com"}}}}
			switch mode {
			case "none":
				bindings = nil
			case "raw":
				bindings = []api.WorkspaceSecret{{Name: "token-a", Env: &api.SecretEnv{Name: "RAW", Mode: "raw"}}}
			case "mixed":
				bindings = append(bindings, api.WorkspaceSecret{Name: "token-a", Env: &api.SecretEnv{Name: "RAW", Mode: "raw"}}, api.WorkspaceSecret{Name: "token-a", File: &api.SecretFile{Path: "/run/secrets/key"}})
			}
			request := workerapi.CreateWorkspaceRequest{Lease: workerapi.RunLeaseFence{ID: f.run.LeaseID.String(), LeaseSequence: 1}, CorrelationID: uuid.NewV7().String(), SandboxDeclaredID: "test-workspace", Secrets: bindings, IdempotencyKey: "guest-ca"}
			var first string
			for range 2 {
				body, err := json.Marshal(request)
				if err != nil {
					t.Fatal(err)
				}
				r := httptest.NewRequest("POST", "/", bytes.NewReader(body)).WithContext(context.WithValue(t.Context(), workerContextKey{}, f.worker))
				response := httptest.NewRecorder()
				f.server.workerCreateWorkspace(response, r)
				var result workerapi.CreateWorkspaceResponse
				if err := json.Unmarshal(response.Body.Bytes(), &result); err != nil {
					t.Fatal(err)
				}
				if response.Code != 200 || result.Completed == nil {
					t.Fatalf("guest create: %d %s", response.Code, response.Body.String())
				}
				if first == "" {
					first = result.Completed.WorkspaceID
				}
				if first != result.Completed.WorkspaceID {
					t.Fatal("guest replay replaced Workspace")
				}
			}
			ca, err := f.q.GetWorkspaceSecretCAPublic(t.Context(), db.GetWorkspaceSecretCAPublicParams{EnvironmentID: pgvalue.UUID(f.fixture.EnvironmentID), WorkspaceID: pgvalue.UUID(uuid.MustParse(first))})
			if err != nil {
				t.Fatal(err)
			}
			if ca.NotAfter.Valid != (mode == "protected" || mode == "mixed") {
				t.Fatal("guest CA presence differs")
			}
		})
	}
}
