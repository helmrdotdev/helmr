package executor

import (
	"context"
	"encoding/json"
	"errors"
	"time"
	"uuid"

	programv0 "github.com/helmrdotdev/helmr/internal/proto/program/v0"
	"github.com/helmrdotdev/helmr/internal/wire"
	"github.com/helmrdotdev/helmr/internal/workerapi"
	"google.golang.org/protobuf/proto"
)

func (task *guestRunLeaseTask) deliverSessionStop(ctx context.Context) (time.Time, error) {
	return task.deliverSessionStopWithCaptureBarrier(ctx, false)
}

// Only the independent poller needs to acquire the capture gate. Synchronous
// event-drain callers already own it and must finish their runtime responses.
func (task *guestRunLeaseTask) deliverSessionStopWithCaptureBarrier(ctx context.Context, independent bool) (time.Time, error) {
	task.stopMu.Lock()
	previous := task.stopDeadline
	task.stopMu.Unlock()
	if !previous.IsZero() {
		return previous, nil
	}
	cp := task.controlPlane.Sessions
	correlation := uuid.NewV7().String()
	var response workerapi.SessionControlResponse
	var deadline time.Time
	err := task.callRunSourceRuntime(ctx, func(callCtx context.Context, lease workerapi.RunLeaseAssignment) error {
		deadline = lease.ExpiresAt
		var e error
		response, e = cp.ReadSessionControl(callCtx, workerapi.SessionControlRequest{Lease: lease.Fence(), CorrelationID: correlation, RunGeneration: task.program.execution.GetRunGeneration()})
		return e
	})
	if err != nil {
		return time.Time{}, err
	}
	if response.CorrelationID != correlation {
		return time.Time{}, errors.New("session control response correlation mismatch")
	}
	if response.HoldID == nil {
		return time.Time{}, nil
	}
	if *response.HoldID == "" || response.Reason == nil || *response.Reason == "" || (response.TurnID != nil && *response.TurnID == "") {
		return time.Time{}, errors.New("session stop identity is incomplete")
	}
	if independent {
		task.renewalGate.Lock()
		defer task.renewalGate.Unlock()
		task.mu.Lock()
		paused := task.capturePaused
		task.mu.Unlock()
		if paused {
			return time.Time{}, nil
		}
	}
	task.stopMu.Lock()
	defer task.stopMu.Unlock()
	if !task.stopDeadline.IsZero() {
		return task.stopDeadline, nil
	}
	// A poll read may have waited behind capture. Re-read current authority on
	// the next poll instead of closing a fresh transport with an expired read.
	if !deadline.After(time.Now()) {
		return time.Time{}, nil
	}

	stop := &programv0.SessionStop{Execution: proto.Clone(task.program.execution).(*programv0.SessionExecution), TurnId: response.TurnID, HoldId: *response.HoldID, Reason: *response.Reason}
	writeCtx, cancelWrite := context.WithDeadline(ctx, deadline)
	defer cancelWrite()
	stream := task.programStream()
	closeStream := stream
	if task.program.protocol != nil {
		closeStream = task.program.protocol.currentStream()
	}
	stopClose := context.AfterFunc(writeCtx, func() { _ = closeStream.Close() })
	defer stopClose()
	if err := wire.WriteSessionStop(stream, stop); err != nil {
		return time.Time{}, err
	}
	task.stopDeadline = deadline
	return deadline, nil
}
func (task *guestRunLeaseTask) pollSessionStop(ctx context.Context) error {
	for {
		deadline, err := task.deliverSessionStopWithCaptureBarrier(ctx, true)
		if err != nil {
			return err
		}
		if !deadline.IsZero() {
			// Renewals cannot give unresponsive customer code an unbounded stop grace.
			timer := time.NewTimer(time.Until(deadline))
			defer timer.Stop()
			select {
			case <-ctx.Done():
				return ctx.Err()
			case <-timer.C:
				return errors.New("session stop did not converge before its lease deadline")
			}
		}
		if err := sleepWithContext(ctx, 100*time.Millisecond); err != nil {
			return err
		}
	}
}
func (task *guestRunLeaseTask) beforeWaitResume(ctx context.Context, decision WaitResumeDecision) error {
	if task.program.execution == nil || (decision.Kind != "cancelled" && decision.Kind != "failed") {
		return nil
	}
	var cancellation struct {
		ReasonCode string `json:"reason_code"`
	}
	if err := json.Unmarshal(decision.Data, &cancellation); err != nil {
		return err
	}
	if cancellation.ReasonCode != "session_stopped" {
		return nil
	}
	deadline, err := task.deliverSessionStop(ctx)
	if err != nil {
		return err
	}
	if deadline.IsZero() {
		return errors.New("stopped Wait has no exact Session stop authority")
	}
	return nil
}
