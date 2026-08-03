package executor

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strings"

	"github.com/helmrdotdev/helmr/internal/api"
	"github.com/helmrdotdev/helmr/internal/client"
	"github.com/helmrdotdev/helmr/internal/ids"
	runv0 "github.com/helmrdotdev/helmr/internal/proto/run/v0"
	"github.com/helmrdotdev/helmr/internal/wire"
)

type childTaskInvokeControl interface {
	InvokeChildTask(context.Context, api.WorkerInvokeChildTaskRequest) (api.WorkerInvokeChildTaskResponse, error)
}

func (task *guestRunLeaseTask) handleChildTaskInvoke(
	ctx context.Context,
	requested *runv0.TaskChildInvokeRequested,
) error {
	request, err := workerChildTaskInvokeRequest(requested)
	if err != nil {
		return err
	}
	control, ok := task.control.(childTaskInvokeControl)
	if !ok {
		return errors.New("Run Lease Task child Task invocation control is required")
	}
	var response api.WorkerInvokeChildTaskResponse
	if err := task.callRunSourceRuntime(ctx, func(
		callCtx context.Context,
		lease api.WorkerRunLeaseAssignment,
	) error {
		request.Lease = lease.Fence()
		var callErr error
		response, callErr = control.InvokeChildTask(callCtx, request)
		return callErr
	}); err != nil {
		if failure, ok := childTaskInvokeFailure(err); ok {
			response = api.WorkerInvokeChildTaskResponse{
				CorrelationID: request.CorrelationID,
				Failed:        &failure,
			}
		} else {
			return fmt.Errorf("invoke child Task: %w", err)
		}
	}
	if response.CorrelationID != request.CorrelationID {
		return errors.New("child Task invocation response correlation ID did not match")
	}
	if response.OpenedWait != nil {
		if response.Failed != nil {
			return errors.New("child Task invocation response contained conflicting outcomes")
		}
		if response.OpenedWait.RunWaitID != request.RunWaitID ||
			response.OpenedWait.ResumeAttachID != request.ResumeAttachID {
			return errors.New("child Task invocation response Wait IDs did not match")
		}
		if task.waits == nil {
			return errors.New("Run Lease Task wait control is required")
		}
		runtimeWait := WaitRequest{
			Leases:                        task,
			CorrelationID:                 request.CorrelationID,
			RunWaitID:                     request.RunWaitID,
			ResumeAttachID:                request.ResumeAttachID,
			Kind:                          api.WorkerRunWaitKindChild,
			ActorSpeculativeInputSequence: request.ActorSpeculativeInputSequence,
			Workspace:                     task.waitWorkspace,
			Checkpointer:                  task.checkpointer,
			Resume: func(_ context.Context, decision WaitResumeDecision) error {
				if strings.TrimSpace(decision.Kind) == "" {
					return errors.New("Program resume kind is required")
				}
				if len(decision.Data) == 0 {
					decision.Data = json.RawMessage(`null`)
				}
				return wire.WriteResumeDecision(task.program.session.Stream(), &runv0.ResumeDecision{
					RunWaitId:      response.OpenedWait.RunWaitID,
					CorrelationId:  request.CorrelationID,
					ResumeAttachId: response.OpenedWait.ResumeAttachID,
					Kind:           decision.Kind,
					DataJson:       string(decision.Data),
				})
			},
		}
		return task.waits.ContinueRunWait(ctx, runtimeWait, *response.OpenedWait)
	}
	if (response.Completed == nil) == (response.Failed == nil) {
		return errors.New("child Task invocation response must contain exactly one outcome")
	}
	kind := "completed"
	data := any(response.Completed)
	if response.Failed != nil {
		kind = "failed"
		data = response.Failed
	}
	encoded, err := json.Marshal(data)
	if err != nil {
		return fmt.Errorf("encode child Task invocation decision: %w", err)
	}
	if err := wire.WriteResumeDecision(task.program.session.Stream(), &runv0.ResumeDecision{
		CorrelationId: request.CorrelationID,
		Kind:          kind,
		DataJson:      string(encoded),
	}); err != nil {
		return fmt.Errorf("write child Task invocation decision: %w", err)
	}
	return nil
}

func workerChildTaskInvokeRequest(
	requested *runv0.TaskChildInvokeRequested,
) (api.WorkerInvokeChildTaskRequest, error) {
	if requested == nil {
		return api.WorkerInvokeChildTaskRequest{}, errors.New("child Task invocation request is required")
	}
	if err := ids.Validate(requested.GetCorrelationId()); err != nil {
		return api.WorkerInvokeChildTaskRequest{}, errors.New("child Task invocation correlation ID is invalid")
	}
	switch requested.GetMethod() {
	case "call":
		if ids.Validate(requested.GetRunWaitId()) != nil ||
			ids.Validate(requested.GetResumeAttachId()) != nil {
			return api.WorkerInvokeChildTaskRequest{}, errors.New("child Task call Wait IDs are invalid")
		}
	case "start":
		if requested.GetRunWaitId() != "" || requested.GetResumeAttachId() != "" {
			return api.WorkerInvokeChildTaskRequest{}, errors.New("child Task start must not contain Wait IDs")
		}
	}
	request := api.WorkerInvokeChildTaskRequest{
		CorrelationID:  requested.GetCorrelationId(),
		RunWaitID:      requested.GetRunWaitId(),
		ResumeAttachID: requested.GetResumeAttachId(),
		TaskDeclaredID: requested.GetDeclaredId(),
		Method:         requested.GetMethod(),
		PayloadPresent: requested.GetPayloadPresent(),
		Workspace:      json.RawMessage(requested.GetWorkspaceJson()),
		Options:        json.RawMessage(requested.GetOptionsJson()),
	}
	if requested.PayloadJson != nil {
		request.Payload = json.RawMessage(requested.GetPayloadJson())
	}
	if requested.IdempotencyKey != nil {
		request.IdempotencyKey = requested.GetIdempotencyKey()
	}
	request.ActorSpeculativeInputSequence = requested.ActorSpeculativeInputSequence
	return request, nil
}

func childTaskInvokeFailure(err error) (api.WorkerRuntimeOperationFailure, bool) {
	var httpErr *client.HTTPError
	if !errors.As(err, &httpErr) {
		return api.WorkerRuntimeOperationFailure{}, false
	}
	if httpErr.StatusCode != http.StatusBadRequest &&
		httpErr.StatusCode != http.StatusUnprocessableEntity &&
		httpErr.StatusCode != http.StatusRequestEntityTooLarge &&
		(httpErr.StatusCode != http.StatusConflict || strings.TrimSpace(httpErr.Code) == "") {
		return api.WorkerRuntimeOperationFailure{}, false
	}
	code := strings.TrimSpace(httpErr.Code)
	if code == "" {
		code = "child_task_start_rejected"
	}
	message := strings.TrimSpace(httpErr.Message)
	if message == "" {
		message = "child Task start request was rejected"
	}
	return api.WorkerRuntimeOperationFailure{
		Code: code, Message: message, Retryable: httpErr.Retryable,
	}, true
}
