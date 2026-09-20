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
	task.stopMu.Lock()
	defer task.stopMu.Unlock()
	if !task.stopDeadline.IsZero() {
		return task.stopDeadline, nil
	}
	cp, ok := task.controlPlane.(SessionExecutionControlPlane)
	if !ok {
		return time.Time{}, errors.New("session control client is required")
	}
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
	stop := &programv0.SessionStop{Execution: proto.Clone(task.program.execution).(*programv0.SessionExecution), TurnId: response.TurnID, HoldId: *response.HoldID, Reason: *response.Reason}
	writeCtx, cancelWrite := context.WithDeadline(ctx, deadline)
	defer cancelWrite()
	stopClose := context.AfterFunc(writeCtx, func() { _ = task.programStream().Close() })
	defer stopClose()
	if err := wire.WriteSessionStop(task.programStream(), stop); err != nil {
		return time.Time{}, err
	}
	task.stopDeadline = deadline
	return deadline, nil
}
func (task *guestRunLeaseTask) pollSessionStop(ctx context.Context) error {
	for {
		deadline, err := task.deliverSessionStop(ctx)
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
	if task.program.execution == nil || decision.Kind != "cancelled" {
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
