package executor

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"

	"github.com/helmrdotdev/helmr/internal/cas"
	workspacev0 "github.com/helmrdotdev/helmr/internal/proto/workspace/v0"
	"github.com/helmrdotdev/helmr/internal/vm"
	"github.com/helmrdotdev/helmr/internal/workerapi"
)

var ErrWorkspaceMountSessionNotFound = errors.New("workspace mount session not found")

type CheckpointSourceReleaser interface {
	// ReleaseCheckpointSource releases the backing VM/workspace mount session after the
	// checkpoint control stream has already been detached by the checkpointer.
	ReleaseCheckpointSource(context.Context) error
}

type WorkspaceMountSessionRegistry interface {
	RegisterWorkspaceMountSession(mount workerapi.WorkspaceMount, session vm.Session, channelToken string) func()
	OpenWorkspaceMountSession(context.Context, string) (WorkspaceMountSession, error)
	FailWorkspaceMountSession(context.Context, string) error
	RenewWorkspaceAuthority(context.Context, *workspacev0.RenewWorkspaceAuthorityRequest) (*workspacev0.WorkspaceAuthorityFence, error)
	BeginWorkspaceFinalization(context.Context, *workspacev0.BeginWorkspaceFinalizationRequest) (*workspacev0.BeginWorkspaceFinalizationResponse, error)
	CaptureWorkspace(context.Context, *workspacev0.CaptureWorkspaceRequest, cas.Store) (WorkspaceCapture, error)
	ResetWorkspace(context.Context, *workspacev0.ResetWorkspaceRequest, cas.Reader) (WorkspaceReset, error)
}

type WorkspaceMountSession struct {
	Session        vm.Session
	ControlSession vm.Session
	ChannelToken   string
	Mount          workerapi.WorkspaceMount
}

type workspaceMountFailureRequest struct {
	result chan error
}

type WorkspaceMountSessions struct {
	mu       sync.RWMutex
	sessions map[string]workspaceMountSessionEntry
}

type workspaceMountSessionEntry struct {
	session      vm.Session
	channelToken string
	mount        workerapi.WorkspaceMount
}

func NewWorkspaceMountSessions() *WorkspaceMountSessions {
	return &WorkspaceMountSessions{sessions: map[string]workspaceMountSessionEntry{}}
}

func (s *WorkspaceMountSessions) RegisterWorkspaceMountSession(mount workerapi.WorkspaceMount, session vm.Session, channelToken string) func() {
	id := strings.TrimSpace(mount.ID)
	if id == "" || session == nil {
		return func() {}
	}
	s.mu.Lock()
	if s.sessions == nil {
		s.sessions = map[string]workspaceMountSessionEntry{}
	}
	s.sessions[id] = workspaceMountSessionEntry{session: session, channelToken: strings.TrimSpace(channelToken), mount: mount}
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
	return WorkspaceMountSession{
		Session:        newBorrowedRunSession(entry.session, stream),
		ControlSession: entry.session,
		ChannelToken:   entry.channelToken,
		Mount:          entry.mount,
	}, nil
}

func (s *WorkspaceMountSessions) FailWorkspaceMountSession(
	ctx context.Context,
	workspaceMountID string,
) error {
	id := strings.TrimSpace(workspaceMountID)
	if id == "" {
		return errors.New("workspace mount failure identity is required")
	}
	s.mu.RLock()
	entry := s.sessions[id]
	s.mu.RUnlock()
	managed, ok := entry.session.(*managedWorkspaceMountSession)
	if !ok || managed == nil {
		return fmt.Errorf("%w: %s", ErrWorkspaceMountSessionNotFound, id)
	}
	return managed.requestFailure(ctx)
}

func (s *WorkspaceMountSessions) RenewWorkspaceAuthority(ctx context.Context, request *workspacev0.RenewWorkspaceAuthorityRequest) (*workspacev0.WorkspaceAuthorityFence, error) {
	if request == nil || request.GetPrevious() == nil || request.GetPrevious().GetFence() == nil {
		return nil, errors.New("previous workspace authority is required")
	}
	fence := request.GetPrevious().GetFence()
	id := strings.TrimSpace(fence.GetWorkspaceMountId())
	s.mu.RLock()
	entry := s.sessions[id]
	s.mu.RUnlock()
	if entry.session == nil {
		return nil, fmt.Errorf("%w: %s", ErrWorkspaceMountSessionNotFound, id)
	}
	if entry.channelToken == "" || request.GetPrevious().GetChannelToken() != entry.channelToken {
		return nil, errors.New("workspace authority channel token does not match the mount session")
	}
	if err := validateWorkspaceMountPhysicalAuthority(fence, entry.mount); err != nil {
		return nil, err
	}
	return renewWorkspaceAuthorityOnSession(ctx, entry.session, request)
}

func (s *WorkspaceMountSessions) BeginWorkspaceFinalization(
	ctx context.Context,
	request *workspacev0.BeginWorkspaceFinalizationRequest,
) (*workspacev0.BeginWorkspaceFinalizationResponse, error) {
	if request == nil || request.GetPrevious() == nil || request.GetPrevious().GetFence() == nil {
		return nil, errors.New("previous workspace authority is required")
	}
	fence := request.GetPrevious().GetFence()
	id := strings.TrimSpace(fence.GetWorkspaceMountId())
	s.mu.RLock()
	entry := s.sessions[id]
	s.mu.RUnlock()
	if entry.session == nil {
		return nil, fmt.Errorf("%w: %s", ErrWorkspaceMountSessionNotFound, id)
	}
	if entry.channelToken == "" || request.GetPrevious().GetChannelToken() != entry.channelToken {
		return nil, errors.New("workspace authority channel token does not match the mount session")
	}
	if err := validateWorkspaceMountPhysicalAuthority(fence, entry.mount); err != nil {
		return nil, err
	}
	return beginWorkspaceFinalizationOnSession(ctx, entry.session, request)
}

func (s *WorkspaceMountSessions) CaptureWorkspace(
	ctx context.Context,
	request *workspacev0.CaptureWorkspaceRequest,
	store cas.Store,
) (WorkspaceCapture, error) {
	entry, err := s.finalizationSession(request.GetEnvelope())
	if err != nil {
		return WorkspaceCapture{}, err
	}
	return captureWorkspaceOnSession(ctx, entry.session, store, request)
}

func (s *WorkspaceMountSessions) ResetWorkspace(
	ctx context.Context,
	request *workspacev0.ResetWorkspaceRequest,
	store cas.Reader,
) (WorkspaceReset, error) {
	entry, err := s.finalizationSession(request.GetEnvelope())
	if err != nil {
		return WorkspaceReset{}, err
	}
	return resetWorkspaceOnSession(ctx, entry.session, store, request)
}

func (s *WorkspaceMountSessions) finalizationSession(
	envelope *workspacev0.WorkspaceFinalizationEnvelope,
) (workspaceMountSessionEntry, error) {
	if envelope == nil || envelope.GetAuthority() == nil || envelope.GetAuthority().GetFence() == nil {
		return workspaceMountSessionEntry{}, errors.New("workspace finalization envelope is required")
	}
	authority := envelope.GetAuthority()
	fence := authority.GetFence()
	id := strings.TrimSpace(fence.GetWorkspaceMountId())
	s.mu.RLock()
	entry := s.sessions[id]
	s.mu.RUnlock()
	if entry.session == nil {
		return workspaceMountSessionEntry{}, fmt.Errorf("%w: %s", ErrWorkspaceMountSessionNotFound, id)
	}
	if entry.channelToken == "" || authority.GetChannelToken() != entry.channelToken {
		return workspaceMountSessionEntry{}, errors.New("workspace authority channel token does not match the mount session")
	}
	if err := validateWorkspaceMountPhysicalAuthority(fence, entry.mount); err != nil {
		return workspaceMountSessionEntry{}, err
	}
	return entry, nil
}

func validateWorkspaceMountPhysicalAuthority(
	fence *workspacev0.WorkspaceAuthorityFence,
	mount workerapi.WorkspaceMount,
) error {
	if fence.GetWorkspaceMountId() != mount.ID ||
		fence.GetWorkspaceId() != mount.WorkspaceID ||
		fence.GetRuntimeInstanceId() != mount.RuntimeInstanceID {
		return errors.New("workspace authority fence does not match the mount session")
	}
	return nil
}

type managedWorkspaceMountSession struct {
	session                      vm.Session
	mu                           sync.RWMutex
	closeStarted                 bool
	closeDone                    chan struct{}
	closeErr                     error
	releaseForCheckpointStarted  bool
	releaseForCheckpointFinished bool
	releaseForCheckpointErr      error
	releaseForCheckpointDone     chan struct{}
	failureRequests              chan workspaceMountFailureRequest
}

func newManagedWorkspaceMountSession(session vm.Session) *managedWorkspaceMountSession {
	return &managedWorkspaceMountSession{
		session:                  session,
		closeDone:                make(chan struct{}),
		releaseForCheckpointDone: make(chan struct{}),
		failureRequests:          make(chan workspaceMountFailureRequest, 1),
	}
}

func (s *managedWorkspaceMountSession) requestFailure(ctx context.Context) error {
	request := workspaceMountFailureRequest{result: make(chan error, 1)}
	select {
	case s.failureRequests <- request:
	case <-ctx.Done():
		return ctx.Err()
	}
	select {
	case err := <-request.result:
		return err
	case <-ctx.Done():
		return ctx.Err()
	}
}

func (s *managedWorkspaceMountSession) Stream() vm.Stream {
	return s.session.Stream()
}

func (s *managedWorkspaceMountSession) OpenStream(ctx context.Context) (vm.Stream, error) {
	return s.session.OpenStream(ctx)
}

func (s *managedWorkspaceMountSession) Wait(ctx context.Context) error {
	return s.session.Wait(ctx)
}

func (s *managedWorkspaceMountSession) Close(ctx context.Context) error {
	return s.close(ctx)
}

func (s *managedWorkspaceMountSession) close(ctx context.Context) error {
	s.mu.Lock()
	if s.closeStarted {
		done := s.closeDone
		s.mu.Unlock()
		select {
		case <-done:
			s.mu.RLock()
			defer s.mu.RUnlock()
			return s.closeErr
		case <-ctx.Done():
			return ctx.Err()
		}
	}
	s.closeStarted = true
	done := s.closeDone
	s.mu.Unlock()

	err := s.session.Close(ctx)
	s.mu.Lock()
	s.closeErr = err
	close(done)
	s.mu.Unlock()
	return err
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
		select {
		case <-done:
		case <-ctx.Done():
			return ctx.Err()
		}
		s.mu.RLock()
		defer s.mu.RUnlock()
		return s.releaseForCheckpointErr
	}
	s.releaseForCheckpointStarted = true
	done := s.releaseForCheckpointDone
	s.mu.Unlock()

	err := s.close(ctx)
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
	parent vm.Session
	stream vm.Stream
	once   sync.Once
	err    error
}

func newBorrowedRunSession(parent vm.Session, stream vm.Stream) vm.Session {
	return &borrowedRunSession{parent: parent, stream: stream}
}

func (s *borrowedRunSession) Stream() vm.Stream {
	return s.stream
}

func (s *borrowedRunSession) OpenStream(context.Context) (vm.Stream, error) {
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
