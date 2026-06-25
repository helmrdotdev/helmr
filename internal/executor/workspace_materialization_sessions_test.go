package executor

import (
	"context"
	"io"
	"testing"

	"github.com/helmrdotdev/helmr/internal/vm"
)

func TestBorrowedRunSessionReleaseCheckpointSourceClosesParentMaterialization(t *testing.T) {
	parent := &borrowedParentSession{stream: discardReadWriteCloser{}}
	runStream := &countingReadWriteCloser{}
	session := newBorrowedRunSession(parent, runStream)

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
