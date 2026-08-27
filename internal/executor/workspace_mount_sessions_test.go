package executor

import (
	"context"
	"errors"
	"io"
	"net"
	"sync/atomic"
	"testing"
	"time"

	"github.com/helmrdotdev/helmr/internal/frameio"
	workspacev0 "github.com/helmrdotdev/helmr/internal/proto/workspace/v0"
	"github.com/helmrdotdev/helmr/internal/vm"
	"github.com/helmrdotdev/helmr/internal/wire"
	"github.com/helmrdotdev/helmr/internal/workerapi"
	"google.golang.org/protobuf/proto"
)

func TestManagedWorkspaceMountSessionClosesPhysicalSessionOnce(t *testing.T) {
	closeErr := errors.New("close failed")
	physical := &blockingCloseSession{started: make(chan struct{}), release: make(chan struct{}), closeErr: closeErr}
	session := newManagedWorkspaceMountSession(physical)

	closeResult := make(chan error, 1)
	go func() { closeResult <- session.Close(context.Background()) }()
	waitForTestSignal(t, physical.started, "physical close start")

	releaseResult := make(chan error, 1)
	go func() { releaseResult <- session.ReleaseCheckpointSource(context.Background()) }()
	close(physical.release)

	if err := waitForTestError(t, closeResult, "managed close"); !errors.Is(err, closeErr) {
		t.Fatalf("managed close error = %v, want %v", err, closeErr)
	}
	if err := waitForTestError(t, releaseResult, "checkpoint release"); !errors.Is(err, closeErr) {
		t.Fatalf("checkpoint release error = %v, want %v", err, closeErr)
	}
	if got := physical.closeCount.Load(); got != 1 {
		t.Fatalf("physical close count = %d, want 1", got)
	}
	released, err := session.CheckpointReleaseResult(context.Background())
	if !errors.Is(err, closeErr) {
		t.Fatalf("checkpoint release result error = %v, want %v", err, closeErr)
	}
	if !released {
		t.Fatal("checkpoint release was not recorded")
	}
}

func TestManagedWorkspaceMountSessionDuplicateReleaseObservesContext(t *testing.T) {
	physical := &blockingCloseSession{started: make(chan struct{}), release: make(chan struct{})}
	session := newManagedWorkspaceMountSession(physical)

	firstRelease := make(chan error, 1)
	go func() { firstRelease <- session.ReleaseCheckpointSource(context.Background()) }()
	waitForTestSignal(t, physical.started, "physical close start")

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if err := session.ReleaseCheckpointSource(ctx); !errors.Is(err, context.Canceled) {
		t.Fatalf("duplicate release error = %v, want context.Canceled", err)
	}

	closeResult := make(chan error, 1)
	go func() { closeResult <- session.Close(context.Background()) }()
	close(physical.release)
	if err := waitForTestError(t, firstRelease, "first checkpoint release"); err != nil {
		t.Fatal(err)
	}
	if err := waitForTestError(t, closeResult, "managed close"); err != nil {
		t.Fatal(err)
	}
	if got := physical.closeCount.Load(); got != 1 {
		t.Fatalf("physical close count = %d, want 1", got)
	}
}

func waitForTestSignal(t *testing.T, signal <-chan struct{}, name string) {
	t.Helper()
	select {
	case <-signal:
	case <-time.After(5 * time.Second):
		t.Fatalf("timed out waiting for %s", name)
	}
}

func waitForTestError(t *testing.T, result <-chan error, name string) error {
	t.Helper()
	select {
	case err := <-result:
		return err
	case <-time.After(5 * time.Second):
		t.Fatalf("timed out waiting for %s", name)
		return nil
	}
}

func TestBorrowedRunSessionReleaseCheckpointSourceClosesParentWorkspaceMount(t *testing.T) {
	parent := &borrowedParentSession{stream: discardReadWriteCloser{}}
	runStream := &countingReadWriteCloser{}
	session := newBorrowedRunSession(parent, testVMStream(runStream))

	checkpointable, ok := session.(vm.CheckpointableSession)
	if !ok {
		t.Fatal("borrowed run session is not checkpointable")
	}
	if _, err := checkpointable.CreateSnapshot(context.Background(), vm.SnapshotRequest{ID: "checkpoint"}); err != nil {
		t.Fatal(err)
	}
	releaser, ok := session.(CheckpointSourceReleaser)
	if !ok {
		t.Fatal("borrowed run session cannot release checkpoint source")
	}
	if err := releaser.ReleaseCheckpointSource(context.Background()); err != nil {
		t.Fatal(err)
	}
	if parent.closeCount != 1 {
		t.Fatalf("parent close count = %d, want 1", parent.closeCount)
	}
	if runStream.closeCount != 0 {
		t.Fatalf("run stream close count = %d, want 0", runStream.closeCount)
	}
}

func TestRenewWorkspaceAuthorityUsesMountedSession(t *testing.T) {
	host, guest := net.Pipe()
	defer host.Close()
	defer guest.Close()
	parent := &borrowedParentSession{stream: discardReadWriteCloser{}, openStream: host}
	registry := NewWorkspaceMountSessions()
	registry.RegisterWorkspaceMountSession(workerapi.WorkspaceMount{
		ID:                "mount-1",
		WorkspaceID:       "workspace-1",
		RuntimeInstanceID: "runtime-1",
		FencingGeneration: 3,
		Target:            workerapi.WorkspaceResetTarget{BaseWorkspaceVersionID: "version-1"},
	}, parent, "channel-1")
	request := &workspacev0.RenewWorkspaceAuthorityRequest{
		Previous: &workspacev0.WorkspaceRunAuthority{
			Fence: &workspacev0.WorkspaceAuthorityFence{
				WorkspaceMountId:       "mount-1",
				WorkspaceId:            "workspace-1",
				RuntimeInstanceId:      "runtime-1",
				MountFencingGeneration: 4,
				RunId:                  "run-1",
				ExpiresAtUnixNano:      100,
				BaseWorkspaceVersionId: "version-2",
			},
			ChannelToken: "channel-1",
		},
		NewExpiresAtUnixNano: 200,
	}
	serverResult := make(chan error, 1)
	go func() {
		header, bodyLength, err := wire.ReadStreamFrameHeader(guest)
		if err != nil {
			serverResult <- err
			return
		}
		if header.Type != wire.StreamTypeWorkspaceAuthorityRenew ||
			header.RunID != "run-1" ||
			header.WorkspaceID != "workspace-1" ||
			header.WorkspaceMountID != "mount-1" || bodyLength != 0 {
			serverResult <- errors.New("unexpected workspace authority renewal header")
			return
		}
		var received workspacev0.RenewWorkspaceAuthorityRequest
		if err := frameio.ReadProtoFrame(guest, &received); err != nil {
			serverResult <- err
			return
		}
		if received.GetNewExpiresAtUnixNano() != 200 {
			serverResult <- errors.New("unexpected renewed expiry")
			return
		}
		renewed := proto.Clone(received.GetPrevious().GetFence()).(*workspacev0.WorkspaceAuthorityFence)
		renewed.ExpiresAtUnixNano = received.GetNewExpiresAtUnixNano()
		serverResult <- frameio.WriteProtoFrame(guest, &workspacev0.RenewWorkspaceAuthorityResponse{Fence: renewed})
	}()
	renewed, err := registry.RenewWorkspaceAuthority(context.Background(), request)
	if err != nil {
		t.Fatal(err)
	}
	if renewed.GetExpiresAtUnixNano() != 200 {
		t.Fatalf("renewed fence = %+v", renewed)
	}
	if renewed.GetBaseWorkspaceVersionId() != "version-2" {
		t.Fatalf("renewed logical frontier = %q, want version-2", renewed.GetBaseWorkspaceVersionId())
	}
	if err := <-serverResult; err != nil {
		t.Fatal(err)
	}
}

func TestBeginWorkspaceFinalizationUsesMountedSession(t *testing.T) {
	host, guest := net.Pipe()
	defer host.Close()
	defer guest.Close()
	parent := &borrowedParentSession{stream: discardReadWriteCloser{}, openStream: host}
	registry := NewWorkspaceMountSessions()
	registry.RegisterWorkspaceMountSession(workerapi.WorkspaceMount{
		ID: "mount-1", WorkspaceID: "workspace-1", RuntimeInstanceID: "runtime-1",
		FencingGeneration: 3, Target: workerapi.WorkspaceResetTarget{BaseWorkspaceVersionID: "version-1"},
	}, parent, "channel-1")
	request := &workspacev0.BeginWorkspaceFinalizationRequest{
		Previous: &workspacev0.WorkspaceRunAuthority{
			Fence: &workspacev0.WorkspaceAuthorityFence{
				WorkspaceMountId: "mount-1", WorkspaceId: "workspace-1", RuntimeInstanceId: "runtime-1",
				MountFencingGeneration: 4, RunId: "run-1", ExpiresAtUnixNano: 100,
				BaseWorkspaceVersionId: "version-1",
			},
			ChannelToken: "channel-1",
		},
		FinalizationExpiresAtUnixNano: 200,
		OperationId:                   "11111111-1111-4111-8111-111111111111",
		Kind:                          "capture",
	}
	serverResult := make(chan error, 1)
	go func() {
		header, bodyLength, err := wire.ReadStreamFrameHeader(guest)
		if err != nil {
			serverResult <- err
			return
		}
		if header.Type != wire.StreamTypeWorkspaceFinalizationBegin ||
			header.RunID != "run-1" || header.WorkspaceID != "workspace-1" ||
			header.WorkspaceMountID != "mount-1" || header.OperationID != request.GetOperationId() || bodyLength != 0 {
			serverResult <- errors.New("unexpected workspace finalization header")
			return
		}
		var received workspacev0.BeginWorkspaceFinalizationRequest
		if err := frameio.ReadProtoFrame(guest, &received); err != nil {
			serverResult <- err
			return
		}
		frozen := proto.Clone(received.GetPrevious().GetFence()).(*workspacev0.WorkspaceAuthorityFence)
		frozen.ExpiresAtUnixNano = received.GetFinalizationExpiresAtUnixNano()
		serverResult <- frameio.WriteProtoFrame(guest, &workspacev0.BeginWorkspaceFinalizationResponse{
			Fence: frozen, OperationId: received.GetOperationId(), Kind: received.GetKind(),
		})
	}()
	response, err := registry.BeginWorkspaceFinalization(context.Background(), request)
	if err != nil {
		t.Fatal(err)
	}
	if response.GetFence().GetExpiresAtUnixNano() != 200 ||
		response.GetOperationId() != request.GetOperationId() || response.GetKind() != request.GetKind() {
		t.Fatalf("finalization response = %+v", response)
	}
	if err := <-serverResult; err != nil {
		t.Fatal(err)
	}
}

func TestFinalizationSessionSeparatesPhysicalIdentityFromLogicalFence(t *testing.T) {
	registry := NewWorkspaceMountSessions()
	registry.RegisterWorkspaceMountSession(workerapi.WorkspaceMount{
		ID: "mount-1", WorkspaceID: "workspace-1", RuntimeInstanceID: "runtime-1",
		FencingGeneration: 3, Target: workerapi.WorkspaceResetTarget{BaseWorkspaceVersionID: "version-1"},
	}, &borrowedParentSession{stream: discardReadWriteCloser{}}, "channel-1")
	envelope := &workspacev0.WorkspaceFinalizationEnvelope{
		Authority: &workspacev0.WorkspaceRunAuthority{
			Fence: &workspacev0.WorkspaceAuthorityFence{
				WorkspaceMountId: "mount-1", WorkspaceId: "workspace-1",
				RuntimeInstanceId: "runtime-1", MountFencingGeneration: 4,
				BaseWorkspaceVersionId: "version-2",
			},
			ChannelToken: "channel-1",
		},
	}
	if _, err := registry.finalizationSession(envelope); err != nil {
		t.Fatalf("advanced logical fence rejected exact physical mount: %v", err)
	}

	envelope.Authority.Fence.RuntimeInstanceId = "other-runtime"
	if _, err := registry.finalizationSession(envelope); err == nil {
		t.Fatal("physical identity mismatch was accepted")
	}
}

func TestRenewWorkspaceAuthorityCancellationPreservesMountedSession(t *testing.T) {
	host, guest := net.Pipe()
	defer guest.Close()
	parent := &borrowedParentSession{stream: discardReadWriteCloser{}, openStream: host}
	registry := NewWorkspaceMountSessions()
	registry.RegisterWorkspaceMountSession(workerapi.WorkspaceMount{
		ID:                "mount-1",
		WorkspaceID:       "workspace-1",
		RuntimeInstanceID: "runtime-1",
		FencingGeneration: 4,
		Target:            workerapi.WorkspaceResetTarget{BaseWorkspaceVersionID: "version-1"},
	}, parent, "channel-1")
	request := &workspacev0.RenewWorkspaceAuthorityRequest{
		Previous: &workspacev0.WorkspaceRunAuthority{
			Fence: &workspacev0.WorkspaceAuthorityFence{
				WorkspaceMountId:       "mount-1",
				WorkspaceId:            "workspace-1",
				RuntimeInstanceId:      "runtime-1",
				MountFencingGeneration: 4,
				RunId:                  "run-1",
				ExpiresAtUnixNano:      100,
				BaseWorkspaceVersionId: "version-1",
			},
			ChannelToken: "channel-1",
		},
		NewExpiresAtUnixNano: 200,
	}
	requestRead := make(chan struct{})
	go func() {
		_, _, _ = wire.ReadStreamFrameHeader(guest)
		var received workspacev0.RenewWorkspaceAuthorityRequest
		_ = frameio.ReadProtoFrame(guest, &received)
		close(requestRead)
		var response workspacev0.RenewWorkspaceAuthorityResponse
		_ = frameio.ReadProtoFrame(guest, &response)
	}()
	ctx, cancel := context.WithCancel(context.Background())
	result := make(chan error, 1)
	go func() {
		_, err := registry.RenewWorkspaceAuthority(ctx, request)
		result <- err
	}()
	waitForTestSignal(t, requestRead, "Workspace authority request")
	cancel()
	if err := waitForTestError(t, result, "Workspace authority cancellation"); !errors.Is(err, context.Canceled) {
		t.Fatalf("renewal error = %v, want context.Canceled", err)
	}
	if parent.closeCount != 0 {
		t.Fatalf("mounted session close count = %d, want 0", parent.closeCount)
	}
}

type borrowedParentSession struct {
	stream     io.ReadWriteCloser
	openStream io.ReadWriteCloser
	artifact   vm.SnapshotArtifact
	closeCount int
}

func (s *borrowedParentSession) Stream() vm.Stream {
	return testVMStream(s.stream)
}

func (s *borrowedParentSession) OpenStream(context.Context) (vm.Stream, error) {
	if s.openStream != nil {
		return testVMStream(s.openStream), nil
	}
	return testVMStream(&countingReadWriteCloser{}), nil
}

func (s *borrowedParentSession) Wait(ctx context.Context) error {
	<-ctx.Done()
	return ctx.Err()
}

func (s *borrowedParentSession) Close(context.Context) error {
	s.closeCount++
	return nil
}

func (s *borrowedParentSession) CreateSnapshot(context.Context, vm.SnapshotRequest) (vm.SnapshotArtifact, error) {
	if s.artifact.VMState.Path != "" {
		return s.artifact, nil
	}
	return vm.SnapshotArtifact{
		VMState:     vm.SnapshotFile{Path: "state"},
		ScratchDisk: vm.SnapshotFile{Path: "scratch"},
		Memory:      []vm.SnapshotFile{{Path: "memory"}},
	}, nil
}

func (s *borrowedParentSession) Resume(context.Context) error {
	return nil
}

type countingReadWriteCloser struct {
	closeCount int
}

type blockingCloseSession struct {
	started    chan struct{}
	release    chan struct{}
	closeCount atomic.Int32
	closeErr   error
}

func (s *blockingCloseSession) Stream() vm.Stream { return testVMStream(discardReadWriteCloser{}) }

func (s *blockingCloseSession) OpenStream(context.Context) (vm.Stream, error) {
	return testVMStream(discardReadWriteCloser{}), nil
}

func (s *blockingCloseSession) Wait(ctx context.Context) error {
	<-ctx.Done()
	return ctx.Err()
}

func (s *blockingCloseSession) Close(context.Context) error {
	if s.closeCount.Add(1) == 1 {
		close(s.started)
	}
	<-s.release
	return s.closeErr
}

func (s *countingReadWriteCloser) Read([]byte) (int, error)    { return 0, io.EOF }
func (s *countingReadWriteCloser) Write(p []byte) (int, error) { return len(p), nil }
func (s *countingReadWriteCloser) Close() error {
	s.closeCount++
	return nil
}
