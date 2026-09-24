package controlplane

import (
	"bytes"
	"context"
	"encoding/json"
	"github.com/helmrdotdev/helmr/internal/cas"
	"github.com/helmrdotdev/helmr/internal/computer"
	"github.com/helmrdotdev/helmr/internal/db"
	"github.com/helmrdotdev/helmr/internal/db/dbtest"
	"github.com/helmrdotdev/helmr/internal/dispatch"
	"github.com/helmrdotdev/helmr/internal/pgvalue"
	"github.com/helmrdotdev/helmr/internal/run/runtest"
	"github.com/helmrdotdev/helmr/internal/workerapi"
	"github.com/helmrdotdev/helmr/internal/workspace"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
	"uuid"
)

type execGenerationFixture struct {
	runtest.Fixture
	server                                            *Server
	worker                                            workerActor
	computerID, baseID, runtimeID, mountID, processID uuid.UUID
	root                                              computer.GenerationRoot
}

func newExecGenerationFixture(t *testing.T) *execGenerationFixture {
	t.Helper()
	fixture := runtest.New(t)
	work := fixture.AddRunLease(t, "starting", time.Now().Add(-time.Minute))
	var workspaceID, baseWorkspaceVersionID, runtimeID, mountID, workspaceLeaseID uuid.UUID
	if err := fixture.Pool.QueryRow(t.Context(), `
SELECT runs.workspace_id, runs.base_workspace_version_id,
       run_leases.runtime_instance_id, workspace_leases.workspace_mount_id,
       workspace_leases.id
  FROM runs
  JOIN run_leases ON run_leases.id = runs.current_run_lease_id
  JOIN workspace_leases ON workspace_leases.owner_run_lease_id = run_leases.id
 WHERE runs.id = $1`, work.RunID).Scan(
		&workspaceID, &baseWorkspaceVersionID, &runtimeID, &mountID, &workspaceLeaseID,
	); err != nil {
		t.Fatal(err)
	}
	claimID := uuid.NewV7()
	processID := uuid.NewV7()
	creatorID := uuid.NewV7()
	dbtest.MustExec(t, t.Context(), fixture.Pool, `
INSERT INTO idempotency_claims (
    id, environment_id, operation, slot_hash, request_fingerprint,
    accepted_at, expires_at
) VALUES ($1, $2, 'workspace.exec', decode(repeat('11', 32), 'hex'),
          decode(repeat('22', 32), 'hex'), now(), now() + interval '30 days')`,
		claimID, fixture.EnvironmentID)
	dbtest.MustExec(t, t.Context(), fixture.Pool, `
INSERT INTO workspace_processes (
    id, org_id, project_id, environment_id, workspace_id, base_workspace_version_id,
    restore_desired_state, region_id, worker_group_id, worker_instance_id,
    worker_epoch, runtime_instance_id, workspace_mount_id, status, request,
    stdin, stdout, stderr, claim_id, created_by_subject_type,
    created_by_subject_id
) VALUES (
    $1, $2, $3, $4, $5, $6, 'active', $7, $8, $9, 1, $10, $11,
    'exit_requested', '{}'::jsonb, ''::bytea, ''::bytea, ''::bytea, $12,
    'api_key', $13
)`, processID, fixture.OrgID, fixture.ProjectID, fixture.EnvironmentID,
		workspaceID, baseWorkspaceVersionID, runtest.Region, runtest.WorkerGroup,
		fixture.WorkerID, runtimeID, mountID, claimID, creatorID.String())
	dbtest.MustExec(t, t.Context(), fixture.Pool, `
UPDATE workspace_leases
   SET owner_run_lease_id = NULL, owner_process_id = $1
 WHERE id = $2`, processID, workspaceLeaseID)
	dbtest.MustExec(t, t.Context(), fixture.Pool, `
UPDATE workspace_mounts
   SET status = 'unmounting', finalization_kind = 'capture',
       finalization_reason_code = 'workspace_exec_completed', stopped_at = now()
 WHERE id = $1`, mountID)

	substrateID := uuid.NewV7()
	dbtest.MustExec(t, t.Context(), fixture.Pool, `INSERT INTO runtime_substrates(id,org_id,project_id,environment_id,deployment_definition_id,substrate_digest,substrate_format,substrate_contract,substrate_size_bytes) SELECT $2,org_id,project_id,environment_id,deployment_definition_id,'sha256:82a76312340ff2dc8b52b1e6ff24308d9d9f54c3cb94e5957660b94afc53bc2d','squashfs','builder-v0',1 FROM runtime_instances WHERE id=$1`, runtimeID, substrateID)
	dbtest.MustExec(t, t.Context(), fixture.Pool, `UPDATE runtime_instances SET runtime_substrate_id=$2 WHERE id=$1`, runtimeID, substrateID)
	store, err := cas.NewFile(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	server := &Server{db: db.New(fixture.Pool), tx: fixture.Pool, cas: store, log: slog.New(slog.NewTextHandler(io.Discard, nil))}
	worker := workerActor{WorkerInstanceID: fixture.WorkerID, WorkerGroupID: runtest.WorkerGroupID, WorkerEpoch: 1}
	if err := fixture.Pool.QueryRow(t.Context(), `SELECT w.claim_version,g.claim_version FROM worker_instances w JOIN worker_groups g ON g.id=w.worker_group_id WHERE w.id=$1`, fixture.WorkerID).Scan(&worker.ClaimVersion, &worker.GroupClaimVersion); err != nil {
		t.Fatal(err)
	}
	root := retainedTestGeneration(t, fixture.Pool, server, runtimeID.String(), computerPublicationKey("exec", pgvalue.UUID(processID), pgvalue.UUID(processID)))
	return &execGenerationFixture{fixture, server, worker, workspaceID, baseWorkspaceVersionID, runtimeID, mountID, processID, root}
}
func (f *execGenerationFixture) call(t *testing.T, handler http.HandlerFunc, body any) *httptest.ResponseRecorder {
	t.Helper()
	raw, err := json.Marshal(body)
	if err != nil {
		t.Fatal(err)
	}
	r := httptest.NewRequest(http.MethodPost, "/", bytes.NewReader(raw)).WithContext(context.WithValue(t.Context(), workerContextKey{}, f.worker))
	w := httptest.NewRecorder()
	handler(w, r)
	return w
}
func (f *execGenerationFixture) capture() workerapi.WorkspaceMountCaptureRequest {
	return workerapi.WorkspaceMountCaptureRequest{OrgID: f.OrgID.String(), WorkspaceMountID: f.mountID.String(), Computer: workerapi.CheckpointComputer{ComputerID: f.computerID.String(), LogicalBytes: f.root.LogicalBytes, Root: f.root}}
}
func TestExecGenerationCaptureAndSettlement(t *testing.T) {
	f := newExecGenerationFixture(t)
	first := f.call(t, f.server.workerCaptureWorkspaceMount, f.capture())
	if first.Code != 200 {
		t.Fatalf("capture: %d %s", first.Code, first.Body)
	}
	var receipt workerapi.WorkspaceMountCaptureResponse
	if err := json.Unmarshal(first.Body.Bytes(), &receipt); err != nil {
		t.Fatal(err)
	}
	replay := f.call(t, f.server.workerCaptureWorkspaceMount, f.capture())
	if replay.Code != 200 || !bytes.Equal(first.Body.Bytes(), replay.Body.Bytes()) {
		t.Fatalf("replay: %d %s", replay.Code, replay.Body)
	}
	var head uuid.UUID
	var status string
	if err := f.Pool.QueryRow(t.Context(), `SELECT head_version_id FROM computers WHERE id=$1`, f.computerID).Scan(&head); err != nil {
		t.Fatal(err)
	}
	if head != f.baseID {
		t.Fatal("capture advanced head before physical close")
	}
	stop := workerapi.WorkspaceMountStopRequest{OrgID: f.OrgID.String(), WorkspaceMountID: f.mountID.String(), CleanupProof: workerapi.RuntimeCleanupProof{Method: workerapi.RuntimeCleanupSessionClosed, CompletedAt: time.Now().UTC()}}
	result := f.call(t, f.server.workerStopWorkspaceMount, stop)
	if result.Code != 200 {
		t.Fatalf("stop: %d %s", result.Code, result.Body)
	}
	replayStop := f.call(t, f.server.workerStopWorkspaceMount, stop)
	if replayStop.Code != 200 || !bytes.Equal(replayStop.Body.Bytes(), result.Body.Bytes()) {
		t.Fatalf("stop replay %d %s", replayStop.Code, replayStop.Body)
	}
	var raw []byte
	if err := f.Pool.QueryRow(t.Context(), `SELECT c.head_version_id,p.status,r.locator FROM computers c JOIN workspace_processes p ON p.workspace_id=c.id JOIN computer_version_roots r ON r.computer_id=c.id AND r.version_id=c.head_version_id WHERE c.id=$1`, f.computerID).Scan(&head, &status, &raw); err != nil {
		t.Fatal(err)
	}
	restored, err := computer.ParseGenerationRoot(raw, f.root.LogicalBytes)
	if err != nil || restored != f.root || head.String() != receipt.VersionID || status != "exited" {
		t.Fatalf("settled %s %s %v", head, status, err)
	}
	authority, err := f.server.db.GetComputerVersionAuthority(t.Context(), db.GetComputerVersionAuthorityParams{OrgID: pgvalue.UUID(f.OrgID), ProjectID: pgvalue.UUID(f.ProjectID), EnvironmentID: pgvalue.UUID(f.EnvironmentID), WorkspaceID: pgvalue.UUID(f.computerID), VersionID: pgvalue.UUID(head)})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := projectComputerMountTarget(db.WorkspaceLease{BaseWorkspaceVersionID: pgvalue.UUID(head)}, authority); err != nil {
		t.Fatal(err)
	}
}
func TestExecGenerationRejectsUnpublishedAndChangedCapture(t *testing.T) {
	f := newExecGenerationFixture(t)
	request := f.capture()
	request.Computer.Root.Pack.Digest = "sha256:" + string(bytes.Repeat([]byte{'f'}, 64))
	if w := f.call(t, f.server.workerCaptureWorkspaceMount, request); w.Code != 409 {
		t.Fatalf("unpublished %d %s", w.Code, w.Body)
	}
	if w := f.call(t, f.server.workerCaptureWorkspaceMount, f.capture()); w.Code != 200 {
		t.Fatalf("capture %d %s", w.Code, w.Body)
	}
	f.root = retainedTestGeneration(t, f.Pool, f.server, f.runtimeID.String(), computerPublicationKey("exec", pgvalue.UUID(f.processID), pgvalue.UUID(f.processID)))
	if w := f.call(t, f.server.workerCaptureWorkspaceMount, f.capture()); w.Code != 409 {
		t.Fatalf("changed capture %d %s", w.Code, w.Body)
	}
}

// Models an already committed save; live save admission is outside this fixture.
func (f *execGenerationFixture) advanceSavedHead(t *testing.T) uuid.UUID {
	t.Helper()
	id := uuid.NewV7()
	dbtest.MustExec(t, t.Context(), f.Pool, `INSERT INTO computer_versions(id,environment_id,workspace_id,parent_version_id,content_digest,size_bytes,entry_count,status,source_workspace_lease_id,ownership_generation,writer_generation,published_at)
 SELECT $2,v.environment_id,v.workspace_id,v.id,v.content_digest,v.size_bytes,v.entry_count,'committed',(SELECT id FROM workspace_leases WHERE owner_process_id=$3),c.ownership_generation,c.writer_generation,now()
 FROM computers c JOIN computer_versions v ON v.id=c.head_version_id WHERE c.id=$1`, f.computerID, id, f.processID)
	raw, err := json.Marshal(f.root)
	if err != nil {
		t.Fatal(err)
	}
	dbtest.MustExec(t, t.Context(), f.Pool, `INSERT INTO computer_version_roots(environment_id,computer_id,version_id,locator) VALUES($1,$2,$3,$4)`, f.EnvironmentID, f.computerID, id, raw)
	dbtest.MustExec(t, t.Context(), f.Pool, `UPDATE computers SET head_version_id=$2 WHERE id=$1`, f.computerID, id)
	return id
}

func TestExecSettlementAfterSavedHeadAdvancement(t *testing.T) {
	for _, mode := range []string{"complete", "recovered complete", "failure", "recovered failure", "changed staged predecessor"} {
		t.Run(mode, func(t *testing.T) {
			f := newExecGenerationFixture(t)
			head := f.advanceSavedHead(t)
			if mode == "recovered failure" {
				dbtest.MustExec(t, t.Context(), f.Pool, `UPDATE workspace_mounts SET finalization_kind=NULL, finalization_reason_code=NULL WHERE id=$1`, f.mountID)
				f.recoverExec(t)
			} else if mode == "failure" {
				w := f.call(t, f.server.workerFailWorkspaceMount, workerapi.WorkspaceMountFailRequest{OrgID: f.OrgID.String(), WorkspaceMountID: f.mountID.String(), Error: json.RawMessage(`{"code":"fixture_failure"}`)})
				if w.Code != 200 {
					t.Fatalf("failure: %d %s", w.Code, w.Body)
				}
			} else {
				if w := f.call(t, f.server.workerCaptureWorkspaceMount, f.capture()); w.Code != 200 {
					t.Fatalf("capture: %d %s", w.Code, w.Body)
				}
				if mode == "changed staged predecessor" {
					head = f.advanceSavedHead(t)
				}
				if mode == "recovered complete" {
					f.recoverExec(t)
				} else {
					w := f.call(t, f.server.workerStopWorkspaceMount, workerapi.WorkspaceMountStopRequest{OrgID: f.OrgID.String(), WorkspaceMountID: f.mountID.String(), CleanupProof: workerapi.RuntimeCleanupProof{Method: workerapi.RuntimeCleanupSessionClosed, CompletedAt: time.Now().UTC()}})
					if mode == "complete" && w.Code != 200 {
						t.Fatalf("stop: %d %s", w.Code, w.Body)
					}
					if mode == "changed staged predecessor" && w.Code == 200 {
						t.Fatal("stale publication replaced newer saved head")
					}
				}
			}
			var origin, saved uuid.UUID
			var parent *uuid.UUID
			var status string
			if err := f.Pool.QueryRow(t.Context(), `SELECT p.base_workspace_version_id,c.head_version_id,v.parent_version_id,c.status FROM workspace_processes p JOIN computers c ON c.id=p.workspace_id JOIN computer_versions v ON v.id=c.head_version_id WHERE p.id=$1`, f.processID).Scan(&origin, &saved, &parent, &status); err != nil {
				t.Fatal(err)
			}
			if origin != f.baseID {
				t.Fatal("execution origin changed")
			}
			if mode == "complete" || mode == "recovered complete" {
				if saved == head || parent == nil || *parent != head {
					t.Fatal("completion did not follow saved predecessor")
				}
			} else if saved != head {
				t.Fatal("failure replaced saved head")
			}
			if (mode == "failure" || mode == "recovered failure") && status != "recovery_required" {
				t.Fatalf("status %s", status)
			}
		})
	}
}

func (f *execGenerationFixture) recoverExec(t *testing.T) {
	t.Helper()
	if _, err := f.server.db.LoseWorkspaceExecMount(t.Context(), db.LoseWorkspaceExecMountParams{WorkspaceMountID: pgvalue.UUID(f.mountID), WorkspaceID: pgvalue.UUID(f.computerID), ReasonCode: pgvalue.Text("fixture_loss")}); err != nil {
		t.Fatal(err)
	}
	key, err := workspace.NewFencingKey(bytes.Repeat([]byte{9}, workspace.FencingKeySize))
	if err != nil {
		t.Fatal(err)
	}
	authority, err := dispatch.NewRunAuthority(f.Pool, key)
	if err != nil {
		t.Fatal(err)
	}
	var revision int64
	if err := f.Pool.QueryRow(t.Context(), `SELECT revision FROM workspace_processes WHERE id=$1`, f.processID).Scan(&revision); err != nil {
		t.Fatal(err)
	}
	if err := authority.RecoverWorkspaceExec(t.Context(), dispatch.RecoverableWorkspaceExecCandidate{OrgID: pgvalue.UUID(f.OrgID), ProcessID: pgvalue.UUID(f.processID), WorkspaceID: pgvalue.UUID(f.computerID), ExpectedRevision: revision}); err != nil {
		t.Fatal(err)
	}
}
