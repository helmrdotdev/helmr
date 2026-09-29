package computerhost

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
	programv0 "github.com/helmrdotdev/helmr/internal/proto/program/v0"
	"github.com/helmrdotdev/helmr/internal/vm"
	"github.com/helmrdotdev/helmr/internal/wire"
	"github.com/helmrdotdev/helmr/internal/workerapi"
	"google.golang.org/protobuf/proto"
)

func TestInstanceMountClosesPhysicalMachineOnce(t *testing.T) {
	closeErr := errors.New("close failed")
	physical := &blockingCloseMachine{started: make(chan struct{}), release: make(chan struct{}), closeErr: closeErr}
	session := newInstanceMount(physical)

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

func TestInstanceMountDuplicateReleaseObservesContext(t *testing.T) {
	physical := &blockingCloseMachine{started: make(chan struct{}), release: make(chan struct{})}
	session := newInstanceMount(physical)

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

// Capture retries a failed source exclusion; a physical close whose join
// timed out is attempted again, while the first result stays recorded.
func TestInstanceMountRetriesTimedOutCheckpointRelease(t *testing.T) {
	calls := 0
	session := newInstanceMount(saveStopSession{stop: func(context.Context) error {
		calls++
		if calls == 1 {
			return context.DeadlineExceeded
		}
		return nil
	}})
	if err := session.ReleaseCheckpointSource(t.Context()); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatal(err)
	}
	if err := session.ReleaseCheckpointSource(t.Context()); err != nil {
		t.Fatal(err)
	}
	if err := session.ReleaseCheckpointSource(t.Context()); err != nil {
		t.Fatal(err)
	}
	if calls != 2 {
		t.Fatalf("physical close calls = %d, want 2", calls)
	}
	if released, err := session.CheckpointReleaseResult(t.Context()); !released || !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("recorded release = %v, %v", released, err)
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

func TestBorrowedChannelCloseLeavesMountedMachineRunning(t *testing.T) {
	runStream := &countingReadWriteCloser{}
	parent := &mountedMachine{stream: discardReadWriteCloser{}, openStream: runStream}
	registry := NewMounts()
	unregister := registry.register(workerapi.ComputerInstanceAssignment{ComputerInstanceID: "runtime-1"}, newInstanceMount(parent), "channel-1")
	defer unregister()
	opened, err := registry.OpenChannel(context.Background(), "runtime-1")
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := opened.Channel.(vm.CheckpointableMachine); ok {
		t.Fatal("borrowed channel advertises checkpoint capture")
	}
	for range 2 {
		if err := opened.Channel.Close(context.Background()); err != nil {
			t.Fatal(err)
		}
	}
	if parent.closeCount != 0 {
		t.Fatalf("parent close count = %d, want 0", parent.closeCount)
	}
	if runStream.closeCount != 1 {
		t.Fatalf("run stream close count = %d, want 1", runStream.closeCount)
	}
}

func TestOpenedComputerMountReleasesPhysicalSourceWithoutClosingRunStream(t *testing.T) {
	runStream := &countingReadWriteCloser{}
	parent := &mountedMachine{stream: discardReadWriteCloser{}, openStream: runStream}
	managed := newInstanceMount(parent)
	registry := NewMounts()
	unregister := registry.register(workerapi.ComputerInstanceAssignment{ComputerInstanceID: "runtime-1"}, managed, "channel-1")
	defer unregister()
	opened, err := registry.OpenChannel(context.Background(), "runtime-1")
	if err != nil {
		t.Fatal(err)
	}
	if err := opened.ReleaseSource(context.Background()); err != nil {
		t.Fatal(err)
	}
	if parent.closeCount != 1 {
		t.Fatalf("parent close count = %d, want 1", parent.closeCount)
	}
	if runStream.closeCount != 0 {
		t.Fatalf("run stream close count = %d, want 0", runStream.closeCount)
	}
	if released, err := managed.CheckpointReleaseResult(context.Background()); !released || err != nil {
		t.Fatalf("checkpoint release result = %t, %v", released, err)
	}
}

func TestRenewComputerAuthorityUsesMountedMachine(t *testing.T) {
	host, guest := net.Pipe()
	defer host.Close()
	defer guest.Close()
	parent := &mountedMachine{stream: discardReadWriteCloser{}, openStream: host}
	registry := NewMounts()
	registry.register(workerapi.ComputerInstanceAssignment{

		ComputerID:         "computer-1",
		ComputerInstanceID: "runtime-1",
		WriterGeneration:   3,
		Target:             workerapi.ComputerMountTarget{BaseComputerDiskVersionID: "version-1"},
	}, newInstanceMount(parent), "channel-1")
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

func TestMountChannelGrantProgramResumeUsesOpenedMount(t *testing.T) {
	host, guest := net.Pipe()
	defer host.Close()
	defer guest.Close()
	grantStream := &closeCountingConn{Conn: host}
	machine := &queuedStreamMachine{streams: []io.ReadWriteCloser{&countingReadWriteCloser{}, grantStream}}
	registry := NewMounts()
	unregister := registry.register(testGrantMount(), newInstanceMount(machine), "channel-1")
	defer unregister()
	opened, err := registry.OpenChannel(context.Background(), "runtime-1")
	if err != nil {
		t.Fatal(err)
	}
	request, want := testProgramResumeGrant()
	serverResult := serveProgramResumeGrant(guest, request, want)
	attach, err := opened.GrantProgramResume(context.Background(), request)
	if err != nil {
		t.Fatal(err)
	}
	if err := <-serverResult; err != nil {
		t.Fatal(err)
	}
	if !proto.Equal(attach, want) {
		t.Fatalf("resume attach = %+v, want %+v", attach, want)
	}
	if got := grantStream.closes.Load(); got != 1 {
		t.Fatalf("grant stream close count = %d, want 1", got)
	}
	if machine.closeCount != 0 {
		t.Fatalf("machine close count = %d, want 0", machine.closeCount)
	}
}

func TestMountChannelGrantProgramResumeKeepsOpenedMountAfterReregistration(t *testing.T) {
	host, guest := net.Pipe()
	defer host.Close()
	defer guest.Close()
	opened := &queuedStreamMachine{streams: []io.ReadWriteCloser{&countingReadWriteCloser{}, host}}
	replacement := &queuedStreamMachine{}
	registry := NewMounts()
	unregister := registry.register(testGrantMount(), newInstanceMount(opened), "channel-1")
	channel, err := registry.OpenChannel(context.Background(), "runtime-1")
	if err != nil {
		t.Fatal(err)
	}
	unregister()
	defer registry.register(testGrantMount(), newInstanceMount(replacement), "channel-2")()
	request, want := testProgramResumeGrant()
	serverResult := serveProgramResumeGrant(guest, request, want)
	attach, err := channel.GrantProgramResume(context.Background(), request)
	if err != nil {
		t.Fatal(err)
	}
	if err := <-serverResult; err != nil {
		t.Fatal(err)
	}
	if !proto.Equal(attach, want) {
		t.Fatalf("resume attach = %+v, want %+v", attach, want)
	}
	if opened.opens != 2 || replacement.opens != 0 {
		t.Fatalf("stream opens: opened mount = %d, replacement mount = %d; want 2, 0", opened.opens, replacement.opens)
	}
}

func testGrantMount() workerapi.ComputerInstanceAssignment {
	return workerapi.ComputerInstanceAssignment{ComputerID: "computer-1", ComputerInstanceID: "runtime-1", WriterGeneration: 3}
}

func testProgramResumeGrant() (*computerv0.GrantProgramResumeRequest, *programv0.ResumeAttach) {
	fence := &computerv0.ComputerAuthorityFence{
		ComputerInstanceId: "runtime-1",
		ComputerId:         "computer-1",
		WriterGeneration:   3,
		RunId:              "run-1",
		AttemptNumber:      2,
		RunLeaseId:         "lease-1",
	}
	request := &computerv0.GrantProgramResumeRequest{
		Authority:    &computerv0.ComputerRunAuthority{Fence: fence, ChannelToken: "channel-1"},
		RunWaitId:    "wait-1",
		CheckpointId: "checkpoint-1",
	}
	attach := &programv0.ResumeAttach{
		RunId:                "run-1",
		AttemptNumber:        2,
		RunLeaseId:           "lease-1",
		RunWaitId:            "wait-1",
		CheckpointId:         "checkpoint-1",
		ResumeRequestVersion: 1,
		ResumeAttachId:       "attach-1",
		CorrelationId:        "correlation-1",
	}
	return request, attach
}

// serveProgramResumeGrant plays the guest side of one resume grant stream and
// checks that the host sent the expected header and request.
func serveProgramResumeGrant(guest net.Conn, request *computerv0.GrantProgramResumeRequest, attach *programv0.ResumeAttach) <-chan error {
	result := make(chan error, 1)
	go func() {
		defer guest.Close()
		header, bodyLength, err := wire.ReadStreamFrameHeader(guest)
		if err != nil {
			result <- err
			return
		}
		if header.Type != wire.StreamTypeProgramResumeGrant ||
			header.RunID != "run-1" ||
			header.ComputerID != "computer-1" ||
			header.ComputerInstanceID != "runtime-1" || bodyLength != 0 {
			result <- errors.New("unexpected program resume grant header")
			return
		}
		var received computerv0.GrantProgramResumeRequest
		if err := frameio.ReadProtoFrame(guest, &received); err != nil {
			result <- err
			return
		}
		if !proto.Equal(&received, request) {
			result <- errors.New("unexpected program resume grant request")
			return
		}
		result <- frameio.WriteProtoFrame(guest, &computerv0.GrantProgramResumeResponse{Fence: request.GetAuthority().GetFence(), Attach: attach})
	}()
	return result
}

// queuedStreamMachine hands out its streams in order, one per OpenStream.
type queuedStreamMachine struct {
	streams    []io.ReadWriteCloser
	opens      int
	closeCount int
}

func (m *queuedStreamMachine) Stream() vm.Stream { return testVMStream(discardReadWriteCloser{}) }

func (m *queuedStreamMachine) OpenStream(context.Context) (vm.Stream, error) {
	if m.opens >= len(m.streams) {
		m.opens++
		return nil, errors.New("no stream available")
	}
	stream := m.streams[m.opens]
	m.opens++
	return testVMStream(stream), nil
}

func (m *queuedStreamMachine) Wait(ctx context.Context) error {
	<-ctx.Done()
	return ctx.Err()
}

func (m *queuedStreamMachine) Close(context.Context) error {
	m.closeCount++
	return nil
}

type closeCountingConn struct {
	net.Conn
	closes atomic.Int32
}

func (c *closeCountingConn) Close() error {
	c.closes.Add(1)
	return c.Conn.Close()
}

func TestRenewComputerAuthorityCancellationPreservesMountedSession(t *testing.T) {
	host, guest := net.Pipe()
	defer guest.Close()
	parent := &mountedMachine{stream: discardReadWriteCloser{}, openStream: host}
	registry := NewMounts()
	registry.register(workerapi.ComputerInstanceAssignment{

		ComputerID:         "computer-1",
		ComputerInstanceID: "runtime-1",
		WriterGeneration:   4,
		Target:             workerapi.ComputerMountTarget{BaseComputerDiskVersionID: "version-1"},
	}, newInstanceMount(parent), "channel-1")
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

type mountedMachine struct {
	stream     io.ReadWriteCloser
	openStream io.ReadWriteCloser
	artifact   vm.SnapshotArtifact
	closeCount int
}

func (s *mountedMachine) Stream() vm.Stream {
	return testVMStream(s.stream)
}

func (s *mountedMachine) OpenStream(context.Context) (vm.Stream, error) {
	if s.openStream != nil {
		return testVMStream(s.openStream), nil
	}
	return testVMStream(&countingReadWriteCloser{}), nil
}

func (s *mountedMachine) Wait(ctx context.Context) error {
	<-ctx.Done()
	return ctx.Err()
}

func (s *mountedMachine) Close(context.Context) error {
	s.closeCount++
	return nil
}

func (s *mountedMachine) CreateSnapshot(context.Context, vm.SnapshotRequest) (vm.SnapshotArtifact, error) {
	if s.artifact.VMState.Path != "" {
		return s.artifact, nil
	}
	return vm.SnapshotArtifact{
		VMState:     vm.SnapshotFile{Path: "state"},
		ScratchDisk: vm.SnapshotFile{Path: "scratch"},
		Memory:      []vm.SnapshotFile{{Path: "memory"}},
	}, nil
}

func (s *mountedMachine) Resume(context.Context) error {
	return nil
}

type countingReadWriteCloser struct {
	closeCount int
}

type blockingCloseMachine struct {
	started    chan struct{}
	release    chan struct{}
	closeCount atomic.Int32
	closeErr   error
}

func (s *blockingCloseMachine) Stream() vm.Stream { return testVMStream(discardReadWriteCloser{}) }

func (s *blockingCloseMachine) OpenStream(context.Context) (vm.Stream, error) {
	return testVMStream(discardReadWriteCloser{}), nil
}

func (s *blockingCloseMachine) Wait(ctx context.Context) error {
	<-ctx.Done()
	return ctx.Err()
}

func (s *blockingCloseMachine) Close(context.Context) error {
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

func (s *mountedMachine) SnapshotLimits() (vm.SnapshotLimits, error) {
	return vm.SnapshotLimits{ComputerBytes: 4096, MemoryBytes: 4096, ScratchBytes: 4096, StateBytes: 10000000, ConfigBytes: 65536}, nil
}

func (s *mountedMachine) PauseComputer(context.Context) (*vm.ComputerSnapshot, error) {
	return s.artifact.Computer, nil
}

func TestRenewComputerAuthorityRejectsDifferentPhysicalWriterBeforeOpeningStream(t *testing.T) {
	for _, generation := range []int64{0, 2, 4} {
		parent := &renewalStreamProbe{}
		registry := NewMounts()
		registry.register(workerapi.ComputerInstanceAssignment{
			ComputerID: "computer-1", ComputerInstanceID: "instance-1", WriterGeneration: 3,
		}, newInstanceMount(parent), "channel-1")
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
	mountedMachine
	opened bool
}

func (s *renewalStreamProbe) OpenStream(context.Context) (vm.Stream, error) {
	s.opened = true
	return nil, errors.New("unexpected stream open")
}
