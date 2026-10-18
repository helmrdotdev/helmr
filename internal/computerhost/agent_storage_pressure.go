package computerhost

import (
	"context"
	"errors"
	"log/slog"
	"time"

	"github.com/helmrdotdev/helmr/internal/workerapi"
)

func (s *agentComputerSaves) setPressureSave(save *workerapi.AgentSave) {
	s.pressureMu.Lock()
	if save == nil {
		s.pressureSave = nil
	} else {
		copy := *save
		s.pressureSave = &copy
	}
	s.pressureMu.Unlock()
	select {
	case s.pressureRequest <- struct{}{}:
	default:
	}
}

// runPressure must progress while the covering cut waits for a guest flush.
// It owns only object retention and local eviction, never a recovery-head or
// coverage receipt. run cancels and joins it before releasing disk/key custody.
func (s *agentComputerSaves) runPressure(ctx context.Context) error {
	reported, wasActive, uncertain := false, false, false
	retry := time.Duration(0)
	for ctx.Err() == nil {
		state := s.changes()
		active := !state.PressureSince.IsZero()
		var stepErr error
		if active != wasActive || (active && !reported) || uncertain {
			request := workerapi.AgentStoragePressure{AllocationIdentity: s.identity, Active: active}
			reportCtx, cancel := context.WithTimeout(ctx, 10*time.Second)
			stepErr = s.client.ReportAgentStoragePressure(reportCtx, request)
			cancel()
			if stepErr == nil {
				wasActive, reported, uncertain = active, true, false
			} else {
				uncertain = true
			}
		}
		if stepErr != nil && !saveOperationRetryable(stepErr) {
			return stepErr
		}
		if active {
			stepErr = errors.Join(stepErr, s.relievePressure(ctx))
		}
		if stepErr != nil {
			if !saveOperationRetryable(stepErr) {
				return stepErr
			}
			slog.Info("Computer pressure preservation retry", "computer_id", s.identity.OwnerID, "error", stepErr)
			retry = min(10*time.Second, max(100*time.Millisecond, retry*2))
		} else {
			retry = 0
		}
		state = s.changes()
		var timer *time.Timer
		var next <-chan time.Time
		if !state.PressureSince.IsZero() || stepErr != nil {
			timer = time.NewTimer(max(time.Second, retry))
			next = timer.C
		}
		select {
		case <-ctx.Done():
			if timer != nil {
				timer.Stop()
			}
			return ctx.Err()
		case <-state.PressureWake:
			if timer != nil {
				timer.Stop()
			}
		case <-s.pressureRequest:
			if timer != nil {
				timer.Stop()
			}
		case <-next:
		}
	}
	return ctx.Err()
}

func (s *agentComputerSaves) relievePressure(ctx context.Context) error {
	s.pressureGate.Lock()
	defer s.pressureGate.Unlock()
	s.pressureMu.Lock()
	save := s.pressureSave
	s.pressureMu.Unlock()
	if s.changes().PressureSince.IsZero() {
		return nil
	}
	capture, err := s.pinCommitted(ctx)
	if err != nil {
		return err
	}
	defer capture.Release()
	// First recover conservative charges/orphans from failed local encoders.
	_, err = capture.Collect(ctx, 1<<20)
	if err != nil {
		return err
	}
	if save == nil || s.changes().PressureSince.IsZero() {
		return nil
	}
	err = capture.Publish(ctx, agentSavePublisher{client: s.client, objects: s.objects, save: *save})
	if err != nil {
		return err
	}
	err = capture.BackRemotely(ctx, 1<<20)
	if err != nil {
		return err
	}
	// Release this immutable pin only after its publisher and verification joined.
	capture.Release()
	_, err = capture.Collect(ctx, 1<<20)
	return err
}
