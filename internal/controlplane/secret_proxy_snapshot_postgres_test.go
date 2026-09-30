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

	"github.com/helmrdotdev/helmr/internal/api"
	"github.com/helmrdotdev/helmr/internal/computer"
	"github.com/helmrdotdev/helmr/internal/db"
	"github.com/helmrdotdev/helmr/internal/db/dbtest"
	"github.com/helmrdotdev/helmr/internal/idempotency"
	"github.com/helmrdotdev/helmr/internal/pgvalue"
	"github.com/helmrdotdev/helmr/internal/run/runtest"
	"github.com/helmrdotdev/helmr/internal/secret"
	"github.com/helmrdotdev/helmr/internal/secretbinding"
	"github.com/helmrdotdev/helmr/internal/workerapi"
	"github.com/helmrdotdev/helmr/internal/workergroup"
	"github.com/jackc/pgx/v5/pgtype"
)

type snapshotFixture struct {
	fixture           runtest.Fixture
	q                 *db.Queries
	store             *secret.Store
	server            *Server
	worker            workergroup.HostPrincipal
	computer, runtime uuid.UUID
	run               runtest.RunLease
	markers           []string
	secrets           []pgtype.UUID
}

func newSnapshotFixture(t *testing.T, count int, root bool) *snapshotFixture {
	t.Helper()
	f := &snapshotFixture{fixture: runtest.New(t)}
	f.run = f.fixture.AddRunLease(t, "starting", time.Now().Add(-time.Minute))
	f.q = db.New(f.fixture.Pool)
	var err error
	f.store, err = secret.New(f.q, f.fixture.Pool, bytes.Repeat([]byte{71}, 32))
	if err != nil {
		t.Fatal(err)
	}
	if err := f.fixture.Pool.QueryRow(t.Context(), "SELECT r.computer_id,r.id FROM computer_instances r JOIN run_leases l ON l.computer_instance_id=r.id WHERE l.id=$1", f.run.LeaseID).Scan(&f.computer, &f.runtime); err != nil {
		t.Fatal(err)
	}
	f.worker = workergroup.HostPrincipal{HostID: f.fixture.WorkerID, GroupID: runtest.WorkerGroupID, Epoch: 1, HostClaimVersion: 1, GroupClaimVersion: 1}
	f.server = &Server{db: f.q, tx: f.fixture.Pool, secretProxy: f.store, computers: computer.NewCreator(f.store), log: slog.New(slog.NewTextHandler(io.Discard, nil))}
	dbtest.MustExec(t, t.Context(), f.fixture.Pool, "UPDATE computer_instances SET guest_channel_token_hash=decode(repeat('01',32),'hex'),guest_channel_token_expires_at=now()+interval '10 minutes' WHERE id=$1", f.runtime)
	for i := 0; i < count; i++ {
		name := string(rune('a' + i))
		record, err := f.store.Create(t.Context(), f.fixture.EnvironmentID, "token-"+name, []byte("old-"+name), "create-"+name)
		if err != nil {
			t.Fatal(err)
		}
		marker := "hlmr_protected_" + strings.Repeat(name, 64)
		f.markers = append(f.markers, marker)
		f.secrets = append(f.secrets, record.ID)
		dbtest.MustExec(t, t.Context(), f.fixture.Pool, "INSERT INTO computer_secrets(computer_id,environment_id,secret_id,placement_kind,placement_target,mode,allowed_origins,placeholder) VALUES($1,$2,$3,'env',$4,'protected',ARRAY['https://example.com'],$5)", f.computer, f.fixture.EnvironmentID, record.ID, "TOKEN_"+name, marker)
	}
	if root {
		createTestComputerCA(t, f.fixture.Pool, f.store, f.fixture.EnvironmentID, f.computer)
	}

	return f
}
func (f *snapshotFixture) params() db.CaptureProtectedSecretEnvelopesParams {
	return db.CaptureProtectedSecretEnvelopesParams{ComputerInstanceID: pgvalue.UUID(f.runtime), WorkerHostID: pgvalue.UUID(f.worker.HostID), WorkerEpoch: 1, WorkerGroupID: pgvalue.UUID(f.worker.GroupID), ClaimVersion: 1, GroupClaimVersion: 1, Origin: "https://example.com", Placeholders: f.markers}
}
func (f *snapshotFixture) invoke(ctx context.Context, resolve bool) *httptest.ResponseRecorder {
	body, _ := json.Marshal(workerapi.SecretProxyRequest{ComputerInstanceID: f.runtime.String()})
	if resolve {
		body, _ = json.Marshal(workerapi.SecretProxyRequest{ComputerInstanceID: f.runtime.String(), Origin: "https://example.com", Placeholders: f.markers})
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

// Only capture is implemented: resolution must not issue another material read.
type snapshotCaptureHook struct {
	db.Querier
	q             *db.Queries
	before, after func()
}

func (h *snapshotCaptureHook) CaptureProtectedSecretEnvelopes(ctx context.Context, p db.CaptureProtectedSecretEnvelopesParams) ([]db.CaptureProtectedSecretEnvelopesRow, error) {
	if h.before != nil {
		h.before()
	}
	rows, err := h.q.CaptureProtectedSecretEnvelopes(ctx, p)
	if h.after != nil {
		h.after()
	}
	return rows, err
}
func TestProtectedSnapshotBeforeAndAfterTransitions(t *testing.T) {
	for _, when := range []string{"before", "after"} {
		for _, change := range []string{"rotate", "revoke", "epoch", "claims", "group", "computer", "runtime", "lease", "token", "writer"} {
			t.Run(when+"/"+change, func(t *testing.T) {
				f := newSnapshotFixture(t, 1, true)
				mutate := func() {
					switch change {
					case "rotate":
						if _, err := f.store.Rotate(t.Context(), f.fixture.EnvironmentID, pgvalue.MustUUIDValue(f.secrets[0]), []byte("new-a"), "rotate"); err != nil {
							t.Fatal(err)
						}
					case "revoke":
						if _, err := f.store.Revoke(t.Context(), f.fixture.EnvironmentID, pgvalue.MustUUIDValue(f.secrets[0]), "revoke"); err != nil {
							t.Fatal(err)
						}
					case "epoch":
						dbtest.MustExec(t, t.Context(), f.fixture.Pool, "UPDATE worker_hosts SET current_epoch=2 WHERE id=$1", f.worker.HostID)
					case "claims":
						dbtest.MustExec(t, t.Context(), f.fixture.Pool, "UPDATE worker_hosts SET claim_version=2 WHERE id=$1", f.worker.HostID)
					case "group":
						dbtest.MustExec(t, t.Context(), f.fixture.Pool, "UPDATE worker_groups SET claim_version=2 WHERE id=$1", f.worker.GroupID)
					case "computer":
						dbtest.MustExec(t, t.Context(), f.fixture.Pool, "UPDATE computers SET desired_state='deleted' WHERE id=$1", f.computer)
					case "runtime":
						dbtest.MustExec(t, t.Context(), f.fixture.Pool, "UPDATE computer_instances SET desired_state='closed',desired_version=desired_version+1 WHERE id=$1", f.runtime)
					case "lease":
						dbtest.MustExec(t, t.Context(), f.fixture.Pool, "UPDATE computer_instances SET writer_expires_at=now()-interval '1 second' WHERE id=$1", f.runtime)
					case "token":
						dbtest.MustExec(t, t.Context(), f.fixture.Pool, "UPDATE computer_instances SET guest_channel_token_expires_at=now()-interval '1 second' WHERE id=$1", f.runtime)
					case "writer":
						dbtest.MustExec(t, t.Context(), f.fixture.Pool, "UPDATE computers SET writer_generation=writer_generation+1 WHERE id=$1", f.computer)
					}
				}
				hook := &snapshotCaptureHook{q: f.q}
				if when == "before" {
					hook.before = mutate
				} else {
					hook.after = mutate
				}
				f.server.db = hook
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
					if when == "before" {
						expected = "new-a"
					}
					if string(result.Values[f.markers[0]]) != expected {
						t.Fatal("captured material changed")
					}
				} else if response.Code == 200 {
					t.Fatal("pre-statement denial released material")
				}
				f.server.db = f.q
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
		p := f.params()
		p.Placeholders = markers
		rows, err := f.q.CaptureProtectedSecretEnvelopes(t.Context(), p)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := f.store.OpenProtected(rows, markers); err == nil {
			t.Fatal("incomplete selector coverage accepted")
		}
	}
	p := f.params()
	p.Origin = "https://other.example.com"
	rows, err := f.q.CaptureProtectedSecretEnvelopes(t.Context(), p)
	if err != nil || len(rows) != 0 {
		t.Fatalf("wrong origin: %v rows=%d", err, len(rows))
	}
	p = f.params()
	p.ComputerInstanceID = pgvalue.UUID(uuid.NewV7())
	rows, err = f.q.CaptureProtectedSecretEnvelopes(t.Context(), p)
	if err != nil || len(rows) != 0 {
		t.Fatal("wrong runtime accepted")
	}
	dbtest.MustExec(t, t.Context(), f.fixture.Pool, "UPDATE computer_instances SET guest_channel_token_expires_at=now()+interval '150 milliseconds' WHERE id=$1", f.runtime)
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
	rows, err = db.New(tx).CaptureProtectedSecretEnvelopes(t.Context(), f.params())
	if err != nil || len(rows) != 0 {
		t.Fatalf("prior transaction clock authorized expired token: rows=%d err=%v", len(rows), err)
	}
}
func TestProtectedSnapshotAtomicMultiSecretVersions(t *testing.T) {
	f := newSnapshotFixture(t, 2, true)
	old, err := f.q.CaptureProtectedSecretEnvelopes(t.Context(), f.params())
	if err != nil {
		t.Fatal(err)
	}
	for i, id := range f.secrets {
		if _, err := f.store.Rotate(t.Context(), f.fixture.EnvironmentID, pgvalue.MustUUIDValue(id), []byte("new"), "rotate-"+string(rune('a'+i))); err != nil {
			t.Fatal(err)
		}
	}
	newer, err := f.q.CaptureProtectedSecretEnvelopes(t.Context(), f.params())
	if err != nil {
		t.Fatal(err)
	}
	done := make(chan error, 1)
	go func() {
		for i := 0; i < 30; i++ {
			selected := old
			if i%2 == 0 {
				selected = newer
			}
			tx, err := f.fixture.Pool.Begin(t.Context())
			if err != nil {
				done <- err
				return
			}
			for _, r := range selected {
				if _, err = tx.Exec(t.Context(), "UPDATE secrets SET current_version_id=$2 WHERE id=$1", r.SecretID, r.VersionID); err != nil {
					break
				}
			}
			if err != nil {
				_ = tx.Rollback(context.Background())
				done <- err
				return
			}
			if err = tx.Commit(t.Context()); err != nil {
				done <- err
				return
			}
		}
		done <- nil
	}()
	for i := 0; i < 60; i++ {
		rows, err := f.q.CaptureProtectedSecretEnvelopes(t.Context(), f.params())
		if err != nil {
			t.Fatal(err)
		}
		if len(rows) != 2 || rows[0].Version != rows[1].Version {
			t.Fatal("mixed atomically committed versions")
		}
		if _, err := f.store.OpenProtected(rows, f.markers); err != nil {
			t.Fatal(err)
		}
	}
	if err := <-done; err != nil {
		t.Fatal(err)
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
	root, err := f.q.GetComputerSecretCAPublic(t.Context(), db.GetComputerSecretCAPublicParams{EnvironmentID: pgvalue.UUID(f.fixture.EnvironmentID), ComputerID: pgvalue.UUID(f.computer)})
	if err != nil {
		t.Fatal(err)
	}

	roots := x509.NewCertPool()
	roots.AppendCertsFromPEM(root.Certificate)
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
func TestProtectedGuestIngressCeilingsBeforeReplay(t *testing.T) {
	f := newSnapshotFixture(t, 1, true)
	dbtest.MustExec(t, t.Context(), f.fixture.Pool, "UPDATE runs SET status='running',started_at=now(),active_started_at=now() WHERE id=$1", f.run.RunID)
	dbtest.MustExec(t, t.Context(), f.fixture.Pool, "UPDATE run_leases SET status='running',started_at=now() WHERE id=$1", f.run.LeaseID)
	dbtest.MustExec(t, t.Context(), f.fixture.Pool, "UPDATE run_attempts SET entrypoint_entered_at=now() WHERE run_id=$1", f.run.RunID)
	other := f.fixture.AddRunLease(t, "starting", time.Now().Add(-time.Minute))
	var targetID uuid.UUID
	if err := f.fixture.Pool.QueryRow(t.Context(), "SELECT computer_id FROM runs WHERE id=$1", other.RunID).Scan(&targetID); err != nil {
		t.Fatal(err)
	}
	dbtest.MustExec(t, t.Context(), f.fixture.Pool, "INSERT INTO computer_secrets(computer_id,environment_id,secret_id,placement_kind,placement_target,mode) VALUES($1,$2,$3,'env','RAW_TOKEN','raw')", targetID, f.fixture.EnvironmentID, f.secrets[0])
	fence := workerapi.RunLeaseFence{ID: f.run.LeaseID.String(), LeaseSequence: 1}
	seed := func(request idempotency.Request, receipt any) {
		tx, err := f.fixture.Pool.Begin(t.Context())
		if err != nil {
			t.Fatal(err)
		}
		defer tx.Rollback(context.Background())
		claims, err := idempotency.TransactionFor(tx)
		if err != nil {
			t.Fatal(err)
		}
		acquired, err := claims.Acquire(t.Context(), request)
		if err != nil {
			t.Fatal(err)
		}
		body, err := json.Marshal(receipt)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := claims.Complete(t.Context(), acquired.Claim, body); err != nil {
			t.Fatal(err)
		}
		if err := tx.Commit(t.Context()); err != nil {
			t.Fatal(err)
		}
	}
	invoke := func(t *testing.T, handler http.HandlerFunc, request any) {
		raw, err := json.Marshal(request)
		if err != nil {
			t.Fatal(err)
		}
		r := httptest.NewRequest("POST", "/", bytes.NewReader(raw)).WithContext(context.WithValue(t.Context(), workerContextKey{}, f.worker))
		response := httptest.NewRecorder()
		handler(response, r)
		if !strings.Contains(response.Body.String(), "secret_unavailable") {
			t.Fatalf("ingress did not deny source ceiling: status=%d body=%s", response.Code, response.Body.String())
		}
	}
	t.Run("create-new-raw", func(t *testing.T) {
		invoke(t, f.server.workerCreateComputer, workerapi.CreateComputerRequest{Lease: fence, CorrelationID: uuid.NewV7().String(), SandboxDeclaredID: "test-computer", Secrets: []secretbinding.Binding{{Name: "token-a", Env: &secretbinding.Env{Name: "RAW", Mode: "raw"}}}, IdempotencyKey: "denied-create"})
	})
	t.Run("create-replayed-target", func(t *testing.T) {
		bindings := []secretbinding.Binding{{Name: "token-a", Env: &secretbinding.Env{Name: "TOKEN", Mode: "protected", AllowedOrigins: []string{"https://example.com"}}}}
		placements, err := secretbinding.NormalizedPlacements(bindings)
		if err != nil {
			t.Fatal(err)
		}
		encoded, _ := json.Marshal(placements)
		request, err := idempotency.NewRuntimeComputerCreateRequest(f.fixture.EnvironmentID, f.run.RunID, "test-computer", "replay-create", idempotency.ComputerCreateFingerprint{Secrets: encoded})
		if err != nil {
			t.Fatal(err)
		}
		now := time.Now()
		seed(request, struct {
			Computer api.ComputerSnapshot `json:"computer"`
		}{Computer: api.ComputerSnapshot{ID: targetID.String(), SandboxID: "test-computer", DeploymentID: f.fixture.DeploymentID.String(), Status: api.ComputerStatusAvailable, CreatedAt: now, UpdatedAt: now, LastActivityAt: now}})
		invoke(t, f.server.workerCreateComputer, workerapi.CreateComputerRequest{Lease: fence, CorrelationID: uuid.NewV7().String(), SandboxDeclaredID: "test-computer", Secrets: bindings, IdempotencyKey: "replay-create"})
	})
	t.Run("child-new-and-replayed", func(t *testing.T) {
		for _, replay := range []bool{false, true} {
			key := "denied-child"
			if replay {
				key = "replay-child"
			}
			target, _ := json.Marshal(api.ComputerIDTarget{ID: targetID.String()})
			request := workerapi.InvokeChildTaskRequest{Lease: fence, CorrelationID: uuid.NewV7().String(), TaskDeclaredID: "test-task", Method: "start", Computer: target, Options: json.RawMessage("{}"), IdempotencyKey: key}
			if replay {
				loc, err := f.q.GetLiveRunLeaseLocators(t.Context(), db.GetLiveRunLeaseLocatorsParams{ID: pgvalue.UUID(f.run.LeaseID), LeaseSequence: 1, WorkerGroupID: pgvalue.UUID(f.worker.GroupID), WorkerHostID: pgvalue.UUID(f.worker.HostID), WorkerEpoch: 1})
				if err != nil {
					t.Fatal(err)
				}
				normalized, err := normalizeWorkerChildTaskRequest(request, loc)
				if err != nil {
					t.Fatal(err)
				}
				claim, err := idempotency.NewTaskChildInvokeRequest(f.fixture.EnvironmentID, f.run.RunID, request.TaskDeclaredID, key, idempotency.TaskChildInvokeFingerprint{Method: request.Method, PayloadPresent: normalized.PayloadPresent, Payload: normalized.Payload, Computer: normalized.fingerprint.Computer, QueueName: normalized.QueueName, ConcurrencyKey: normalized.ConcurrencyKey, Priority: normalized.Priority, QueuedTTLMS: normalized.QueuedTTLMS, RetryPolicy: normalized.RetryPolicy, Metadata: normalized.Metadata, Tags: normalized.Tags})
				if err != nil {
					t.Fatal(err)
				}
				seed(claim, childTaskReceipt{RunID: other.RunID.String(), ComputerID: targetID.String()})
			}
			invoke(t, f.server.workerInvokeChildTask, request)
		}
	})
}

func TestProtectedSnapshotRootExpiryUsesCapturedStatementTime(t *testing.T) {
	f := newSnapshotFixture(t, 1, false)
	created := time.Now().AddDate(-10, 0, 0).Add(2 * time.Second)
	dbtest.MustExec(t, t.Context(), f.fixture.Pool, "UPDATE computers SET created_at=$2 WHERE id=$1", f.computer, created)
	root := createTestComputerCA(t, f.fixture.Pool, f.store, f.fixture.EnvironmentID, f.computer)
	rows, err := f.q.CaptureProtectedSecretEnvelopes(t.Context(), f.params())
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
	if _, err := f.store.OpenProtected(rows, f.markers); err != nil {
		t.Fatalf("already captured request lost in-flight authority: %v", err)
	}
	after, err := db.New(tx).CaptureProtectedSecretEnvelopes(t.Context(), f.params())
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.store.OpenProtected(after, f.markers); !errors.Is(err, secret.ErrProxyTrustExpired) {
		t.Fatalf("expired root result=%v", err)
	}
}

func TestProtectedSnapshotMultipleBindingsAndRawExclusion(t *testing.T) {
	f := newSnapshotFixture(t, 1, true)
	alias := "hlmr_protected_" + strings.Repeat("d", 64)
	dbtest.MustExec(t, t.Context(), f.fixture.Pool, "INSERT INTO computer_secrets(computer_id,environment_id,secret_id,placement_kind,placement_target,mode,allowed_origins,placeholder) VALUES($1,$2,$3,'env','ALIAS','protected',ARRAY['https://example.com'],$4)", f.computer, f.fixture.EnvironmentID, f.secrets[0], alias)
	f.markers = append(f.markers, alias)
	rows, err := f.q.CaptureProtectedSecretEnvelopes(t.Context(), f.params())
	if err != nil {
		t.Fatal(err)
	}
	values, err := f.store.OpenProtected(rows, f.markers)
	if err != nil || len(values) != 2 || !bytes.Equal(values[f.markers[0]], values[alias]) {
		t.Fatalf("same Secret aliases: %v", err)
	}
	// Duplicate material rows must fail complete coverage before any decryption.
	if _, err := f.store.OpenProtected([]db.CaptureProtectedSecretEnvelopesRow{rows[0], rows[0]}, f.markers); err == nil {
		t.Fatal("duplicate result row accepted")
	}
	dbtest.MustExec(t, t.Context(), f.fixture.Pool, "UPDATE computer_secrets SET mode='raw',allowed_origins='{}',placeholder='' WHERE computer_id=$1 AND placement_target='ALIAS'", f.computer)
	rows, err = f.q.CaptureProtectedSecretEnvelopes(t.Context(), f.params())
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.store.OpenProtected(rows, f.markers); err == nil {
		t.Fatal("raw binding resolved as protected")
	}
}

func TestSecretProxyHTTPRejectsUnactivatedHostAndUnknownInstance(t *testing.T) {
	f := newSnapshotFixture(t, 1, true)
	if response := f.invoke(t.Context(), false); response.Code != http.StatusOK {
		t.Fatalf("prepare=%d %s", response.Code, response.Body.String())
	}
	dbtest.MustExec(t, t.Context(), f.fixture.Pool, `UPDATE worker_hosts SET status='registering',activated_at=NULL WHERE id=$1`, f.fixture.WorkerID)
	for _, resolve := range []bool{false, true} {
		if response := f.invoke(t.Context(), resolve); response.Code != http.StatusConflict {
			t.Fatalf("registering host resolve=%v status=%d", resolve, response.Code)
		}
	}
	dbtest.MustExec(t, t.Context(), f.fixture.Pool, `UPDATE worker_hosts SET status='active',activated_at=clock_timestamp() WHERE id=$1`, f.fixture.WorkerID)
	f.runtime = uuid.NewV7()
	for _, resolve := range []bool{false, true} {
		if response := f.invoke(t.Context(), resolve); response.Code != http.StatusConflict {
			t.Fatalf("unknown instance resolve=%v status=%d", resolve, response.Code)
		}
	}
}
