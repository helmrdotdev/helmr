package executor

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"

	"github.com/helmrdotdev/helmr/internal/httpclient"
	"github.com/helmrdotdev/helmr/internal/ids"
	programv0 "github.com/helmrdotdev/helmr/internal/proto/program/v0"
	"github.com/helmrdotdev/helmr/internal/wire"
	"github.com/helmrdotdev/helmr/internal/workerapi"
)

type childTaskInvokeControlPlane interface {
	InvokeChildTask(context.Context, workerapi.InvokeChildTaskRequest) (workerapi.InvokeChildTaskResponse, error)
}

func (task *guestRunLeaseTask) handleChildTaskInvoke(
	ctx context.Context,
	requested *programv0.TaskChildInvokeRequested,
) error {
	request, err := workerChildTaskInvokeRequest(requested)
	if err != nil {
		return err
	}
	controlPlane, ok := task.controlPlane.(childTaskInvokeControlPlane)
	if !ok {
		return errors.New("run lease task child task invocation control plane is required")
	}
	var response workerapi.InvokeChildTaskResponse
	if err := task.callRunSourceRuntime(ctx, func(
		callCtx context.Context,
		lease workerapi.RunLeaseAssignment,
	) error {
		request.Lease = lease.Fence()
		var callErr error
		response, callErr = controlPlane.InvokeChildTask(callCtx, request)
		return callErr
	}); err != nil {
		if failure, ok := childTaskInvokeFailure(err); ok {
			response = workerapi.InvokeChildTaskResponse{
				CorrelationID: request.CorrelationID,
				Failed:        &failure,
			}
		} else {
			return fmt.Errorf("invoke child task: %w", err)
		}
	}
	if response.CorrelationID != request.CorrelationID {
		return errors.New("child task invocation response correlation ID did not match")
	}
	if response.OpenedWait != nil {
		if response.Failed != nil {
			return errors.New("child task invocation response contained conflicting outcomes")
		}
		if response.OpenedWait.RunWaitID != request.RunWaitID ||
			response.OpenedWait.ResumeAttachID != request.ResumeAttachID {
			return errors.New("child task invocation response wait IDs did not match")
		}
		if task.waits == nil {
			return errors.New("run lease task wait control plane is required")
		}
		runtimeWait := WaitRequest{
			Leases:                        task,
			CorrelationID:                 request.CorrelationID,
			RunWaitID:                     request.RunWaitID,
			ResumeAttachID:                request.ResumeAttachID,
			Kind:                          workerapi.RunWaitKindChild,
			ActorSpeculativeInputSequence: request.ActorSpeculativeInputSequence,
			Workspace:                     task.waitWorkspace,
			Checkpointer:                  task.checkpointer,
			Resume: func(_ context.Context, decision WaitResumeDecision) error {
				if strings.TrimSpace(decision.Kind) == "" {
					return errors.New("program resume kind is required")
				}
				if len(decision.Data) == 0 {
					decision.Data = json.RawMessage(`null`)
				}
				return wire.WriteResumeDecision(task.program.session.Stream(), &programv0.ResumeDecision{
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
	if request.Method == "call" && response.Completed != nil {
		return errors.New("completed child task call response requires an opened Wait")
	}
	if (response.Completed == nil) == (response.Failed == nil) {
		return errors.New("child task invocation response must contain exactly one outcome")
	}
	kind := "completed"
	data := any(response.Completed)
	if response.Failed != nil {
		kind = "failed"
		data = response.Failed
	}
	encoded, err := json.Marshal(data)
	if err != nil {
		return fmt.Errorf("encode child task invocation decision: %w", err)
	}
	if err := wire.WriteResumeDecision(task.program.session.Stream(), &programv0.ResumeDecision{
		CorrelationId:  request.CorrelationID,
		RunWaitId:      request.RunWaitID,
		ResumeAttachId: request.ResumeAttachID,
		Kind:           kind,
		DataJson:       string(encoded),
	}); err != nil {
		return fmt.Errorf("write child task invocation decision: %w", err)
	}
	return nil
}

func workerChildTaskInvokeRequest(
	requested *programv0.TaskChildInvokeRequested,
) (workerapi.InvokeChildTaskRequest, error) {
	if requested == nil {
		return workerapi.InvokeChildTaskRequest{}, errors.New("child task invocation request is required")
	}
	if err := ids.Validate(requested.GetCorrelationId()); err != nil {
		return workerapi.InvokeChildTaskRequest{}, errors.New("child task invocation correlation ID is invalid")
	}
	switch requested.GetMethod() {
	case "call":
		if ids.Validate(requested.GetRunWaitId()) != nil ||
			ids.Validate(requested.GetResumeAttachId()) != nil {
			return workerapi.InvokeChildTaskRequest{}, errors.New("child task call wait IDs are invalid")
		}
	case "start":
		if requested.GetRunWaitId() != "" || requested.GetResumeAttachId() != "" {
			return workerapi.InvokeChildTaskRequest{}, errors.New("child task start must not contain wait IDs")
		}
	default:
		return workerapi.InvokeChildTaskRequest{}, errors.New("child task invocation method is invalid")
	}
	request := workerapi.InvokeChildTaskRequest{
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

func childTaskInvokeFailure(err error) (workerapi.RuntimeOperationFailure, bool) {
	var httpErr *httpclient.Error
	if !errors.As(err, &httpErr) || !semanticRuntimeHTTPError(httpErr) {
		return workerapi.RuntimeOperationFailure{}, false
	}
	code := runtimeOperationCode(httpErr, "child_task_start_rejected")
	message := strings.TrimSpace(httpErr.Message)
	if message == "" {
		message = "child Task start request was rejected"
	}
	return workerapi.RuntimeOperationFailure{
		Code: code, Message: message, Retryable: runtimeOperationRetryable(code),
	}, true
}
