package executor

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	"math"
	"strings"
	"time"

	"github.com/helmrdotdev/helmr/internal/api"
	"github.com/helmrdotdev/helmr/internal/ids"
	programv0 "github.com/helmrdotdev/helmr/internal/proto/program/v0"
	"github.com/helmrdotdev/helmr/internal/wire"
	"github.com/helmrdotdev/helmr/internal/workerapi"
)

func (task *guestRunLeaseTask) handleWorkspaceRuntime(
	ctx context.Context,
	event *programv0.RunEvent,
) error {
	controlPlane, ok := task.controlPlane.(WorkspaceRuntimeControlPlane)
	if !ok {
		return errors.New("run lease task workspace runtime control plane is required")
	}
	var correlationID string
	var completed any
	var failed *workerapi.RuntimeOperationFailure
	switch value := event.Event.(type) {
	case *programv0.RunEvent_WorkspaceCreateRequested:
		request, err := workerWorkspaceCreateRequest(value.WorkspaceCreateRequested)
		if err != nil {
			return err
		}
		correlationID = request.CorrelationID
		var response workerapi.CreateWorkspaceResponse
		if err := task.callRunSourceRuntime(ctx, func(
			callCtx context.Context,
			lease workerapi.RunLeaseAssignment,
		) error {
			request.Lease = lease.Fence()
			var callErr error
			response, callErr = controlPlane.CreateRunWorkspace(callCtx, request)
			return callErr
		}); err != nil {
			return fmt.Errorf("create workspace: %w", err)
		}
		if response.CorrelationID != correlationID {
			return errors.New("workspace create response correlation mismatch")
		}
		if response.Completed != nil {
			completed = response.Completed
		}
		failed = response.Failed
	case *programv0.RunEvent_WorkspaceRetrieveRequested:
		request, err := workerWorkspaceRetrieveRequest(
			value.WorkspaceRetrieveRequested.GetCorrelationId(),
			value.WorkspaceRetrieveRequested.GetWorkspace(),
		)
		if err != nil {
			return err
		}
		correlationID = request.CorrelationID
		var response workerapi.RetrieveWorkspaceResponse
		if err := task.callRunSourceRuntime(ctx, func(
			callCtx context.Context,
			lease workerapi.RunLeaseAssignment,
		) error {
			request.Lease = lease.Fence()
			var callErr error
			response, callErr = controlPlane.RetrieveRunWorkspace(callCtx, request)
			return callErr
		}); err != nil {
			return fmt.Errorf("retrieve workspace: %w", err)
		}
		if response.CorrelationID != correlationID {
			return errors.New("workspace retrieve response correlation mismatch")
		}
		if response.Completed != nil {
			completed = response.Completed
		}
		failed = response.Failed
	case *programv0.RunEvent_WorkspaceExecRequested:
		request, err := workerWorkspaceExecRequest(value.WorkspaceExecRequested)
		if err != nil {
			return err
		}
		correlationID = request.CorrelationID
		response, err := task.executeWorkspaceRuntime(ctx, controlPlane, request)
		if err != nil {
			return err
		}
		if response.Completed != nil {
			completed = response.Completed
		}
		failed = response.Failed
	case *programv0.RunEvent_WorkspaceDeleteRequested:
		base, err := workerWorkspaceRetrieveRequest(
			value.WorkspaceDeleteRequested.GetCorrelationId(),
			value.WorkspaceDeleteRequested.GetWorkspace(),
		)
		if err != nil {
			return err
		}
		request := workerapi.DeleteWorkspaceRequest{
			RetrieveWorkspaceRequest: base,
			IdempotencyKey:           value.WorkspaceDeleteRequested.GetIdempotencyKey(),
		}
		correlationID = request.CorrelationID
		var response workerapi.DeleteWorkspaceResponse
		if err := task.callRunSourceRuntime(ctx, func(
			callCtx context.Context,
			lease workerapi.RunLeaseAssignment,
		) error {
			request.Lease = lease.Fence()
			var callErr error
			response, callErr = controlPlane.DeleteRunWorkspace(callCtx, request)
			return callErr
		}); err != nil {
			return fmt.Errorf("delete workspace: %w", err)
		}
		if response.CorrelationID != correlationID {
			return errors.New("workspace delete response correlation mismatch")
		}
		if response.Completed != nil {
			completed = response.Completed
		}
		failed = response.Failed
	default:
		return errors.New("unsupported workspace runtime event")
	}
	if (completed == nil) == (failed == nil) {
		return errors.New("workspace runtime response must contain exactly one result")
	}
	kind := "completed"
	payload := completed
	if failed != nil {
		if strings.TrimSpace(failed.Code) == "" || strings.TrimSpace(failed.Message) == "" {
			return errors.New("workspace runtime failure is invalid")
		}
		kind, payload = "failed", failed
	}
	data, err := json.Marshal(payload)
	if err != nil {
		return fmt.Errorf("encode workspace runtime decision: %w", err)
	}
	return wire.WriteResumeDecision(task.program.session.Stream(), &programv0.ResumeDecision{
		CorrelationId: correlationID,
		Kind:          kind,
		DataJson:      string(data),
	})
}

func (task *guestRunLeaseTask) executeWorkspaceRuntime(
	ctx context.Context,
	controlPlane WorkspaceRuntimeControlPlane,
	request workerapi.ExecuteWorkspaceRequest,
) (workerapi.ExecuteWorkspaceResponse, error) {
	var response workerapi.ExecuteWorkspaceResponse
	if err := task.callRunSourceRuntime(ctx, func(
		callCtx context.Context,
		lease workerapi.RunLeaseAssignment,
	) error {
		request.Lease = lease.Fence()
		var callErr error
		response, callErr = controlPlane.ExecuteRunWorkspace(callCtx, request)
		return callErr
	}); err != nil {
		return workerapi.ExecuteWorkspaceResponse{}, fmt.Errorf("execute workspace: %w", err)
	}
	if response.CorrelationID != request.CorrelationID {
		return workerapi.ExecuteWorkspaceResponse{}, errors.New("workspace exec response correlation mismatch")
	}
	for response.Pending != nil {
		if response.Completed != nil || response.Failed != nil ||
			validateRuntimeWorkspaceProcessID(response.Pending.ProcessID) != nil {
			return workerapi.ExecuteWorkspaceResponse{}, errors.New("workspace exec pending response is invalid")
		}
		timer := time.NewTimer(250 * time.Millisecond)
		select {
		case <-ctx.Done():
			timer.Stop()
			return workerapi.ExecuteWorkspaceResponse{}, ctx.Err()
		case <-timer.C:
		}
		poll := workerapi.PollWorkspaceExecRequest{
			RetrieveWorkspaceRequest: request.RetrieveWorkspaceRequest,
			ProcessID:                response.Pending.ProcessID,
		}
		response = workerapi.ExecuteWorkspaceResponse{}
		if err := task.callRunSourceRuntime(ctx, func(
			callCtx context.Context,
			lease workerapi.RunLeaseAssignment,
		) error {
			poll.Lease = lease.Fence()
			var callErr error
			response, callErr = controlPlane.PollRunWorkspaceExec(callCtx, poll)
			return callErr
		}); err != nil {
			return workerapi.ExecuteWorkspaceResponse{}, fmt.Errorf("poll workspace exec: %w", err)
		}
		if response.CorrelationID != request.CorrelationID {
			return workerapi.ExecuteWorkspaceResponse{}, errors.New("workspace exec poll response correlation mismatch")
		}
	}
	if (response.Completed == nil) == (response.Failed == nil) {
		return workerapi.ExecuteWorkspaceResponse{}, errors.New("workspace exec response must contain exactly one terminal result")
	}
	return response, nil
}

func workerWorkspaceCreateRequest(
	requested *programv0.WorkspaceCreateRequested,
) (workerapi.CreateWorkspaceRequest, error) {
	if requested == nil {
		return workerapi.CreateWorkspaceRequest{}, errors.New("workspace create request is required")
	}
	if err := validateRuntimeWorkspaceCorrelation(requested.GetCorrelationId()); err != nil {
		return workerapi.CreateWorkspaceRequest{}, err
	}
	if err := api.ValidateSandboxDeclaredID(requested.GetDeclaredId()); err != nil {
		return workerapi.CreateWorkspaceRequest{}, err
	}
	secrets := make([]api.WorkspaceSecret, 0, len(requested.GetSecrets()))
	for _, placement := range requested.GetSecrets() {
		if placement == nil {
			return workerapi.CreateWorkspaceRequest{}, errors.New("workspace secret placement is required")
		}
		secret := api.WorkspaceSecret{Name: placement.GetName()}
		switch value := placement.GetPlacement().(type) {
		case *programv0.WorkspaceSecretPlacement_Env:
			secret.Env = value.Env
		case *programv0.WorkspaceSecretPlacement_File:
			secret.File = value.File
		default:
			return workerapi.CreateWorkspaceRequest{}, errors.New("workspace secret target is required")
		}
		if err := api.ValidateWorkspaceSecret(secret); err != nil {
			return workerapi.CreateWorkspaceRequest{}, err
		}
		secrets = append(secrets, secret)
	}
	return workerapi.CreateWorkspaceRequest{
		CorrelationID: requested.GetCorrelationId(), SandboxDeclaredID: requested.GetDeclaredId(),
		Key: requested.Key, Secrets: secrets, IdempotencyKey: requested.GetIdempotencyKey(),
	}, nil
}

func workerWorkspaceRetrieveRequest(
	correlationID string,
	address *programv0.WorkspaceAddress,
) (workerapi.RetrieveWorkspaceRequest, error) {
	if err := validateRuntimeWorkspaceCorrelation(correlationID); err != nil {
		return workerapi.RetrieveWorkspaceRequest{}, err
	}
	if address == nil {
		return workerapi.RetrieveWorkspaceRequest{}, errors.New("workspace address is required")
	}
	if err := api.ValidateWorkspaceID(address.GetWorkspaceId()); err != nil {
		return workerapi.RetrieveWorkspaceRequest{}, err
	}
	return workerapi.RetrieveWorkspaceRequest{
		CorrelationID: correlationID,
		Workspace:     workerapi.WorkspaceAddress{WorkspaceID: address.GetWorkspaceId()},
	}, nil
}

func workerWorkspaceExecRequest(
	requested *programv0.WorkspaceExecRequested,
) (workerapi.ExecuteWorkspaceRequest, error) {
	if requested == nil {
		return workerapi.ExecuteWorkspaceRequest{}, errors.New("workspace exec request is required")
	}
	base, err := workerWorkspaceRetrieveRequest(requested.GetCorrelationId(), requested.GetWorkspace())
	if err != nil {
		return workerapi.ExecuteWorkspaceRequest{}, err
	}
	request := workerapi.ExecuteWorkspaceRequest{
		RetrieveWorkspaceRequest: base,
		Command:                  append([]string{}, requested.GetCommand()...),
		Cwd:                      requested.GetCwd(),
		Env:                      make(map[string]string, len(requested.GetEnv())),
		Stdin:                    append([]byte{}, requested.GetStdin()...),
		IdempotencyKey:           requested.GetIdempotencyKey(),
	}
	maps.Copy(request.Env, requested.GetEnv())
	if requested.TimeoutMs != nil {
		if requested.GetTimeoutMs() > math.MaxInt64 {
			return workerapi.ExecuteWorkspaceRequest{}, errors.New("workspace exec timeout is invalid")
		}
		timeout := int64(requested.GetTimeoutMs())
		request.TimeoutMS = &timeout
	}
	return request, nil
}

func validateRuntimeWorkspaceCorrelation(value string) error {
	if err := ids.Validate(value); err != nil {
		return errors.New("workspace runtime correlation ID is invalid")
	}
	return nil
}

func validateRuntimeWorkspaceProcessID(value string) error {
	if err := ids.Validate(value); err != nil {
		return errors.New("workspace exec process ID is invalid")
	}
	return nil
}
