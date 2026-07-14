package executor

import (
	"context"
	"errors"
	"io"
	"sync/atomic"
	"testing"
	"time"

	"github.com/helmrdotdev/helmr/internal/api"
	"github.com/helmrdotdev/helmr/internal/vm"
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
	session := newBorrowedRunSession(parent, runStream, nil)

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

func TestOpenWorkspaceMountSessionMarksForegroundRun(t *testing.T) {
	gate := NewBackgroundWorkGate()
	registry := NewWorkspaceMountSessions()
	registry.BackgroundGate = gate
	unregister := registry.RegisterWorkspaceMountSession(api.WorkerWorkspaceMount{ID: "mat-1"}, &borrowedParentSession{stream: discardReadWriteCloser{}}, "channel-token")
	defer unregister()

	backgroundCtx, finishBackground, ok := gate.BeginBackground(context.Background())
	defer finishBackground()
	if !ok {
		t.Fatalf("background work did not start before run borrow")
	}

	borrowed, err := registry.OpenWorkspaceMountSession(context.Background(), "mat-1")
	if err != nil {
		t.Fatal(err)
	}
	select {
	case <-backgroundCtx.Done():
	default:
		t.Fatalf("background work was not cancelled when run borrowed the workspaceMount")
	}

	_, finishWhileBorrowed, ok := gate.BeginBackground(context.Background())
	defer finishWhileBorrowed()
	if ok {
		t.Fatalf("background work started while a run had borrowed the workspaceMount")
	}

	if err := borrowed.Session.Close(context.Background()); err != nil {
		t.Fatal(err)
	}
	_, finishAfterRun, ok := gate.BeginBackground(context.Background())
	defer finishAfterRun()
	if !ok {
		t.Fatalf("background work did not start after borrowed run session closed")
	}
}

type borrowedParentSession struct {
	stream     io.ReadWriteCloser
	artifact   vm.SnapshotArtifact
	closeCount int
}

func (s *borrowedParentSession) Stream() io.ReadWriteCloser {
	return s.stream
}

func (s *borrowedParentSession) OpenStream(context.Context) (io.ReadWriteCloser, error) {
	return &countingReadWriteCloser{}, nil
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

func (s *blockingCloseSession) Stream() io.ReadWriteCloser { return discardReadWriteCloser{} }

func (s *blockingCloseSession) OpenStream(context.Context) (io.ReadWriteCloser, error) {
	return discardReadWriteCloser{}, nil
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
