package controlplane

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
	"uuid"

	"github.com/helmrdotdev/helmr/internal/cas"
	"github.com/helmrdotdev/helmr/internal/compute"
	"github.com/helmrdotdev/helmr/internal/computer"
	"github.com/helmrdotdev/helmr/internal/db"
	"github.com/helmrdotdev/helmr/internal/db/dbtest"
	"github.com/helmrdotdev/helmr/internal/pgvalue"
	"github.com/helmrdotdev/helmr/internal/run/runtest"
	"github.com/helmrdotdev/helmr/internal/workerapi"
	"github.com/jackc/pgx/v5/pgtype"
)

type initialPublicationFixture struct {
	runtest.Fixture
	server  *Server
	worker  workerActor
	request workerapi.ComputerInitializationRequest
	runtime pgtype.UUID
	run     runtest.RunLease
	data    []byte
}

func newInitialPublicationFixture(t *testing.T) initialPublicationFixture {
	t.Helper()
	f, run, runtime := prepareReservedRuntimeFailure(t)
	dbtest.MustExec(t, t.Context(), f.Pool, `UPDATE deployment_definitions SET manifest=$2::jsonb WHERE id=$1`, f.WorkspaceDefinitionID, fmt.Sprintf(`{"image":{"artifactDigest":%q,"mediaType":"application/octet-stream"},"resources":{"milliCpu":1000,"memoryMiB":1024}}`, dbtest.Digest("run-lease-image")))
	diskBytes := int64(compute.WorkspaceGuestEphemeralDiskMiB) * 1048576
	dbtest.MustExec(t, t.Context(), f.Pool, `UPDATE runtime_instances SET reserved_guest_ephemeral_disk_bytes=$2 WHERE id=$1`, runtime, diskBytes)
	dbtest.MustExec(t, t.Context(), f.Pool, `UPDATE workspace_versions SET status='initializing',artifact_id=NULL,content_digest=NULL,size_bytes=0,published_at=NULL WHERE workspace_id=(SELECT workspace_id FROM runtime_instances WHERE id=$1)`, runtime)
	store, err := cas.NewFile(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	data := []byte("opaque encrypted initial disk candidate")
	request := workerapi.ComputerInitializationRequest{RuntimeInstanceID: pgvalue.UUIDString(runtime), DesiredVersion: 1,
		Disk:         workerapi.CASObject{Digest: dbtest.Digest(string(data)), SizeBytes: int64(len(data)), MediaType: computer.DiskMediaType},
		LogicalBytes: diskBytes, InitialConfig: json.RawMessage(`{"User":"root","WorkingDir":"/workspace"}`)}
	return initialPublicationFixture{Fixture: f, run: run, runtime: runtime, server: &Server{db: db.New(f.Pool), tx: f.Pool, cas: store},
		worker: workerActor{WorkerInstanceID: f.WorkerID, WorkerGroupID: runtest.WorkerGroupID, WorkerEpoch: 1}, request: request, data: data}
}

func (f initialPublicationFixture) call(t *testing.T, publish bool, request workerapi.ComputerInitializationRequest) *httptest.ResponseRecorder {
	t.Helper()
	body, err := json.Marshal(request)
	if err != nil {
		t.Fatal(err)
	}
	r := httptest.NewRequest(http.MethodPost, "/", bytes.NewReader(body)).WithContext(context.WithValue(t.Context(), workerContextKey{}, f.worker))
	w := httptest.NewRecorder()
	if publish {
		f.server.workerPublishComputerInitialization(w, r)
	} else {
		f.server.workerRegisterComputerInitialization(w, r)
	}
	return w
}
func (f initialPublicationFixture) upload(t *testing.T) {
	t.Helper()
	if _, err := f.server.cas.Put(t.Context(), computer.DiskMediaType, bytes.NewReader(f.data)); err != nil {
		t.Fatal(err)
	}
}
func requireInitialStatus(t *testing.T, w *httptest.ResponseRecorder, status int) workerapi.ComputerInitializationResponse {
	t.Helper()
	if w.Code != status {
		t.Fatalf("status=%d want=%d: %s", w.Code, status, w.Body)
	}
	var result workerapi.ComputerInitializationResponse
	if status == http.StatusOK {
		if err := json.Unmarshal(w.Body.Bytes(), &result); err != nil {
			t.Fatal(err)
		}
	}
	return result
}
func TestComputerInitialPublicationAndHistoricalReplay(t *testing.T) {
	f := newInitialPublicationFixture(t)
	registered := requireInitialStatus(t, f.call(t, false, f.request), http.StatusOK)
	if registered.Status != "registered" || registered.ArtifactID != "" {
		t.Fatal("registration fabricated persistence")
	}
	requireInitialStatus(t, f.call(t, true, f.request), http.StatusServiceUnavailable)
	f.upload(t)
	published := requireInitialStatus(t, f.call(t, true, f.request), http.StatusOK)
	if published.Status != "consumed" || published.ID != registered.ID || published.VersionID != registered.VersionID || published.ArtifactID == "" {
		t.Fatalf("publication lost identity: %+v", published)
	}
	// A closed source is historical authority only; exact replay cannot reopen it.
	dbtest.MustExec(t, t.Context(), f.Pool, `UPDATE runtime_instances SET desired_state='closed',desired_version=desired_version+1 WHERE id=$1`, f.runtime)
	replayed := requireInitialStatus(t, f.call(t, true, f.request), http.StatusOK)
	if replayed != published {
		t.Fatal("lost-reply replay changed receipt")
	}
	changed := f.request
	changed.InitialConfig = json.RawMessage(`{"User":"1000"}`)
	requireInitialStatus(t, f.call(t, true, changed), http.StatusConflict)
	var artifacts int
	if err := f.Pool.QueryRow(t.Context(), `SELECT count(*) FROM artifacts WHERE digest=$1`, f.request.Disk.Digest).Scan(&artifacts); err != nil || artifacts != 1 {
		t.Fatalf("replay created another artifact: %d %v", artifacts, err)
	}
}
func TestComputerInitialPublicationRejectsRevokedAuthority(t *testing.T) {
	for _, change := range []string{"worker epoch", "close", "expiry", "cancel", "capacity"} {
		t.Run(change, func(t *testing.T) {
			f := newInitialPublicationFixture(t)
			requireInitialStatus(t, f.call(t, false, f.request), http.StatusOK)
			f.upload(t)
			switch change {
			case "worker epoch":
				dbtest.MustExec(t, t.Context(), f.Pool, `UPDATE worker_instances SET current_epoch=current_epoch+1 WHERE id=$1`, f.WorkerID)
			case "close":
				dbtest.MustExec(t, t.Context(), f.Pool, `UPDATE runtime_instances SET desired_state='closed',desired_version=desired_version+1 WHERE id=$1`, f.runtime)
			case "expiry":
				dbtest.MustExec(t, t.Context(), f.Pool, `UPDATE runtime_instances SET preparation_expires_at=now()-interval '1 second' WHERE id=$1`, f.runtime)
			case "cancel":
				dbtest.MustExec(t, t.Context(), f.Pool, `UPDATE runs SET status='cancel_requested' WHERE id=$1`, f.run.RunID)
			case "capacity":
				f.request.LogicalBytes += 4096
			}
			response := f.call(t, true, f.request)
			if response.Code < 400 {
				t.Fatalf("revoked publisher succeeded: %s", response.Body)
			}
			var status string
			var artifact pgtype.UUID
			if err := f.Pool.QueryRow(t.Context(), `SELECT status,artifact_id FROM computer_initializations WHERE runtime_instance_id=$1`, f.runtime).Scan(&status, &artifact); err != nil {
				t.Fatal(err)
			}
			if status != "registered" || artifact.Valid {
				t.Fatal("rejection changed candidate")
			}
			var published int
			if err := f.Pool.QueryRow(t.Context(), `SELECT count(*) FROM artifacts WHERE digest=$1`, f.request.Disk.Digest).Scan(&published); err != nil || published != 0 {
				t.Fatalf("rejection published artifact: %d %v", published, err)
			}
		})
	}
}

type initialPublicationStorageHook struct {
	cas.Store
	beforeStat func()
}

func (s initialPublicationStorageHook) Stat(ctx context.Context, digest string) (cas.Object, error) {
	s.beforeStat()
	return s.Store.Stat(ctx, digest)
}
func TestComputerInitialPublicationRechecksAfterStorageLookup(t *testing.T) {
	f := newInitialPublicationFixture(t)
	requireInitialStatus(t, f.call(t, false, f.request), http.StatusOK)
	f.upload(t)
	// Storage is consulted outside the transaction. Revocation here must win.
	f.server.cas = initialPublicationStorageHook{Store: f.server.cas, beforeStat: func() {
		dbtest.MustExec(t, t.Context(), f.Pool, `UPDATE runtime_instances SET desired_state='closed',desired_version=desired_version+1 WHERE id=$1`, f.runtime)
	}}
	requireInitialStatus(t, f.call(t, true, f.request), http.StatusConflict)
	var status string
	if err := f.Pool.QueryRow(t.Context(), `SELECT status FROM computer_initializations WHERE runtime_instance_id=$1`, f.runtime).Scan(&status); err != nil || status != "registered" {
		t.Fatalf("stale storage result published: %s %v", status, err)
	}
}
func TestComputerInitialPublicationConcurrentLostReplies(t *testing.T) {
	f := newInitialPublicationFixture(t)
	requireInitialStatus(t, f.call(t, true, f.request), http.StatusConflict)
	requireInitialStatus(t, f.call(t, false, f.request), http.StatusOK)
	f.upload(t)
	start := make(chan struct{})
	responses := make(chan *httptest.ResponseRecorder, 4)
	for range 4 {
		go func() { <-start; responses <- f.call(t, true, f.request) }()
	}
	close(start)
	var receipt workerapi.ComputerInitializationResponse
	for range 4 {
		got := requireInitialStatus(t, <-responses, http.StatusOK)
		if receipt.ID != "" && got != receipt {
			t.Fatal("concurrent publishers produced different receipts")
		}
		receipt = got
	}
}

func TestComputerInitialPublicationExpiryDuringCandidateLock(t *testing.T) {
	for _, kind := range []string{"queue", "preparation"} {
		t.Run(kind, func(t *testing.T) {
			f := newInitialPublicationFixture(t)
			requireInitialStatus(t, f.call(t, false, f.request), http.StatusOK)
			f.upload(t)
			var deadline time.Time
			statement := `UPDATE runs SET queued_expires_at=clock_timestamp()+interval '2 seconds' WHERE id=$1 RETURNING queued_expires_at`
			var id any = f.run.RunID
			if kind == "preparation" {
				statement = `UPDATE runtime_instances SET preparation_expires_at=clock_timestamp()+interval '2 seconds' WHERE id=$1 RETURNING preparation_expires_at`
				id = f.runtime
			}
			if err := f.Pool.QueryRow(t.Context(), statement, id).Scan(&deadline); err != nil {
				t.Fatal(err)
			}
			blocker, err := f.Pool.Begin(t.Context())
			if err != nil {
				t.Fatal(err)
			}
			defer blocker.Rollback(context.Background())
			var locked pgtype.UUID
			if err := blocker.QueryRow(t.Context(), `SELECT id FROM computer_initializations WHERE runtime_instance_id=$1 FOR UPDATE`, f.runtime).Scan(&locked); err != nil {
				t.Fatal(err)
			}
			response := make(chan *httptest.ResponseRecorder, 1)
			go func() { response <- f.call(t, true, f.request) }()
			ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
			defer cancel()
			ticker := time.NewTicker(10 * time.Millisecond)
			defer ticker.Stop()
			blocked := false
			for {
				var waiting, expired bool
				if err := f.Pool.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM pg_stat_activity WHERE datname=current_database() AND wait_event_type='Lock'), clock_timestamp()>=$1`, deadline).Scan(&waiting, &expired); err != nil {
					t.Fatal(err)
				}
				blocked = blocked || waiting
				if expired {
					break
				}
				select {
				case <-ticker.C:
				case <-ctx.Done():
					t.Fatal(ctx.Err())
				}
			}
			if !blocked {
				t.Fatal("publication did not reach the candidate row lock")
			}
			if err := blocker.Rollback(ctx); err != nil {
				t.Fatal(err)
			}
			select {
			case result := <-response:
				requireInitialStatus(t, result, http.StatusConflict)
			case <-ctx.Done():
				t.Fatal(ctx.Err())
			}
			var unpublished bool
			if err := f.Pool.QueryRow(ctx, `SELECT v.status='initializing' AND c.status='registered' AND c.artifact_id IS NULL
AND NOT EXISTS(SELECT 1 FROM artifacts WHERE digest=c.digest)
FROM computer_initializations c JOIN workspace_versions v ON v.id=c.version_id WHERE c.runtime_instance_id=$1`, f.runtime).Scan(&unpublished); err != nil || !unpublished {
				t.Fatalf("deadline rejection left publication writes: %v %v", unpublished, err)
			}
		})
	}
}

func TestComputerInitialPublicationForExplicitExec(t *testing.T) {
	f := newInitialPublicationFixture(t)
	claim, process := pgvalue.UUID(uuid.NewV7()), pgvalue.UUID(uuid.NewV7())
	var computerID, root pgtype.UUID
	if err := f.Pool.QueryRow(t.Context(), `SELECT workspace_id,reserved_workspace_version_id FROM runtime_instances WHERE id=$1`, f.runtime).Scan(&computerID, &root); err != nil {
		t.Fatal(err)
	}
	dbtest.MustExec(t, t.Context(), f.Pool, `UPDATE workspaces SET owner_run_id=NULL WHERE id=$1`, computerID)
	dbtest.MustExec(t, t.Context(), f.Pool, `INSERT INTO idempotency_claims (id,environment_id,operation,slot_hash,request_fingerprint,accepted_at,expires_at)
VALUES ($1,$2,'workspace.exec',decode(repeat('51',32),'hex'),decode(repeat('52',32),'hex'),now(),now()+interval '30 days')`, claim, f.EnvironmentID)
	if _, err := db.New(f.Pool).CreateWorkspaceExec(t.Context(), db.CreateWorkspaceExecParams{
		ID: process, OrgID: pgvalue.UUID(f.OrgID), ProjectID: pgvalue.UUID(f.ProjectID), EnvironmentID: pgvalue.UUID(f.EnvironmentID),
		WorkspaceID: computerID, BaseWorkspaceVersionID: root, RestoreDesiredState: "active", Request: []byte(`{"command":["echo","ready"]}`), Stdin: []byte{},
		ClaimID: claim, CreatedBySubjectType: "user", CreatedBySubjectID: "test-user",
	}); err != nil {
		t.Fatal(err)
	}
	dbtest.MustExec(t, t.Context(), f.Pool, `UPDATE runtime_instances SET reserved_run_id=NULL,reserved_attempt_number=NULL,program_deployment_id=NULL,reserved_process_id=$2 WHERE id=$1`, f.runtime, process)
	requireInitialStatus(t, f.call(t, false, f.request), http.StatusOK)
	f.upload(t)
	got := requireInitialStatus(t, f.call(t, true, f.request), http.StatusOK)
	if got.Status != "consumed" || got.VersionID != pgvalue.UUIDString(root) {
		t.Fatalf("explicit exec publication: %+v", got)
	}
}

func TestComputerPreparationSourceTracksPublishedRoot(t *testing.T) {
	f := newInitialPublicationFixture(t)
	seed := initializingComputerSourceRow(t)
	// Bind a valid admitted deployment to this reserved runtime. The existing
	// publication fixture's opaque candidate isolates the database protocol;
	// disk encoding/authentication is exercised by the computer package.
	dbtest.MustExec(t, t.Context(), f.Pool, `WITH lifetime AS (INSERT INTO cas_object_lifetimes (digest) VALUES ($2) ON CONFLICT DO NOTHING) INSERT INTO cas_objects (org_id,digest,size_bytes,media_type) VALUES ($1,$2,$3,$4)`,
		f.OrgID, seed.WorkspaceImageDigest, seed.WorkspaceImageSizeBytes, seed.WorkspaceImageMediaType)
	dbtest.MustExec(t, t.Context(), f.Pool, `UPDATE artifacts SET digest=$2,size_bytes=$3,media_type=$4
 WHERE id=(SELECT artifact_id FROM deployment_definitions WHERE id=$1)`,
		f.WorkspaceDefinitionID, seed.WorkspaceImageDigest, seed.WorkspaceImageSizeBytes, seed.WorkspaceImageMediaType)
	dbtest.MustExec(t, t.Context(), f.Pool, `UPDATE deployment_definitions SET manifest=$2 WHERE id=$1`, f.WorkspaceDefinitionID, seed.SandboxManifest)
	read := func() workerapi.RuntimeComputerSource {
		t.Helper()
		rows, err := f.server.db.ListRuntimeReconcileTargets(t.Context(), db.ListRuntimeReconcileTargetsParams{
			WorkerGroupID: pgvalue.UUID(f.worker.WorkerGroupID), WorkerInstanceID: pgvalue.UUID(f.worker.WorkerInstanceID),
			WorkerEpoch: f.worker.WorkerEpoch, ObservationFreshnessSeconds: workerapi.WorkerObservationFreshnessSeconds, RowLimit: 64,
		})
		if err != nil {
			t.Fatal(err)
		}
		if len(rows) != 1 {
			t.Fatalf("preparation rows: %d", len(rows))
		}
		source, err := projectRuntimeComputerSource(rows[0])
		if err != nil {
			t.Fatal(err)
		}
		return source
	}
	initial := read()
	if initial.Seed == nil || initial.Disk != nil || initial.Config.User != "1000" {
		t.Fatalf("initial: %+v", initial)
	}
	requireInitialStatus(t, f.call(t, false, f.request), http.StatusOK)
	if source := read(); source.Seed == nil || source.Disk != nil {
		t.Fatal("registration exposed unpublished disk")
	}
	f.upload(t)
	published := requireInitialStatus(t, f.call(t, true, f.request), http.StatusOK)
	continued := read()
	if continued.Seed != nil || continued.Disk == nil || continued.Disk.Digest != f.request.Disk.Digest ||
		continued.Config.User != "root" || continued.VersionID != initial.VersionID || continued.VersionID != published.VersionID {
		t.Fatalf("published: %+v", continued)
	}
	// Initial configuration is Computer state. Projection must not require an
	// upload receipt, even after the deployment's defaults change. Deleting the
	// fixture receipt here proves that dependency is absent; it is not a GC policy.
	dbtest.MustExec(t, t.Context(), f.Pool, `DELETE FROM computer_initializations WHERE runtime_instance_id=$1`, f.runtime)
	dbtest.MustExec(t, t.Context(), f.Pool, `UPDATE deployment_definitions SET manifest=jsonb_set(manifest,'{image,config}', '{"User":"changed"}') WHERE id=$1`, f.WorkspaceDefinitionID)
	if source := read(); source.Config.User != "root" || source.Config.WorkingDir != "/workspace" || source.Disk == nil || source.Disk.Digest != f.request.Disk.Digest {
		t.Fatalf("continuation depended on receipt or new deployment: %+v", source)
	}
}
