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
	runv0 "github.com/helmrdotdev/helmr/internal/proto/run/v0"
	"github.com/helmrdotdev/helmr/internal/wire"
	"github.com/helmrdotdev/helmr/internal/workerapi"
)

func (task *guestRunLeaseTask) handleWorkspaceRuntime(
	ctx context.Context,
	event *runv0.RunEvent,
) error {
	controlPlane, ok := task.controlPlane.(WorkspaceRuntimeControlPlane)
	if !ok {
		return errors.New("Run Lease Task Workspace runtime Control Plane is required")
	}
	var correlationID string
	var completed any
	var failed *workerapi.RuntimeOperationFailure
	switch value := event.Event.(type) {
	case *runv0.RunEvent_WorkspaceCreateRequested:
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
			return fmt.Errorf("create Workspace: %w", err)
		}
		if response.CorrelationID != correlationID {
			return errors.New("Workspace create response correlation mismatch")
		}
		if response.Completed != nil {
			completed = response.Completed
		}
		failed = response.Failed
	case *runv0.RunEvent_WorkspaceRetrieveRequested:
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
			return fmt.Errorf("retrieve Workspace: %w", err)
		}
		if response.CorrelationID != correlationID {
			return errors.New("Workspace retrieve response correlation mismatch")
		}
		if response.Completed != nil {
			completed = response.Completed
		}
		failed = response.Failed
	case *runv0.RunEvent_WorkspaceFileReadRequested:
		base, err := workerWorkspaceRetrieveRequest(
			value.WorkspaceFileReadRequested.GetCorrelationId(),
			value.WorkspaceFileReadRequested.GetWorkspace(),
		)
		if err != nil {
			return err
		}
		request := workerapi.ReadWorkspaceFileRequest{
			RetrieveWorkspaceRequest: base,
			Path:                     value.WorkspaceFileReadRequested.GetPath(),
		}
		correlationID = request.CorrelationID
		var response workerapi.ReadWorkspaceFileResponse
		if err := task.callRunSourceRuntime(ctx, func(
			callCtx context.Context,
			lease workerapi.RunLeaseAssignment,
		) error {
			request.Lease = lease.Fence()
			var callErr error
			response, callErr = controlPlane.ReadRunWorkspaceFile(callCtx, request)
			return callErr
		}); err != nil {
			return fmt.Errorf("read Workspace file: %w", err)
		}
		if response.CorrelationID != correlationID {
			return errors.New("Workspace file read response correlation mismatch")
		}
		if response.Completed != nil {
			completed = response.Completed
		}
		failed = response.Failed
	case *runv0.RunEvent_WorkspaceFileStatRequested:
		base, err := workerWorkspaceRetrieveRequest(
			value.WorkspaceFileStatRequested.GetCorrelationId(),
			value.WorkspaceFileStatRequested.GetWorkspace(),
		)
		if err != nil {
			return err
		}
		request := workerapi.ReadWorkspaceFileRequest{
			RetrieveWorkspaceRequest: base,
			Path:                     value.WorkspaceFileStatRequested.GetPath(),
		}
		correlationID = request.CorrelationID
		var response workerapi.StatWorkspaceFileResponse
		if err := task.callRunSourceRuntime(ctx, func(
			callCtx context.Context,
			lease workerapi.RunLeaseAssignment,
		) error {
			request.Lease = lease.Fence()
			var callErr error
			response, callErr = controlPlane.StatRunWorkspaceFile(callCtx, request)
			return callErr
		}); err != nil {
			return fmt.Errorf("stat Workspace file: %w", err)
		}
		if response.CorrelationID != correlationID {
			return errors.New("Workspace file stat response correlation mismatch")
		}
		if response.Completed != nil {
			completed = response.Completed
		}
		failed = response.Failed
	case *runv0.RunEvent_WorkspaceFileListRequested:
		base, err := workerWorkspaceRetrieveRequest(
			value.WorkspaceFileListRequested.GetCorrelationId(),
			value.WorkspaceFileListRequested.GetWorkspace(),
		)
		if err != nil {
			return err
		}
		if value.WorkspaceFileListRequested.GetLimit() > math.MaxInt32 {
			return errors.New("Workspace file list limit is invalid")
		}
		request := workerapi.ListWorkspaceFilesRequest{
			ReadWorkspaceFileRequest: workerapi.ReadWorkspaceFileRequest{
				RetrieveWorkspaceRequest: base,
				Path:                     value.WorkspaceFileListRequested.GetPath(),
			},
			Cursor: value.WorkspaceFileListRequested.GetCursor(),
			Limit:  int32(value.WorkspaceFileListRequested.GetLimit()),
		}
		correlationID = request.CorrelationID
		var response workerapi.ListWorkspaceFilesResponse
		if err := task.callRunSourceRuntime(ctx, func(
			callCtx context.Context,
			lease workerapi.RunLeaseAssignment,
		) error {
			request.Lease = lease.Fence()
			var callErr error
			response, callErr = controlPlane.ListRunWorkspaceFiles(callCtx, request)
			return callErr
		}); err != nil {
			return fmt.Errorf("list Workspace files: %w", err)
		}
		if response.CorrelationID != correlationID {
			return errors.New("Workspace file list response correlation mismatch")
		}
		if response.Completed != nil {
			completed = response.Completed
		}
		failed = response.Failed
	case *runv0.RunEvent_WorkspaceExecRequested:
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
	case *runv0.RunEvent_WorkspaceDeleteRequested:
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
			return fmt.Errorf("delete Workspace: %w", err)
		}
		if response.CorrelationID != correlationID {
			return errors.New("Workspace delete response correlation mismatch")
		}
		if response.Completed != nil {
			completed = response.Completed
		}
		failed = response.Failed
	default:
		return errors.New("unsupported Workspace runtime event")
	}
	if (completed == nil) == (failed == nil) {
		return errors.New("Workspace runtime response must contain exactly one result")
	}
	kind := "completed"
	payload := completed
	if failed != nil {
		if strings.TrimSpace(failed.Code) == "" || strings.TrimSpace(failed.Message) == "" {
			return errors.New("Workspace runtime failure is invalid")
		}
		kind, payload = "failed", failed
	}
	data, err := json.Marshal(payload)
	if err != nil {
		return fmt.Errorf("encode Workspace runtime decision: %w", err)
	}
	return wire.WriteResumeDecision(task.program.session.Stream(), &runv0.ResumeDecision{
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
		return workerapi.ExecuteWorkspaceResponse{}, fmt.Errorf("execute Workspace: %w", err)
	}
	if response.CorrelationID != request.CorrelationID {
		return workerapi.ExecuteWorkspaceResponse{}, errors.New("Workspace exec response correlation mismatch")
	}
	for response.Pending != nil {
		if response.Completed != nil || response.Failed != nil ||
			validateRuntimeWorkspaceProcessID(response.Pending.ProcessID) != nil {
			return workerapi.ExecuteWorkspaceResponse{}, errors.New("Workspace exec pending response is invalid")
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
			return workerapi.ExecuteWorkspaceResponse{}, fmt.Errorf("poll Workspace exec: %w", err)
		}
		if response.CorrelationID != request.CorrelationID {
			return workerapi.ExecuteWorkspaceResponse{}, errors.New("Workspace exec poll response correlation mismatch")
		}
	}
	if (response.Completed == nil) == (response.Failed == nil) {
		return workerapi.ExecuteWorkspaceResponse{}, errors.New("Workspace exec response must contain exactly one terminal result")
	}
	return response, nil
}

func workerWorkspaceCreateRequest(
	requested *runv0.WorkspaceCreateRequested,
) (workerapi.CreateWorkspaceRequest, error) {
	if requested == nil {
		return workerapi.CreateWorkspaceRequest{}, errors.New("Workspace create request is required")
	}
	if err := validateRuntimeWorkspaceCorrelation(requested.GetCorrelationId()); err != nil {
		return workerapi.CreateWorkspaceRequest{}, err
	}
	if err := api.ValidateWorkspaceDeclaredID(requested.GetDeclaredId()); err != nil {
		return workerapi.CreateWorkspaceRequest{}, err
	}
	secrets := make([]api.WorkspaceSecret, 0, len(requested.GetSecrets()))
	for _, placement := range requested.GetSecrets() {
		if placement == nil {
			return workerapi.CreateWorkspaceRequest{}, errors.New("Workspace Secret placement is required")
		}
		secret := api.WorkspaceSecret{Name: placement.GetName()}
		switch value := placement.GetPlacement().(type) {
		case *runv0.WorkspaceSecretPlacement_Env:
			secret.Env = value.Env
		case *runv0.WorkspaceSecretPlacement_File:
			secret.File = value.File
		default:
			return workerapi.CreateWorkspaceRequest{}, errors.New("Workspace Secret target is required")
		}
		if err := api.ValidateWorkspaceSecret(secret); err != nil {
			return workerapi.CreateWorkspaceRequest{}, err
		}
		secrets = append(secrets, secret)
	}
	return workerapi.CreateWorkspaceRequest{
		CorrelationID: requested.GetCorrelationId(), WorkspaceDeclaredID: requested.GetDeclaredId(),
		Key: requested.Key, Secrets: secrets, IdempotencyKey: requested.GetIdempotencyKey(),
	}, nil
}

func workerWorkspaceRetrieveRequest(
	correlationID string,
	address *runv0.WorkspaceAddress,
) (workerapi.RetrieveWorkspaceRequest, error) {
	if err := validateRuntimeWorkspaceCorrelation(correlationID); err != nil {
		return workerapi.RetrieveWorkspaceRequest{}, err
	}
	if address == nil {
		return workerapi.RetrieveWorkspaceRequest{}, errors.New("Workspace address is required")
	}
	request := workerapi.RetrieveWorkspaceRequest{CorrelationID: correlationID}
	switch value := address.GetAddress().(type) {
	case *runv0.WorkspaceAddress_WorkspaceId:
		request.Workspace.WorkspaceID = value.WorkspaceId
	case *runv0.WorkspaceAddress_WorkspaceKey:
		request.Workspace.WorkspaceKey = value.WorkspaceKey
	default:
		return workerapi.RetrieveWorkspaceRequest{}, errors.New("Workspace address is required")
	}
	return request, nil
}

func workerWorkspaceExecRequest(
	requested *runv0.WorkspaceExecRequested,
) (workerapi.ExecuteWorkspaceRequest, error) {
	if requested == nil {
		return workerapi.ExecuteWorkspaceRequest{}, errors.New("Workspace exec request is required")
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
			return workerapi.ExecuteWorkspaceRequest{}, errors.New("Workspace exec timeout is invalid")
		}
		timeout := int64(requested.GetTimeoutMs())
		request.TimeoutMS = &timeout
	}
	return request, nil
}

func validateRuntimeWorkspaceCorrelation(value string) error {
	if err := ids.Validate(value); err != nil {
		return errors.New("Workspace runtime correlation ID is invalid")
	}
	return nil
}

func validateRuntimeWorkspaceProcessID(value string) error {
	if err := ids.Validate(value); err != nil {
		return errors.New("Workspace exec process ID is invalid")
	}
	return nil
}
