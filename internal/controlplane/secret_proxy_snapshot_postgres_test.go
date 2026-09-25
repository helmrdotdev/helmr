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
	"github.com/helmrdotdev/helmr/internal/db"
	"github.com/helmrdotdev/helmr/internal/db/dbtest"
	"github.com/helmrdotdev/helmr/internal/idempotency"
	"github.com/helmrdotdev/helmr/internal/pgvalue"
	"github.com/helmrdotdev/helmr/internal/run/runtest"
	"github.com/helmrdotdev/helmr/internal/secret"
	"github.com/helmrdotdev/helmr/internal/workerapi"
	"github.com/jackc/pgx/v5/pgtype"
)

type snapshotFixture struct {
	fixture                   runtest.Fixture
	q                         *db.Queries
	store                     *secret.Store
	server                    *Server
	worker                    workerActor
	workspace, runtime, mount uuid.UUID
	run                       runtest.RunLease
	markers                   []string
	secrets                   []pgtype.UUID
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
	if err := f.fixture.Pool.QueryRow(t.Context(), "SELECT r.workspace_id,r.id,m.id FROM runtime_instances r JOIN workspace_mounts m ON m.runtime_instance_id=r.id JOIN run_leases l ON l.runtime_instance_id=r.id WHERE l.id=$1", f.run.LeaseID).Scan(&f.workspace, &f.runtime, &f.mount); err != nil {
		t.Fatal(err)
	}
	f.worker = workerActor{WorkerInstanceID: f.fixture.WorkerID, WorkerGroupID: runtest.WorkerGroupID, WorkerEpoch: 1, ClaimVersion: 1, GroupClaimVersion: 1}
	f.server = &Server{db: f.q, tx: f.fixture.Pool, secretProxy: f.store, log: slog.New(slog.NewTextHandler(io.Discard, nil))}
	dbtest.MustExec(t, t.Context(), f.fixture.Pool, "UPDATE workspace_mounts SET guest_channel_token_hash='synthetic',guest_channel_token_expires_at=now()+interval '10 minutes' WHERE id=$1", f.mount)
	for i := 0; i < count; i++ {
		name := string(rune('a' + i))
		record, err := f.store.Create(t.Context(), f.fixture.EnvironmentID, "token-"+name, []byte("old-"+name), "create-"+name)
		if err != nil {
			t.Fatal(err)
		}
		marker := "hlmr_protected_" + strings.Repeat(name, 64)
		f.markers = append(f.markers, marker)
		f.secrets = append(f.secrets, record.ID)
		dbtest.MustExec(t, t.Context(), f.fixture.Pool, "INSERT INTO workspace_secrets(workspace_id,environment_id,secret_id,placement_kind,placement_target,mode,allowed_origins,placeholder) VALUES($1,$2,$3,'env',$4,'protected',ARRAY['https://example.com'],$5)", f.workspace, f.fixture.EnvironmentID, record.ID, "TOKEN_"+name, marker)
	}
	if root {
		createTestWorkspaceCA(t, f.fixture.Pool, f.store, f.fixture.EnvironmentID, f.workspace)
	}

	return f
}
func (f *snapshotFixture) params() db.CaptureProtectedSecretEnvelopesParams {
	return db.CaptureProtectedSecretEnvelopesParams{RuntimeInstanceID: pgvalue.UUID(f.runtime), WorkerInstanceID: pgvalue.UUID(f.worker.WorkerInstanceID), WorkerEpoch: 1, WorkerGroupID: pgvalue.UUID(f.worker.WorkerGroupID), ClaimVersion: 1, GroupClaimVersion: 1, Origin: "https://example.com", Placeholders: f.markers}
}
func (f *snapshotFixture) invoke(ctx context.Context, resolve bool) *httptest.ResponseRecorder {
	body, _ := json.Marshal(workerapi.SecretProxyRequest{RuntimeInstanceID: f.runtime.String()})
	if resolve {
		body, _ = json.Marshal(workerapi.SecretProxyRequest{RuntimeInstanceID: f.runtime.String(), Origin: "https://example.com", Placeholders: f.markers})
	}
	req := httptest.NewRequest("POST", "/", bytes.NewReader(body)).WithContext(context.WithValue(ctx, workerContextKey{}, f.worker))
	response := httptest.NewRecorder()
	f.server.workerSecretProxy(response, req, resolve)
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
		for _, change := range []string{"rotate", "revoke", "epoch", "claims", "group", "workspace", "runtime", "lease", "token", "fence", "writer"} {
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
						dbtest.MustExec(t, t.Context(), f.fixture.Pool, "UPDATE worker_instances SET current_epoch=2 WHERE id=$1", f.worker.WorkerInstanceID)
					case "claims":
						dbtest.MustExec(t, t.Context(), f.fixture.Pool, "UPDATE worker_instances SET claim_version=2 WHERE id=$1", f.worker.WorkerInstanceID)
					case "group":
						dbtest.MustExec(t, t.Context(), f.fixture.Pool, "UPDATE worker_groups SET claim_version=2 WHERE id=$1", f.worker.WorkerGroupID)
					case "workspace":
						dbtest.MustExec(t, t.Context(), f.fixture.Pool, "UPDATE computers SET desired_state='deleted' WHERE id=$1", f.workspace)
					case "runtime":
						dbtest.MustExec(t, t.Context(), f.fixture.Pool, "UPDATE runtime_instances SET desired_state='closed',desired_version=desired_version+1 WHERE id=$1", f.runtime)
					case "lease":
						dbtest.MustExec(t, t.Context(), f.fixture.Pool, "UPDATE workspace_leases SET expires_at=now()-interval '1 second' WHERE runtime_instance_id=$1", f.runtime)
					case "token":
						dbtest.MustExec(t, t.Context(), f.fixture.Pool, "UPDATE workspace_mounts SET guest_channel_token_expires_at=now()-interval '1 second' WHERE id=$1", f.mount)
					case "fence":
						dbtest.MustExec(t, t.Context(), f.fixture.Pool, "UPDATE workspace_mounts SET fencing_generation=fencing_generation+1 WHERE id=$1", f.mount)
					case "writer":
						dbtest.MustExec(t, t.Context(), f.fixture.Pool, "UPDATE computers SET writer_generation=writer_generation+1 WHERE id=$1", f.workspace)
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
	p.RuntimeInstanceID = pgvalue.UUID(uuid.NewV7())
	rows, err = f.q.CaptureProtectedSecretEnvelopes(t.Context(), p)
	if err != nil || len(rows) != 0 {
		t.Fatal("wrong runtime accepted")
	}
	dbtest.MustExec(t, t.Context(), f.fixture.Pool, "UPDATE workspace_mounts SET guest_channel_token_expires_at=now()+interval '150 milliseconds' WHERE id=$1", f.mount)
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
	root, err := f.q.GetWorkspaceSecretCAPublic(t.Context(), db.GetWorkspaceSecretCAPublicParams{EnvironmentID: pgvalue.UUID(f.fixture.EnvironmentID), WorkspaceID: pgvalue.UUID(f.workspace)})
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
func TestProtectedSnapshotDoesNotJoinMountStopLockGraph(t *testing.T) {
	f := newSnapshotFixture(t, 1, true)
	dbtest.MustExec(t, t.Context(), f.fixture.Pool, "UPDATE workspace_mounts SET status='unmounting' WHERE id=$1", f.mount)
	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()
	holder, err := f.fixture.Pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer holder.Rollback(context.Background())
	if _, err := holder.Exec(ctx, "SELECT id FROM runtime_instances WHERE id=$1 FOR UPDATE", f.runtime); err != nil {
		t.Fatal(err)
	}
	done := make(chan error, 1)
	go func() {
		_, e := f.q.StopWorkspaceMount(ctx, db.StopWorkspaceMountParams{OrgID: pgvalue.UUID(f.fixture.OrgID), ID: pgvalue.UUID(f.mount), WorkerInstanceID: pgvalue.UUID(f.worker.WorkerInstanceID), WorkerEpoch: 1, RuntimeInstanceID: pgvalue.UUID(f.runtime), FencingGeneration: 2, ReasonCode: pgvalue.Text("test_stop"), CleanupProof: []byte("{}")})
		done <- e
	}()
	for {
		var waiting bool
		if err := f.fixture.Pool.QueryRow(ctx, "SELECT EXISTS(SELECT 1 FROM pg_stat_activity WHERE datname=current_database() AND wait_event_type='Lock')").Scan(&waiting); err != nil {
			t.Fatal(err)
		}
		if waiting {
			break
		}
		select {
		case err := <-done:
			t.Fatalf("stop did not wait: %v", err)
		case <-ctx.Done():
			t.Fatal(ctx.Err())
		case <-time.After(10 * time.Millisecond):
		}
	}
	for _, resolve := range []bool{true, false} {
		response := f.invoke(ctx, resolve)
		if response.Code == http.StatusOK {
			t.Fatal("unmounting authority accepted")
		}
		if ctx.Err() != nil {
			t.Fatal("proxy participated in teardown lock graph")
		}
	}
	if err := holder.Commit(ctx); err != nil {
		t.Fatal(err)
	}
	if err := <-done; err != nil {
		t.Fatal(err)
	}
}

func TestProtectedGuestIngressCeilingsBeforeReplay(t *testing.T) {
	f := newSnapshotFixture(t, 1, true)
	dbtest.MustExec(t, t.Context(), f.fixture.Pool, "UPDATE runs SET status='running',started_at=now(),active_started_at=now() WHERE id=$1", f.run.RunID)
	dbtest.MustExec(t, t.Context(), f.fixture.Pool, "UPDATE run_leases SET status='running',started_at=now() WHERE id=$1", f.run.LeaseID)
	dbtest.MustExec(t, t.Context(), f.fixture.Pool, "UPDATE run_attempts SET entrypoint_entered_at=now() WHERE run_id=$1", f.run.RunID)
	other := f.fixture.AddRunLease(t, "starting", time.Now().Add(-time.Minute))
	var targetID uuid.UUID
	if err := f.fixture.Pool.QueryRow(t.Context(), "SELECT workspace_id FROM runs WHERE id=$1", other.RunID).Scan(&targetID); err != nil {
		t.Fatal(err)
	}
	dbtest.MustExec(t, t.Context(), f.fixture.Pool, "INSERT INTO workspace_secrets(workspace_id,environment_id,secret_id,placement_kind,placement_target,mode) VALUES($1,$2,$3,'env','RAW_TOKEN','raw')", targetID, f.fixture.EnvironmentID, f.secrets[0])
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
		invoke(t, f.server.workerCreateWorkspace, workerapi.CreateWorkspaceRequest{Lease: fence, CorrelationID: uuid.NewV7().String(), SandboxDeclaredID: "test-workspace", Secrets: []api.WorkspaceSecret{{Name: "token-a", Env: &api.SecretEnv{Name: "RAW", Mode: "raw"}}}, IdempotencyKey: "denied-create"})
	})
	t.Run("create-replayed-target", func(t *testing.T) {
		bindings := []api.WorkspaceSecret{{Name: "token-a", Env: &api.SecretEnv{Name: "TOKEN", Mode: "protected", AllowedOrigins: []string{"https://example.com"}}}}
		placements, err := normalizeWorkspaceSecretPlacements(bindings)
		if err != nil {
			t.Fatal(err)
		}
		encoded, _ := json.Marshal(placements)
		request, err := idempotency.NewRuntimeWorkspaceCreateRequest(f.fixture.EnvironmentID, f.run.RunID, "test-workspace", "replay-create", idempotency.WorkspaceCreateFingerprint{Secrets: encoded})
		if err != nil {
			t.Fatal(err)
		}
		now := time.Now()
		seed(request, workspaceCreateReceipt{Workspace: api.WorkspaceSnapshot{ID: targetID.String(), SandboxID: "test-workspace", DeploymentID: f.fixture.DeploymentID.String(), Status: api.WorkspaceStatusAvailable, CreatedAt: now, UpdatedAt: now, LastActivityAt: now}})
		invoke(t, f.server.workerCreateWorkspace, workerapi.CreateWorkspaceRequest{Lease: fence, CorrelationID: uuid.NewV7().String(), SandboxDeclaredID: "test-workspace", Secrets: bindings, IdempotencyKey: "replay-create"})
	})
	t.Run("target-exec-new-and-replayed", func(t *testing.T) {
		for _, replay := range []bool{false, true} {
			key := "denied-exec"
			if replay {
				key = "replay-exec"
			}
			if replay {
				normalized, err := normalizeWorkspaceExec(workspaceExecRequest{Command: []string{"true"}})
				if err != nil {
					t.Fatal(err)
				}
				request, err := idempotency.NewWorkspaceExecRequest(f.fixture.EnvironmentID, targetID, key, idempotency.WorkspaceExecFingerprint{Command: normalized.command, Cwd: normalized.cwd, Env: normalized.envJSON, StdinHash: normalized.stdinHash, TimeoutMS: normalized.timeoutMS})
				if err != nil {
					t.Fatal(err)
				}
				seed(request, map[string]any{"process_id": uuid.NewV7().String()})
			}
			invoke(t, f.server.workerExecuteWorkspace, workerapi.ExecuteWorkspaceRequest{RetrieveWorkspaceRequest: workerapi.RetrieveWorkspaceRequest{Lease: fence, CorrelationID: uuid.NewV7().String(), Workspace: workerapi.WorkspaceAddress{WorkspaceID: targetID.String()}}, Command: []string{"true"}, IdempotencyKey: key})
		}
	})
	t.Run("child-new-and-replayed", func(t *testing.T) {
		for _, replay := range []bool{false, true} {
			key := "denied-child"
			if replay {
				key = "replay-child"
			}
			target, _ := json.Marshal(api.WorkspaceIDTarget{ID: targetID.String()})
			request := workerapi.InvokeChildTaskRequest{Lease: fence, CorrelationID: uuid.NewV7().String(), TaskDeclaredID: "test-task", Method: "start", Workspace: target, Options: json.RawMessage("{}"), IdempotencyKey: key}
			if replay {
				loc, err := f.q.GetLiveRunLeaseLocators(t.Context(), db.GetLiveRunLeaseLocatorsParams{ID: pgvalue.UUID(f.run.LeaseID), LeaseSequence: 1, WorkerGroupID: pgvalue.UUID(f.worker.WorkerGroupID), WorkerInstanceID: pgvalue.UUID(f.worker.WorkerInstanceID), WorkerEpoch: 1})
				if err != nil {
					t.Fatal(err)
				}
				normalized, err := normalizeWorkerChildTaskRequest(request, loc)
				if err != nil {
					t.Fatal(err)
				}
				claim, err := idempotency.NewTaskChildInvokeRequest(f.fixture.EnvironmentID, f.run.RunID, request.TaskDeclaredID, key, idempotency.TaskChildInvokeFingerprint{Method: request.Method, PayloadPresent: normalized.PayloadPresent, Payload: normalized.Payload, Workspace: normalized.fingerprint.Workspace, QueueName: normalized.QueueName, ConcurrencyKey: normalized.ConcurrencyKey, Priority: normalized.Priority, QueuedTTLMS: normalized.QueuedTTLMS, RetryPolicy: normalized.RetryPolicy, Metadata: normalized.Metadata, Tags: normalized.Tags})
				if err != nil {
					t.Fatal(err)
				}
				seed(claim, childTaskReceipt{RunID: other.RunID.String(), WorkspaceID: targetID.String()})
			}
			invoke(t, f.server.workerInvokeChildTask, request)
		}
	})
}

func TestProtectedSnapshotCurrentProcessOwnerIgnoresLiveRunHistory(t *testing.T) {
	f := newSnapshotFixture(t, 1, true)
	tx, err := f.fixture.Pool.Begin(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback(context.Background())
	claims, err := idempotency.TransactionFor(tx)
	if err != nil {
		t.Fatal(err)
	}
	request, err := idempotency.NewWorkspaceExecRequest(f.fixture.EnvironmentID, f.workspace, "process-owner", idempotency.WorkspaceExecFingerprint{Command: []string{"true"}})
	if err != nil {
		t.Fatal(err)
	}
	acquired, err := claims.Acquire(t.Context(), request)
	if err != nil {
		t.Fatal(err)
	}
	process := uuid.NewV7()
	_, err = tx.Exec(t.Context(), `INSERT INTO workspace_processes(id,org_id,project_id,environment_id,workspace_id,base_workspace_version_id,restore_desired_state,region_id,worker_group_id,worker_instance_id,worker_epoch,runtime_instance_id,workspace_mount_id,status,request,claim_id,started_at)
 SELECT $1,r.org_id,r.project_id,r.environment_id,r.workspace_id,m.materialized_version_id,'active',r.region_id,r.worker_group_id,r.worker_instance_id,r.worker_epoch,r.id,m.id,'running','{}'::jsonb,$2,now()
 FROM runtime_instances r JOIN workspace_mounts m ON m.runtime_instance_id=r.id WHERE r.id=$3`, process, acquired.Claim.ID, f.runtime)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := tx.Exec(t.Context(), "UPDATE workspace_leases SET owner_run_lease_id=NULL,owner_process_id=$2 WHERE runtime_instance_id=$1", f.runtime, process); err != nil {
		t.Fatal(err)
	}
	if err := tx.Commit(t.Context()); err != nil {
		t.Fatal(err)
	}
	for _, resolve := range []bool{true, false} {
		if r := f.invoke(t.Context(), resolve); r.Code != 200 {
			t.Fatalf("current process rejected: %s", r.Body.String())
		}
	}
	dbtest.MustExec(t, t.Context(), f.fixture.Pool, "UPDATE workspace_processes SET status='exit_requested',stdout=''::bytea,stderr=''::bytea WHERE id=$1", process)
	// The old Run lease is still starting/unexpired on this runtime; it is not
	// the Workspace Lease's owner and must authorize neither renewal nor use.
	for _, resolve := range []bool{true, false} {
		if r := f.invoke(t.Context(), resolve); r.Code == 200 {
			t.Fatal("historical Run authorized exited current process")
		}
	}
}
func TestProtectedSnapshotRootExpiryUsesCapturedStatementTime(t *testing.T) {
	f := newSnapshotFixture(t, 1, false)
	created := time.Now().AddDate(-10, 0, 0).Add(2 * time.Second)
	dbtest.MustExec(t, t.Context(), f.fixture.Pool, "UPDATE computers SET created_at=$2 WHERE id=$1", f.workspace, created)
	root := createTestWorkspaceCA(t, f.fixture.Pool, f.store, f.fixture.EnvironmentID, f.workspace)
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
	dbtest.MustExec(t, t.Context(), f.fixture.Pool, "INSERT INTO workspace_secrets(workspace_id,environment_id,secret_id,placement_kind,placement_target,mode,allowed_origins,placeholder) VALUES($1,$2,$3,'env','ALIAS','protected',ARRAY['https://example.com'],$4)", f.workspace, f.fixture.EnvironmentID, f.secrets[0], alias)
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
	dbtest.MustExec(t, t.Context(), f.fixture.Pool, "UPDATE workspace_secrets SET mode='raw',allowed_origins='{}',placeholder='' WHERE workspace_id=$1 AND placement_target='ALIAS'", f.workspace)
	rows, err = f.q.CaptureProtectedSecretEnvelopes(t.Context(), f.params())
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.store.OpenProtected(rows, f.markers); err == nil {
		t.Fatal("raw binding resolved as protected")
	}
}
