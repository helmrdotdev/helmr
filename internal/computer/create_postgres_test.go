package computer

import (
	"bytes"
	"encoding/json"
	"errors"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"
	"uuid"

	"github.com/helmrdotdev/helmr/internal/db"
	"github.com/helmrdotdev/helmr/internal/db/dbtest"
	"github.com/helmrdotdev/helmr/internal/idempotency"
	"github.com/helmrdotdev/helmr/internal/pgvalue"
	"github.com/helmrdotdev/helmr/internal/secret"
	"github.com/helmrdotdev/helmr/internal/secretbinding"
)

func TestCreatePersistsCAWithCreationAndRollsBackFailures(t *testing.T) {
	for _, mode := range []string{"protected", "mixed", "raw", "none", "missing-store", "generation-failure", "after-generation-failure"} {
		t.Run(mode, func(t *testing.T) {
			f := newFixture(t)
			request := Request{Scope: f.scope, DeclaredID: declaredID, IdempotencyKey: "ca-create"}
			protected := protectedBinding("TOKEN", "https://example.com")
			switch mode {
			case "none":
			case "raw":
				request.Secrets = []secretbinding.Binding{rawBinding("RAW")}
			case "mixed":
				request.Secrets = []secretbinding.Binding{protected, rawBinding("RAW"), {Name: "API_TOKEN", File: &secretbinding.File{Path: "/run/secrets/key"}}}
			default:
				request.Secrets = []secretbinding.Binding{protected}
			}
			creator := f.creator()
			switch mode {
			case "missing-store":
				creator = NewCreator((*secret.Store)(nil))
			case "generation-failure":
				creator = NewCreator(&secret.Store{})
			case "after-generation-failure":
				dbtest.MustExec(t, t.Context(), f.Pool, `CREATE FUNCTION reject_ca_binding() RETURNS trigger LANGUAGE plpgsql AS $$ BEGIN
        IF NOT EXISTS (SELECT 1 FROM computers WHERE id=NEW.computer_id AND secret_ca_certificate IS NOT NULL) THEN RAISE EXCEPTION 'CA not generated'; END IF;
        RAISE EXCEPTION 'synthetic post-generation failure'; END $$;
        CREATE TRIGGER reject_ca_binding BEFORE INSERT ON computer_secrets FOR EACH ROW EXECUTE FUNCTION reject_ca_binding();`)
			}
			before := f.count(t, "SELECT count(*) FROM computers")
			result, err := creator.Create(t.Context(), f.Pool, request)
			if strings.Contains(mode, "failure") || mode == "missing-store" {
				if err == nil {
					t.Fatal("creation unexpectedly succeeded")
				}
				if mode == "after-generation-failure" && !strings.Contains(err.Error(), "synthetic post-generation failure") {
					t.Fatal(err)
				}
				if after := f.count(t, "SELECT count(*) FROM computers"); after != before {
					t.Fatal("failed transaction retained Computer")
				}
				if mode == "after-generation-failure" {
					dbtest.MustExec(t, t.Context(), f.Pool, "DROP TRIGGER reject_ca_binding ON computer_secrets")
				}
				if result, err = f.creator().Create(t.Context(), f.Pool, request); err != nil {
					t.Fatalf("rolled-back receipt blocked retry: %v", err)
				}
			} else if err != nil {
				t.Fatal(err)
			}
			ca, err := db.New(f.Pool).GetComputerSecretCAPublic(t.Context(), db.GetComputerSecretCAPublicParams{EnvironmentID: pgvalue.UUID(f.EnvironmentID), ComputerID: pgvalue.UUID(result.ComputerID)})
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
				protectedEnv, err := ReadProtectedEnv(t.Context(), db.New(f.Pool), pgvalue.UUID(f.EnvironmentID), pgvalue.UUID(result.ComputerID))
				if err != nil || protectedEnv == nil || protectedEnv.Env["TOKEN"] == "" || !bytes.Equal(protectedEnv.CA, ca.Certificate) {
					t.Fatalf("protected env = %+v, %v", protectedEnv, err)
				}
			}
			// A replay issues no CA, so it succeeds without an issuer.
			replay, err := NewCreator((*secret.Store)(nil)).Create(t.Context(), f.Pool, request)
			if err != nil || !replay.Replayed || replay.ComputerID != result.ComputerID {
				t.Fatalf("replay: %+v %v", replay, err)
			}
		})
	}
}

func TestCreateConcurrentIdempotentCreationIssuesOneCA(t *testing.T) {
	f := newFixture(t)
	request := Request{Scope: f.scope, DeclaredID: declaredID, IdempotencyKey: "same-ca", Secrets: []secretbinding.Binding{protectedBinding("TOKEN", "https://example.com")}}
	var wg sync.WaitGroup
	results := make(chan Created, 8)
	errs := make(chan error, 8)
	for range 8 {
		wg.Go(func() { r, e := f.creator().Create(t.Context(), f.Pool, request); results <- r; errs <- e })
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
	if count := f.count(t, "SELECT count(*) FROM computers WHERE secret_ca_certificate IS NOT NULL"); count != 1 {
		t.Fatalf("CA count %d", count)
	}
}

func TestCreatePersistsFixedMixedBindings(t *testing.T) {
	f := newFixture(t)
	request := Request{Scope: f.scope, DeclaredID: declaredID, IdempotencyKey: "mixed-protected", Secrets: []secretbinding.Binding{
		protectedBinding("GH_TOKEN", "HTTPS://API.GITHUB.COM:443/"),
		rawBinding("RAW_TOKEN"),
		{Name: "API_TOKEN", File: &secretbinding.File{Path: "/run/secrets/key"}},
	}}
	result, err := f.creator().Create(t.Context(), f.Pool, request)
	if err != nil {
		t.Fatal(err)
	}
	rows, err := db.New(f.Pool).ListComputerSecrets(t.Context(), pgvalue.UUID(result.ComputerID))
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 3 || len(result.Snapshot.Secrets) != 3 {
		t.Fatal("mixed bindings not persisted")
	}
	for _, row := range rows {
		if row.PlacementTarget == "GH_TOKEN" {
			if row.Mode != "protected" || len(row.AllowedOrigins) != 1 || row.AllowedOrigins[0] != "https://api.github.com" || len(row.Placeholder) != len("hlmr_protected_")+64 {
				t.Fatal("protected metadata or selector incorrect")
			}
		} else if row.Mode != "raw" || row.Placeholder != "" || len(row.AllowedOrigins) != 0 {
			t.Fatal("raw binding acquired protected state")
		}
		if row.SecretID != rows[0].SecretID {
			t.Fatal("bindings changed stable Secret identity")
		}
	}
	replay, err := f.creator().Create(t.Context(), f.Pool, request)
	if err != nil || !replay.Replayed || replay.ComputerID != result.ComputerID {
		t.Fatal("creation replay changed Computer")
	}
	created, err := json.Marshal(result.Snapshot)
	if err != nil {
		t.Fatal(err)
	}
	replayed, err := json.Marshal(replay.Snapshot)
	if err != nil || !bytes.Equal(created, replayed) {
		t.Fatalf("replayed snapshot %s differs from %s", replayed, created)
	}
	after, err := db.New(f.Pool).ListComputerSecrets(t.Context(), pgvalue.UUID(result.ComputerID))
	if err != nil || !reflect.DeepEqual(rows, after) {
		t.Fatal("creation replay changed fixed selectors")
	}
	if bytes.Contains(created, []byte("hlmr_protected_")) || bytes.Contains(created, []byte("ciphertext")) || bytes.Contains(created, []byte("private_key")) {
		t.Fatal("snapshot exposed private transport data")
	}
}

func TestCreateRejectsUnavailableInputsWithoutWrites(t *testing.T) {
	f := newFixture(t)
	key := "taken"
	if _, err := f.creator().Create(t.Context(), f.Pool, Request{Scope: f.scope, DeclaredID: declaredID, Key: &key}); err != nil {
		t.Fatal(err)
	}
	padded := " padded "
	var input InputError
	var keyConflict KeyConflictError
	var idempotencyConflict idempotency.ConflictError
	for name, test := range map[string]struct {
		request Request
		want    func(error) bool
	}{
		"invalid key":     {Request{Scope: f.scope, DeclaredID: declaredID, Key: &padded}, func(err error) bool { return errors.As(err, &input) }},
		"invalid binding": {Request{Scope: f.scope, DeclaredID: declaredID, Secrets: []secretbinding.Binding{{Name: "API_TOKEN"}}}, func(err error) bool { return errors.As(err, &input) }},
		"not deployed":    {Request{Scope: f.scope, DeclaredID: "absent-computer"}, func(err error) bool { return errors.Is(err, ErrNotDeployed) }},
		"missing secret": {Request{Scope: f.scope, DeclaredID: declaredID, Secrets: []secretbinding.Binding{{Name: "MISSING", Env: &secretbinding.Env{Name: "TOKEN", Mode: "raw"}}}}, func(err error) bool {
			return errors.Is(err, ErrSecretUnavailable)
		}},
		"key conflict": {Request{Scope: f.scope, DeclaredID: declaredID, Key: &key}, func(err error) bool { return errors.As(err, &keyConflict) && keyConflict.Key == key }},
	} {
		t.Run(name, func(t *testing.T) {
			before := f.count(t, "SELECT count(*) FROM computers")
			if _, err := f.creator().Create(t.Context(), f.Pool, test.request); !test.want(err) {
				t.Fatalf("error = %v", err)
			}
			if after := f.count(t, "SELECT count(*) FROM computers"); after != before {
				t.Fatal("rejected creation inserted a Computer")
			}
		})
	}
	first := Request{Scope: f.scope, DeclaredID: declaredID, IdempotencyKey: "fingerprinted"}
	if _, err := f.creator().Create(t.Context(), f.Pool, first); err != nil {
		t.Fatal(err)
	}
	changed := first
	changed.Secrets = []secretbinding.Binding{rawBinding("RAW")}
	if _, err := f.creator().Create(t.Context(), f.Pool, changed); !errors.As(err, &idempotencyConflict) {
		t.Fatalf("changed replay = %v", err)
	}
	dbtest.MustExec(t, t.Context(), f.Pool, `UPDATE idempotency_claims SET receipt=NULL,receipt_expires_at=now()-interval '1 day',receipt_pruned_at=now() WHERE operation='computer.create'`)
	var expired idempotency.ExpiredError
	if _, err := f.creator().Create(t.Context(), f.Pool, first); !errors.As(err, &expired) {
		t.Fatalf("pruned replay = %v", err)
	}
}

func TestReadRetainsCreationProvenanceWithoutSandboxDeclaration(t *testing.T) {
	f := newFixture(t)
	computerID := f.insertComputer(t, "provenance")
	dbtest.MustExec(t, t.Context(), f.Pool, `DELETE FROM deployment_definitions WHERE environment_id=$1 AND kind='sandbox'`, f.EnvironmentID)
	q := db.New(f.Pool)
	snapshot, err := Read(t.Context(), q, f.scope, computerID)
	if err != nil {
		t.Fatal(err)
	}
	if snapshot.DeploymentID != f.DeploymentID.String() || snapshot.SandboxID != declaredID || snapshot.Residency != "cold" ||
		len(snapshot.Secrets) != 1 || snapshot.Secrets[0].Env == nil || snapshot.Secrets[0].Env.Name != "API_TOKEN" {
		t.Fatalf("creation provenance: %+v", snapshot)
	}
	item, err := FindByKey(t.Context(), q, f.scope, "provenance")
	if err != nil || item.ID != computerID.String() || item.DeploymentID != snapshot.DeploymentID || item.SandboxID != snapshot.SandboxID {
		t.Fatalf("key provenance: %+v, %v", item, err)
	}
	if _, err := FindByKey(t.Context(), q, f.scope, "absent"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("absent key = %v", err)
	}
	if _, err := Read(t.Context(), q, f.scope, uuid.NewV7()); !errors.Is(err, ErrNotFound) {
		t.Fatalf("absent Computer = %v", err)
	}
	other := f.scope
	other.ProjectID = uuid.NewV7()
	if _, err := Read(t.Context(), q, other, computerID); !errors.Is(err, ErrNotFound) {
		t.Fatalf("Computer outside scope = %v", err)
	}
}

func TestListPagesNewestFirst(t *testing.T) {
	f := newFixture(t)
	ids := []uuid.UUID{f.insertComputer(t, "first"), f.insertComputer(t, "second"), f.insertComputer(t, "third")}
	q := db.New(f.Pool)
	page, err := List(t.Context(), q, f.scope, ListPage{Limit: 2})
	if err != nil || len(page.Items) != 2 || !page.More {
		t.Fatalf("first page = %+v, %v", page, err)
	}
	last := page.Items[1]
	rest, err := List(t.Context(), q, f.scope, ListPage{Limit: 2, After: &ListPosition{CreatedAt: last.CreatedAt, ID: uuid.MustParse(last.ID)}})
	if err != nil || len(rest.Items) != 1 || rest.More {
		t.Fatalf("second page = %+v, %v", rest, err)
	}
	seen := map[string]bool{page.Items[0].ID: true, page.Items[1].ID: true, rest.Items[0].ID: true}
	for _, id := range ids {
		if !seen[id.String()] {
			t.Fatalf("Computer %s missing from pages", id)
		}
	}
}
