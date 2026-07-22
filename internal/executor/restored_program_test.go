package executor

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net"
	"runtime"
	"sync"
	"testing"

	"github.com/helmrdotdev/helmr/internal/api"
	"github.com/helmrdotdev/helmr/internal/cas"
	"github.com/helmrdotdev/helmr/internal/frameio"
	runv0 "github.com/helmrdotdev/helmr/internal/proto/run/v0"
	workspacev0 "github.com/helmrdotdev/helmr/internal/proto/workspace/v0"
	"github.com/helmrdotdev/helmr/internal/vm"
	"github.com/helmrdotdev/helmr/internal/wire"
)

func TestStartRestoredProgramOrdersGrantStartProofAndRelease(t *testing.T) {
	claim := testRestoredProgramClaim(t)
	resumeGuest, resumeHost := net.Pipe()
	grantGuest, grantHost := net.Pipe()
	parent := &queuedStreamSession{streams: []vm.Stream{testVMStream(resumeHost), testVMStream(grantHost)}}
	mounts := NewWorkspaceMountSessions()
	unregister := mounts.RegisterWorkspaceMountSession(
		api.WorkerWorkspaceMount{
			ID: claim.Lease.WorkspaceMountID, WorkspaceID: claim.Lease.WorkspaceID,
			RuntimeInstanceID: claim.Lease.RuntimeInstanceID, BaseVersionID: claim.Lease.BaseWorkspaceVersionID,
			FencingGeneration: claim.Lease.MountFencingGeneration, RestoreCheckpointID: "checkpoint-1",
		}, parent, "restored-channel",
	)
	defer unregister()
	control := &restoredProgramControl{lease: claim.Lease}
	guestErr := make(chan error, 2)
	go func() { guestErr <- serveRestoredGrant(grantGuest) }()
	go func() { guestErr <- serveRestoredResume(resumeGuest) }()
	program, err := (GuestRunner{WorkspaceMounts: mounts}).startRestoredProgram(
		context.Background(), &claim, control,
	)
	if err != nil {
		t.Fatal(err)
	}
	defer program.session.Close(context.Background())
	if program.entrypoint.GetTask() == nil || !equalRunLeaseReceipt(program.lease, claim.Lease) {
		t.Fatalf("restored Program = %+v", program)
	}
	for range 2 {
		if err := <-guestErr; err != nil {
			t.Fatal(err)
		}
	}
	control.mu.Lock()
	defer control.mu.Unlock()
	if !control.started || !control.released || control.releaseCalls != 2 {
		t.Fatalf("start=%v release=%v release calls=%d", control.started, control.released, control.releaseCalls)
	}
}

func TestRestoredProgramDecisionPreservesTerminalUnion(t *testing.T) {
	tests := []struct {
		name     string
		decision api.WorkerRunLeaseDecision
		wantKind string
		wantData string
		noResult bool
		wantErr  bool
	}{
		{name: "completed absent", decision: api.WorkerRunLeaseDecision{Completed: &api.WorkerRunLeaseCompleted{NoResult: &struct{}{}}}, wantKind: "completed", noResult: true},
		{name: "completed null", decision: api.WorkerRunLeaseDecision{Completed: &api.WorkerRunLeaseCompleted{ResultJSON: json.RawMessage(`null`)}}, wantKind: "completed", wantData: "null"},
		{name: "completed missing variant", decision: api.WorkerRunLeaseDecision{Completed: &api.WorkerRunLeaseCompleted{}}, wantErr: true},
		{name: "failed", decision: api.WorkerRunLeaseDecision{Failed: &api.WorkerRunLeaseFailed{ReasonCode: "token_expired", Error: json.RawMessage(`{"message":"expired"}`)}}, wantKind: "failed", wantData: `{"reason_code":"token_expired","error":{"message":"expired"}}`},
		{name: "cancelled", decision: api.WorkerRunLeaseDecision{Cancelled: &api.WorkerRunLeaseCancelled{ReasonCode: "run_cancelled"}}, wantKind: "cancelled", wantData: `{"reason_code":"run_cancelled"}`},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			kind, data, noResult, err := restoredProgramDecision(test.decision)
			if test.wantErr {
				if err == nil {
					t.Fatal("invalid decision was accepted")
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			if kind != test.wantKind || string(data) != test.wantData || noResult != test.noResult {
				t.Fatalf("decision = %q %s no_result=%v, want %q %s no_result=%v", kind, data, noResult, test.wantKind, test.wantData, test.noResult)
			}
		})
	}
}

func TestValidatePreparedRuntimeRestoreExactTupleAndMembership(t *testing.T) {
	artifact := func(digest, mediaType string, size int64) api.WorkerCheckpointArtifact {
		return api.WorkerCheckpointArtifact{Digest: digest, MediaType: mediaType, SizeBytes: size}
	}
	checkpoint := api.WorkerCheckpointManifest{
		RecoveryPoint: api.WorkerCheckpointRecoveryPoint{
			ID: "checkpoint-1", RunID: "run-1", AttemptNumber: 2, RunWaitID: "wait-1", CorrelationID: "correlation-1",
			Runtime: api.WorkerCheckpointRuntime{Backend: "firecracker", ID: "runtime-shape", Arch: runtime.GOARCH,
				ABI: "abi-1", KernelDigest: "kernel", InitramfsDigest: "initramfs", RootfsDigest: "rootfs", ConfigDigest: "config"},
		},
		RuntimeState: api.WorkerCheckpointRuntimeState{
			ConfigArtifact:      artifact("config-object", cas.CheckpointRuntimeConfigMediaType, 10),
			VMStateArtifact:     artifact("state-object", cas.CheckpointVMStateMediaType, 20),
			MemoryArtifacts:     []api.WorkerCheckpointArtifact{artifact("memory-object", cas.CheckpointMemoryMediaType, 30)},
			ScratchDiskArtifact: artifact("scratch-object", cas.CheckpointScratchDiskMediaType, 40),
		},
		WorkspaceState: api.WorkerCheckpointWorkspaceState{Base: api.WorkerCheckpointWorkspaceBase{
			ArtifactDigest: "workspace-object", ArtifactSizeBytes: 50,
			ArtifactMediaType: "workspace-media", ArtifactEncoding: "workspace-encoding", MountPath: "/workspace",
		}},
	}
	manifest, err := json.Marshal(checkpoint)
	if err != nil {
		t.Fatal(err)
	}
	object := func(value api.WorkerCheckpointArtifact) api.CASObject {
		return api.CASObject{Digest: value.Digest, SizeBytes: value.SizeBytes, MediaType: value.MediaType}
	}
	target := api.WorkerRuntimeReconcileTarget{Source: api.WorkerRuntimeSource{
		WorkspaceArtifact: api.WorkerWorkspaceArtifact{Digest: "workspace-object", SizeBytes: 50,
			MediaType: "workspace-media", Encoding: "workspace-encoding"},
		Restore: &api.WorkerRuntimeRestore{
			CheckpointID: "checkpoint-1", RunID: "run-1", AttemptNumber: 2, RunWaitID: "wait-1",
			Kind: "suspend", Manifest: manifest,
			Artifacts: []api.WorkerRunLeaseCheckpointArtifact{
				{Role: "runtime_config", Object: object(checkpoint.RuntimeState.ConfigArtifact)},
				{Role: "vm_state", Object: object(checkpoint.RuntimeState.VMStateArtifact)},
				{Role: "memory", Object: object(checkpoint.RuntimeState.MemoryArtifacts[0])},
				{Role: "scratch_disk", Object: object(checkpoint.RuntimeState.ScratchDiskArtifact)},
			},
		},
	}}
	if _, err := validatePreparedRuntimeRestore(target); err != nil {
		t.Fatal(err)
	}
	target.Source.Restore.Artifacts[2].Role = "vm_state"
	if _, err := validatePreparedRuntimeRestore(target); err == nil {
		t.Fatal("mismatched Checkpoint Artifact membership was accepted")
	}
}

func serveRestoredGrant(conn net.Conn) error {
	defer conn.Close()
	header, bodyLen, err := wire.ReadStreamFrameHeader(conn)
	if err != nil {
		return err
	}
	if header.Type != wire.StreamTypeProgramResumeGrant || bodyLen != 0 {
		return errors.New("unexpected restored grant header")
	}
	var request workspacev0.GrantProgramResumeRequest
	if err := frameio.ReadProtoFrame(conn, &request); err != nil {
		return err
	}
	return frameio.WriteProtoFrame(conn, &workspacev0.GrantProgramResumeResponse{
		Fence: request.GetAuthority().GetFence(), RunWaitId: request.GetRunWaitId(),
		CheckpointId: request.GetCheckpointId(), ResumeAttachId: request.GetResumeAttachId(),
		ResumeRequestVersion: request.GetResumeRequestVersion(), CorrelationId: request.GetCorrelationId(),
	})
}

func serveRestoredResume(conn net.Conn) error {
	defer conn.Close()
	var attach runv0.ResumeAttach
	if err := frameio.ReadProtoFrame(conn, &attach); err != nil {
		return err
	}
	var decision runv0.ResumeDecision
	if err := frameio.ReadProtoFrame(conn, &decision); err != nil {
		return err
	}
	if decision.GetKind() != "completed" || decision.GetDataJson() != "" || !decision.GetNoResult() ||
		decision.GetCorrelationId() != attach.GetCorrelationId() {
		return errors.New("unexpected restored decision")
	}
	return frameio.WriteProtoFrame(conn, &runv0.ResumeAck{
		RunWaitId: attach.GetRunWaitId(), CheckpointId: attach.GetCheckpointId(),
		ResumeAttachId: attach.GetResumeAttachId(), ResumeRequestVersion: attach.GetResumeRequestVersion(),
		RunLeaseId: attach.GetRunLeaseId(), CorrelationId: attach.GetCorrelationId(),
	})
}

func testRestoredProgramClaim(t *testing.T) api.WorkerRunLeaseClaimResponse {
	t.Helper()
	claim := testFreshProgramClaim(t)
	checkpoint := api.WorkerCheckpointManifest{RecoveryPoint: api.WorkerCheckpointRecoveryPoint{
		ID: "checkpoint-1", RunID: claim.Lease.RunID, AttemptNumber: claim.Lease.AttemptNumber,
		RunWaitID: "wait-1", CorrelationID: "correlation-1",
	}}
	manifest, err := json.Marshal(checkpoint)
	if err != nil {
		t.Fatal(err)
	}
	claim.Secrets = nil
	claim.Execution = api.WorkerRunLeaseExecution{Restore: &api.WorkerRunLeaseRestore{
		RunWaitID: "wait-1", CheckpointID: "checkpoint-1", ResumeAttachID: "attach-1",
		ResumeRequestVersion: 4, Recreated: &api.WorkerRunLeaseRecreatedRestore{Kind: "suspend", Manifest: manifest},
		Decision: api.WorkerRunLeaseDecision{Completed: &api.WorkerRunLeaseCompleted{NoResult: &struct{}{}}},
	}}
	return claim
}

type queuedStreamSession struct {
	mu      sync.Mutex
	streams []vm.Stream
}

func (s *queuedStreamSession) Stream() vm.Stream { return nil }
func (s *queuedStreamSession) OpenStream(context.Context) (vm.Stream, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if len(s.streams) == 0 {
		return nil, io.EOF
	}
	stream := s.streams[0]
	s.streams = s.streams[1:]
	return stream, nil
}
func (*queuedStreamSession) Close(context.Context) error    { return nil }
func (*queuedStreamSession) Wait(ctx context.Context) error { <-ctx.Done(); return ctx.Err() }

type restoredProgramControl struct {
	mu           sync.Mutex
	lease        api.WorkerRunLeaseReceipt
	started      bool
	released     bool
	releaseCalls int
}

func (c *restoredProgramControl) ClaimRunLease(context.Context, api.WorkerRunLeaseWork) (api.WorkerRunLeaseClaimResponse, error) {
	return api.WorkerRunLeaseClaimResponse{}, errors.New("unexpected claim")
}
func (c *restoredProgramControl) AcknowledgeRunStart(_ context.Context, request api.WorkerRunStartRequest) (api.WorkerRunStartResponse, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if request.Restore == nil || !equalRunLeaseReceipt(request.Lease, c.lease) {
		return api.WorkerRunStartResponse{}, errors.New("unexpected restore start")
	}
	c.started = true
	return api.WorkerRunStartResponse{Lease: c.lease}, nil
}
func (*restoredProgramControl) AcknowledgeRunEntrypoint(context.Context, api.WorkerRunEntrypointRequest) error {
	return errors.New("unexpected entrypoint")
}
func (c *restoredProgramControl) RenewRunLease(context.Context, api.WorkerRunLeaseReceipt) (api.WorkerRunLeaseRenewResponse, error) {
	return api.WorkerRunLeaseRenewResponse{Lease: c.lease}, nil
}
func (*restoredProgramControl) BeginRunFinalization(context.Context, api.WorkerBeginRunFinalizationRequest) (api.WorkerBeginRunFinalizationResponse, error) {
	return api.WorkerBeginRunFinalizationResponse{}, errors.New("unexpected finalization")
}
func (*restoredProgramControl) CompleteTask(context.Context, api.WorkerCompleteTaskRequest) error {
	return errors.New("unexpected completion")
}
func (*restoredProgramControl) CompleteActor(context.Context, api.WorkerCompleteActorRequest) error {
	return errors.New("unexpected Actor completion")
}
func (*restoredProgramControl) CommitActorTurn(context.Context, api.WorkerCommitActorTurnRequest) (api.WorkerCommitActorTurnResponse, error) {
	return api.WorkerCommitActorTurnResponse{}, errors.New("unexpected Actor turn commit")
}
func (*restoredProgramControl) AppendRunLog(context.Context, api.WorkerRunLeaseReceipt, api.WorkerLogStream, uint64, []byte) error {
	return nil
}
func (*restoredProgramControl) CreateRunWait(context.Context, api.WorkerCreateRunWaitRequest) (api.WorkerCreateRunWaitResponse, error) {
	return api.WorkerCreateRunWaitResponse{}, errors.New("unexpected wait")
}
func (*restoredProgramControl) PollRunWait(context.Context, api.WorkerRunWaitPollRequest) (api.WorkerRunWaitPollResponse, error) {
	return api.WorkerRunWaitPollResponse{}, errors.New("unexpected poll")
}
func (*restoredProgramControl) AcknowledgeRunWaitResume(context.Context, api.WorkerRunWaitResumeAckRequest) (api.WorkerRunWaitResumeAckResponse, error) {
	return api.WorkerRunWaitResumeAckResponse{}, errors.New("unexpected old resume")
}
func (*restoredProgramControl) CaptureRunWaitWorkspace(context.Context, api.WorkerRunWaitWorkspaceCaptureRequest) (api.WorkerRunWaitWorkspaceCaptureResponse, error) {
	return api.WorkerRunWaitWorkspaceCaptureResponse{}, errors.New("unexpected capture")
}
func (*restoredProgramControl) MarkCheckpointReady(context.Context, api.WorkerCheckpointReadyRequest) (api.WorkerCheckpointResponse, error) {
	return api.WorkerCheckpointResponse{}, errors.New("unexpected checkpoint")
}
func (*restoredProgramControl) MarkCheckpointFailed(context.Context, api.WorkerCheckpointFailedRequest) (api.WorkerCheckpointResponse, error) {
	return api.WorkerCheckpointResponse{}, errors.New("unexpected checkpoint failure")
}
func (c *restoredProgramControl) AcknowledgeRunResumeRelease(_ context.Context, request api.WorkerRunResumeReleaseRequest) (api.WorkerRunResumeReleaseResponse, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.releaseCalls++
	if !c.started || request.RunLeaseID != c.lease.ID {
		return api.WorkerRunResumeReleaseResponse{}, errors.New("release before start")
	}
	if c.releaseCalls == 1 {
		return api.WorkerRunResumeReleaseResponse{}, errors.New("transient lost release response")
	}
	c.released = true
	return api.WorkerRunResumeReleaseResponse{Lease: c.lease, RunWaitID: request.RunWaitID,
		CheckpointID: request.CheckpointID, ResumeAttachID: request.ResumeAttachID,
		ResumeRequestVersion: request.ResumeRequestVersion}, nil
}
