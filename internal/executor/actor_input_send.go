package executor

import (
	"context"
	"encoding/json"
	"errors"
	"github.com/helmrdotdev/helmr/internal/api"
	"github.com/helmrdotdev/helmr/internal/ids"
	programv0 "github.com/helmrdotdev/helmr/internal/proto/program/v0"
	"github.com/helmrdotdev/helmr/internal/workerapi"
)

const maxJavaScriptSafeInteger = int64(9007199254740991)

type SessionSubmitControlPlane interface {
	SendRunSession(context.Context, workerapi.SubmitSessionDataRequest) (workerapi.SubmitSessionDataResponse, error)
	EnqueueRunSession(context.Context, workerapi.SubmitSessionDataRequest) (workerapi.SubmitSessionDataResponse, error)
	SendRunTurnMessage(context.Context, workerapi.SubmitSessionDataRequest) (workerapi.SubmitSessionDataResponse, error)
}

func (task *guestRunLeaseTask) handleSessionSubmit(ctx context.Context, requested *programv0.SessionSubmitRequested) error {
	request, err := workerSessionSubmitRequest(requested)
	if err != nil {
		return err
	}
	cp, ok := task.controlPlane.(SessionSubmitControlPlane)
	if !ok {
		return errors.New("Session submission control plane is required")
	}
	var response workerapi.SubmitSessionDataResponse
	err = task.callRunSourceRuntime(ctx, func(callCtx context.Context, lease workerapi.RunLeaseAssignment) error {
		request.Lease = lease.Fence()
		var e error
		switch requested.GetMode() {
		case "send":
			response, e = cp.SendRunSession(callCtx, request)
		case "enqueue":
			response, e = cp.EnqueueRunSession(callCtx, request)
		case "message":
			response, e = cp.SendRunTurnMessage(callCtx, request)
		}
		return e
	})
	if err != nil {
		return err
	}
	if response.CorrelationID != request.CorrelationID || (response.Completed == nil) == (response.Failed == nil) {
		return errors.New("Session admission receipt mismatch")
	}
	if r := response.Completed; r != nil {
		if ids.Validate(r.ID) != nil || ids.Validate(r.TurnID) != nil || (r.Kind != "turn" && r.Kind != "message") {
			return errors.New("Session admission receipt is invalid")
		}
	}
	return task.writeRuntimeResult(request.CorrelationID, response.Completed, response.Failed)
}
func workerSessionSubmitRequest(requested *programv0.SessionSubmitRequested) (workerapi.SubmitSessionDataRequest, error) {
	if requested == nil || ids.Validate(requested.GetCorrelationId()) != nil {
		return workerapi.SubmitSessionDataRequest{}, errors.New("Session submission correlation is invalid")
	}
	r := workerapi.SubmitSessionDataRequest{CorrelationID: requested.GetCorrelationId(), SessionID: requested.GetSessionId(), TurnID: requested.TurnId, Data: json.RawMessage(requested.GetDataJson()), IdempotencyKey: requested.GetIdempotencyKey()}
	if err := api.ValidateSessionID(r.SessionID); err != nil {
		return r, err
	}
	if err := api.ValidateSessionDataRequest(api.SessionDataRequest{Data: r.Data, IdempotencyKey: r.IdempotencyKey}); err != nil {
		return r, err
	}
	switch requested.GetMode() {
	case "send", "enqueue":
		if r.TurnID != nil {
			return r, errors.New("Session submission cannot name a Turn")
		}
	case "message":
		if r.TurnID == nil || ids.Validate(*r.TurnID) != nil {
			return r, errors.New("exact message requires Turn identity")
		}
	default:
		return r, errors.New("Session submission mode is invalid")
	}
	return r, nil
}
