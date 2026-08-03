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
		*runv0.RunEvent_ActorStatusRequested,
		*runv0.RunEvent_ActorCloseRequested,
		*runv0.RunEvent_ActorOutputPageRequested:
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
		return errors.New("Run Lease Task Actor runtime Control Plane is required")
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
			return fmt.Errorf("start Actor: %w", err)
		}
		if response.Completed != nil {
			completed = response.Completed
		}
		failed = response.Failed
		if response.CorrelationID != correlationID {
			return errors.New("Actor start response correlation mismatch")
		}
	case *runv0.RunEvent_ActorStatusRequested:
		request, err := workerActorReferenceRequest(value.ActorStatusRequested)
		if err != nil {
			return err
		}
		correlationID = request.CorrelationID
		var response workerapi.ActorStatusResponse
		err = task.callRunSourceRuntime(ctx, func(
			callCtx context.Context,
			lease workerapi.RunLeaseAssignment,
		) error {
			request.Lease = lease.Fence()
			var callErr error
			response, callErr = controlPlane.GetRunActorStatus(callCtx, request)
			return callErr
		})
		if err != nil {
			return fmt.Errorf("read Actor status: %w", err)
		}
		if response.Completed != nil {
			completed = response.Completed
		}
		failed = response.Failed
		if response.CorrelationID != correlationID {
			return errors.New("Actor status response correlation mismatch")
		}
	case *runv0.RunEvent_ActorCloseRequested:
		base, err := workerActorReferenceRequestFromClose(value.ActorCloseRequested)
		if err != nil {
			return err
		}
		request := workerapi.CloseActorRequest{
			ActorReferenceRequest: base,
			IdempotencyKey:        value.ActorCloseRequested.GetIdempotencyKey(),
		}
		correlationID = request.CorrelationID
		var response workerapi.CloseActorResponse
		err = task.callRunSourceRuntime(ctx, func(
			callCtx context.Context,
			lease workerapi.RunLeaseAssignment,
		) error {
			request.Lease = lease.Fence()
			var callErr error
			response, callErr = controlPlane.CloseRunActor(callCtx, request)
			return callErr
		})
		if err != nil {
			return fmt.Errorf("close Actor: %w", err)
		}
		if response.Completed != nil {
			completed = response.Completed
		}
		failed = response.Failed
		if response.CorrelationID != correlationID {
			return errors.New("Actor close response correlation mismatch")
		}
	case *runv0.RunEvent_ActorOutputPageRequested:
		base, err := workerActorReferenceRequestFromOutput(value.ActorOutputPageRequested)
		if err != nil {
			return err
		}
		request := workerapi.ReadActorOutputPageRequest{
			ActorReferenceRequest: base,
			Limit:                 int32(value.ActorOutputPageRequested.GetLimit()),
		}
		if value.ActorOutputPageRequested.After != nil {
			after := value.ActorOutputPageRequested.GetAfter()
			request.After = &after
		}
		correlationID = request.CorrelationID
		var response workerapi.ReadActorOutputPageResponse
		err = task.callRunSourceRuntime(ctx, func(
			callCtx context.Context,
			lease workerapi.RunLeaseAssignment,
		) error {
			request.Lease = lease.Fence()
			var callErr error
			response, callErr = controlPlane.ReadRunActorOutputPage(callCtx, request)
			return callErr
		})
		if err != nil {
			return fmt.Errorf("read Actor output page: %w", err)
		}
		if response.Completed != nil {
			completed = response.Completed
		}
		failed = response.Failed
		if response.CorrelationID != correlationID {
			return errors.New("Actor output page response correlation mismatch")
		}
	default:
		return errors.New("unsupported Actor runtime event")
	}
	if (completed == nil) == (failed == nil) {
		return errors.New("Actor runtime response must contain exactly one result")
	}
	kind := "completed"
	payload := completed
	if failed != nil {
		if strings.TrimSpace(failed.Code) == "" || strings.TrimSpace(failed.Message) == "" {
			return errors.New("Actor runtime failure is invalid")
		}
		kind, payload = "failed", failed
	}
	data, err := json.Marshal(payload)
	if err != nil {
		return fmt.Errorf("encode Actor runtime decision: %w", err)
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
		return workerapi.StartActorRequest{}, errors.New("Actor start request is required")
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
			return workerapi.StartActorRequest{}, errors.New("Actor start run options are invalid")
		}
		var trailing any
		if err := decoder.Decode(&trailing); !errors.Is(err, io.EOF) {
			return workerapi.StartActorRequest{}, errors.New("Actor start run options contain a trailing value")
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
	switch address := requested.GetWorkspace().(type) {
	case *runv0.ActorStartRequested_WorkspaceId:
		request.Workspace.ID = &address.WorkspaceId
	case *runv0.ActorStartRequested_WorkspaceKey:
		request.Workspace.Key = &address.WorkspaceKey
	default:
		return workerapi.StartActorRequest{}, errors.New("Actor start Workspace address is required")
	}
	if err := api.ValidateActorDeclaredID(request.ActorDeclaredID); err != nil {
		return workerapi.StartActorRequest{}, err
	}
	if err := api.ValidateStartActorRequest(api.StartActorRequest{
		Key: request.Key, Input: request.Input, IdempotencyKey: request.IdempotencyKey,
		Workspace: request.Workspace, Run: request.Run,
	}); err != nil {
		return workerapi.StartActorRequest{}, err
	}
	return request, nil
}

func workerActorReferenceRequest(
	requested *runv0.ActorStatusRequested,
) (workerapi.ActorReferenceRequest, error) {
	if requested == nil {
		return workerapi.ActorReferenceRequest{}, errors.New("Actor status request is required")
	}
	request := workerapi.ActorReferenceRequest{
		CorrelationID: requested.GetCorrelationId(), ActorDeclaredID: requested.GetDeclaredId(),
	}
	switch address := requested.GetAddress().(type) {
	case *runv0.ActorStatusRequested_ActorId:
		request.ActorID = address.ActorId
	case *runv0.ActorStatusRequested_ActorKey:
		request.ActorKey = address.ActorKey
	default:
		return workerapi.ActorReferenceRequest{}, errors.New("Actor address is required")
	}
	return validateWorkerActorReference(request)
}

func workerActorReferenceRequestFromClose(
	requested *runv0.ActorCloseRequested,
) (workerapi.ActorReferenceRequest, error) {
	if requested == nil {
		return workerapi.ActorReferenceRequest{}, errors.New("Actor close request is required")
	}
	request := workerapi.ActorReferenceRequest{
		CorrelationID: requested.GetCorrelationId(), ActorDeclaredID: requested.GetDeclaredId(),
	}
	switch address := requested.GetAddress().(type) {
	case *runv0.ActorCloseRequested_ActorId:
		request.ActorID = address.ActorId
	case *runv0.ActorCloseRequested_ActorKey:
		request.ActorKey = address.ActorKey
	default:
		return workerapi.ActorReferenceRequest{}, errors.New("Actor address is required")
	}
	return validateWorkerActorReference(request)
}

func workerActorReferenceRequestFromOutput(
	requested *runv0.ActorOutputPageRequested,
) (workerapi.ActorReferenceRequest, error) {
	if requested == nil {
		return workerapi.ActorReferenceRequest{}, errors.New("Actor output page request is required")
	}
	if requested.GetLimit() < 1 || requested.GetLimit() > 100 ||
		(requested.After != nil &&
			(requested.GetAfter() < 0 || requested.GetAfter() > maxJavaScriptSafeInteger)) {
		return workerapi.ActorReferenceRequest{}, errors.New("Actor output page bounds are invalid")
	}
	request := workerapi.ActorReferenceRequest{
		CorrelationID: requested.GetCorrelationId(), ActorDeclaredID: requested.GetDeclaredId(),
	}
	switch address := requested.GetAddress().(type) {
	case *runv0.ActorOutputPageRequested_ActorId:
		request.ActorID = address.ActorId
	case *runv0.ActorOutputPageRequested_ActorKey:
		request.ActorKey = address.ActorKey
	default:
		return workerapi.ActorReferenceRequest{}, errors.New("Actor address is required")
	}
	return validateWorkerActorReference(request)
}

func validateWorkerActorReference(
	request workerapi.ActorReferenceRequest,
) (workerapi.ActorReferenceRequest, error) {
	if err := validateRuntimeActorCorrelation(request.CorrelationID); err != nil {
		return workerapi.ActorReferenceRequest{}, err
	}
	if err := api.ValidateActorDeclaredID(request.ActorDeclaredID); err != nil {
		return workerapi.ActorReferenceRequest{}, err
	}
	if err := api.ValidateActorReference(api.ActorReference{
		ActorID: request.ActorID, ActorKey: request.ActorKey,
	}); err != nil {
		return workerapi.ActorReferenceRequest{}, err
	}
	return request, nil
}

func validateRuntimeActorCorrelation(value string) error {
	if err := ids.Validate(value); err != nil {
		return errors.New("Actor runtime correlation ID is invalid")
	}
	return nil
}
