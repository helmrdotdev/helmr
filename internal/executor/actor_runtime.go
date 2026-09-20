package executor

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"strings"

	"github.com/helmrdotdev/helmr/internal/api"
	"github.com/helmrdotdev/helmr/internal/ids"
	programv0 "github.com/helmrdotdev/helmr/internal/proto/program/v0"
	"github.com/helmrdotdev/helmr/internal/wire"
	"github.com/helmrdotdev/helmr/internal/workerapi"
)

func (task *guestRunLeaseTask) handleResourceRuntime(
	ctx context.Context,
	event *programv0.RunEvent,
) error {
	switch value := event.GetEvent().(type) {
	case *programv0.RunEvent_TurnReadyRequested, *programv0.RunEvent_TurnSettlementBeginRequested, *programv0.RunEvent_TurnMessageClaimRequested, *programv0.RunEvent_TurnMessageCompleteRequested:
		return task.handleTurnCommand(ctx, event)
	case *programv0.RunEvent_SessionOutputWriteRequested:
		return task.handleSessionOutput(ctx, value.SessionOutputWriteRequested)
	case *programv0.RunEvent_TurnOutputWriteRequested:
		return task.handleTurnOutput(ctx, value.TurnOutputWriteRequested)
	case *programv0.RunEvent_SessionTurnRetrieveRequested, *programv0.RunEvent_SessionTurnInterruptRequested, *programv0.RunEvent_SessionResumeRequested:
		return task.handleSessionReferenceCommand(ctx, event)
	case *programv0.RunEvent_ActorStartRequested,
		*programv0.RunEvent_SessionStatusRequested,
		*programv0.RunEvent_SessionCloseRequested,
		*programv0.RunEvent_SessionCancelRequested,
		*programv0.RunEvent_SessionEventsRequested:
		return task.handleActorRuntime(ctx, event)
	default:
		return task.handleWorkspaceRuntime(ctx, event)
	}
}

func (task *guestRunLeaseTask) handleActorRuntime(
	ctx context.Context,
	event *programv0.RunEvent,
) error {
	controlPlane, ok := task.controlPlane.(ActorRuntimeControlPlane)
	if !ok {
		return errors.New("run lease task actor runtime control plane is required")
	}

	var correlationID string
	var completed any
	var failed *workerapi.RuntimeOperationFailure
	switch value := event.Event.(type) {
	case *programv0.RunEvent_ActorStartRequested:
		request, err := workerActorStartRequest(value.ActorStartRequested)
		if err != nil {
			return err
		}
		correlationID = request.CorrelationID
		var response workerapi.StartActorResponse
		err = task.callRunSourceRuntime(ctx, func(
			callCtx context.Context,
			lease workerapi.RunLeaseAssignment,
		) error {
			request.Lease = lease.Fence()
			var callErr error
			response, callErr = controlPlane.StartRunActor(callCtx, request)
			return callErr
		})
		if err != nil {
			return fmt.Errorf("start actor: %w", err)
		}
		if response.Completed != nil {
			completed = response.Completed
		}
		failed = response.Failed
		if response.CorrelationID != correlationID {
			return errors.New("actor start response correlation mismatch")
		}
	case *programv0.RunEvent_SessionStatusRequested:
		request, err := workerSessionReferenceRequest(value.SessionStatusRequested)
		if err != nil {
			return err
		}
		correlationID = request.CorrelationID
		var response workerapi.SessionStatusResponse
		err = task.callRunSourceRuntime(ctx, func(
			callCtx context.Context,
			lease workerapi.RunLeaseAssignment,
		) error {
			request.Lease = lease.Fence()
			var callErr error
			response, callErr = controlPlane.GetRunSessionStatus(callCtx, request)
			return callErr
		})
		if err != nil {
			return fmt.Errorf("read session status: %w", err)
		}
		if response.Completed != nil {
			completed = response.Completed
		}
		failed = response.Failed
		if response.CorrelationID != correlationID {
			return errors.New("session status response correlation mismatch")
		}
	case *programv0.RunEvent_SessionCloseRequested:
		base, err := workerSessionReferenceRequestFromClose(value.SessionCloseRequested)
		if err != nil {
			return err
		}
		request := workerapi.CloseSessionRequest{
			SessionReferenceRequest: base,
			IdempotencyKey:          value.SessionCloseRequested.GetIdempotencyKey(),
		}
		correlationID = request.CorrelationID
		var response workerapi.CloseSessionResponse
		err = task.callRunSourceRuntime(ctx, func(
			callCtx context.Context,
			lease workerapi.RunLeaseAssignment,
		) error {
			request.Lease = lease.Fence()
			var callErr error
			response, callErr = controlPlane.CloseRunSession(callCtx, request)
			return callErr
		})
		if err != nil {
			return fmt.Errorf("close actor: %w", err)
		}
		if response.Completed != nil {
			completed = response.Completed
		}
		failed = response.Failed
		if response.CorrelationID != correlationID {
			return errors.New("session close response correlation mismatch")
		}
	case *programv0.RunEvent_SessionCancelRequested:
		base, err := workerSessionReferenceRequestFromCancel(value.SessionCancelRequested)
		if err != nil {
			return err
		}
		request := workerapi.CancelSessionRequest{
			SessionReferenceRequest: base,
			IdempotencyKey:          value.SessionCancelRequested.GetIdempotencyKey(),
		}
		correlationID = request.CorrelationID
		var response workerapi.CancelSessionResponse
		err = task.callRunSourceRuntime(ctx, func(
			callCtx context.Context,
			lease workerapi.RunLeaseAssignment,
		) error {
			request.Lease = lease.Fence()
			var callErr error
			response, callErr = controlPlane.CancelRunSession(callCtx, request)
			return callErr
		})
		if err != nil {
			return fmt.Errorf("cancel actor: %w", err)
		}
		if response.Completed != nil {
			completed = response.Completed
		}
		failed = response.Failed
		if response.CorrelationID != correlationID {
			return errors.New("session cancel response correlation mismatch")
		}
	case *programv0.RunEvent_SessionEventsRequested:
		base, err := workerSessionReferenceRequestFromEvents(value.SessionEventsRequested)
		if err != nil {
			return err
		}
		request := workerapi.ReadSessionEventsRequest{
			SessionReferenceRequest: base,
			Limit:                   int32(value.SessionEventsRequested.GetLimit()),
		}
		if value.SessionEventsRequested.After != nil {
			after := value.SessionEventsRequested.GetAfter()
			request.After = &after
		}
		correlationID = request.CorrelationID
		var response workerapi.ReadSessionEventsResponse
		err = task.callRunSourceRuntime(ctx, func(
			callCtx context.Context,
			lease workerapi.RunLeaseAssignment,
		) error {
			request.Lease = lease.Fence()
			var callErr error
			response, callErr = controlPlane.ReadRunSessionEvents(callCtx, request)
			return callErr
		})
		if err != nil {
			return fmt.Errorf("read session events: %w", err)
		}
		if response.Completed != nil {
			completed = response.Completed
		}
		failed = response.Failed
		if response.CorrelationID != correlationID {
			return errors.New("session events response correlation mismatch")
		}
	default:
		return errors.New("unsupported actor runtime event")
	}
	if (completed == nil) == (failed == nil) {
		return errors.New("actor runtime response must contain exactly one result")
	}
	kind := "completed"
	payload := completed
	if failed != nil {
		if strings.TrimSpace(failed.Code) == "" || strings.TrimSpace(failed.Message) == "" {
			return errors.New("actor runtime failure is invalid")
		}
		kind, payload = "failed", failed
	}
	data, err := json.Marshal(payload)
	if err != nil {
		return fmt.Errorf("encode actor runtime decision: %w", err)
	}
	return wire.WriteResumeDecision(task.programStream(), &programv0.ResumeDecision{
		CorrelationId: correlationID,
		Kind:          kind,
		DataJson:      string(data),
	})
}

func workerActorStartRequest(
	requested *programv0.ActorStartRequested,
) (workerapi.StartActorRequest, error) {
	if requested == nil {
		return workerapi.StartActorRequest{}, errors.New("actor start request is required")
	}
	if err := validateRuntimeActorCorrelation(requested.GetCorrelationId()); err != nil {
		return workerapi.StartActorRequest{}, err
	}
	var run *api.StartActorRunOptions
	if requested.GetRunOptionsJson() != "" {
		var parsed api.StartActorRunOptions
		decoder := json.NewDecoder(strings.NewReader(requested.GetRunOptionsJson()))
		decoder.DisallowUnknownFields()
		if err := decoder.Decode(&parsed); err != nil {
			return workerapi.StartActorRequest{}, errors.New("actor start run options are invalid")
		}
		var trailing any
		if err := decoder.Decode(&trailing); !errors.Is(err, io.EOF) {
			return workerapi.StartActorRequest{}, errors.New("actor start run options contain a trailing value")
		}
		run = &parsed
	}
	request := workerapi.StartActorRequest{
		CorrelationID: requested.GetCorrelationId(), ActorDeclaredID: requested.GetDeclaredId(),
		Key:            requested.Key,
		IdempotencyKey: requested.GetIdempotencyKey(), Run: run,
	}
	request.Workspace.ID = requested.GetWorkspaceId()
	if err := api.ValidateActorDeclaredID(request.ActorDeclaredID); err != nil {
		return workerapi.StartActorRequest{}, err
	}
	if err := api.ValidateActorStartOptions(api.ActorStartOptions{
		Key:       request.Key,
		Workspace: request.Workspace, Run: request.Run,
	}); err != nil {
		return workerapi.StartActorRequest{}, err
	}
	return request, nil
}

func workerSessionReferenceRequest(
	requested *programv0.SessionStatusRequested,
) (workerapi.SessionReferenceRequest, error) {
	if requested == nil {
		return workerapi.SessionReferenceRequest{}, errors.New("session status request is required")
	}
	request := workerapi.SessionReferenceRequest{
		CorrelationID: requested.GetCorrelationId(), SessionID: requested.GetSessionId(),
	}
	return validateWorkerSessionReference(request)
}

func workerSessionReferenceRequestFromClose(
	requested *programv0.SessionCloseRequested,
) (workerapi.SessionReferenceRequest, error) {
	if requested == nil {
		return workerapi.SessionReferenceRequest{}, errors.New("session close request is required")
	}
	request := workerapi.SessionReferenceRequest{
		CorrelationID: requested.GetCorrelationId(), SessionID: requested.GetSessionId(),
	}
	return validateWorkerSessionReference(request)
}

func workerSessionReferenceRequestFromCancel(
	requested *programv0.SessionCancelRequested,
) (workerapi.SessionReferenceRequest, error) {
	if requested == nil {
		return workerapi.SessionReferenceRequest{}, errors.New("session cancel request is required")
	}
	request := workerapi.SessionReferenceRequest{
		CorrelationID: requested.GetCorrelationId(), SessionID: requested.GetSessionId(),
	}
	return validateWorkerSessionReference(request)
}

func workerSessionReferenceRequestFromEvents(
	requested *programv0.SessionEventsRequested,
) (workerapi.SessionReferenceRequest, error) {
	if requested == nil {
		return workerapi.SessionReferenceRequest{}, errors.New("session events request is required")
	}
	if requested.GetLimit() < 1 || requested.GetLimit() > 1000 ||
		(requested.After != nil &&
			(requested.GetAfter() < 0 || requested.GetAfter() > maxJavaScriptSafeInteger)) {
		return workerapi.SessionReferenceRequest{}, errors.New("session events bounds are invalid")
	}
	request := workerapi.SessionReferenceRequest{
		CorrelationID: requested.GetCorrelationId(), SessionID: requested.GetSessionId(),
	}
	return validateWorkerSessionReference(request)
}

func validateWorkerSessionReference(
	request workerapi.SessionReferenceRequest,
) (workerapi.SessionReferenceRequest, error) {
	if err := validateRuntimeActorCorrelation(request.CorrelationID); err != nil {
		return workerapi.SessionReferenceRequest{}, err
	}
	if err := api.ValidateSessionID(request.SessionID); err != nil {
		return workerapi.SessionReferenceRequest{}, err
	}
	return request, nil
}

func validateRuntimeActorCorrelation(value string) error {
	if err := ids.Validate(value); err != nil {
		return errors.New("actor runtime correlation ID is invalid")
	}
	return nil
}
