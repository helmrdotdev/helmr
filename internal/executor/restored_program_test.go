package executor

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net"
	"sync"
	"testing"

	"github.com/helmrdotdev/helmr/internal/api"
	"github.com/helmrdotdev/helmr/internal/cas"
	"github.com/helmrdotdev/helmr/internal/deployment"
	"github.com/helmrdotdev/helmr/internal/frameio"
	runv0 "github.com/helmrdotdev/helmr/internal/proto/run/v0"
	workspacev0 "github.com/helmrdotdev/helmr/internal/proto/workspace/v0"
	"github.com/helmrdotdev/helmr/internal/vm"
	"github.com/helmrdotdev/helmr/internal/wire"
	"github.com/helmrdotdev/helmr/internal/workerapi"
)

func TestStartRestoredProgramOrdersGrantStartProofAndRelease(t *testing.T) {
	claim := testRestoredProgramClaim(t)
	resumeGuest, resumeHost := net.Pipe()
	grantGuest, grantHost := net.Pipe()
	parent := &queuedStreamSession{streams: []vm.Stream{testVMStream(resumeHost), testVMStream(grantHost)}}
	mounts := NewWorkspaceMountSessions()
	unregister := mounts.RegisterWorkspaceMountSession(
		workerapi.WorkspaceMount{
			ID: claim.Lease.WorkspaceMountID, WorkspaceID: claim.Lease.WorkspaceID,
			RuntimeInstanceID: claim.Lease.RuntimeInstanceID, BaseVersionID: claim.Lease.BaseWorkspaceVersionID,
			FencingGeneration: claim.Lease.MountFencingGeneration, RestoreCheckpointID: "checkpoint-1",
		}, parent, "restored-channel",
	)
	defer unregister()
	controlPlane := &restoredProgramControlPlane{lease: claim.Lease, wantStart: "restore"}
	guestErr := make(chan error, 2)
	go func() { guestErr <- serveRestoredGrant(grantGuest) }()
	go func() { guestErr <- serveRestoredResume(resumeGuest) }()
	program, err := (ProgramRunner{WorkspaceMounts: mounts}).startResumedProgram(
		context.Background(), &claim, controlPlane,
	)
	if err != nil {
		t.Fatal(err)
	}
	defer program.session.Close(context.Background())
	if program.entrypoint.GetTask() == nil || !equalRunLeaseAssignment(program.lease, claim.Lease) {
		t.Fatalf("restored Program = %+v", program)
	}
	for range 2 {
		if err := <-guestErr; err != nil {
			t.Fatal(err)
		}
	}
	controlPlane.mu.Lock()
	defer controlPlane.mu.Unlock()
	if !controlPlane.started || !controlPlane.released || controlPlane.releaseCalls != 2 {
		t.Fatalf("start=%v release=%v release calls=%d", controlPlane.started, controlPlane.released, controlPlane.releaseCalls)
	}
}

func TestRetainedRestoreAndParentAttachResumeOnExistingMount(t *testing.T) {
	tests := []struct {
		name      string
		wantStart string
		wantActor bool
		prepare   func(workerapi.RunLeaseClaimResponse) workerapi.RunLeaseClaimResponse
	}{
		{
			name:      "retained restore",
			wantStart: "restore",
			prepare: func(claim workerapi.RunLeaseClaimResponse) workerapi.RunLeaseClaimResponse {
				restore := claim.Execution.Restore
				restore.Recreated = nil
				restore.Retained = &workerapi.RunLeaseRetainedRestore{
					EnclosingRunWaitID: "outer-wait-1",
				}
				return claim
			},
		},
		{
			name:      "parent attach",
			wantStart: "parent",
			wantActor: true,
			prepare: func(claim workerapi.RunLeaseClaimResponse) workerapi.RunLeaseClaimResponse {
				restore := claim.Execution.Restore
				claim.Execution = workerapi.RunLeaseExecution{
					Attach: &workerapi.RunLeaseAttach{
						Parent: &workerapi.RunLeaseParentAttach{
							RunWaitID:            restore.RunWaitID,
							CheckpointID:         restore.CheckpointID,
							ResumeAttachID:       restore.ResumeAttachID,
							ResumeRequestVersion: restore.ResumeRequestVersion,
							CorrelationID:        restore.CorrelationID,
							EntrypointKind:       "actor",
							EntrypointDeclaredID: "operator",
							Decision:             restore.Decision,
						},
					},
				}
				return claim
			},
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			claim := test.prepare(testRestoredProgramClaim(t))
			resumeGuest, resumeHost := net.Pipe()
			grantGuest, grantHost := net.Pipe()
			parent := &queuedStreamSession{
				streams: []vm.Stream{
					testVMStream(resumeHost),
					testVMStream(grantHost),
				},
			}
			mount := testWorkspaceMount(claim.Lease)
			mounts := NewWorkspaceMountSessions()
			unregister := mounts.RegisterWorkspaceMountSession(
				mount,
				parent,
				"restored-channel",
			)
			defer unregister()
			controlPlane := &restoredProgramControlPlane{
				lease:     claim.Lease,
				wantStart: test.wantStart,
			}
			guestErr := make(chan error, 2)
			go func() { guestErr <- serveRestoredGrant(grantGuest) }()
			go func() { guestErr <- serveRestoredResume(resumeGuest) }()
			program, err := (ProgramRunner{
				WorkspaceMounts: mounts,
			}).startResumedProgram(
				context.Background(),
				&claim,
				controlPlane,
			)
			if err != nil {
				t.Fatal(err)
			}
			defer program.session.Close(context.Background())
			if (program.entrypoint.GetActor() != nil) != test.wantActor {
				t.Fatalf("resumed entrypoint = %+v", program.entrypoint)
			}
			for range 2 {
				if err := <-guestErr; err != nil {
					t.Fatal(err)
				}
			}
		})
	}
}

func TestRestoredProgramDecisionPreservesTerminalUnion(t *testing.T) {
	tests := []struct {
		name     string
		decision workerapi.RunLeaseDecision
		wantKind string
		wantData string
		noResult bool
		wantErr  bool
	}{
		{name: "completed absent", decision: workerapi.RunLeaseDecision{Completed: &workerapi.RunLeaseCompleted{NoResult: &struct{}{}}}, wantKind: "completed", noResult: true},
		{name: "completed null", decision: workerapi.RunLeaseDecision{Completed: &workerapi.RunLeaseCompleted{ResultJSON: json.RawMessage(`null`)}}, wantKind: "completed", wantData: "null"},
		{name: "completed missing variant", decision: workerapi.RunLeaseDecision{Completed: &workerapi.RunLeaseCompleted{}}, wantErr: true},
		{name: "failed", decision: workerapi.RunLeaseDecision{Failed: &workerapi.RunLeaseFailed{ReasonCode: "token_expired", Error: json.RawMessage(`{"message":"expired"}`)}}, wantKind: "failed", wantData: `{"reason_code":"token_expired","error":{"message":"expired"}}`},
		{name: "cancelled", decision: workerapi.RunLeaseDecision{Cancelled: &workerapi.RunLeaseCancelled{ReasonCode: "run_cancelled"}}, wantKind: "cancelled", wantData: `{"reason_code":"run_cancelled"}`},
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
	artifact := func(digest, mediaType string, size int64) workerapi.CheckpointArtifact {
		return workerapi.CheckpointArtifact{Digest: digest, MediaType: mediaType, SizeBytes: size}
	}
	checkpoint := workerapi.CheckpointManifest{
		RecoveryPoint: workerapi.CheckpointRecoveryPoint{
			ID: "checkpoint-1", RunID: "run-1", AttemptNumber: 2, RunWaitID: "wait-1", CorrelationID: "correlation-1",
			Runtime: workerapi.CheckpointRuntime{Backend: "firecracker", ID: "runtime-shape", Arch: testCheckpointRuntimeArchitecture(),
				ABI: "abi-1", KernelDigest: "kernel", InitramfsDigest: "initramfs", RootfsDigest: "rootfs", ConfigDigest: "config"},
		},
		RuntimeState: workerapi.CheckpointRuntimeState{
			ConfigArtifact:      artifact("config-object", cas.CheckpointRuntimeConfigMediaType, 10),
			VMStateArtifact:     artifact("state-object", cas.CheckpointVMStateMediaType, 20),
			MemoryArtifacts:     []workerapi.CheckpointArtifact{artifact("memory-object", cas.CheckpointMemoryMediaType, 30)},
			ScratchDiskArtifact: artifact("scratch-object", cas.CheckpointScratchDiskMediaType, 40),
		},
		WorkspaceState: workerapi.CheckpointWorkspaceState{Base: workerapi.CheckpointWorkspaceBase{
			ArtifactDigest: "workspace-object", ArtifactSizeBytes: 50,
			ArtifactMediaType: "workspace-media", ArtifactEncoding: "workspace-encoding", MountPath: "/workspace",
		}},
	}
	manifest, err := json.Marshal(checkpoint)
	if err != nil {
		t.Fatal(err)
	}
	object := func(value workerapi.CheckpointArtifact) workerapi.CASObject {
		return workerapi.CASObject{Digest: value.Digest, SizeBytes: value.SizeBytes, MediaType: value.MediaType}
	}
	target := workerapi.RuntimeReconcileTarget{Source: workerapi.RuntimeSource{
		WorkspaceArtifact: workerapi.WorkspaceArtifact{Digest: "workspace-object", SizeBytes: 50,
			MediaType: "workspace-media", Encoding: "workspace-encoding"},
		Restore: &workerapi.RuntimeRestore{
			CheckpointID: "checkpoint-1", RunID: "run-1", AttemptNumber: 2, RunWaitID: "wait-1",
			Kind: "suspend", Manifest: manifest,
			Artifacts: []workerapi.RunLeaseCheckpointArtifact{
				{Role: "runtime_config", Object: object(checkpoint.RuntimeState.ConfigArtifact)},
				{Role: "vm_state", Object: object(checkpoint.RuntimeState.VMStateArtifact)},
				{Role: "memory", Object: object(checkpoint.RuntimeState.MemoryArtifacts[0])},
				{Role: "scratch_disk", Object: object(checkpoint.RuntimeState.ScratchDiskArtifact)},
			},
		},
	}}
	if _, err := validatePreparedRuntimeRestore(target, deployment.ArchitectureX8664); err != nil {
		t.Fatal(err)
	}
	target.Source.Restore.Artifacts[2].Role = "vm_state"
	if _, err := validatePreparedRuntimeRestore(target, deployment.ArchitectureX8664); err == nil {
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

func testRestoredProgramClaim(t *testing.T) workerapi.RunLeaseClaimResponse {
	t.Helper()
	claim := testFreshProgramClaim(t)
	checkpoint := workerapi.CheckpointManifest{RecoveryPoint: workerapi.CheckpointRecoveryPoint{
		ID: "checkpoint-1", RunID: claim.Lease.RunID, AttemptNumber: claim.Lease.AttemptNumber,
		RunWaitID: "wait-1", CorrelationID: "correlation-1",
	}}
	manifest, err := json.Marshal(checkpoint)
	if err != nil {
		t.Fatal(err)
	}
	claim.Secrets = nil
	claim.Execution = workerapi.RunLeaseExecution{Restore: &workerapi.RunLeaseRestore{
		RunWaitID: "wait-1", CheckpointID: "checkpoint-1", ResumeAttachID: "attach-1",
		ResumeRequestVersion: 4, CorrelationID: "correlation-1",
		EntrypointKind: "task", EntrypointDeclaredID: "deploy",
		Recreated: &workerapi.RunLeaseRecreatedRestore{Kind: "suspend", Manifest: manifest},
		Decision:  workerapi.RunLeaseDecision{Completed: &workerapi.RunLeaseCompleted{NoResult: &struct{}{}}},
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

type restoredProgramControlPlane struct {
	mu           sync.Mutex
	lease        workerapi.RunLeaseAssignment
	started      bool
	released     bool
	releaseCalls int
	wantStart    string
}

func (c *restoredProgramControlPlane) ClaimRunLease(context.Context, workerapi.RunLeaseWork) (workerapi.RunLeaseClaimResponse, error) {
	return workerapi.RunLeaseClaimResponse{}, errors.New("unexpected claim")
}
func (c *restoredProgramControlPlane) AcknowledgeRunStart(_ context.Context, request workerapi.RunStartRequest) (workerapi.RunStartResponse, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	validArm := request.Restore != nil &&
		request.Fresh == nil &&
		request.Attach == nil
	if c.wantStart == "parent" {
		validArm = request.Restore == nil &&
			request.Fresh == nil &&
			request.Attach != nil &&
			request.Attach.Parent != nil &&
			request.Attach.Child == nil
	}
	if !validArm || request.Lease != c.lease.Fence() {
		return workerapi.RunStartResponse{}, errors.New("unexpected restore start")
	}
	c.started = true
	return workerapi.RunStartResponse{Lease: c.lease.Fence()}, nil
}
func (*restoredProgramControlPlane) AcknowledgeRunEntrypoint(context.Context, workerapi.RunEntrypointRequest) error {
	return errors.New("unexpected entrypoint")
}
func (c *restoredProgramControlPlane) RenewRunLease(context.Context, workerapi.RunLeaseAssignment) (workerapi.RunLeaseRenewResponse, error) {
	return workerapi.RunLeaseRenewResponse{
		Lease: c.lease.Fence(), ExpiresAt: c.lease.ExpiresAt,
		BaseWorkspaceVersionID: c.lease.BaseWorkspaceVersionID,
	}, nil
}
func (*restoredProgramControlPlane) BeginRunFinalization(context.Context, workerapi.BeginRunFinalizationRequest) (workerapi.BeginRunFinalizationResponse, error) {
	return workerapi.BeginRunFinalizationResponse{}, errors.New("unexpected finalization")
}
func (*restoredProgramControlPlane) CompleteTask(context.Context, workerapi.CompleteTaskRequest) error {
	return errors.New("unexpected completion")
}
func (*restoredProgramControlPlane) CompleteActor(context.Context, workerapi.CompleteActorRequest) error {
	return errors.New("unexpected Actor completion")
}
func (*restoredProgramControlPlane) CommitActorTurn(context.Context, workerapi.CommitActorTurnRequest) (workerapi.CommitActorTurnResponse, error) {
	return workerapi.CommitActorTurnResponse{}, errors.New("unexpected Actor turn commit")
}
func (*restoredProgramControlPlane) SendRunActorInput(context.Context, workerapi.SendActorInputRequest) (workerapi.SendActorInputResponse, error) {
	return workerapi.SendActorInputResponse{}, errors.New("unexpected Actor input send")
}

func (*restoredProgramControlPlane) AppendActorOutput(context.Context, workerapi.AppendActorOutputRequest) (workerapi.AppendActorOutputResponse, error) {
	return workerapi.AppendActorOutputResponse{}, errors.New("unexpected Actor output append")
}
func (*restoredProgramControlPlane) CreateRuntimeToken(context.Context, workerapi.CreateTokenRequest) (api.TokenResponse, error) {
	return api.TokenResponse{}, errors.New("unexpected Token create")
}
func (*restoredProgramControlPlane) AppendRunLog(context.Context, workerapi.RunLeaseAssignment, workerapi.LogStream, uint64, []byte) error {
	return nil
}
func (*restoredProgramControlPlane) CreateRunWait(context.Context, workerapi.CreateRunWaitRequest) (workerapi.CreateRunWaitResponse, error) {
	return workerapi.CreateRunWaitResponse{}, errors.New("unexpected wait")
}
func (*restoredProgramControlPlane) PollRunWait(context.Context, workerapi.RunWaitPollRequest) (workerapi.RunWaitPollResponse, error) {
	return workerapi.RunWaitPollResponse{}, errors.New("unexpected poll")
}
func (*restoredProgramControlPlane) AcknowledgeRunWaitResume(context.Context, workerapi.RunWaitResumeAckRequest) (workerapi.RunWaitResumeAckResponse, error) {
	return workerapi.RunWaitResumeAckResponse{}, errors.New("unexpected old resume")
}
func (*restoredProgramControlPlane) MarkCheckpointReady(context.Context, workerapi.CheckpointReadyRequest) (workerapi.CheckpointResponse, error) {
	return workerapi.CheckpointResponse{}, errors.New("unexpected checkpoint")
}
func (*restoredProgramControlPlane) MarkCheckpointFailed(context.Context, workerapi.CheckpointFailedRequest) (workerapi.CheckpointResponse, error) {
	return workerapi.CheckpointResponse{}, errors.New("unexpected checkpoint failure")
}
func (c *restoredProgramControlPlane) AcknowledgeRunResumeRelease(_ context.Context, request workerapi.RunResumeReleaseRequest) (workerapi.RunResumeReleaseResponse, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.releaseCalls++
	if !c.started || request.Lease != c.lease.Fence() {
		return workerapi.RunResumeReleaseResponse{}, errors.New("release before start")
	}
	if c.releaseCalls == 1 {
		return workerapi.RunResumeReleaseResponse{}, errors.New("transient lost release response")
	}
	c.released = true
	return workerapi.RunResumeReleaseResponse{Lease: c.lease.Fence(), RunWaitID: request.RunWaitID,
		CheckpointID: request.CheckpointID, ResumeAttachID: request.ResumeAttachID,
		ResumeRequestVersion: request.ResumeRequestVersion}, nil
}
