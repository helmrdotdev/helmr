package controlplane

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
	"uuid"

	"github.com/helmrdotdev/helmr/internal/cas"
	"github.com/helmrdotdev/helmr/internal/db"
	"github.com/helmrdotdev/helmr/internal/db/dbtest"
	"github.com/helmrdotdev/helmr/internal/deployment"
	"github.com/helmrdotdev/helmr/internal/dispatch"
	"github.com/helmrdotdev/helmr/internal/pgvalue"
	runauthority "github.com/helmrdotdev/helmr/internal/run"
	"github.com/helmrdotdev/helmr/internal/run/runtest"
	"github.com/helmrdotdev/helmr/internal/workerapi"
	"github.com/helmrdotdev/helmr/internal/workspace"
	"github.com/jackc/pgx/v5/pgconn"
)

// Only the environment, Worker and empty Workspace are seeded. Actor, turn,
// checkpoint and resumed lease authority are produced by their owning operations.
// Physical Worker observations and VM snapshot bytes are test inputs, not KVM execution.
type actorCheckpointFixture struct {
	runtest.Fixture
	server                                *Server
	worker                                workerActor
	placement                             *dispatch.Authority
	workspaceID, rootID, sessionID, runID uuid.UUID
	claim                                 runLeaseClaimAuthority
}

func newActorCheckpointFixture(t *testing.T) *actorCheckpointFixture {
	t.Helper()
	b := runtest.New(t)
	dbtest.MustExec(t, t.Context(), b.Pool, `UPDATE worker_pools SET capacity_guest_ephemeral_disk_bytes=274877906944, per_vm_guest_ephemeral_disk_bytes=34359738368 WHERE id=$1`, b.WorkerPoolID)
	dbtest.MustExec(t, t.Context(), b.Pool, `UPDATE worker_instances SET epoch_guest_ephemeral_disk_bytes=274877906944, per_vm_guest_ephemeral_disk_bytes=34359738368 WHERE id=$1`, b.WorkerID)
	store, err := cas.NewFile(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	f := &actorCheckpointFixture{Fixture: b, workspaceID: uuid.NewV7(), rootID: uuid.NewV7(), server: &Server{db: db.New(b.Pool), tx: b.Pool, cas: store, log: slog.Default()}, worker: workerActor{WorkerInstanceID: b.WorkerID, WorkerGroupID: runtest.WorkerGroupID, WorkerEpoch: 1, ClaimVersion: 1, GroupClaimVersion: 1}}
	key, err := workspace.NewFencingKey(bytes.Repeat([]byte{9}, workspace.FencingKeySize))
	if err != nil {
		t.Fatal(err)
	}
	f.placement, err = dispatch.NewRunAuthority(b.Pool, key)
	if err != nil {
		t.Fatal(err)
	}
	dbtest.MustExec(t, t.Context(), b.Pool, `UPDATE deployments SET queue_config=$2 WHERE id=$1`, b.DeploymentID, []byte(`{"formatVersion":0,"queues":[{"concurrencyLimit":2,"name":"default"},{"name":"priority"}]}`))
	dbtest.MustExec(t, t.Context(), b.Pool, `UPDATE environments SET current_deployment_id=$2 WHERE id=$1`, b.EnvironmentID, b.DeploymentID)
	dbtest.MustExec(t, t.Context(), b.Pool, `UPDATE deployment_definitions SET manifest=$2::jsonb WHERE id=$1`, b.WorkspaceDefinitionID, fmt.Sprintf(`{"image":{"artifactDigest":%q,"mediaType":"application/octet-stream"},"resources":{"milliCpu":1000,"memoryMiB":1024}}`, dbtest.Digest("image")))
	manifest, digest, err := deployment.CanonicalManifestAndDigest([]byte(`{"idleTimeoutMs":1,"run":{"maxDurationMs":300000,"queue":"default","retry":{"enabled":false}}}`))
	if err != nil {
		t.Fatal(err)
	}
	dbtest.MustExec(t, t.Context(), b.Pool, `INSERT INTO deployment_definitions(id,environment_id,deployment_id,kind,declared_id,manifest_version,manifest,manifest_digest) VALUES($1,$2,$3,'actor','frontier',0,$4,$5)`, uuid.NewV7(), b.EnvironmentID, b.DeploymentID, manifest, digest[:])
	tx, err := b.Pool.Begin(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback(context.Background())
	dbtest.MustExec(t, t.Context(), tx, `SET CONSTRAINTS ALL DEFERRED`)
	dbtest.MustExec(t, t.Context(), tx, `INSERT INTO workspaces(id,environment_id,region_id,sandbox_declared_id,deployment_definition_id,head_version_id) VALUES($1,$2,$3,'test-workspace',$4,$5)`, f.workspaceID, b.EnvironmentID, runtest.Region, b.WorkspaceDefinitionID, f.rootID)
	dbtest.MustExec(t, t.Context(), tx, `INSERT INTO workspace_versions(id,environment_id,workspace_id,kind,state,content_digest,size_bytes,entry_count,ownership_generation,writer_generation,published_at) VALUES($1,$2,$3,'system','committed',$4,0,0,0,0,now())`, f.rootID, b.EnvironmentID, f.workspaceID, workspace.CanonicalEmptyTreeDigest)
	if err := tx.Commit(t.Context()); err != nil {
		t.Fatal(err)
	}
	started, err := f.server.startActor(t.Context(), actorStartRequest{OrgID: b.OrgID, ProjectID: b.ProjectID, EnvironmentID: b.EnvironmentID, ActorDeclaredID: "frontier", WorkspaceID: f.workspaceID, InputPresent: true, Input: json.RawMessage(`{"sequence":1}`)})
	if err != nil {
		t.Fatal(err)
	}
	f.sessionID = started.SessionID
	f.runID = started.BootRunID
	f.placeAndStart(t)
	return f
}

func (f *actorCheckpointFixture) workerCall(t *testing.T, handler http.HandlerFunc, body any, result any) {
	t.Helper()
	raw, err := json.Marshal(body)
	if err != nil {
		t.Fatal(err)
	}
	r := httptest.NewRequest(http.MethodPost, "/", bytes.NewReader(raw))
	r = r.WithContext(context.WithValue(t.Context(), workerContextKey{}, f.worker))
	w := httptest.NewRecorder()
	handler(w, r)
	if w.Code != http.StatusOK && w.Code != http.StatusNoContent {
		t.Fatalf("Worker request %T: status=%d body=%s", body, w.Code, w.Body.String())
	}
	if result != nil {
		if err := json.Unmarshal(w.Body.Bytes(), result); err != nil {
			t.Fatal(err)
		}
	}
}

func (f *actorCheckpointFixture) placeAndStart(t *testing.T) {
	t.Helper()
	f.placeAndClaim(t)
	f.startClaim(t)
}

func (f *actorCheckpointFixture) placeAndClaim(t *testing.T) {
	t.Helper()
	ctx := t.Context()
	q := f.server.db
	var version int64
	if err := f.Pool.QueryRow(ctx, `SELECT state_version FROM runs WHERE id=$1`, f.runID).Scan(&version); err != nil {
		t.Fatal(err)
	}
	candidate := dispatch.ReadyRunCandidate{OrgID: pgvalue.UUID(f.OrgID), RunID: pgvalue.UUID(f.runID), ExpectedRunStateVersion: version}
	reserved, err := f.placement.PlaceReadyRun(ctx, candidate)
	if err != nil {
		t.Fatalf("dispatch reserve: %v", err)
	}
	var rt db.RuntimeInstance
	// Report physical preparation through the same DB transition as the Worker.
	if err := f.Pool.QueryRow(ctx, `SELECT desired_version,observed_version,vm_vcpu_count,cpu_config_digest FROM runtime_instances WHERE id=$1`, reserved.RuntimeInstanceID).Scan(&rt.DesiredVersion, &rt.ObservedVersion, &rt.VMVCPUCount, &rt.CPUConfigDigest); err != nil {
		t.Fatal(err)
	}
	var substrate uuid.UUID
	if err := f.Pool.QueryRow(ctx, `INSERT INTO runtime_substrates(id,org_id,project_id,environment_id,deployment_definition_id,substrate_digest,substrate_format,substrate_contract,substrate_size_bytes) VALUES($1,$2,$3,$4,$5,$6,'squashfs','builder-v0',1) ON CONFLICT ON CONSTRAINT runtime_substrates_input_key DO UPDATE SET substrate_digest=EXCLUDED.substrate_digest RETURNING id`, uuid.NewV7(), f.OrgID, f.ProjectID, f.EnvironmentID, f.WorkspaceDefinitionID, dbtest.Digest("frontier-substrate")).Scan(&substrate); err != nil {
		t.Fatal(err)
	}
	_, err = q.MarkRuntimeInstanceReady(ctx, db.MarkRuntimeInstanceReadyParams{ID: reserved.RuntimeInstanceID, WorkerInstanceID: pgvalue.UUID(f.WorkerID), WorkerEpoch: 1, DesiredVersion: rt.DesiredVersion, ExpectedObservedVersion: rt.ObservedVersion, RuntimeSubstrateID: pgvalue.UUID(substrate), VMVCPUCount: rt.VMVCPUCount, CPUConfigDigest: rt.CPUConfigDigest})
	if err != nil {
		t.Fatal(err)
	}
	_, err = f.placement.PlaceReadyRun(ctx, candidate)
	if err != nil {
		t.Fatalf("dispatch mount: %v", err)
	}
	var mount workerapi.WorkspaceMountClaimResponse
	f.workerCall(t, f.server.workerClaimWorkspaceMount, workerapi.WorkspaceMountClaimRequest{}, &mount)
	if mount.Mount == nil {
		t.Fatal("no mount claimed")
	}
	f.workerCall(t, f.server.workerMarkWorkspaceMountMounted, workerapi.WorkspaceMountMountedRequest{OrgID: f.OrgID.String(), WorkspaceMountID: mount.Mount.ID}, nil)
	granted, err := f.placement.PlaceReadyRun(ctx, candidate)
	if err != nil || !granted.LeaseCreated {
		t.Fatalf("dispatch grant: %+v %v", granted, err)
	}
	f.claim, _, err = f.server.claimRunLease(ctx, f.worker, granted.Lease.ID, granted.Lease.LeaseSequence)
	if err != nil {
		t.Fatalf("claim: %v", err)
	}
}

func (f *actorCheckpointFixture) startClaim(t *testing.T) {
	t.Helper()
	request := workerapi.RunStartRequest{Lease: f.fence(), Fresh: &workerapi.RunStartFresh{}}
	if f.claim.runtime.RestoreCheckpointID.Valid {
		w := f.claim.runWait
		request.Fresh = nil
		request.Restore = &workerapi.RunStartRestore{RunWaitID: pgvalue.UUIDString(w.ID), CheckpointID: pgvalue.UUIDString(w.SuspendCheckpointID), ResumeAttachID: pgvalue.UUIDString(w.ResumeAttachID), ResumeRequestVersion: w.ResumeRequestVersion}
	}
	f.workerCall(t, f.server.workerStart, request, nil)
	if request.Restore != nil {
		r := request.Restore
		f.workerCall(t, f.server.workerAcknowledgeRunResumeRelease, workerapi.RunResumeReleaseRequest{Lease: f.fence(), RunWaitID: r.RunWaitID, CheckpointID: r.CheckpointID, ResumeAttachID: r.ResumeAttachID, ResumeRequestVersion: r.ResumeRequestVersion}, nil)
	}
	if request.Fresh != nil {
		f.workerCall(t, f.server.workerEnterRunEntrypoint, workerapi.RunEntrypointRequest{Lease: f.fence(), EntrypointKind: "actor", EntrypointDeclaredID: "frontier"}, nil)
	}
}

func (f *actorCheckpointFixture) fence() workerapi.RunLeaseFence {
	return workerapi.RunLeaseFence{ID: pgvalue.UUIDString(f.claim.runLease.ID), LeaseSequence: f.claim.runLease.LeaseSequence}
}

func (f *actorCheckpointFixture) capture(t *testing.T, value string) workerapi.CheckpointWorkspaceCapture {
	t.Helper()
	root := t.TempDir()
	if err := os.WriteFile(filepath.Join(root, "marker"), []byte(value), 0600); err != nil {
		t.Fatal(err)
	}
	a, tree, cleanup, err := workspace.CaptureWorkspaceArtifactContext(t.Context(), root, t.TempDir(), root, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer cleanup()
	body, err := os.Open(a.Path)
	if err != nil {
		t.Fatal(err)
	}
	defer body.Close()
	obj, err := f.server.cas.Put(t.Context(), a.MediaType, body)
	if err != nil {
		t.Fatal(err)
	}
	return workerapi.CheckpointWorkspaceCapture{Tree: workerapi.WorkspaceTreeIdentity{Digest: tree.Digest, SizeBytes: tree.SizeBytes, EntryCount: int32(tree.EntryCount)}, Artifact: workerapi.WorkspaceArtifact{Digest: obj.Digest, SizeBytes: obj.SizeBytes, MediaType: a.MediaType, Encoding: a.Encoding, EntryCount: int32(a.EntryCount)}}
}

func (f *actorCheckpointFixture) turn(t *testing.T, sequence int64, capture workerapi.CheckpointWorkspaceCapture, changed bool) workerapi.CommitActorTurnResponse {
	t.Helper()
	var base uuid.UUID
	if err := f.Pool.QueryRow(t.Context(), `SELECT base_version_id FROM workspace_leases WHERE owner_run_lease_id=$1`, f.claim.runLease.ID).Scan(&base); err != nil {
		t.Fatal(err)
	}
	req := workerapi.CommitActorTurnRequest{Lease: f.fence(), CorrelationID: uuid.NewV7().String(), TargetInputSequence: sequence, BaseWorkspaceVersionID: base.String(), Tree: capture.Tree}
	if changed {
		req.Artifact = &capture.Artifact
	}
	parsed, err := parseActorTurnCommitRequest(req)
	if err != nil {
		t.Fatal(err)
	}
	out, err := f.server.commitActorTurn(t.Context(), f.worker, req, parsed)
	if err != nil {
		t.Fatalf("commit turn %d: %v", sequence, err)
	}
	replayed, err := f.server.commitActorTurn(t.Context(), f.worker, req, parsed)
	if err != nil || replayed != out {
		t.Fatalf("turn replay: %+v %v", replayed, err)
	}
	var head, leaseBase uuid.UUID
	var committed int64
	if err := f.Pool.QueryRow(t.Context(), `SELECT w.head_version_id,l.base_version_id,s.committed_input_sequence FROM workspaces w JOIN workspace_leases l ON l.workspace_id=w.id JOIN sessions s ON s.id=$3 WHERE w.id=$1 AND l.owner_run_lease_id=$2`, f.workspaceID, f.claim.runLease.ID, f.sessionID).Scan(&head, &leaseBase, &committed); err != nil {
		t.Fatal(err)
	}
	if head.String() != out.WorkspaceVersionID || leaseBase != head || committed != sequence {
		t.Fatalf("turn frontier did not converge: head=%s lease=%s cursor=%d", head, leaseBase, committed)
	}
	return out
}

func (f *actorCheckpointFixture) suspend(t *testing.T, capture workerapi.CheckpointWorkspaceCapture) workerapi.CheckpointResponse {
	t.Helper()
	seq := int64(1)
	waitID := uuid.NewV7()
	attach := uuid.NewV7()
	params, _ := json.Marshal(workerActorInputWaitParams{SessionID: f.sessionID.String(), AfterInputSequence: seq})
	f.workerCall(t, f.server.workerCreateRunWait, workerapi.CreateRunWaitRequest{CorrelationID: uuid.NewV7().String(), Lease: f.fence(), RunWaitID: waitID.String(), ResumeAttachID: attach.String(), Kind: "actor_input", Params: params, ActorSpeculativeInputSequence: &seq}, nil)
	parsed, err := parseRunLeaseFence(f.fence())
	if err != nil {
		t.Fatal(err)
	}
	w, err := f.server.requestWorkerRunWaitCheckpoint(t.Context(), f.worker, f.fence(), parsed, waitID)
	if err != nil {
		t.Fatal(err)
	}
	req := validCheckpointReadyRequest()
	req.Lease = f.fence()
	req.RunWaitID = waitID.String()
	req.CheckpointID = pgvalue.UUIDString(w.SuspendCheckpointID)
	req.RequestVersion = w.CheckpointRequestVersion
	req.WorkspaceCapture = capture
	rp := &req.Manifest.RecoveryPoint
	rp.ID = req.CheckpointID
	rp.RunID = f.runID.String()
	rp.RunWaitID = req.RunWaitID
	rp.Runtime.ID = f.RuntimeIdentityID
	rp.Runtime.KernelDigest = dbtest.Digest("run-lease-kernel")
	rp.Runtime.InitramfsDigest = dbtest.Digest("run-lease-initramfs")
	rp.Runtime.RootfsDigest = dbtest.Digest("run-lease-rootfs")
	rp.Runtime.VMVCPUCount = 1
	rp.Runtime.CPUConfigDigest = f.CPUConfigDigest
	rp.Runtime.Substrate = &workerapi.CheckpointRuntimeSubstrate{Digest: dbtest.Digest("frontier-substrate"), Format: "squashfs", Contract: "builder-v0", SizeBytes: 1}
	// Use the committed version artifact as the actual source base descriptor.
	req.Manifest.WorkspaceState.Base = workerapi.CheckpointWorkspaceBase{ArtifactDigest: capture.Artifact.Digest, ArtifactSizeBytes: capture.Artifact.SizeBytes, ArtifactMediaType: capture.Artifact.MediaType, ArtifactEncoding: capture.Artifact.Encoding, MountPath: "/workspace"}
	for _, a := range []*workerapi.CheckpointArtifact{&req.Manifest.RuntimeState.ConfigArtifact, &req.Manifest.RuntimeState.VMStateArtifact, &req.Manifest.RuntimeState.ScratchDiskArtifact, &req.Manifest.RuntimeState.MemoryArtifacts[0]} {
		obj, err := f.server.cas.Put(t.Context(), a.MediaType, strings.NewReader(a.MediaType))
		if err != nil {
			t.Fatal(err)
		}
		a.Digest = obj.Digest
		a.SizeBytes = obj.SizeBytes
	}
	var out workerapi.CheckpointResponse
	f.workerCall(t, f.server.workerMarkCheckpointReady, req, &out)
	f.reportRuntimeClosed(t)
	return out
}

func (f *actorCheckpointFixture) reportRuntimeClosed(t *testing.T) {
	t.Helper()
	var desired, observed int64
	if err := f.Pool.QueryRow(t.Context(), `SELECT desired_version,observed_version FROM runtime_instances WHERE id=$1`, f.claim.runtime.ID).Scan(&desired, &observed); err != nil {
		t.Fatal(err)
	}
	f.workerCall(t, f.server.workerMarkRuntimeInstanceClosed, workerapi.RuntimeInstanceStateRequest{ID: pgvalue.UUIDString(f.claim.runtime.ID), WorkerEpoch: 1, DesiredVersion: desired, ExpectedObservedVersion: observed, CleanupProof: &workerapi.RuntimeCleanupProof{Method: workerapi.RuntimeCleanupSessionClosed, CompletedAt: time.Now()}}, nil)
}

func (f *actorCheckpointFixture) close(t *testing.T) {
	t.Helper()
	if _, err := f.server.closeActor(t.Context(), actorCloseRequest{EnvironmentID: f.EnvironmentID, SessionID: f.sessionID, WorkspaceID: f.workspaceID}); err != nil {
		t.Fatal(err)
	}
}

func (f *actorCheckpointFixture) complete(t *testing.T, sequence int64, content workerapi.CheckpointWorkspaceCapture) {
	t.Helper()
	a := f.claim
	var base uuid.UUID
	if err := f.Pool.QueryRow(t.Context(), `SELECT base_version_id FROM workspace_leases WHERE owner_run_lease_id=$1`, a.runLease.ID).Scan(&base); err != nil {
		t.Fatal(err)
	}
	assignment := workerapi.RunLeaseAssignment{ID: f.fence().ID, RunID: f.runID.String(), AttemptNumber: 1, LeaseSequence: f.fence().LeaseSequence, WorkerInstanceID: f.WorkerID.String(), WorkerEpoch: 1, RuntimeInstanceID: pgvalue.UUIDString(a.runtime.ID), RuntimeIdentityID: a.runtime.RuntimeIdentityID, WorkspaceID: f.workspaceID.String(), WorkspaceMountID: pgvalue.UUIDString(a.workspaceMount.ID), WorkspaceLeaseID: pgvalue.UUIDString(a.workspaceLease.ID), BaseWorkspaceVersionID: base.String(), OwnershipGeneration: a.workspace.OwnershipGeneration, WriterGeneration: a.workspace.WriterGeneration, MountFencingGeneration: a.workspaceMount.FencingGeneration, ExpiresAt: a.workspaceLease.ExpiresAt.Time}
	operation := uuid.NewV7().String()
	begin := workerapi.BeginRunFinalizationRequest{Lease: f.fence(), ProgramQuiesced: workerapi.RunQuiescenceProof{RunID: f.runID.String(), AttemptNumber: 1, RunLeaseID: f.fence().ID}, OperationID: operation, Kind: workerapi.RunFinalizationCapture}
	var began workerapi.BeginRunFinalizationResponse
	f.workerCall(t, f.server.workerBeginRunFinalization, begin, &began)
	assignment.ExpiresAt = began.ExpiresAt
	capture := validTaskWorkspaceCapture(t, assignment)
	capture.Tree = content.Tree
	capture.Artifact = content.Artifact
	capture.Receipt.OperationID = operation
	setCaptureFingerprint(t, capture)
	req := workerapi.CompleteActorRequest{Lease: f.fence(), Outcome: workerapi.ActorOutcome{TerminalInputSequence: sequence, Succeeded: &workerapi.ActorSucceeded{}}, Workspace: workerapi.TaskWorkspaceProof{Captured: capture}}
	parsed, err := parseActorCompletionRequest(req)
	if err != nil {
		t.Fatal(err)
	}
	if err := f.server.completeActor(t.Context(), f.worker, req, parsed); err != nil {
		t.Fatalf("completion: %v", err)
	}
	if err := f.server.completeActor(t.Context(), f.worker, req, parsed); err != nil {
		t.Fatalf("completion replay: %v", err)
	}
	var state, outcome, lease string
	var committed int64
	if err := f.Pool.QueryRow(t.Context(), `SELECT s.state,a.terminal_outcome,s.committed_input_sequence,w.state FROM sessions s JOIN run_attempts a ON a.run_id=$3 JOIN workspace_leases w ON w.owner_run_lease_id=$2 WHERE s.id=$1`, f.sessionID, a.runLease.ID, f.runID).Scan(&state, &outcome, &committed, &lease); err != nil {
		t.Fatal(err)
	}
	if state != "closed" || outcome != "succeeded" || committed != sequence || lease != "released" {
		t.Fatalf("completion state=%s/%s/%d/%s", state, outcome, committed, lease)
	}
}

func TestActorCheckpointFrontierPostgres(t *testing.T) {
	for _, mode := range []string{"changed", "unchanged", "close", "cancel"} {
		t.Run(mode, func(t *testing.T) {
			f := newActorCheckpointFixture(t)
			first := f.capture(t, "input1")
			turn := f.turn(t, 1, first, true)
			checkpoint := f.suspend(t, first)
			sourceLeaseID := f.claim.workspaceLease.ID
			var root, head, parent uuid.UUID
			var generation int64
			if err := f.Pool.QueryRow(t.Context(), `SELECT a.base_workspace_version_id,w.head_version_id,v.parent_version_id,v.writer_generation FROM run_attempts a JOIN workspaces w ON w.id=a.workspace_id JOIN workspace_versions v ON v.id=$2 WHERE a.run_id=$1 AND a.number=1`, f.runID, uuid.MustParse(checkpoint.WorkspaceVersionID)).Scan(&root, &head, &parent, &generation); err != nil {
				t.Fatal(err)
			}
			if root != f.rootID || head.String() != turn.WorkspaceVersionID || parent != head || root == head || generation != 1 {
				t.Fatalf("invalid produced suspend frontier: %s %s %s %d", root, head, parent, generation)
			}
			t.Logf("owning turn/checkpoint produced root != head, private parent=head, source generation=%d", generation)
			if mode == "close" {
				f.close(t)
			} else {
				_, err := f.server.appendActorInput(t.Context(), appendActorInputRequest{EnvironmentID: f.EnvironmentID, SessionID: f.sessionID, RecordID: uuid.NewV7(), Data: json.RawMessage(`{"sequence":2}`), SourceKind: "external"})
				if err != nil {
					t.Fatal(err)
				}
			}
			f.placeAndStart(t)
			if mode == "cancel" {
				canceler, err := runauthority.NewCanceler(f.Pool)
				if err != nil {
					t.Fatal(err)
				}
				if _, err := canceler.Cancel(t.Context(), runauthority.CancellationRequest{OrgID: f.OrgID, ProjectID: f.ProjectID, EnvironmentID: f.EnvironmentID, RunID: f.runID}); err != nil {
					t.Fatal(err)
				}
				req := workerapi.CommitActorTurnRequest{Lease: f.fence(), CorrelationID: uuid.NewV7().String(), TargetInputSequence: 2, BaseWorkspaceVersionID: checkpoint.WorkspaceVersionID, Tree: first.Tree}
				parsed, err := parseActorTurnCommitRequest(req)
				if err != nil {
					t.Fatal(err)
				}
				if _, err := f.server.commitActorTurn(t.Context(), f.worker, req, parsed); !errors.Is(err, errStaleActorTurnCommit) {
					t.Fatalf("cancelled writer turn: %v", err)
				}
				var status string
				var retainedHead uuid.UUID
				if err := f.Pool.QueryRow(t.Context(), `SELECT r.status,w.head_version_id FROM runs r JOIN workspaces w ON w.id=r.workspace_id WHERE r.id=$1`, f.runID).Scan(&status, &retainedHead); err != nil {
					t.Fatal(err)
				}
				if status != "cancelled" || retainedHead != head {
					t.Fatal("cancellation changed the committed frontier")
				}
				return
			}
			if f.claim.workspace.WriterGeneration != generation+1 {
				t.Fatal("resumed writer did not advance")
			}
			terminal := int64(1)
			last := first
			if mode != "close" {
				if mode == "changed" {
					last = f.capture(t, "input2")
				}
				second := f.turn(t, 2, last, mode == "changed")
				terminal = 2
				if mode == "unchanged" && second.WorkspaceVersionID != checkpoint.WorkspaceVersionID {
					t.Fatal("unchanged turn did not publish private version")
				}
				f.close(t)
			}
			f.complete(t, terminal, last)
			var retainedGeneration int64
			var retainedSource, retainedParent uuid.UUID
			if err := f.Pool.QueryRow(t.Context(), `SELECT writer_generation,source_workspace_lease_id,parent_version_id FROM workspace_versions WHERE id=$1`, uuid.MustParse(checkpoint.WorkspaceVersionID)).Scan(&retainedGeneration, &retainedSource, &retainedParent); err != nil {
				t.Fatal(err)
			}
			if retainedGeneration != generation || retainedParent != head || pgvalue.UUID(retainedSource) != sourceLeaseID {
				t.Fatal("checkpoint provenance rewritten")
			}
			t.Logf("restored %s turn/completion and replay succeeded; source generation=%d, resumed=%d", mode, retainedGeneration, f.claim.workspace.WriterGeneration)
		})
	}
}

func TestActorCheckpointFrontierRejectsInvalidRestorePostgres(t *testing.T) {
	f := newActorCheckpointFixture(t)
	first := f.capture(t, "input1")
	f.turn(t, 1, first, true)
	checkpoint := f.suspend(t, first)
	_, err := f.server.appendActorInput(t.Context(), appendActorInputRequest{EnvironmentID: f.EnvironmentID, SessionID: f.sessionID, RecordID: uuid.NewV7(), Data: json.RawMessage(`{"sequence":2}`), SourceKind: "external"})
	if err != nil {
		t.Fatal(err)
	}
	f.placeAndStart(t)
	// Each negative uses a rolled-back transaction over the produced restore.
	// Invalid rows are deliberate corruption tests, not modeled positive states.
	for _, test := range []struct {
		name, sql  string
		constraint string
		args       []any
		mutate     func(*runLeaseClaimAuthority)
	}{
		{name: "wrong Run", mutate: func(a *runLeaseClaimAuthority) { a.run.ID = pgvalue.NewUUIDv7() }},
		{name: "wrong attempt", mutate: func(a *runLeaseClaimAuthority) { a.attempt.Number++ }},
		{name: "wrong head", mutate: func(a *runLeaseClaimAuthority) { a.workspace.HeadVersionID = pgvalue.UUID(f.rootID) }},
		{name: "wrong current generation", mutate: func(a *runLeaseClaimAuthority) { a.workspace.WriterGeneration++ }},
		{name: "wrong private parent", sql: `UPDATE workspace_versions SET parent_version_id=$2 WHERE id=$1`, args: []any{uuid.MustParse(checkpoint.WorkspaceVersionID), f.rootID}},
		{name: "private generation rewritten", constraint: "workspace_versions_workspace_id_source_workspace_lease_id__fkey", sql: `UPDATE workspace_versions SET writer_generation=2 WHERE id=$1`, args: []any{uuid.MustParse(checkpoint.WorkspaceVersionID)}},
		{name: "wrong source lease", constraint: "run_checkpoints_workspace_id_source_run_lease_id_source_wo_fkey", sql: `UPDATE run_checkpoints SET source_workspace_lease_id=$2 WHERE id=$1`, args: []any{uuid.MustParse(checkpoint.CheckpointID), f.claim.workspaceLease.ID}},
		{name: "wrong private artifact", constraint: "workspace_versions_environment_id_artifact_id_artifact_kin_fkey", sql: `UPDATE workspace_versions SET artifact_id=(SELECT program_artifact_id FROM deployments WHERE id=$2) WHERE id=$1`, args: []any{uuid.MustParse(checkpoint.WorkspaceVersionID), f.DeploymentID}},
		{name: "source still running", constraint: "runtime_instances_check4", sql: `UPDATE runtime_instances SET observed_state='ready' WHERE id=(SELECT runtime_instance_id FROM run_leases WHERE id=(SELECT source_run_lease_id FROM run_checkpoints WHERE id=$1))`, args: []any{uuid.MustParse(checkpoint.CheckpointID)}},
		{name: "checkpoint invalid", sql: `UPDATE run_checkpoints SET state='invalid',invalidated_at=now(),invalidation_reason_code='test' WHERE id=$1`, args: []any{uuid.MustParse(checkpoint.CheckpointID)}},
	} {
		t.Run(test.name, func(t *testing.T) {
			tx, err := f.Pool.Begin(t.Context())
			if err != nil {
				t.Fatal(err)
			}
			defer tx.Rollback(context.Background())
			if test.sql != "" {
				_, err := tx.Exec(t.Context(), test.sql, test.args...)
				if test.constraint != "" {
					var pgerr *pgconn.PgError
					if !errors.As(err, &pgerr) || (pgerr.Code != "23503" && pgerr.Code != "23514") || pgerr.ConstraintName != test.constraint {
						t.Fatalf("expected owning storage constraint %s: %v", test.constraint, err)
					}
					t.Logf("rejected by owning schema constraint %s; invalid state never admitted", pgerr.ConstraintName)
					return
				}
				if err != nil {
					t.Fatal(err)
				}
			}
			a := f.claim
			if test.mutate != nil {
				test.mutate(&a)
			}
			q := db.New(tx)
			base, err := getActorTurnVersion(t.Context(), q, a, a.workspaceLease.BaseVersionID)
			if err == nil {
				err = validateRestoredActorBase(t.Context(), q, a, base)
			}
			if err == nil {
				t.Fatal("invalid restore admitted")
			}
		})
	}
}

func TestActorCheckpointFrontierExpiredBeforePlacementPostgres(t *testing.T) {
	f := newActorCheckpointFixture(t)
	first := f.capture(t, "input1")
	f.turn(t, 1, first, true)
	cp := f.suspend(t, first)
	if _, err := f.server.appendActorInput(t.Context(), appendActorInputRequest{EnvironmentID: f.EnvironmentID, SessionID: f.sessionID, RecordID: uuid.NewV7(), Data: json.RawMessage(`{"sequence":2}`), SourceKind: "external"}); err != nil {
		t.Fatal(err)
	}
	dbtest.MustExec(t, t.Context(), f.Pool, `UPDATE run_checkpoints SET expires_at=transaction_timestamp()-interval '1 second' WHERE id=$1`, uuid.MustParse(cp.CheckpointID))
	var state int64
	if err := f.Pool.QueryRow(t.Context(), `SELECT state_version FROM runs WHERE id=$1`, f.runID).Scan(&state); err != nil {
		t.Fatal(err)
	}
	_, err := f.placement.PlaceReadyRun(t.Context(), dispatch.ReadyRunCandidate{OrgID: pgvalue.UUID(f.OrgID), RunID: pgvalue.UUID(f.runID), ExpectedRunStateVersion: state})
	if !errors.Is(err, dispatch.ErrCandidateChanged) {
		t.Fatalf("expired checkpoint placement: %v", err)
	}
	var leases int
	if err := f.Pool.QueryRow(t.Context(), `SELECT count(*) FROM workspace_leases WHERE workspace_id=$1 AND state='active'`, f.workspaceID).Scan(&leases); err != nil {
		t.Fatal(err)
	}
	if leases != 0 {
		t.Fatal("expired checkpoint granted a writer")
	}
}

// Inject only the deadline; the owning recovery operation must expire both
// leases and requeue the same wait/checkpoint before any replacement placement.
func (f *actorCheckpointFixture) expireRestore(t *testing.T) {
	t.Helper()
	dbtest.MustExec(t, t.Context(), f.Pool, `WITH expired AS (
 UPDATE run_leases SET start_deadline_at=assigned_at+interval '1 millisecond', expires_at=assigned_at+interval '2 milliseconds' WHERE id=$1 RETURNING id,expires_at
) UPDATE workspace_leases SET expires_at=expired.expires_at FROM expired WHERE owner_run_lease_id=expired.id`, f.claim.runLease.ID)
	dbtest.MustExec(t, t.Context(), f.Pool, `SELECT pg_sleep(0.01)`)
	recovered, err := f.placement.RecoverExpiredRunResumes(t.Context(), 10)
	if err != nil || len(recovered) != 1 {
		t.Fatalf("recover: %v %v", recovered, err)
	}
	var status, waitState, leaseState string
	var checkpoint uuid.UUID
	if err := f.Pool.QueryRow(t.Context(), `SELECT r.status,w.suspension_state,w.suspend_checkpoint_id,l.state FROM runs r JOIN run_waits w ON w.run_id=r.id AND w.attempt_number=r.current_attempt_number JOIN run_leases l ON l.id=$2 WHERE r.id=$1`, f.runID, f.claim.runLease.ID).Scan(&status, &waitState, &checkpoint, &leaseState); err != nil {
		t.Fatal(err)
	}
	if status != "queued" || waitState != "resume_pending" || leaseState != "expired" || pgvalue.UUID(checkpoint) != f.claim.runtime.RestoreCheckpointID {
		t.Fatalf("unexpected recovered state: %s/%s/%s/%s", status, waitState, leaseState, checkpoint)
	}
	f.workerCall(t, f.server.workerStopWorkspaceMount, workerapi.WorkspaceMountStopRequest{
		OrgID: f.OrgID.String(), WorkspaceMountID: pgvalue.UUIDString(f.claim.workspaceMount.ID),
		CleanupProof: workerapi.RuntimeCleanupProof{Method: workerapi.RuntimeCleanupSessionClosed, CompletedAt: time.Now()},
	}, nil)
}

func TestActorCheckpointFrontierRecoveredRestorePostgres(t *testing.T) {
	for _, changed := range []bool{true, false} {
		t.Run(fmt.Sprintf("changed=%t", changed), func(t *testing.T) {
			f := newActorCheckpointFixture(t)
			first := f.capture(t, "input1")
			turn := f.turn(t, 1, first, true)
			cp := f.suspend(t, first)
			sourceLease := f.claim.workspaceLease.ID
			if _, err := f.server.appendActorInput(t.Context(), appendActorInputRequest{EnvironmentID: f.EnvironmentID, SessionID: f.sessionID, RecordID: uuid.NewV7(), Data: json.RawMessage(`{"sequence":2}`), SourceKind: "external"}); err != nil {
				t.Fatal(err)
			}
			for generation := int64(2); generation <= 3; generation++ {
				f.placeAndClaim(t)
				if f.claim.workspace.WriterGeneration != generation {
					t.Fatal("restore grant did not advance writer")
				}
				f.expireRestore(t)
				t.Logf("owning recovery requeued writer %d, preserving checkpoint %s", generation, cp.CheckpointID)
			}
			f.placeAndStart(t)
			if f.claim.workspace.WriterGeneration != 4 || f.claim.attempt.BaseWorkspaceVersionID != pgvalue.UUID(f.rootID) {
				t.Fatal("replacement writer/immutable attempt root mismatch")
			}
			last := first
			if changed {
				last = f.capture(t, "input2")
			}
			second := f.turn(t, 2, last, changed)
			if !changed && second.WorkspaceVersionID != cp.WorkspaceVersionID {
				t.Fatal("unchanged recovered turn did not publish private version")
			}
			f.close(t)
			f.complete(t, 2, last)
			var parent, source uuid.UUID
			var generation int64
			if err := f.Pool.QueryRow(t.Context(), `SELECT parent_version_id,source_workspace_lease_id,writer_generation FROM workspace_versions WHERE id=$1`, uuid.MustParse(cp.WorkspaceVersionID)).Scan(&parent, &source, &generation); err != nil {
				t.Fatal(err)
			}
			if parent.String() != turn.WorkspaceVersionID || pgvalue.UUID(source) != sourceLease || generation != 1 {
				t.Fatal("recovery rewrote immutable checkpoint provenance")
			}
			t.Log("replacement writer4 completed with source writer1 unchanged; turn/completion replay and cursor/head convergence passed")
		})
	}
}

func TestActorCheckpointFrontierRejectsWrongRecoveredWriterPostgres(t *testing.T) {
	for _, name := range []string{"Run", "attempt", "checkpoint", "private base", "generation", "ownership", "head", "owner", "terminal reason", "terminal state"} {
		t.Run(name, func(t *testing.T) {
			f := newActorCheckpointFixture(t)
			first := f.capture(t, "input1")
			f.turn(t, 1, first, true)
			f.suspend(t, first)
			if _, err := f.server.appendActorInput(t.Context(), appendActorInputRequest{EnvironmentID: f.EnvironmentID, SessionID: f.sessionID, RecordID: uuid.NewV7(), Data: json.RawMessage(`{"sequence":2}`), SourceKind: "external"}); err != nil {
				t.Fatal(err)
			}
			f.placeAndClaim(t)
			f.expireRestore(t)
			var sql string
			var args []any
			switch name {
			case "Run":
				sql, args = `UPDATE run_leases SET run_id=$2 WHERE id=$1`, []any{f.claim.runLease.ID, uuid.NewV7()}
			case "attempt":
				sql, args = `UPDATE run_leases SET attempt_number=attempt_number+1 WHERE id=$1`, []any{f.claim.runLease.ID}
			case "checkpoint":
				sql, args = `UPDATE runtime_instances SET restore_checkpoint_id=NULL WHERE id=$1`, []any{f.claim.runtime.ID}
			case "private base":
				sql, args = `UPDATE workspace_leases SET base_version_id=$2 WHERE id=$1`, []any{f.claim.workspaceLease.ID, f.rootID}
			case "generation":
				sql, args = `UPDATE workspaces SET writer_generation=writer_generation+1 WHERE id=$1`, []any{f.workspaceID}
			case "ownership":
				sql, args = `UPDATE workspaces SET ownership_generation=ownership_generation+1 WHERE id=$1`, []any{f.workspaceID}
			case "head":
				sql, args = `UPDATE workspaces SET head_version_id=$2 WHERE id=$1`, []any{f.workspaceID, f.rootID}
			case "owner":
				sql, args = `UPDATE workspaces SET owner_session_id=NULL WHERE id=$1`, []any{f.workspaceID}
			case "terminal reason":
				sql, args = `UPDATE run_leases SET terminal_reason_code='max_active_duration_exceeded' WHERE id=$1`, []any{f.claim.runLease.ID}
			case "terminal state":
				sql, args = `UPDATE workspace_leases SET state='fenced' WHERE id=$1`, []any{f.claim.workspaceLease.ID}
			}
			_, err := f.Pool.Exec(t.Context(), sql, args...)
			if name == "Run" || name == "attempt" {
				var pgerr *pgconn.PgError
				if !errors.As(err, &pgerr) || pgerr.Code != "23503" {
					t.Fatalf("expected source-scoping FK rejection: %v", err)
				}
				t.Logf("unrelated %s rejected by owning FK %s before dispatch", name, pgerr.ConstraintName)
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			var state int64
			if err := f.Pool.QueryRow(t.Context(), `SELECT state_version FROM runs WHERE id=$1`, f.runID).Scan(&state); err != nil {
				t.Fatal(err)
			}
			_, err = f.placement.PlaceReadyRun(t.Context(), dispatch.ReadyRunCandidate{OrgID: pgvalue.UUID(f.OrgID), RunID: pgvalue.UUID(f.runID), ExpectedRunStateVersion: state})
			if !errors.Is(err, dispatch.ErrCandidateChanged) {
				t.Fatalf("wrong recovered %s admitted: %v", name, err)
			}
		})
	}
}
