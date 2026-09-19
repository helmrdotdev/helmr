package executor

import (
	"context"
	"errors"
	"github.com/helmrdotdev/helmr/internal/ids"
	programv0 "github.com/helmrdotdev/helmr/internal/proto/program/v0"
	"github.com/helmrdotdev/helmr/internal/workerapi"
)

type SessionReferenceControlPlane interface {
	GetRunSessionTurn(context.Context, workerapi.TurnReferenceRequest) (workerapi.SessionTurnResponse, error)
	InterruptRunSessionTurn(context.Context, workerapi.InterruptSessionTurnRequest) (workerapi.InterruptSessionTurnResponse, error)
	ResumeRunSession(context.Context, workerapi.ResumeSessionRequest) (workerapi.ResumeSessionResponse, error)
}

func (task *guestRunLeaseTask) handleSessionReferenceCommand(ctx context.Context, event *programv0.RunEvent) error {
	cp, ok := task.controlPlane.(SessionReferenceControlPlane)
	if !ok {
		return errors.New("Session reference control plane is required")
	}
	var base workerapi.SessionReferenceRequest
	var turn, hold, key string
	switch v := event.Event.(type) {
	case *programv0.RunEvent_SessionTurnRetrieveRequested:
		r := v.SessionTurnRetrieveRequested
		base.CorrelationID = r.GetCorrelationId()
		base.SessionID = r.GetSessionId()
		turn = r.GetTurnId()
	case *programv0.RunEvent_SessionTurnInterruptRequested:
		r := v.SessionTurnInterruptRequested
		base.CorrelationID = r.GetCorrelationId()
		base.SessionID = r.GetSessionId()
		turn = r.GetTurnId()
		key = r.GetIdempotencyKey()
	case *programv0.RunEvent_SessionResumeRequested:
		r := v.SessionResumeRequested
		base.CorrelationID = r.GetCorrelationId()
		base.SessionID = r.GetSessionId()
		hold = r.GetHoldId()
		key = r.GetIdempotencyKey()
	default:
		return errors.New("unsupported Session reference command")
	}
	if _, err := validateWorkerSessionReference(base); err != nil {
		return err
	}
	if turn != "" {
		if ids.Validate(turn) != nil {
			return errors.New("Turn identity is invalid")
		}
	} else if ids.Validate(hold) != nil {
		return errors.New("Session hold identity is invalid")
	}
	var correlation string
	var completed any
	var failed *workerapi.RuntimeOperationFailure
	err := task.callRunSourceRuntime(ctx, func(callCtx context.Context, lease workerapi.RunLeaseAssignment) error {
		base.Lease = lease.Fence()
		switch event.Event.(type) {
		case *programv0.RunEvent_SessionTurnRetrieveRequested:
			r, e := cp.GetRunSessionTurn(callCtx, workerapi.TurnReferenceRequest{SessionReferenceRequest: base, TurnID: turn})
			correlation = r.CorrelationID
			if r.Completed != nil {
				completed = r.Completed
			}
			failed = r.Failed
			return e
		case *programv0.RunEvent_SessionTurnInterruptRequested:
			r, e := cp.InterruptRunSessionTurn(callCtx, workerapi.InterruptSessionTurnRequest{TurnReferenceRequest: workerapi.TurnReferenceRequest{SessionReferenceRequest: base, TurnID: turn}, IdempotencyKey: key})
			correlation = r.CorrelationID
			if r.Completed != nil {
				completed = r.Completed
			}
			failed = r.Failed
			return e
		default:
			r, e := cp.ResumeRunSession(callCtx, workerapi.ResumeSessionRequest{SessionReferenceRequest: base, HoldID: hold, IdempotencyKey: key})
			correlation = r.CorrelationID
			if r.Completed != nil {
				completed = r.Completed
			}
			failed = r.Failed
			return e
		}
	})
	if err != nil {
		return err
	}
	if correlation != base.CorrelationID || (completed == nil) == (failed == nil) {
		return errors.New("Session reference receipt mismatch")
	}
	return task.writeRuntimeResult(correlation, completed, failed)
}
