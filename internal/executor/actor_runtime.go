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
	runv0 "github.com/helmrdotdev/helmr/internal/proto/run/v0"
	"github.com/helmrdotdev/helmr/internal/wire"
	"github.com/helmrdotdev/helmr/internal/workerapi"
)

func (task *guestRunLeaseTask) handleResourceRuntime(
	ctx context.Context,
	event *runv0.RunEvent,
) error {
	switch event.GetEvent().(type) {
	case *runv0.RunEvent_ActorStartRequested,
		*runv0.RunEvent_SessionStatusRequested,
		*runv0.RunEvent_SessionCloseRequested,
		*runv0.RunEvent_SessionOutputPageRequested:
		return task.handleActorRuntime(ctx, event)
	default:
		return task.handleWorkspaceRuntime(ctx, event)
	}
}

func (task *guestRunLeaseTask) handleActorRuntime(
	ctx context.Context,
	event *runv0.RunEvent,
) error {
	controlPlane, ok := task.controlPlane.(ActorRuntimeControlPlane)
	if !ok {
		return errors.New("run lease task actor runtime control plane is required")
	}

	var correlationID string
	var completed any
	var failed *workerapi.RuntimeOperationFailure
	switch value := event.Event.(type) {
	case *runv0.RunEvent_ActorStartRequested:
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
	case *runv0.RunEvent_SessionStatusRequested:
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
	case *runv0.RunEvent_SessionCloseRequested:
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
	case *runv0.RunEvent_SessionOutputPageRequested:
		base, err := workerSessionReferenceRequestFromOutput(value.SessionOutputPageRequested)
		if err != nil {
			return err
		}
		request := workerapi.ReadSessionOutputPageRequest{
			SessionReferenceRequest: base,
			Limit:                   int32(value.SessionOutputPageRequested.GetLimit()),
		}
		if value.SessionOutputPageRequested.After != nil {
			after := value.SessionOutputPageRequested.GetAfter()
			request.After = &after
		}
		correlationID = request.CorrelationID
		var response workerapi.ReadSessionOutputPageResponse
		err = task.callRunSourceRuntime(ctx, func(
			callCtx context.Context,
			lease workerapi.RunLeaseAssignment,
		) error {
			request.Lease = lease.Fence()
			var callErr error
			response, callErr = controlPlane.ReadRunSessionOutputPage(callCtx, request)
			return callErr
		})
		if err != nil {
			return fmt.Errorf("read session output page: %w", err)
		}
		if response.Completed != nil {
			completed = response.Completed
		}
		failed = response.Failed
		if response.CorrelationID != correlationID {
			return errors.New("session output page response correlation mismatch")
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
	return wire.WriteResumeDecision(task.program.session.Stream(), &runv0.ResumeDecision{
		CorrelationId: correlationID,
		Kind:          kind,
		DataJson:      string(data),
	})
}

func workerActorStartRequest(
	requested *runv0.ActorStartRequested,
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
		Key: requested.Key, InputPresent: requested.InputJson != nil,
		IdempotencyKey: requested.GetIdempotencyKey(), Run: run,
	}
	if requested.InputJson != nil {
		request.Input = json.RawMessage(requested.GetInputJson())
	}
	request.Workspace.ID = requested.GetWorkspaceId()
	if err := api.ValidateActorDeclaredID(request.ActorDeclaredID); err != nil {
		return workerapi.StartActorRequest{}, err
	}
	if err := api.ValidateActorStartOptions(api.ActorStartOptions{
		Key: request.Key, Input: request.Input,
		Workspace: request.Workspace, Run: request.Run,
	}); err != nil {
		return workerapi.StartActorRequest{}, err
	}
	return request, nil
}

func workerSessionReferenceRequest(
	requested *runv0.SessionStatusRequested,
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
	requested *runv0.SessionCloseRequested,
) (workerapi.SessionReferenceRequest, error) {
	if requested == nil {
		return workerapi.SessionReferenceRequest{}, errors.New("session close request is required")
	}
	request := workerapi.SessionReferenceRequest{
		CorrelationID: requested.GetCorrelationId(), SessionID: requested.GetSessionId(),
	}
	return validateWorkerSessionReference(request)
}

func workerSessionReferenceRequestFromOutput(
	requested *runv0.SessionOutputPageRequested,
) (workerapi.SessionReferenceRequest, error) {
	if requested == nil {
		return workerapi.SessionReferenceRequest{}, errors.New("session output page request is required")
	}
	if requested.GetLimit() < 1 || requested.GetLimit() > 100 ||
		(requested.After != nil &&
			(requested.GetAfter() < 0 || requested.GetAfter() > maxJavaScriptSafeInteger)) {
		return workerapi.SessionReferenceRequest{}, errors.New("session output page bounds are invalid")
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
