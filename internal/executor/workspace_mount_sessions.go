package executor

import (
	"context"
	"errors"
	"fmt"
	"io"
	"strings"
	"sync"

	"github.com/helmrdotdev/helmr/internal/api"
	"github.com/helmrdotdev/helmr/internal/vm"
)

var ErrWorkspaceMountSessionNotFound = errors.New("workspace mount session not found")

type CheckpointSourceReleaser interface {
	// ReleaseCheckpointSource releases the backing VM/workspace mount session after the
	// checkpoint control stream has already been detached by the checkpointer.
	ReleaseCheckpointSource(context.Context) error
}

type WorkspaceMountSessionRegistry interface {
	RegisterWorkspaceMountSession(mount api.WorkerWorkspaceMount, session vm.Session, channelToken string) func()
	OpenWorkspaceMountSession(context.Context, string) (WorkspaceMountSession, error)
}

type WorkspaceMountSession struct {
	Session      vm.Session
	ChannelToken string
}

type WorkspaceMountSessions struct {
	mu             sync.RWMutex
	sessions       map[string]workspaceMountSessionEntry
	BackgroundGate *BackgroundWorkGate
	RuntimePool    *PreparedRuntimePool
}

type workspaceMountSessionEntry struct {
	mount        api.WorkerWorkspaceMount
	session      vm.Session
	channelToken string
}

func NewWorkspaceMountSessions() *WorkspaceMountSessions {
	return &WorkspaceMountSessions{sessions: map[string]workspaceMountSessionEntry{}}
}

func (s *WorkspaceMountSessions) RegisterWorkspaceMountSession(mount api.WorkerWorkspaceMount, session vm.Session, channelToken string) func() {
	id := strings.TrimSpace(mount.ID)
	if id == "" || session == nil {
		return func() {}
	}
	s.mu.Lock()
	if s.sessions == nil {
		s.sessions = map[string]workspaceMountSessionEntry{}
	}
	s.sessions[id] = workspaceMountSessionEntry{mount: mount, session: session, channelToken: strings.TrimSpace(channelToken)}
	s.mu.Unlock()
	return func() {
		s.mu.Lock()
		if current := s.sessions[id]; current.session == session {
			delete(s.sessions, id)
		}
		s.mu.Unlock()
	}
}

func (s *WorkspaceMountSessions) OpenWorkspaceMountSession(ctx context.Context, workspaceMountID string) (WorkspaceMountSession, error) {
	id := strings.TrimSpace(workspaceMountID)
	if id == "" {
		return WorkspaceMountSession{}, errors.New("workspace mount id is required")
	}
	s.mu.RLock()
	entry := s.sessions[id]
	s.mu.RUnlock()
	if entry.session == nil {
		return WorkspaceMountSession{}, fmt.Errorf("%w: %s", ErrWorkspaceMountSessionNotFound, id)
	}
	if entry.channelToken == "" {
		return WorkspaceMountSession{}, fmt.Errorf("workspace mount session %s missing channel token", id)
	}
	stream, err := entry.session.OpenStream(ctx)
	if err != nil {
		return WorkspaceMountSession{}, fmt.Errorf("open workspace mount stream %s: %w", id, err)
	}
	endForeground := s.beginForegroundRun()
	refillAfterRun := func() {
		endForeground()
		if s.RuntimePool != nil {
			s.RuntimePool.Refill(context.Background(), entry.mount)
		}
	}
	return WorkspaceMountSession{
		Session:      newBorrowedRunSession(entry.session, stream, refillAfterRun),
		ChannelToken: entry.channelToken,
	}, nil
}

func (s *WorkspaceMountSessions) beginForegroundRun() func() {
	if s == nil || s.BackgroundGate == nil {
		return func() {}
	}
	return s.BackgroundGate.BeginForeground()
}

type managedWorkspaceMountSession struct {
	session                      vm.Session
	mu                           sync.RWMutex
	releaseForCheckpointStarted  bool
	releaseForCheckpointFinished bool
	releaseForCheckpointErr      error
	releaseForCheckpointDone     chan struct{}
}

func newManagedWorkspaceMountSession(session vm.Session) *managedWorkspaceMountSession {
	return &managedWorkspaceMountSession{
		session:                  session,
		releaseForCheckpointDone: make(chan struct{}),
	}
}

func (s *managedWorkspaceMountSession) Stream() io.ReadWriteCloser {
	return s.session.Stream()
}

func (s *managedWorkspaceMountSession) OpenStream(ctx context.Context) (io.ReadWriteCloser, error) {
	return s.session.OpenStream(ctx)
}

func (s *managedWorkspaceMountSession) Wait(ctx context.Context) error {
	return s.session.Wait(ctx)
}

func (s *managedWorkspaceMountSession) Close(ctx context.Context) error {
	return s.session.Close(ctx)
}

func (s *managedWorkspaceMountSession) CreateSnapshot(ctx context.Context, request vm.SnapshotRequest) (vm.SnapshotArtifact, error) {
	checkpointable, ok := s.session.(vm.CheckpointableSession)
	if !ok {
		return vm.SnapshotArtifact{}, errors.New("workspace mount session does not support checkpoint snapshots")
	}
	return checkpointable.CreateSnapshot(ctx, request)
}

func (s *managedWorkspaceMountSession) Resume(ctx context.Context) error {
	checkpointable, ok := s.session.(vm.CheckpointableSession)
	if !ok {
		return errors.New("workspace mount session does not support checkpoint resume")
	}
	return checkpointable.Resume(ctx)
}

func (s *managedWorkspaceMountSession) ReleaseCheckpointSource(ctx context.Context) error {
	s.mu.Lock()
	if s.releaseForCheckpointStarted {
		done := s.releaseForCheckpointDone
		s.mu.Unlock()
		<-done
		s.mu.RLock()
		defer s.mu.RUnlock()
		return s.releaseForCheckpointErr
	}
	s.releaseForCheckpointStarted = true
	done := s.releaseForCheckpointDone
	s.mu.Unlock()

	err := s.session.Close(ctx)
	s.mu.Lock()
	s.releaseForCheckpointErr = err
	s.releaseForCheckpointFinished = true
	close(done)
	s.mu.Unlock()
	return err
}

func (s *managedWorkspaceMountSession) CheckpointReleaseResult(ctx context.Context) (bool, error) {
	s.mu.RLock()
	started := s.releaseForCheckpointStarted
	finished := s.releaseForCheckpointFinished
	done := s.releaseForCheckpointDone
	err := s.releaseForCheckpointErr
	s.mu.RUnlock()
	if !started {
		return false, nil
	}
	if !finished {
		select {
		case <-done:
		case <-ctx.Done():
			return true, ctx.Err()
		}
		s.mu.RLock()
		err = s.releaseForCheckpointErr
		s.mu.RUnlock()
	}
	return true, err
}

type borrowedRunSession struct {
	parent        vm.Session
	stream        io.ReadWriteCloser
	endForeground func()
	once          sync.Once
	err           error
}

func newBorrowedRunSession(parent vm.Session, stream io.ReadWriteCloser, endForeground func()) vm.Session {
	if endForeground == nil {
		endForeground = func() {}
	}
	return &borrowedRunSession{parent: parent, stream: stream, endForeground: endForeground}
}

func (s *borrowedRunSession) Stream() io.ReadWriteCloser {
	return s.stream
}

func (s *borrowedRunSession) OpenStream(context.Context) (io.ReadWriteCloser, error) {
	return nil, errors.New("borrowed run session does not support opening nested streams")
}

func (s *borrowedRunSession) Wait(ctx context.Context) error {
	if s.parent != nil {
		return s.parent.Wait(ctx)
	}
	<-ctx.Done()
	return ctx.Err()
}

func (s *borrowedRunSession) Close(context.Context) error {
	s.once.Do(func() {
		defer s.endForeground()
		if s.stream != nil {
			s.err = s.stream.Close()
		}
	})
	return s.err
}

func (s *borrowedRunSession) ReleaseCheckpointSource(ctx context.Context) error {
	if releaser, ok := s.parent.(CheckpointSourceReleaser); ok {
		return releaser.ReleaseCheckpointSource(ctx)
	}
	return s.parent.Close(ctx)
}

func (s *borrowedRunSession) CreateSnapshot(ctx context.Context, request vm.SnapshotRequest) (vm.SnapshotArtifact, error) {
	checkpointable, ok := s.parent.(vm.CheckpointableSession)
	if !ok {
		return vm.SnapshotArtifact{}, errors.New("workspace mount session does not support checkpoint snapshots")
	}
	artifact, err := checkpointable.CreateSnapshot(ctx, request)
	if err != nil {
		return vm.SnapshotArtifact{}, err
	}
	return artifact, nil
}

func (s *borrowedRunSession) Resume(ctx context.Context) error {
	checkpointable, ok := s.parent.(vm.CheckpointableSession)
	if !ok {
		return errors.New("workspace mount session does not support checkpoint resume")
	}
	return checkpointable.Resume(ctx)
}
