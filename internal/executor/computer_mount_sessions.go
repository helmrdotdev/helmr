package executor

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"time"

	computerv0 "github.com/helmrdotdev/helmr/internal/proto/computer/v0"
	"github.com/helmrdotdev/helmr/internal/vm"
	"github.com/helmrdotdev/helmr/internal/workerapi"
)

var ErrComputerMountSessionNotFound = errors.New("computer mount session not found")

type ComputerMountSessionRegistry interface {
	RegisterComputerMountSession(mount workerapi.ComputerInstanceAssignment, session *managedComputerMountSession, channelToken string) func()
	OpenComputerInstanceSession(context.Context, string) (ComputerMountSession, error)
	FailComputerInstanceSession(context.Context, string) error
	RenewComputerAuthority(context.Context, *computerv0.RenewComputerAuthorityRequest) (*computerv0.ComputerAuthorityFence, error)
}

// ComputerMountSession is one Run's view of a mounted Computer. Session carries
// only the Run's borrowed stream; closing it never stops the machine.
// ReleaseSource is bound to the physical mount and releases it as a checkpoint
// source after the Run's control stream has detached.
type ComputerMountSession struct {
	Session        vm.Machine
	ControlSession vm.Machine
	ReleaseSource  func(context.Context) error
	ChannelToken   string
	Mount          workerapi.ComputerInstanceAssignment
}

type computerMountFailureRequest struct {
	result chan error
}

type ComputerMountSessions struct {
	mu       sync.RWMutex
	sessions map[string]computerMountSessionEntry
}

type computerMountSessionEntry struct {
	session      *managedComputerMountSession
	channelToken string
	mount        workerapi.ComputerInstanceAssignment
}

func NewComputerMountSessions() *ComputerMountSessions {
	return &ComputerMountSessions{sessions: map[string]computerMountSessionEntry{}}
}

func (s *ComputerMountSessions) RegisterComputerMountSession(mount workerapi.ComputerInstanceAssignment, session *managedComputerMountSession, channelToken string) func() {
	id := strings.TrimSpace(mount.ComputerInstanceID)
	if id == "" || session == nil {
		return func() {}
	}
	s.mu.Lock()
	if s.sessions == nil {
		s.sessions = map[string]computerMountSessionEntry{}
	}
	s.sessions[id] = computerMountSessionEntry{session: session, channelToken: strings.TrimSpace(channelToken), mount: mount}
	s.mu.Unlock()
	return func() {
		s.mu.Lock()
		if current := s.sessions[id]; current.session == session {
			delete(s.sessions, id)
		}
		s.mu.Unlock()
	}
}

func (s *ComputerMountSessions) OpenComputerInstanceSession(ctx context.Context, computerInstanceID string) (ComputerMountSession, error) {
	id := strings.TrimSpace(computerInstanceID)
	if id == "" {
		return ComputerMountSession{}, errors.New("computer mount id is required")
	}
	s.mu.RLock()
	entry := s.sessions[id]
	s.mu.RUnlock()
	if entry.session == nil {
		return ComputerMountSession{}, fmt.Errorf("%w: %s", ErrComputerMountSessionNotFound, id)
	}
	if entry.channelToken == "" {
		return ComputerMountSession{}, fmt.Errorf("computer mount session %s missing channel token", id)
	}
	stream, err := entry.session.OpenStream(ctx)
	if err != nil {
		return ComputerMountSession{}, fmt.Errorf("open computer mount stream %s: %w", id, err)
	}
	return ComputerMountSession{
		Session:        newBorrowedRunSession(entry.session, stream),
		ControlSession: entry.session,
		ReleaseSource:  entry.session.ReleaseCheckpointSource,
		ChannelToken:   entry.channelToken,
		Mount:          entry.mount,
	}, nil
}

func (s *ComputerMountSessions) FailComputerInstanceSession(
	ctx context.Context,
	computerInstanceID string,
) error {
	id := strings.TrimSpace(computerInstanceID)
	if id == "" {
		return errors.New("computer mount failure identity is required")
	}
	s.mu.RLock()
	entry := s.sessions[id]
	s.mu.RUnlock()
	if entry.session == nil {
		return fmt.Errorf("%w: %s", ErrComputerMountSessionNotFound, id)
	}
	return entry.session.requestFailure(ctx)
}

func (s *ComputerMountSessions) RenewComputerAuthority(ctx context.Context, request *computerv0.RenewComputerAuthorityRequest) (*computerv0.ComputerAuthorityFence, error) {
	if request == nil || request.GetPrevious() == nil || request.GetPrevious().GetFence() == nil {
		return nil, errors.New("previous computer authority is required")
	}
	fence := request.GetPrevious().GetFence()
	id := strings.TrimSpace(fence.GetComputerInstanceId())
	s.mu.RLock()
	entry := s.sessions[id]
	s.mu.RUnlock()
	if entry.session == nil {
		return nil, fmt.Errorf("%w: %s", ErrComputerMountSessionNotFound, id)
	}
	if entry.channelToken == "" || request.GetPrevious().GetChannelToken() != entry.channelToken {
		return nil, errors.New("computer authority channel token does not match the mount session")
	}
	if err := validateComputerMountPhysicalAuthority(fence, entry.mount); err != nil {
		return nil, err
	}
	return renewComputerAuthorityOnSession(ctx, entry.session, request)
}

func validateComputerMountPhysicalAuthority(
	fence *computerv0.ComputerAuthorityFence,
	mount workerapi.ComputerInstanceAssignment,
) error {
	if fence.GetComputerInstanceId() != mount.ComputerInstanceID ||
		fence.GetComputerId() != mount.ComputerID ||
		fence.GetWriterGeneration() <= 0 ||
		fence.GetWriterGeneration() != mount.WriterGeneration {
		return errors.New("computer authority fence does not match the mount session")
	}
	return nil
}

type managedComputerMountSession struct {
	saves                        runtimeComputerSaves
	session                      vm.Machine
	mu                           sync.RWMutex
	closeAttempt                 *computerSessionClose
	releaseForCheckpointStarted  bool
	releaseForCheckpointFinished bool
	releaseForCheckpointErr      error
	releaseForCheckpointDone     chan struct{}
	failureRequests              chan computerMountFailureRequest
}

func newManagedComputerMountSession(session vm.Machine) *managedComputerMountSession {
	return &managedComputerMountSession{
		session:                  session,
		releaseForCheckpointDone: make(chan struct{}),
		failureRequests:          make(chan computerMountFailureRequest, 1),
	}
}

func (s *managedComputerMountSession) requestFailure(ctx context.Context) error {
	request := computerMountFailureRequest{result: make(chan error, 1)}
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

func (s *managedComputerMountSession) Stream() vm.Stream {
	return s.session.Stream()
}

func (s *managedComputerMountSession) OpenStream(ctx context.Context) (vm.Stream, error) {
	return s.session.OpenStream(ctx)
}

func (s *managedComputerMountSession) Wait(ctx context.Context) error {
	return s.session.Wait(ctx)
}

func (s *managedComputerMountSession) Close(ctx context.Context) error {
	return s.close(ctx)
}

func (s *managedComputerMountSession) close(ctx context.Context) error {
	// A failed handoff still requires physical exclusion. Keep its error visible
	// so callers cannot mistake cleanup for a successfully settled save.
	saveErr := s.saves.Quiesce(ctx)

	s.mu.Lock()
	if attempt := s.closeAttempt; attempt != nil {
		s.mu.Unlock()
		select {
		case <-attempt.done:
			return attempt.err
		case <-ctx.Done():
			return ctx.Err()
		}
	}
	attempt := &computerSessionClose{done: make(chan struct{})}
	s.closeAttempt = attempt
	s.mu.Unlock()

	stopCtx := ctx
	cancelStop := func() {}
	if saveErr != nil {
		// Reconciliation can consume the caller deadline. Physical exclusion
		// still gets a bounded attempt, with the failed handoff reported below.
		stopCtx, cancelStop = context.WithTimeout(context.WithoutCancel(ctx), 30*time.Second)
	}
	defer cancelStop()
	stopErr := s.session.Close(stopCtx)
	err := errors.Join(saveErr, stopErr)
	s.mu.Lock()
	attempt.err = err
	if errors.Is(stopErr, context.DeadlineExceeded) || errors.Is(stopErr, context.Canceled) {
		// Physical Close may time out joining capture before its once-only
		// cleanup starts. Retain each waiter's result, but allow a later retry.
		s.closeAttempt = nil
	}
	close(attempt.done)
	s.mu.Unlock()
	return err
}

func (s *managedComputerMountSession) ReleaseCheckpointSource(ctx context.Context) error {
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

func (s *managedComputerMountSession) CheckpointReleaseResult(ctx context.Context) (bool, error) {
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

// borrowedRunSession is a Run's stream on a mounted Computer. It exposes only
// that stream and the mount's lifetime; checkpoint and save operations belong to
// the physical mount owner.
type borrowedRunSession struct {
	parent vm.Machine
	stream vm.Stream
	once   sync.Once
	err    error
}

func newBorrowedRunSession(parent vm.Machine, stream vm.Stream) vm.Machine {
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

// Close closes only the borrowed stream. The mounted machine stays running.
func (s *borrowedRunSession) Close(context.Context) error {
	s.once.Do(func() {
		if s.stream != nil {
			s.err = s.stream.Close()
		}
	})
	return s.err
}

type computerSessionClose struct {
	done chan struct{}
	err  error
}
