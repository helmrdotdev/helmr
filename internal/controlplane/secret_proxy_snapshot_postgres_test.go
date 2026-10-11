package controlplane

import (
	"bytes"
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"
	"uuid"

	"github.com/helmrdotdev/helmr/internal/agent"
	"github.com/helmrdotdev/helmr/internal/agent/agenttest"
	"github.com/helmrdotdev/helmr/internal/db"
	"github.com/helmrdotdev/helmr/internal/db/dbtest"
	"github.com/helmrdotdev/helmr/internal/pgvalue"
	"github.com/helmrdotdev/helmr/internal/secret"
	"github.com/helmrdotdev/helmr/internal/workerapi"
	"github.com/helmrdotdev/helmr/internal/workergroup"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
)

type snapshotFixture struct {
	fixture            agenttest.Fixture
	q                  *db.Queries
	store              *secret.Store
	server             *Server
	worker             workergroup.HostPrincipal
	computer, instance uuid.UUID
	markers            []string
	secrets            []pgtype.UUID
}

func newSnapshotFixture(t *testing.T, count int, root bool) *snapshotFixture {
	t.Helper()
	f := &snapshotFixture{fixture: agenttest.New(t)}
	f.q = db.New(f.fixture.Pool)
	var err error
	f.store, err = secret.New(f.q, f.fixture.Pool, bytes.Repeat([]byte{71}, 32))
	if err != nil {
		t.Fatal(err)
	}
	f.computer, f.instance = f.fixture.Computer, f.fixture.Computer
	f.worker = workergroup.HostPrincipal{HostID: f.fixture.Worker, GroupID: f.fixture.Group, Epoch: 1, HostClaimVersion: 1, GroupClaimVersion: 1}
	f.server = &Server{db: f.q, tx: f.fixture.Pool, secretProxy: f.store, log: slog.New(slog.NewTextHandler(io.Discard, nil))}
	dbtest.MustExec(t, t.Context(), f.fixture.Pool, "UPDATE worker_hosts SET observed_at=clock_timestamp() WHERE id=$1", f.fixture.Worker)
	for i := 0; i < count; i++ {
		name := string(rune('a' + i))
		record, err := f.store.Create(t.Context(), f.fixture.Environment, "token-"+name, []byte("old-"+name), "create-"+name)
		if err != nil {
			t.Fatal(err)
		}
		marker := "hlmr_protected_" + strings.Repeat(name, 64)
		f.markers = append(f.markers, secret.RuntimeSelector(f.fixture.Environment, "process", f.fixture.Session, 1, marker))
		f.secrets = append(f.secrets, record.ID)
		dbtest.MustExec(t, t.Context(), f.fixture.Pool, "INSERT INTO computer_secret_bindings(computer_id,environment_id,secret_id,placement_kind,placement_target,mode,allowed_origins,placeholder) VALUES($1,$2,$3,'env',$4,'protected',ARRAY['https://example.com'],$5)", f.computer, f.fixture.Environment, record.ID, "TOKEN_"+name, marker)
		dbtest.MustExec(t, t.Context(), f.fixture.Pool, `INSERT INTO secret_exposures(environment_id,session_id,process_epoch,secret_id,version_id,revocation_generation) SELECT $1,$2,1,id,current_version_id,revocation_generation FROM secrets WHERE id=$3`, f.fixture.Environment, f.fixture.Session, record.ID)
	}
	if root {
		createTestComputerCA(t, f.fixture.Pool, f.store, f.fixture.Environment, f.computer)
	}

	return f
}
func (f *snapshotFixture) capture(ctx context.Context, database db.DBTX) ([]secret.ProtectedCapture, error) {
	return agent.CaptureComputerProtectedSecrets(ctx, database, f.worker, f.instance, "https://example.com", f.markers)
}
func (f *snapshotFixture) invoke(ctx context.Context, resolve bool) *httptest.ResponseRecorder {
	body, _ := json.Marshal(workerapi.SecretProxyRequest{ComputerInstanceID: f.instance.String()})
	if resolve {
		body, _ = json.Marshal(workerapi.SecretProxyRequest{ComputerInstanceID: f.instance.String(), Origin: "https://example.com", Placeholders: f.markers})
	}
	req := httptest.NewRequest("POST", "/", bytes.NewReader(body)).WithContext(context.WithValue(ctx, workerContextKey{}, f.worker))
	response := httptest.NewRecorder()
	if resolve {
		f.server.workerResolveSecretProxy(response, req)
	} else {
		f.server.workerPrepareSecretProxy(response, req)
	}
	return response
}

// Hooks straddle the actual primary capture SELECT, preserving its statement snapshot.
type snapshotCaptureHook struct {
	db.TxDB
	before, after func()
}

func (h *snapshotCaptureHook) Query(ctx context.Context, sql string, args ...any) (pgx.Rows, error) {
	capture := strings.Contains(sql, "candidates AS")
	if capture && h.before != nil {
		h.before()
	}
	rows, err := h.TxDB.Query(ctx, sql, args...)
	if capture && h.after != nil {
		finished := false
		defer func() {
			if !finished && rows != nil {
				rows.Close()
			}
		}()
		h.after()
		finished = true
	}
	return rows, err
}
func TestProtectedSnapshotBeforeAndAfterTransitions(t *testing.T) {
	for _, when := range []string{"before", "after"} {
		for _, change := range []string{"rotate", "revoke", "epoch", "claims", "group", "computer", "process", "lease", "fence"} {
			t.Run(when+"/"+change, func(t *testing.T) {
				f := newSnapshotFixture(t, 1, true)
				mutate := func() {
					switch change {
					case "rotate":
						if _, err := f.store.Rotate(t.Context(), f.fixture.Environment, pgvalue.MustUUIDValue(f.secrets[0]), []byte("new-a"), "rotate"); err != nil {
							t.Fatal(err)
						}
					case "revoke":
						if _, err := f.store.Revoke(t.Context(), f.fixture.Environment, pgvalue.MustUUIDValue(f.secrets[0]), "revoke"); err != nil {
							t.Fatal(err)
						}
					case "epoch":
						dbtest.MustExec(t, t.Context(), f.fixture.Pool, "UPDATE worker_hosts SET current_epoch=2 WHERE id=$1", f.worker.HostID)
					case "claims":
						dbtest.MustExec(t, t.Context(), f.fixture.Pool, "UPDATE worker_hosts SET claim_version=2 WHERE id=$1", f.worker.HostID)
					case "group":
						dbtest.MustExec(t, t.Context(), f.fixture.Pool, "UPDATE worker_groups SET claim_version=2 WHERE id=$1", f.worker.GroupID)
					case "computer":
						dbtest.MustExec(t, t.Context(), f.fixture.Pool, "UPDATE computers SET deleted_at=clock_timestamp() WHERE id=$1", f.computer)
					case "process":
						dbtest.MustExec(t, t.Context(), f.fixture.Pool, "UPDATE session_processes SET fenced_at=clock_timestamp(),status='lost' WHERE session_id=$1", f.fixture.Session)
					case "lease":
						dbtest.MustExec(t, t.Context(), f.fixture.Pool, "UPDATE computer_leases SET expires_at=clock_timestamp()-interval '1 second' WHERE computer_instance_id=$1", f.instance)
					case "fence":
						dbtest.MustExec(t, t.Context(), f.fixture.Pool, "UPDATE computer_leases SET fenced_at=clock_timestamp(),fence_evidence='fixture VM stopped',status='lost' WHERE computer_instance_id=$1", f.instance)
					}
				}
				hook := &snapshotCaptureHook{TxDB: f.fixture.Pool}
				if when == "before" {
					hook.before = mutate
				} else {
					hook.after = mutate
				}
				f.server.tx = hook
				response := f.invoke(t.Context(), true)
				if when == "after" || change == "rotate" {
					if response.Code != 200 {
						t.Fatalf("authorized captured result denied: %s", response.Body.String())
					}
					var result workerapi.SecretProxyResolution
					if err := json.Unmarshal(response.Body.Bytes(), &result); err != nil {
						t.Fatal(err)
					}
					expected := "old-a"

					if string(result.Values[f.markers[0]]) != expected {
						t.Fatal("captured material changed")
					}
				} else if response.Code == 200 {
					t.Fatal("pre-statement denial released material")
				}
				f.server.tx = f.fixture.Pool
				next := f.invoke(t.Context(), true)
				if change != "rotate" && next.Code == 200 {
					t.Fatal("next statement used stale authorization")
				}
			})
		}
	}
}
func TestProtectedSnapshotCoverageAndExpiry(t *testing.T) {
	f := newSnapshotFixture(t, 2, true)
	for _, markers := range [][]string{nil, {f.markers[0], f.markers[0]}, {f.markers[0], "hlmr_protected_" + strings.Repeat("c", 64)}, {"forged"}} {
		rows, err := agent.CaptureComputerProtectedSecrets(t.Context(), f.fixture.Pool, f.worker, f.instance, "https://example.com", markers)
		if err == nil {
			if _, err = f.store.OpenProtectedCapture(rows, markers); err == nil {
				t.Fatal("incomplete coverage accepted")
			}
		}
	}
	if _, err := agent.CaptureComputerProtectedSecrets(t.Context(), f.fixture.Pool, f.worker, f.instance, "https://other.example.com", f.markers); err == nil {
		t.Fatal("wrong origin accepted")
	}
	if _, err := agent.CaptureComputerProtectedSecrets(t.Context(), f.fixture.Pool, f.worker, uuid.NewV7(), "https://example.com", f.markers); err == nil {
		t.Fatal("wrong instance accepted")
	}
	dbtest.MustExec(t, t.Context(), f.fixture.Pool, "UPDATE computer_leases SET expires_at=clock_timestamp()+interval '150 milliseconds' WHERE computer_instance_id=$1", f.instance)
	tx, err := f.fixture.Pool.Begin(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback(context.Background())
	var isolation string
	if err := tx.QueryRow(t.Context(), "SHOW transaction_isolation").Scan(&isolation); err != nil || isolation != "read committed" {
		t.Fatalf("isolation=%s %v", isolation, err)
	}
	time.Sleep(200 * time.Millisecond)
	rows, err := f.capture(t.Context(), tx)
	if err == nil || len(rows) != 0 {
		t.Fatalf("prior transaction clock authorized expired token: rows=%d err=%v", len(rows), err)
	}
}
func TestProtectedSnapshotPinsMultiSecretVersionsAcrossRotation(t *testing.T) {
	f := newSnapshotFixture(t, 2, true)
	for i, id := range f.secrets {
		if _, err := f.store.Rotate(t.Context(), f.fixture.Environment, pgvalue.MustUUIDValue(id), []byte("new"), "rotate-"+string(rune('a'+i))); err != nil {
			t.Fatal(err)
		}
	}
	rows, err := f.capture(t.Context(), f.fixture.Pool)
	if err != nil || len(rows) != 2 {
		t.Fatalf("capture=%v err=%v", rows, err)
	}
	values, err := f.store.OpenProtectedCapture(rows, f.markers)
	if err != nil || string(values[f.markers[0]]) != "old-a" || string(values[f.markers[1]]) != "old-b" {
		t.Fatalf("pins changed: %v", err)
	}
}
func TestProtectedPreparationReusesPersistedRoot(t *testing.T) {
	f := newSnapshotFixture(t, 1, true)
	var wg sync.WaitGroup
	failures := make(chan string, 8)
	preparations := make(chan workerapi.SecretProxyPreparation, 8)
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			r := f.invoke(t.Context(), false)
			if r.Code != 200 {
				failures <- r.Body.String()
			} else {
				var prep workerapi.SecretProxyPreparation
				if err := json.Unmarshal(r.Body.Bytes(), &prep); err != nil {
					failures <- err.Error()
				} else {
					preparations <- prep
				}
			}
		}()
	}
	wg.Wait()
	close(failures)
	for failure := range failures {
		t.Error(failure)
	}
	var certificatePEM []byte
	if err := f.fixture.Pool.QueryRow(t.Context(), "SELECT proxy_ca_certificate FROM computers WHERE id=$1", f.computer).Scan(&certificatePEM); err != nil {
		t.Fatal(err)
	}

	roots := x509.NewCertPool()
	roots.AppendCertsFromPEM(certificatePEM)
	close(preparations)
	for prep := range preparations {
		pair, err := tls.X509KeyPair(prep.Certificate, prep.PrivateKey)
		if err != nil {
			t.Fatal(err)
		}
		certificate, err := x509.ParseCertificate(pair.Certificate[0])
		if err != nil {
			t.Fatal(err)
		}
		if _, err := certificate.Verify(x509.VerifyOptions{Roots: roots, DNSName: "example.com"}); err != nil {
			t.Fatalf("signed with discarded root: %v", err)
		}
		clear(prep.PrivateKey)
	}

}
func TestProtectedSnapshotRootExpiryUsesCapturedStatementTime(t *testing.T) {
	f := newSnapshotFixture(t, 1, false)
	created := time.Now().AddDate(-10, 0, 0).Add(2 * time.Second)
	dbtest.MustExec(t, t.Context(), f.fixture.Pool, "UPDATE computers SET created_at=$2 WHERE id=$1", f.computer, created)
	root := createTestComputerCA(t, f.fixture.Pool, f.store, f.fixture.Environment, f.computer)
	rows, err := f.capture(t.Context(), f.fixture.Pool)
	if err != nil || len(rows) != 1 {
		t.Fatalf("capture before root expiry: %v", err)
	}
	tx, err := f.fixture.Pool.Begin(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback(context.Background())
	// Establish an older transaction timestamp without reusing it for authority.
	if _, err := tx.Exec(t.Context(), "SELECT transaction_timestamp()"); err != nil {
		t.Fatal(err)
	}
	time.Sleep(time.Until(root.NotAfter) + 50*time.Millisecond)
	if _, err := f.store.OpenProtectedCapture(rows, f.markers); err != nil {
		t.Fatalf("already captured request lost in-flight authority: %v", err)
	}
	after, err := f.capture(t.Context(), tx)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.store.OpenProtectedCapture(after, f.markers); !errors.Is(err, secret.ErrProxyTrustExpired) {
		t.Fatalf("expired root result=%v", err)
	}
}

func TestProtectedSnapshotMultipleBindingsAndRawExclusion(t *testing.T) {
	f := newSnapshotFixture(t, 1, true)
	alias := "hlmr_protected_" + strings.Repeat("d", 64)
	dbtest.MustExec(t, t.Context(), f.fixture.Pool, "INSERT INTO computer_secret_bindings(computer_id,environment_id,secret_id,placement_kind,placement_target,mode,allowed_origins,placeholder) VALUES($1,$2,$3,'env','ALIAS','protected',ARRAY['https://example.com'],$4)", f.computer, f.fixture.Environment, f.secrets[0], alias)
	alias = secret.RuntimeSelector(f.fixture.Environment, "process", f.fixture.Session, 1, alias)
	f.markers = append(f.markers, alias)
	rows, err := f.capture(t.Context(), f.fixture.Pool)
	if err != nil {
		t.Fatal(err)
	}
	values, err := f.store.OpenProtectedCapture(rows, f.markers)
	if err != nil || len(values) != 2 || !bytes.Equal(values[f.markers[0]], values[alias]) {
		t.Fatalf("same Secret aliases: %v", err)
	}
	// Duplicate material rows must fail complete coverage before any decryption.
	if _, err := f.store.OpenProtectedCapture([]secret.ProtectedCapture{rows[0], rows[0]}, f.markers); err == nil {
		t.Fatal("duplicate result row accepted")
	}
	dbtest.MustExec(t, t.Context(), f.fixture.Pool, "UPDATE computer_secret_bindings SET mode='raw',allowed_origins='{}',placeholder=NULL WHERE computer_id=$1 AND placement_target='ALIAS'", f.computer)
	rows, err = f.capture(t.Context(), f.fixture.Pool)
	if err == nil {
		if _, err := f.store.OpenProtectedCapture(rows, f.markers); err == nil {
			t.Fatal("raw binding resolved as protected")
		}
	}
}

func TestSecretProxyHTTPRejectsUnactivatedHostAndUnknownInstance(t *testing.T) {
	f := newSnapshotFixture(t, 1, true)
	if response := f.invoke(t.Context(), false); response.Code != http.StatusOK {
		t.Fatalf("prepare=%d %s", response.Code, response.Body.String())
	}
	dbtest.MustExec(t, t.Context(), f.fixture.Pool, `UPDATE worker_hosts SET status='registering',activated_at=NULL WHERE id=$1`, f.fixture.Worker)
	for _, resolve := range []bool{false, true} {
		if response := f.invoke(t.Context(), resolve); response.Code != http.StatusConflict {
			t.Fatalf("registering host resolve=%v status=%d", resolve, response.Code)
		}
	}
	dbtest.MustExec(t, t.Context(), f.fixture.Pool, `UPDATE worker_hosts SET status='active',activated_at=clock_timestamp() WHERE id=$1`, f.fixture.Worker)
	f.instance = uuid.NewV7()
	for _, resolve := range []bool{false, true} {
		if response := f.invoke(t.Context(), resolve); response.Code != http.StatusConflict {
			t.Fatalf("unknown instance resolve=%v status=%d", resolve, response.Code)
		}
	}
}
