package executor

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strings"

	"github.com/google/uuid"
	"github.com/helmrdotdev/helmr/internal/api"
	"github.com/helmrdotdev/helmr/internal/client"
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
	response, err := func() (api.WorkerInvokeChildTaskResponse, error) {
		task.mu.Lock()
		defer task.mu.Unlock()
		if task.finished || task.finalizingKind != "" {
			return api.WorkerInvokeChildTaskResponse{}, errors.New(
				"Run Lease Task cannot invoke a child Task",
			)
		}
		request.Lease = task.lease
		control, ok := task.control.(childTaskInvokeControl)
		if !ok {
			return api.WorkerInvokeChildTaskResponse{}, errors.New(
				"Run Lease Task child Task invocation control is required",
			)
		}
		var response api.WorkerInvokeChildTaskResponse
		requestCtx, cancel, err := runLeaseLogContext(ctx, task.lease.ExpiresAt)
		if err != nil {
			return api.WorkerInvokeChildTaskResponse{}, err
		}
		defer cancel()
		if err := retryRunLeaseRequest(requestCtx, func(callCtx context.Context) error {
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
				return api.WorkerInvokeChildTaskResponse{}, fmt.Errorf(
					"invoke child Task: %w",
					err,
				)
			}
		}
		return response, nil
	}()
	if err != nil {
		return err
	}
	if response.CorrelationID != request.CorrelationID {
		return errors.New("child Task invocation response correlation ID did not match")
	}
	if response.OpenedWait != nil {
		if response.Failed != nil {
			return errors.New("child Task invocation response contained conflicting outcomes")
		}
		if task.waits == nil {
			return errors.New("Run Lease Task wait control is required")
		}
		runtimeWait := WaitRequest{
			Leases:                        task,
			CorrelationID:                 request.CorrelationID,
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
					RunWaitId:     response.OpenedWait.RunWaitID,
					CorrelationId: request.CorrelationID,
					Kind:          decision.Kind,
					DataJson:      string(decision.Data),
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
	correlationID, err := uuid.Parse(requested.GetCorrelationId())
	if err != nil || correlationID == uuid.Nil || correlationID.String() != requested.GetCorrelationId() {
		return api.WorkerInvokeChildTaskRequest{}, errors.New("child Task invocation correlation ID is invalid")
	}
	request := api.WorkerInvokeChildTaskRequest{
		CorrelationID:  requested.GetCorrelationId(),
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
