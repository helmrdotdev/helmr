package executor

import (
	"context"
	"io"
	"testing"

	"github.com/helmrdotdev/helmr/internal/api"
	"github.com/helmrdotdev/helmr/internal/vm"
)

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

func (s *countingReadWriteCloser) Read([]byte) (int, error)    { return 0, io.EOF }
func (s *countingReadWriteCloser) Write(p []byte) (int, error) { return len(p), nil }
func (s *countingReadWriteCloser) Close() error {
	s.closeCount++
	return nil
}
