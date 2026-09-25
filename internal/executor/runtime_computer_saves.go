package executor

import (
	"context"
	"errors"
	"math"
	"sync"
	"uuid"

	"github.com/helmrdotdev/helmr/internal/vm"
	"github.com/helmrdotdev/helmr/internal/workerapi"
)

// runtimeComputerSaves belongs to the physical mount, not to a borrowed Run
// stream. A pending operation retains its original authority through settlement.
// Quiesce irreversibly prevents admission before joining that operation.
type runtimeComputerSaves struct {
	mu                    sync.Mutex
	authorities           []*computerSaveAuthority
	loopCancel            context.CancelFunc
	loopDone              chan struct{}
	sequence              int64
	pending               *computerSave
	stopped               bool
	settling              bool
	runtimeID, computerID string
}

// start coalesces ticks while an operation is running. Failed operations must be
// reconciled before another is admitted; they are never replaced by a new ID.
func (s *runtimeComputerSaves) start(ctx context.Context, client ComputerSaveClient, objects generationObjectPublisher, authority workerapi.ComputerSaveBeginRequest, runtimeID, computerID string, capture func(context.Context) (computerSaveCapture, error)) (bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.settling {
		return false, nil
	}
	if s.runtimeID != "" && (s.runtimeID != runtimeID || s.computerID != computerID) {
		return false, errors.New("computer save owner identity changed")
	}
	if s.stopped {
		return false, errors.New("computer saves are quiescing")
	}
	if err := ctx.Err(); err != nil {
		return false, err
	}
	if s.pending != nil {
		select {
		case <-s.pending.done:
		default:
			return false, nil
		}
		if s.pending.rejected {
			s.sequence = s.pending.request.Sequence - 1
		}
		if !s.pending.released {
			return false, errors.Join(errors.New("previous Computer save requires settlement"), s.pending.err)
		}
	}
	if s.sequence == math.MaxInt64 {
		return false, errors.New("computer save sequence exhausted")
	}
	authority.SaveID = uuid.NewV7().String()
	authority.Sequence = s.sequence + 1
	pending, err := startComputerSave(ctx, client, objects, authority, runtimeID, computerID, capture)
	if err != nil {
		return false, err
	}
	s.runtimeID, s.computerID = runtimeID, computerID
	s.pending = pending
	s.sequence = authority.Sequence
	return true, nil
}

func (s *runtimeComputerSaves) Quiesce(ctx context.Context) error {
	s.mu.Lock()
	s.stopped = true
	cancel, done := s.loopCancel, s.loopDone
	pending := s.pending
	if cancel != nil {
		cancel()
	}
	if pending != nil {
		pending.cancel()
	}
	s.mu.Unlock()
	if done != nil {
		select {
		case <-done:
		case <-ctx.Done():
			return ctx.Err()
		}
	}
	if pending == nil {
		return nil
	}
	return pending.Quiesce(ctx)
}

// A live cut resumes before upload. The concrete retained capture must also
// support durable local source adoption after the CP commits its receipt.
type liveComputerCaptureSession interface {
	CaptureComputer(context.Context) (*vm.ComputerSnapshot, error)
}

func captureComputerSave(ctx context.Context, session vm.Session, computerID string) (computerSaveCapture, error) {
	source, ok := session.(liveComputerCaptureSession)
	if !ok {
		return nil, errors.New("runtime cannot capture a live Computer")
	}
	snapshot, err := source.CaptureComputer(ctx)
	if err != nil {
		return nil, err
	}
	if snapshot == nil || snapshot.Capture == nil {
		return nil, errors.New("live Computer capture is missing")
	}
	capture, ok := snapshot.Capture.(computerSaveCapture)
	if !ok || snapshot.ComputerID != computerID {
		snapshot.Capture.Release()
		return nil, errors.New("live Computer capture differs from save owner")
	}
	return capture, nil
}

// settle reconciles a failed background operation without stopping future ticks.
// It never replaces an uncertain operation; terminal Quiesce can join it safely.
func (s *runtimeComputerSaves) settle(ctx context.Context) error {
	s.mu.Lock()
	if s.settling {
		s.mu.Unlock()
		return errors.New("computer save settlement already active")
	}
	s.settling = true
	pending := s.pending
	s.mu.Unlock()
	defer func() { s.mu.Lock(); s.settling = false; s.mu.Unlock() }()
	if pending == nil {
		return nil
	}
	return pending.Quiesce(ctx)
}
