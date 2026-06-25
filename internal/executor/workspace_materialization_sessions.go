package executor

import (
	"context"
	"errors"
	"fmt"
	"io"
	"strings"
	"sync"

	"github.com/helmrdotdev/helmr/internal/vm"
)

var ErrWorkspaceMaterializationSessionNotFound = errors.New("workspace materialization session not found")

type CheckpointSourceReleaser interface {
	// ReleaseCheckpointSource releases the backing VM/materialization after the
	// checkpoint control stream has already been detached by the checkpointer.
	ReleaseCheckpointSource(context.Context) error
}

type WorkspaceMaterializationSessionRegistry interface {
	RegisterWorkspaceMaterializationSession(materializationID string, session vm.Session) func()
	OpenWorkspaceMaterializationSession(context.Context, string) (vm.Session, error)
}

type WorkspaceMaterializationSessions struct {
	mu       sync.RWMutex
	sessions map[string]vm.Session
}

func NewWorkspaceMaterializationSessions() *WorkspaceMaterializationSessions {
	return &WorkspaceMaterializationSessions{sessions: map[string]vm.Session{}}
}

func (s *WorkspaceMaterializationSessions) RegisterWorkspaceMaterializationSession(materializationID string, session vm.Session) func() {
	id := strings.TrimSpace(materializationID)
	if id == "" || session == nil {
		return func() {}
	}
	s.mu.Lock()
	if s.sessions == nil {
		s.sessions = map[string]vm.Session{}
	}
	s.sessions[id] = session
	s.mu.Unlock()
	return func() {
		s.mu.Lock()
		if current := s.sessions[id]; current == session {
			delete(s.sessions, id)
		}
		s.mu.Unlock()
	}
}

func (s *WorkspaceMaterializationSessions) OpenWorkspaceMaterializationSession(ctx context.Context, materializationID string) (vm.Session, error) {
	id := strings.TrimSpace(materializationID)
	if id == "" {
		return nil, errors.New("workspace materialization id is required")
	}
	s.mu.RLock()
	session := s.sessions[id]
	s.mu.RUnlock()
	if session == nil {
		return nil, fmt.Errorf("%w: %s", ErrWorkspaceMaterializationSessionNotFound, id)
	}
	stream, err := session.OpenStream(ctx)
	if err != nil {
		return nil, fmt.Errorf("open workspace materialization stream %s: %w", id, err)
	}
	return newBorrowedRunSession(session, stream), nil
}

type managedWorkspaceMaterializationSession struct {
	session                      vm.Session
	mu                           sync.RWMutex
	releaseForCheckpointStarted  bool
	releaseForCheckpointFinished bool
	releaseForCheckpointErr      error
	releaseForCheckpointDone     chan struct{}
}

func newManagedWorkspaceMaterializationSession(session vm.Session) *managedWorkspaceMaterializationSession {
	return &managedWorkspaceMaterializationSession{
		session:                  session,
		releaseForCheckpointDone: make(chan struct{}),
	}
}

func (s *managedWorkspaceMaterializationSession) Stream() io.ReadWriteCloser {
	return s.session.Stream()
}

func (s *managedWorkspaceMaterializationSession) OpenStream(ctx context.Context) (io.ReadWriteCloser, error) {
	return s.session.OpenStream(ctx)
}

func (s *managedWorkspaceMaterializationSession) Wait(ctx context.Context) error {
	return s.session.Wait(ctx)
}

func (s *managedWorkspaceMaterializationSession) Close(ctx context.Context) error {
	return s.session.Close(ctx)
}

func (s *managedWorkspaceMaterializationSession) CreateSnapshot(ctx context.Context, request vm.SnapshotRequest) (vm.SnapshotArtifact, error) {
	checkpointable, ok := s.session.(vm.CheckpointableSession)
	if !ok {
		return vm.SnapshotArtifact{}, errors.New("workspace materialization session does not support checkpoint snapshots")
	}
	return checkpointable.CreateSnapshot(ctx, request)
}

func (s *managedWorkspaceMaterializationSession) Resume(ctx context.Context) error {
	checkpointable, ok := s.session.(vm.CheckpointableSession)
	if !ok {
		return errors.New("workspace materialization session does not support checkpoint resume")
	}
	return checkpointable.Resume(ctx)
}

func (s *managedWorkspaceMaterializationSession) ReleaseCheckpointSource(ctx context.Context) error {
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

func (s *managedWorkspaceMaterializationSession) CheckpointReleaseResult(ctx context.Context) (bool, error) {
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
	parent vm.Session
	stream io.ReadWriteCloser
	once   sync.Once
	err    error
}

func newBorrowedRunSession(parent vm.Session, stream io.ReadWriteCloser) vm.Session {
	return &borrowedRunSession{parent: parent, stream: stream}
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
		return vm.SnapshotArtifact{}, errors.New("workspace materialization session does not support checkpoint snapshots")
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
		return errors.New("workspace materialization session does not support checkpoint resume")
	}
	return checkpointable.Resume(ctx)
}
