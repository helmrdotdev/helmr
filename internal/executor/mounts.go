package executor

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"time"

	computerv0 "github.com/helmrdotdev/helmr/internal/proto/computer/v0"
	programv0 "github.com/helmrdotdev/helmr/internal/proto/program/v0"
	"github.com/helmrdotdev/helmr/internal/vm"
	"github.com/helmrdotdev/helmr/internal/workerapi"
)

var ErrMountNotFound = errors.New("computer mount session not found")

type MountRegistry interface {
	Register(mount workerapi.ComputerInstanceAssignment, instance *instanceMount, channelToken string) func()
	OpenChannel(context.Context, string) (MountChannel, error)
	RequestFailure(context.Context, string) error
	RenewComputerAuthority(context.Context, *computerv0.RenewComputerAuthorityRequest) (*computerv0.ComputerAuthorityFence, error)
}

// MountChannel is one Run's view of a mounted Computer. Channel carries
// only the Run's borrowed stream; closing it never stops the machine.
// ReleaseSource is bound to the physical mount and releases it as a checkpoint
// source after the Run's control stream has detached. GrantProgramResume is
// likewise bound to the physical mount the channel was opened on.
type MountChannel struct {
	Channel vm.Machine
	// ReleaseSource is always set by OpenChannel.
	ReleaseSource func(context.Context) error
	// GrantProgramResume is always set by OpenChannel.
	GrantProgramResume func(context.Context, *computerv0.GrantProgramResumeRequest) (*programv0.ResumeAttach, error)
	ChannelToken       string
	Mount              workerapi.ComputerInstanceAssignment
}

type mountFailureRequest struct {
	result chan error
}

type Mounts struct {
	mu     sync.RWMutex
	mounts map[string]mountEntry
}

type mountEntry struct {
	instance     *instanceMount
	channelToken string
	mount        workerapi.ComputerInstanceAssignment
}

func NewMounts() *Mounts {
	return &Mounts{mounts: map[string]mountEntry{}}
}

func (s *Mounts) Register(mount workerapi.ComputerInstanceAssignment, instance *instanceMount, channelToken string) func() {
	id := strings.TrimSpace(mount.ComputerInstanceID)
	if id == "" || instance == nil {
		return func() {}
	}
	s.mu.Lock()
	if s.mounts == nil {
		s.mounts = map[string]mountEntry{}
	}
	s.mounts[id] = mountEntry{instance: instance, channelToken: strings.TrimSpace(channelToken), mount: mount}
	s.mu.Unlock()
	return func() {
		s.mu.Lock()
		if current := s.mounts[id]; current.instance == instance {
			delete(s.mounts, id)
		}
		s.mu.Unlock()
	}
}

func (s *Mounts) OpenChannel(ctx context.Context, computerInstanceID string) (MountChannel, error) {
	id := strings.TrimSpace(computerInstanceID)
	if id == "" {
		return MountChannel{}, errors.New("computer mount id is required")
	}
	s.mu.RLock()
	entry := s.mounts[id]
	s.mu.RUnlock()
	if entry.instance == nil {
		return MountChannel{}, fmt.Errorf("%w: %s", ErrMountNotFound, id)
	}
	if entry.channelToken == "" {
		return MountChannel{}, fmt.Errorf("computer mount session %s missing channel token", id)
	}
	stream, err := entry.instance.OpenStream(ctx)
	if err != nil {
		return MountChannel{}, fmt.Errorf("open computer mount stream %s: %w", id, err)
	}
	return MountChannel{
		Channel:       newBorrowedChannel(entry.instance, stream),
		ReleaseSource: entry.instance.ReleaseCheckpointSource,
		GrantProgramResume: func(ctx context.Context, request *computerv0.GrantProgramResumeRequest) (*programv0.ResumeAttach, error) {
			return grantProgramResumeOnMachine(ctx, entry.instance, request)
		},
		ChannelToken: entry.channelToken,
		Mount:        entry.mount,
	}, nil
}

func (s *Mounts) RequestFailure(
	ctx context.Context,
	computerInstanceID string,
) error {
	id := strings.TrimSpace(computerInstanceID)
	if id == "" {
		return errors.New("computer mount failure identity is required")
	}
	s.mu.RLock()
	entry := s.mounts[id]
	s.mu.RUnlock()
	if entry.instance == nil {
		return fmt.Errorf("%w: %s", ErrMountNotFound, id)
	}
	return entry.instance.requestFailure(ctx)
}

func (s *Mounts) RenewComputerAuthority(ctx context.Context, request *computerv0.RenewComputerAuthorityRequest) (*computerv0.ComputerAuthorityFence, error) {
	if request == nil || request.GetPrevious() == nil || request.GetPrevious().GetFence() == nil {
		return nil, errors.New("previous computer authority is required")
	}
	fence := request.GetPrevious().GetFence()
	id := strings.TrimSpace(fence.GetComputerInstanceId())
	s.mu.RLock()
	entry := s.mounts[id]
	s.mu.RUnlock()
	if entry.instance == nil {
		return nil, fmt.Errorf("%w: %s", ErrMountNotFound, id)
	}
	if entry.channelToken == "" || request.GetPrevious().GetChannelToken() != entry.channelToken {
		return nil, errors.New("computer authority channel token does not match the mount session")
	}
	if err := validateComputerMountPhysicalAuthority(fence, entry.mount); err != nil {
		return nil, err
	}
	return renewComputerAuthorityOnMachine(ctx, entry.instance, request)
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

type instanceMount struct {
	saves                        runtimeComputerSaves
	machine                      vm.Machine
	mu                           sync.RWMutex
	closeAttempt                 *instanceMountClose
	releaseForCheckpointStarted  bool
	releaseForCheckpointFinished bool
	releaseForCheckpointErr      error
	releaseForCheckpointDone     chan struct{}
	failureRequests              chan mountFailureRequest
}

func newInstanceMount(machine vm.Machine) *instanceMount {
	return &instanceMount{
		machine:                  machine,
		releaseForCheckpointDone: make(chan struct{}),
		failureRequests:          make(chan mountFailureRequest, 1),
	}
}

func (s *instanceMount) requestFailure(ctx context.Context) error {
	request := mountFailureRequest{result: make(chan error, 1)}
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

func (s *instanceMount) Stream() vm.Stream {
	return s.machine.Stream()
}

func (s *instanceMount) OpenStream(ctx context.Context) (vm.Stream, error) {
	return s.machine.OpenStream(ctx)
}

func (s *instanceMount) Wait(ctx context.Context) error {
	return s.machine.Wait(ctx)
}

func (s *instanceMount) Close(ctx context.Context) error {
	return s.close(ctx)
}

func (s *instanceMount) close(ctx context.Context) error {
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
	attempt := &instanceMountClose{done: make(chan struct{})}
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
	stopErr := s.machine.Close(stopCtx)
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

func (s *instanceMount) ReleaseCheckpointSource(ctx context.Context) error {
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

func (s *instanceMount) CheckpointReleaseResult(ctx context.Context) (bool, error) {
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

// borrowedChannel is a Run's stream on a mounted Computer. It exposes only
// that stream and the mount's lifetime; checkpoint and save operations belong to
// the physical mount owner.
type borrowedChannel struct {
	parent vm.Machine
	stream vm.Stream
	once   sync.Once
	err    error
}

func newBorrowedChannel(parent vm.Machine, stream vm.Stream) vm.Machine {
	return &borrowedChannel{parent: parent, stream: stream}
}

func (s *borrowedChannel) Stream() vm.Stream {
	return s.stream
}

func (s *borrowedChannel) OpenStream(context.Context) (vm.Stream, error) {
	return nil, errors.New("borrowed run session does not support opening nested streams")
}

func (s *borrowedChannel) Wait(ctx context.Context) error {
	if s.parent != nil {
		return s.parent.Wait(ctx)
	}
	<-ctx.Done()
	return ctx.Err()
}

// Close closes only the borrowed stream. The mounted machine stays running.
func (s *borrowedChannel) Close(context.Context) error {
	s.once.Do(func() {
		if s.stream != nil {
			s.err = s.stream.Close()
		}
	})
	return s.err
}

type instanceMountClose struct {
	done chan struct{}
	err  error
}
