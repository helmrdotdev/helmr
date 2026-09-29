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
	computerv0 "github.com/helmrdotdev/helmr/internal/proto/computer/v0"
	"github.com/helmrdotdev/helmr/internal/vm"
	"github.com/helmrdotdev/helmr/internal/wire"
	"github.com/helmrdotdev/helmr/internal/workerapi"
	"google.golang.org/protobuf/proto"
)

func TestManagedComputerMountSessionClosesPhysicalSessionOnce(t *testing.T) {
	closeErr := errors.New("close failed")
	physical := &blockingCloseSession{started: make(chan struct{}), release: make(chan struct{}), closeErr: closeErr}
	session := newManagedComputerMountSession(physical)

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

func TestManagedComputerMountSessionDuplicateReleaseObservesContext(t *testing.T) {
	physical := &blockingCloseSession{started: make(chan struct{}), release: make(chan struct{})}
	session := newManagedComputerMountSession(physical)

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

func TestBorrowedRunSessionReleaseCheckpointSourceClosesParentComputerMount(t *testing.T) {
	parent := &borrowedParentSession{stream: discardReadWriteCloser{}}
	runStream := &countingReadWriteCloser{}
	session := newBorrowedRunSession(parent, testVMStream(runStream))

	checkpointable, ok := session.(vm.CheckpointableMachine)
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

func TestRenewComputerAuthorityUsesMountedSession(t *testing.T) {
	host, guest := net.Pipe()
	defer host.Close()
	defer guest.Close()
	parent := &borrowedParentSession{stream: discardReadWriteCloser{}, openStream: host}
	registry := NewComputerMountSessions()
	registry.RegisterComputerMountSession(workerapi.ComputerInstanceAssignment{

		ComputerID:         "computer-1",
		ComputerInstanceID: "runtime-1",
		WriterGeneration:   3,
		Target:             workerapi.ComputerMountTarget{BaseComputerDiskVersionID: "version-1"},
	}, parent, "channel-1")
	request := &computerv0.RenewComputerAuthorityRequest{
		Previous: &computerv0.ComputerRunAuthority{
			Fence: &computerv0.ComputerAuthorityFence{
				ComputerInstanceId:        "runtime-1",
				ComputerId:                "computer-1",
				WriterGeneration:          3,
				RunId:                     "run-1",
				ExpiresAtUnixNano:         100,
				BaseComputerDiskVersionId: "version-2",
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
		if header.Type != wire.StreamTypeComputerAuthorityRenew ||
			header.RunID != "run-1" ||
			header.ComputerID != "computer-1" ||
			header.ComputerInstanceID != "runtime-1" || bodyLength != 0 {
			serverResult <- errors.New("unexpected computer authority renewal header")
			return
		}
		var received computerv0.RenewComputerAuthorityRequest
		if err := frameio.ReadProtoFrame(guest, &received); err != nil {
			serverResult <- err
			return
		}
		if received.GetNewExpiresAtUnixNano() != 200 {
			serverResult <- errors.New("unexpected renewed expiry")
			return
		}
		renewed := proto.Clone(received.GetPrevious().GetFence()).(*computerv0.ComputerAuthorityFence)
		renewed.ExpiresAtUnixNano = received.GetNewExpiresAtUnixNano()
		serverResult <- frameio.WriteProtoFrame(guest, &computerv0.RenewComputerAuthorityResponse{Fence: renewed})
	}()
	renewed, err := registry.RenewComputerAuthority(context.Background(), request)
	if err != nil {
		t.Fatal(err)
	}
	if renewed.GetExpiresAtUnixNano() != 200 {
		t.Fatalf("renewed fence = %+v", renewed)
	}
	if renewed.GetBaseComputerDiskVersionId() != "version-2" {
		t.Fatalf("renewed logical frontier = %q, want version-2", renewed.GetBaseComputerDiskVersionId())
	}
	if err := <-serverResult; err != nil {
		t.Fatal(err)
	}
}

func TestRenewComputerAuthorityCancellationPreservesMountedSession(t *testing.T) {
	host, guest := net.Pipe()
	defer guest.Close()
	parent := &borrowedParentSession{stream: discardReadWriteCloser{}, openStream: host}
	registry := NewComputerMountSessions()
	registry.RegisterComputerMountSession(workerapi.ComputerInstanceAssignment{

		ComputerID:         "computer-1",
		ComputerInstanceID: "runtime-1",
		WriterGeneration:   4,
		Target:             workerapi.ComputerMountTarget{BaseComputerDiskVersionID: "version-1"},
	}, parent, "channel-1")
	request := &computerv0.RenewComputerAuthorityRequest{
		Previous: &computerv0.ComputerRunAuthority{
			Fence: &computerv0.ComputerAuthorityFence{
				ComputerInstanceId:        "runtime-1",
				ComputerId:                "computer-1",
				WriterGeneration:          4,
				RunId:                     "run-1",
				ExpiresAtUnixNano:         100,
				BaseComputerDiskVersionId: "version-1",
			},
			ChannelToken: "channel-1",
		},
		NewExpiresAtUnixNano: 200,
	}
	requestRead := make(chan struct{})
	go func() {
		_, _, _ = wire.ReadStreamFrameHeader(guest)
		var received computerv0.RenewComputerAuthorityRequest
		_ = frameio.ReadProtoFrame(guest, &received)
		close(requestRead)
		var response computerv0.RenewComputerAuthorityResponse
		_ = frameio.ReadProtoFrame(guest, &response)
	}()
	ctx, cancel := context.WithCancel(context.Background())
	result := make(chan error, 1)
	go func() {
		_, err := registry.RenewComputerAuthority(ctx, request)
		result <- err
	}()
	waitForTestSignal(t, requestRead, "Computer authority request")
	cancel()
	if err := waitForTestError(t, result, "Computer authority cancellation"); !errors.Is(err, context.Canceled) {
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

func (s *borrowedParentSession) QuiesceComputerSaves(context.Context) error { return nil }

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

func (s *borrowedParentSession) SnapshotLimits() (vm.SnapshotLimits, error) {
	return vm.SnapshotLimits{ComputerBytes: 4096, MemoryBytes: 4096, ScratchBytes: 4096, StateBytes: 10000000, ConfigBytes: 65536}, nil
}

func (s *borrowedParentSession) PauseComputer(context.Context) (*vm.ComputerSnapshot, error) {
	return s.artifact.Computer, nil
}

func TestRenewComputerAuthorityRejectsDifferentPhysicalWriterBeforeOpeningStream(t *testing.T) {
	for _, generation := range []int64{0, 2, 4} {
		parent := &renewalStreamProbe{}
		registry := NewComputerMountSessions()
		registry.RegisterComputerMountSession(workerapi.ComputerInstanceAssignment{
			ComputerID: "computer-1", ComputerInstanceID: "instance-1", WriterGeneration: 3,
		}, parent, "channel-1")
		_, err := registry.RenewComputerAuthority(t.Context(), &computerv0.RenewComputerAuthorityRequest{
			Previous: &computerv0.ComputerRunAuthority{ChannelToken: "channel-1", Fence: &computerv0.ComputerAuthorityFence{
				ComputerId: "computer-1", ComputerInstanceId: "instance-1", WriterGeneration: generation,
				RunId: "run-1", ExpiresAtUnixNano: 100,
			}}, NewExpiresAtUnixNano: 200,
		})
		if err == nil || parent.opened {
			t.Fatalf("writer %d: error=%v opened=%t", generation, err, parent.opened)
		}
	}
}

type renewalStreamProbe struct {
	borrowedParentSession
	opened bool
}

func (s *renewalStreamProbe) OpenStream(context.Context) (vm.Stream, error) {
	s.opened = true
	return nil, errors.New("unexpected stream open")
}
