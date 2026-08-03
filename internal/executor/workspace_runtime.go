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
)

func (task *guestRunLeaseTask) handleWorkspaceRuntime(
	ctx context.Context,
	event *runv0.RunEvent,
) error {
	control, ok := task.control.(WorkspaceRuntimeControl)
	if !ok {
		return errors.New("Run Lease Task Workspace runtime control is required")
	}
	var correlationID string
	var completed any
	var failed *api.WorkerRuntimeOperationFailure
	switch value := event.Event.(type) {
	case *runv0.RunEvent_WorkspaceCreateRequested:
		request, err := workerWorkspaceCreateRequest(value.WorkspaceCreateRequested)
		if err != nil {
			return err
		}
		correlationID = request.CorrelationID
		var response api.WorkerCreateWorkspaceResponse
		if err := task.callRunSourceRuntime(ctx, func(
			callCtx context.Context,
			lease api.WorkerRunLeaseAssignment,
		) error {
			request.Lease = lease.Fence()
			var callErr error
			response, callErr = control.CreateRunWorkspace(callCtx, request)
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
		var response api.WorkerRetrieveWorkspaceResponse
		if err := task.callRunSourceRuntime(ctx, func(
			callCtx context.Context,
			lease api.WorkerRunLeaseAssignment,
		) error {
			request.Lease = lease.Fence()
			var callErr error
			response, callErr = control.RetrieveRunWorkspace(callCtx, request)
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
		request := api.WorkerReadWorkspaceFileRequest{
			WorkerRetrieveWorkspaceRequest: base,
			Path:                           value.WorkspaceFileReadRequested.GetPath(),
		}
		correlationID = request.CorrelationID
		var response api.WorkerReadWorkspaceFileResponse
		if err := task.callRunSourceRuntime(ctx, func(
			callCtx context.Context,
			lease api.WorkerRunLeaseAssignment,
		) error {
			request.Lease = lease.Fence()
			var callErr error
			response, callErr = control.ReadRunWorkspaceFile(callCtx, request)
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
		request := api.WorkerReadWorkspaceFileRequest{
			WorkerRetrieveWorkspaceRequest: base,
			Path:                           value.WorkspaceFileStatRequested.GetPath(),
		}
		correlationID = request.CorrelationID
		var response api.WorkerStatWorkspaceFileResponse
		if err := task.callRunSourceRuntime(ctx, func(
			callCtx context.Context,
			lease api.WorkerRunLeaseAssignment,
		) error {
			request.Lease = lease.Fence()
			var callErr error
			response, callErr = control.StatRunWorkspaceFile(callCtx, request)
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
		request := api.WorkerListWorkspaceFilesRequest{
			WorkerReadWorkspaceFileRequest: api.WorkerReadWorkspaceFileRequest{
				WorkerRetrieveWorkspaceRequest: base,
				Path:                           value.WorkspaceFileListRequested.GetPath(),
			},
			Cursor: value.WorkspaceFileListRequested.GetCursor(),
			Limit:  int32(value.WorkspaceFileListRequested.GetLimit()),
		}
		correlationID = request.CorrelationID
		var response api.WorkerListWorkspaceFilesResponse
		if err := task.callRunSourceRuntime(ctx, func(
			callCtx context.Context,
			lease api.WorkerRunLeaseAssignment,
		) error {
			request.Lease = lease.Fence()
			var callErr error
			response, callErr = control.ListRunWorkspaceFiles(callCtx, request)
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
		response, err := task.executeWorkspaceRuntime(ctx, control, request)
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
		request := api.WorkerDeleteWorkspaceRequest{
			WorkerRetrieveWorkspaceRequest: base,
			IdempotencyKey:                 value.WorkspaceDeleteRequested.GetIdempotencyKey(),
		}
		correlationID = request.CorrelationID
		var response api.WorkerDeleteWorkspaceResponse
		if err := task.callRunSourceRuntime(ctx, func(
			callCtx context.Context,
			lease api.WorkerRunLeaseAssignment,
		) error {
			request.Lease = lease.Fence()
			var callErr error
			response, callErr = control.DeleteRunWorkspace(callCtx, request)
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
	control WorkspaceRuntimeControl,
	request api.WorkerExecuteWorkspaceRequest,
) (api.WorkerExecuteWorkspaceResponse, error) {
	var response api.WorkerExecuteWorkspaceResponse
	if err := task.callRunSourceRuntime(ctx, func(
		callCtx context.Context,
		lease api.WorkerRunLeaseAssignment,
	) error {
		request.Lease = lease.Fence()
		var callErr error
		response, callErr = control.ExecuteRunWorkspace(callCtx, request)
		return callErr
	}); err != nil {
		return api.WorkerExecuteWorkspaceResponse{}, fmt.Errorf("execute Workspace: %w", err)
	}
	if response.CorrelationID != request.CorrelationID {
		return api.WorkerExecuteWorkspaceResponse{}, errors.New("Workspace exec response correlation mismatch")
	}
	for response.Pending != nil {
		if response.Completed != nil || response.Failed != nil ||
			validateRuntimeWorkspaceProcessID(response.Pending.ProcessID) != nil {
			return api.WorkerExecuteWorkspaceResponse{}, errors.New("Workspace exec pending response is invalid")
		}
		timer := time.NewTimer(250 * time.Millisecond)
		select {
		case <-ctx.Done():
			timer.Stop()
			return api.WorkerExecuteWorkspaceResponse{}, ctx.Err()
		case <-timer.C:
		}
		poll := api.WorkerPollWorkspaceExecRequest{
			WorkerRetrieveWorkspaceRequest: request.WorkerRetrieveWorkspaceRequest,
			ProcessID:                      response.Pending.ProcessID,
		}
		response = api.WorkerExecuteWorkspaceResponse{}
		if err := task.callRunSourceRuntime(ctx, func(
			callCtx context.Context,
			lease api.WorkerRunLeaseAssignment,
		) error {
			poll.Lease = lease.Fence()
			var callErr error
			response, callErr = control.PollRunWorkspaceExec(callCtx, poll)
			return callErr
		}); err != nil {
			return api.WorkerExecuteWorkspaceResponse{}, fmt.Errorf("poll Workspace exec: %w", err)
		}
		if response.CorrelationID != request.CorrelationID {
			return api.WorkerExecuteWorkspaceResponse{}, errors.New("Workspace exec poll response correlation mismatch")
		}
	}
	if (response.Completed == nil) == (response.Failed == nil) {
		return api.WorkerExecuteWorkspaceResponse{}, errors.New("Workspace exec response must contain exactly one terminal result")
	}
	return response, nil
}

func workerWorkspaceCreateRequest(
	requested *runv0.WorkspaceCreateRequested,
) (api.WorkerCreateWorkspaceRequest, error) {
	if requested == nil {
		return api.WorkerCreateWorkspaceRequest{}, errors.New("Workspace create request is required")
	}
	if err := validateRuntimeWorkspaceCorrelation(requested.GetCorrelationId()); err != nil {
		return api.WorkerCreateWorkspaceRequest{}, err
	}
	if err := api.ValidateWorkspaceDeclaredID(requested.GetDeclaredId()); err != nil {
		return api.WorkerCreateWorkspaceRequest{}, err
	}
	secrets := make([]api.WorkspaceSecret, 0, len(requested.GetSecrets()))
	for _, placement := range requested.GetSecrets() {
		if placement == nil {
			return api.WorkerCreateWorkspaceRequest{}, errors.New("Workspace Secret placement is required")
		}
		secret := api.WorkspaceSecret{Name: placement.GetName()}
		switch value := placement.GetPlacement().(type) {
		case *runv0.WorkspaceSecretPlacement_Env:
			secret.Env = value.Env
		case *runv0.WorkspaceSecretPlacement_File:
			secret.File = value.File
		default:
			return api.WorkerCreateWorkspaceRequest{}, errors.New("Workspace Secret target is required")
		}
		if err := api.ValidateWorkspaceSecret(secret); err != nil {
			return api.WorkerCreateWorkspaceRequest{}, err
		}
		secrets = append(secrets, secret)
	}
	return api.WorkerCreateWorkspaceRequest{
		CorrelationID: requested.GetCorrelationId(), WorkspaceDeclaredID: requested.GetDeclaredId(),
		Key: requested.Key, Secrets: secrets, IdempotencyKey: requested.GetIdempotencyKey(),
	}, nil
}

func workerWorkspaceRetrieveRequest(
	correlationID string,
	address *runv0.WorkspaceAddress,
) (api.WorkerRetrieveWorkspaceRequest, error) {
	if err := validateRuntimeWorkspaceCorrelation(correlationID); err != nil {
		return api.WorkerRetrieveWorkspaceRequest{}, err
	}
	if address == nil {
		return api.WorkerRetrieveWorkspaceRequest{}, errors.New("Workspace address is required")
	}
	request := api.WorkerRetrieveWorkspaceRequest{CorrelationID: correlationID}
	switch value := address.GetAddress().(type) {
	case *runv0.WorkspaceAddress_WorkspaceId:
		request.Workspace.WorkspaceID = value.WorkspaceId
	case *runv0.WorkspaceAddress_WorkspaceKey:
		request.Workspace.WorkspaceKey = value.WorkspaceKey
	default:
		return api.WorkerRetrieveWorkspaceRequest{}, errors.New("Workspace address is required")
	}
	return request, nil
}

func workerWorkspaceExecRequest(
	requested *runv0.WorkspaceExecRequested,
) (api.WorkerExecuteWorkspaceRequest, error) {
	if requested == nil {
		return api.WorkerExecuteWorkspaceRequest{}, errors.New("Workspace exec request is required")
	}
	base, err := workerWorkspaceRetrieveRequest(requested.GetCorrelationId(), requested.GetWorkspace())
	if err != nil {
		return api.WorkerExecuteWorkspaceRequest{}, err
	}
	request := api.WorkerExecuteWorkspaceRequest{
		WorkerRetrieveWorkspaceRequest: base,
		Command:                        append([]string{}, requested.GetCommand()...),
		Cwd:                            requested.GetCwd(),
		Env:                            make(map[string]string, len(requested.GetEnv())),
		Stdin:                          append([]byte{}, requested.GetStdin()...),
		IdempotencyKey:                 requested.GetIdempotencyKey(),
	}
	maps.Copy(request.Env, requested.GetEnv())
	if requested.TimeoutMs != nil {
		if requested.GetTimeoutMs() > math.MaxInt64 {
			return api.WorkerExecuteWorkspaceRequest{}, errors.New("Workspace exec timeout is invalid")
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
