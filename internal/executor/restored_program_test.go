package executor

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net"
	"sync"
	"testing"
	"time"

	"github.com/helmrdotdev/helmr/internal/api"
	"github.com/helmrdotdev/helmr/internal/cas"
	"github.com/helmrdotdev/helmr/internal/deployment"
	"github.com/helmrdotdev/helmr/internal/frameio"
	runv0 "github.com/helmrdotdev/helmr/internal/proto/run/v0"
	workspacev0 "github.com/helmrdotdev/helmr/internal/proto/workspace/v0"
	"github.com/helmrdotdev/helmr/internal/sha256sum"
	"github.com/helmrdotdev/helmr/internal/vm"
	"github.com/helmrdotdev/helmr/internal/wire"
	"github.com/helmrdotdev/helmr/internal/workerapi"
	"google.golang.org/protobuf/proto"
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
			RuntimeInstanceID: claim.Lease.RuntimeInstanceID,
			Target:            workerapi.WorkspaceResetTarget{BaseWorkspaceVersionID: claim.Lease.BaseWorkspaceVersionID},
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

func TestStartRestoredProgramRenewsAuthorityUntilRelease(t *testing.T) {
	claim := testRestoredProgramClaim(t)
	claim.Lease.ExpiresAt = time.Now().Add(300 * time.Millisecond)
	resumeGuest, resumeHost := net.Pipe()
	grantGuest, grantHost := net.Pipe()
	renewGuest, renewHost := net.Pipe()
	parent := &queuedStreamSession{streams: []vm.Stream{
		testVMStream(resumeHost), testVMStream(grantHost), testVMStream(renewHost),
	}}
	mounts := NewWorkspaceMountSessions()
	unregister := mounts.RegisterWorkspaceMountSession(
		workerapi.WorkspaceMount{
			ID: claim.Lease.WorkspaceMountID, WorkspaceID: claim.Lease.WorkspaceID,
			RuntimeInstanceID: claim.Lease.RuntimeInstanceID,
			Target:            workerapi.WorkspaceResetTarget{BaseWorkspaceVersionID: claim.Lease.BaseWorkspaceVersionID},
			FencingGeneration: claim.Lease.MountFencingGeneration, RestoreCheckpointID: "checkpoint-1",
		}, parent, "restored-channel",
	)
	defer unregister()
	releaseGate := make(chan struct{})
	controlPlane := &restoredProgramControlPlane{
		lease: claim.Lease, wantStart: "restore", releaseGate: releaseGate,
		renewExpiresAt: time.Now().Add(time.Minute), renewed: make(chan struct{}),
	}
	guestErr := make(chan error, 3)
	go func() { guestErr <- serveRestoredGrant(grantGuest) }()
	go func() { guestErr <- serveRestoredResume(resumeGuest) }()
	go func() { guestErr <- serveRestoredAuthorityRenewal(renewGuest) }()
	programResult := make(chan struct {
		program freshProgram
		err     error
	}, 1)
	go func() {
		program, err := (ProgramRunner{WorkspaceMounts: mounts}).startResumedProgram(
			context.Background(), &claim, controlPlane,
		)
		programResult <- struct {
			program freshProgram
			err     error
		}{program: program, err: err}
	}()
	select {
	case <-controlPlane.renewed:
	case <-time.After(3 * time.Second):
		t.Fatal("restored admission did not renew while release was blocked")
	}
	close(releaseGate)
	result := <-programResult
	if result.err != nil {
		t.Fatal(result.err)
	}
	defer result.program.session.Close(context.Background())
	if !result.program.lease.ExpiresAt.Equal(controlPlane.renewExpiresAt) ||
		result.program.authority.GetFence().GetExpiresAtUnixNano() != controlPlane.renewExpiresAt.UnixNano() {
		t.Fatalf("restored Program did not return latest authority: lease=%v fence=%d",
			result.program.lease.ExpiresAt, result.program.authority.GetFence().GetExpiresAtUnixNano())
	}
	for range 3 {
		if err := <-guestErr; err != nil {
			t.Fatal(err)
		}
	}
}

func TestStartRestoredProgramStopsBlockedStartAtStartDeadline(t *testing.T) {
	claim := testRestoredProgramClaim(t)
	claim.Lease.StartDeadlineAt = time.Now().Add(50 * time.Millisecond).UTC()
	resumeGuest, resumeHost := net.Pipe()
	defer resumeGuest.Close()
	grantGuest, grantHost := net.Pipe()
	parent := &queuedStreamSession{streams: []vm.Stream{testVMStream(resumeHost), testVMStream(grantHost)}}
	mounts := NewWorkspaceMountSessions()
	unregister := mounts.RegisterWorkspaceMountSession(
		workerapi.WorkspaceMount{
			ID: claim.Lease.WorkspaceMountID, WorkspaceID: claim.Lease.WorkspaceID,
			RuntimeInstanceID: claim.Lease.RuntimeInstanceID,
			Target:            workerapi.WorkspaceResetTarget{BaseWorkspaceVersionID: claim.Lease.BaseWorkspaceVersionID},
			FencingGeneration: claim.Lease.MountFencingGeneration, RestoreCheckpointID: "checkpoint-1",
		}, parent, "restored-channel",
	)
	defer unregister()
	startGate := make(chan struct{})
	controlPlane := &restoredProgramControlPlane{
		lease: claim.Lease, wantStart: "restore", startGate: startGate,
	}
	grantDone := make(chan error, 1)
	go func() { grantDone <- serveRestoredGrant(grantGuest) }()
	started := time.Now()
	_, err := (ProgramRunner{WorkspaceMounts: mounts}).startResumedProgram(
		context.Background(), &claim, controlPlane,
	)
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("startResumedProgram() error = %v", err)
	}
	if elapsed := time.Since(started); elapsed > time.Second {
		t.Fatalf("blocked restored start stopped after %s", elapsed)
	}
	if err := <-grantDone; err != nil {
		t.Fatal(err)
	}
	controlPlane.mu.Lock()
	defer controlPlane.mu.Unlock()
	if controlPlane.started || controlPlane.renewCalls != 0 {
		t.Fatalf("started=%v renew calls=%d", controlPlane.started, controlPlane.renewCalls)
	}
	if claim.Workspace.WriteCapability != "" {
		t.Fatal("timed-out restored claim retained its write capability")
	}
}

func TestStartRestoredProgramCancellationClosesBlockedAttach(t *testing.T) {
	claim := testRestoredProgramClaim(t)
	resume := newBlockingWriteStream()
	grantGuest, grantHost := net.Pipe()
	parent := &queuedStreamSession{streams: []vm.Stream{resume, testVMStream(grantHost)}}
	mounts := NewWorkspaceMountSessions()
	unregister := mounts.RegisterWorkspaceMountSession(
		workerapi.WorkspaceMount{
			ID: claim.Lease.WorkspaceMountID, WorkspaceID: claim.Lease.WorkspaceID,
			RuntimeInstanceID: claim.Lease.RuntimeInstanceID,
			Target:            workerapi.WorkspaceResetTarget{BaseWorkspaceVersionID: claim.Lease.BaseWorkspaceVersionID},
			FencingGeneration: claim.Lease.MountFencingGeneration, RestoreCheckpointID: "checkpoint-1",
		}, parent, "restored-channel",
	)
	defer unregister()
	controlPlane := &restoredProgramControlPlane{lease: claim.Lease, wantStart: "restore"}
	grantDone := make(chan error, 1)
	go func() { grantDone <- serveRestoredGrant(grantGuest) }()
	ctx, cancel := context.WithCancel(context.Background())
	result := make(chan error, 1)
	go func() {
		_, err := (ProgramRunner{WorkspaceMounts: mounts}).startResumedProgram(ctx, &claim, controlPlane)
		result <- err
	}()
	if err := <-grantDone; err != nil {
		t.Fatal(err)
	}
	select {
	case <-resume.entered:
	case <-time.After(time.Second):
		t.Fatal("restored attach write did not start")
	}
	cancel()
	select {
	case err := <-result:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("blocked restored attach cancellation = %v", err)
		}
	case <-time.After(time.Second):
		t.Fatal("blocked restored attach ignored cancellation")
	}
}

type blockingWriteStream struct {
	entered   chan struct{}
	closed    chan struct{}
	enterOnce sync.Once
	closeOnce sync.Once
}

func newBlockingWriteStream() *blockingWriteStream {
	return &blockingWriteStream{entered: make(chan struct{}), closed: make(chan struct{})}
}

func (stream *blockingWriteStream) Read([]byte) (int, error) {
	<-stream.closed
	return 0, io.ErrClosedPipe
}

func (stream *blockingWriteStream) Write([]byte) (int, error) {
	stream.enterOnce.Do(func() { close(stream.entered) })
	<-stream.closed
	return 0, io.ErrClosedPipe
}

func (stream *blockingWriteStream) Close() error {
	stream.closeOnce.Do(func() { close(stream.closed) })
	return nil
}

func TestValidateResumedProgramMountSeparatesPhysicalIdentityFromLogicalFence(t *testing.T) {
	claim := testRestoredProgramClaim(t)
	mount := testWorkspaceMount(claim.Lease)
	mount.FencingGeneration = claim.Lease.MountFencingGeneration - 1
	mount.RestoreCheckpointID = "checkpoint-1"
	resume := resumedProgramAdmission{checkpointID: "checkpoint-1"}
	if err := validateResumedProgramMount(claim.Lease, mount, resume); err != nil {
		t.Fatalf("advanced logical fence rejected exact physical mount: %v", err)
	}

	tests := []struct {
		name   string
		mutate func(*workerapi.WorkspaceMount)
	}{
		{name: "mount ID", mutate: func(mount *workerapi.WorkspaceMount) { mount.ID = "other-mount" }},
		{name: "Workspace ID", mutate: func(mount *workerapi.WorkspaceMount) { mount.WorkspaceID = "other-workspace" }},
		{name: "Runtime Instance", mutate: func(mount *workerapi.WorkspaceMount) { mount.RuntimeInstanceID = "other-runtime" }},
		{name: "base Workspace version", mutate: func(mount *workerapi.WorkspaceMount) { mount.Target.BaseWorkspaceVersionID = "other-version" }},
		{name: "restore checkpoint", mutate: func(mount *workerapi.WorkspaceMount) { mount.RestoreCheckpointID = "other-checkpoint" }},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			mismatched := mount
			test.mutate(&mismatched)
			if err := validateResumedProgramMount(claim.Lease, mismatched, resume); err == nil {
				t.Fatal("physical identity mismatch was accepted")
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
	cpuConfigDigest := sha256sum.DigestBytes([]byte("cpu-config"))
	artifact := func(digest, mediaType string, size int64) workerapi.CheckpointArtifact {
		return workerapi.CheckpointArtifact{Digest: digest, MediaType: mediaType, SizeBytes: size}
	}
	checkpoint := workerapi.CheckpointManifest{
		RecoveryPoint: workerapi.CheckpointRecoveryPoint{
			ID: "checkpoint-1", RunID: "run-1", AttemptNumber: 2, RunWaitID: "wait-1", CorrelationID: "correlation-1",
			Runtime: workerapi.CheckpointRuntime{Backend: "firecracker", ID: "runtime-shape", Arch: testCheckpointRuntimeArchitecture(),
				Contract: "abi-1", KernelDigest: "kernel", InitramfsDigest: "initramfs", RootfsDigest: "rootfs", ConfigDigest: "config",
				VMVCPUCount: 2, CPUConfigDigest: cpuConfigDigest},
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
		return workerapi.CASObject(value)
	}
	target := workerapi.RuntimeReconcileTarget{Source: workerapi.RuntimeSource{
		VMVCPUCount: 2, CPUConfigDigest: cpuConfigDigest,
		WorkspaceTarget: &workerapi.WorkspaceResetTarget{
			BaseWorkspaceVersionID: "target-version",
			Tree:                   workerapi.WorkspaceTreeIdentity{Digest: "sha256:target", SizeBytes: 75, EntryCount: 1},
			Artifact: &workerapi.WorkspaceArtifact{Digest: "captured-workspace-object", SizeBytes: 75,
				MediaType: "workspace-media", Encoding: "workspace-encoding"},
		},
		Restore: &workerapi.RuntimeRestore{
			CheckpointID: "checkpoint-1", RunID: "run-1", AttemptNumber: 2, RunWaitID: "wait-1",
			Manifest: manifest,
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
	target.Source.CPUConfigDigest = sha256sum.DigestBytes([]byte("other-cpu-config"))
	if _, err := validatePreparedRuntimeRestore(target, deployment.ArchitectureX8664); err == nil {
		t.Fatal("mismatched runtime reservation CPU shape was accepted")
	}
	target.Source.CPUConfigDigest = cpuConfigDigest
	target.Source.Restore.Artifacts[2].Role = "vm_state"
	if _, err := validatePreparedRuntimeRestore(target, deployment.ArchitectureX8664); err == nil {
		t.Fatal("mismatched Checkpoint Artifact membership was accepted")
	}
	target.Source.Restore.Artifacts[2].Role = "memory"
	checkpoint.WorkspaceState.Base.MountPath = "/other"
	target.Source.Restore.Manifest, err = json.Marshal(checkpoint)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := validatePreparedRuntimeRestore(target, deployment.ArchitectureX8664); err == nil {
		t.Fatal("noncanonical Checkpoint manifest Workspace mount was accepted")
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

func serveRestoredAuthorityRenewal(conn net.Conn) error {
	defer conn.Close()
	header, _, err := wire.ReadStreamFrameHeader(conn)
	if err != nil {
		return err
	}
	if header.Type != wire.StreamTypeWorkspaceAuthorityRenew {
		return errors.New("unexpected restored authority renewal stream")
	}
	var request workspacev0.RenewWorkspaceAuthorityRequest
	if err := frameio.ReadProtoFrame(conn, &request); err != nil {
		return err
	}
	fence := proto.Clone(request.GetPrevious().GetFence()).(*workspacev0.WorkspaceAuthorityFence)
	fence.ExpiresAtUnixNano = request.GetNewExpiresAtUnixNano()
	return frameio.WriteProtoFrame(conn, &workspacev0.RenewWorkspaceAuthorityResponse{Fence: fence})
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
		Manifest: manifest,
		Decision: workerapi.RunLeaseDecision{Completed: &workerapi.RunLeaseCompleted{NoResult: &struct{}{}}},
	}}
	return claim
}

type queuedStreamSession struct {
	mu      sync.Mutex
	streams []vm.Stream
	opened  []vm.Stream
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
	s.opened = append(s.opened, stream)
	return stream, nil
}
func (s *queuedStreamSession) Close(context.Context) error {
	s.mu.Lock()
	streams := append(append([]vm.Stream(nil), s.opened...), s.streams...)
	s.mu.Unlock()
	var err error
	for _, stream := range streams {
		err = errors.Join(err, stream.Close())
	}
	return err
}
func (*queuedStreamSession) Wait(ctx context.Context) error { <-ctx.Done(); return ctx.Err() }

type restoredProgramControlPlane struct {
	mu             sync.Mutex
	lease          workerapi.RunLeaseAssignment
	started        bool
	released       bool
	releaseCalls   int
	wantStart      string
	startGate      <-chan struct{}
	releaseGate    <-chan struct{}
	renewExpiresAt time.Time
	renewCalls     int
	renewed        chan struct{}
	renewOnce      sync.Once
}

func (c *restoredProgramControlPlane) ClaimRunLease(context.Context, workerapi.RunLeaseWork) (workerapi.RunLeaseClaimResponse, error) {
	return workerapi.RunLeaseClaimResponse{}, errors.New("unexpected claim")
}
func (c *restoredProgramControlPlane) AcknowledgeRunStart(ctx context.Context, request workerapi.RunStartRequest) (workerapi.RunStartResponse, error) {
	if c.startGate != nil {
		select {
		case <-c.startGate:
		case <-ctx.Done():
			return workerapi.RunStartResponse{}, ctx.Err()
		}
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	validArm := request.Restore != nil && request.Fresh == nil
	if !validArm || request.Lease != c.lease.Fence() {
		return workerapi.RunStartResponse{}, errors.New("unexpected restore start")
	}
	c.started = true
	return workerapi.RunStartResponse{Lease: c.lease.Fence()}, nil
}
func (*restoredProgramControlPlane) AcknowledgeRunEntrypoint(context.Context, workerapi.RunEntrypointRequest) error {
	return errors.New("unexpected entrypoint")
}

func (c *restoredProgramControlPlane) RenewRunLease(_ context.Context, previous workerapi.RunLeaseAssignment) (workerapi.RunLeaseRenewResponse, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if !c.started {
		return workerapi.RunLeaseRenewResponse{}, errors.New("renew before start")
	}
	expiresAt := c.lease.ExpiresAt
	c.renewCalls++
	if !c.renewExpiresAt.IsZero() {
		expiresAt = c.renewExpiresAt
		c.lease.ExpiresAt = expiresAt
		c.renewOnce.Do(func() { close(c.renewed) })
	}
	return workerapi.RunLeaseRenewResponse{
		Lease: previous.Fence(), ExpiresAt: expiresAt,
		BaseWorkspaceVersionID: previous.BaseWorkspaceVersionID,
	}, nil
}
func (*restoredProgramControlPlane) BeginRunFinalization(context.Context, workerapi.BeginRunFinalizationRequest) (workerapi.BeginRunFinalizationResponse, error) {
	return workerapi.BeginRunFinalizationResponse{}, errors.New("unexpected finalization")
}
func (*restoredProgramControlPlane) CompleteTask(context.Context, workerapi.CompleteTaskRequest) error {
	return errors.New("unexpected completion")
}
func (*restoredProgramControlPlane) CompleteActor(context.Context, workerapi.CompleteActorRequest) error {
	return errors.New("unexpected actor completion")
}
func (*restoredProgramControlPlane) CommitActorTurn(context.Context, workerapi.CommitActorTurnRequest) (workerapi.CommitActorTurnResponse, error) {
	return workerapi.CommitActorTurnResponse{}, errors.New("unexpected actor turn commit")
}
func (*restoredProgramControlPlane) SendRunActorInput(context.Context, workerapi.SendActorInputRequest) (workerapi.SendActorInputResponse, error) {
	return workerapi.SendActorInputResponse{}, errors.New("unexpected actor input send")
}

func (*restoredProgramControlPlane) AppendActorOutput(context.Context, workerapi.AppendActorOutputRequest) (workerapi.AppendActorOutputResponse, error) {
	return workerapi.AppendActorOutputResponse{}, errors.New("unexpected actor output append")
}
func (*restoredProgramControlPlane) CreateRuntimeToken(context.Context, workerapi.CreateTokenRequest) (api.TokenResponse, error) {
	return api.TokenResponse{}, errors.New("unexpected token create")
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
func (c *restoredProgramControlPlane) AcknowledgeRunResumeRelease(ctx context.Context, request workerapi.RunResumeReleaseRequest) (workerapi.RunResumeReleaseResponse, error) {
	if c.releaseGate != nil {
		select {
		case <-c.releaseGate:
		case <-ctx.Done():
			return workerapi.RunResumeReleaseResponse{}, ctx.Err()
		}
	}
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
