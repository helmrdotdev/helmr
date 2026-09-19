package executor

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"

	"github.com/helmrdotdev/helmr/internal/api"
	"github.com/helmrdotdev/helmr/internal/ids"
	programv0 "github.com/helmrdotdev/helmr/internal/proto/program/v0"
	"github.com/helmrdotdev/helmr/internal/wire"
	"github.com/helmrdotdev/helmr/internal/workerapi"
	"google.golang.org/protobuf/proto"
)

type SessionExecutionControlPlane interface {
	WriteTurnOutput(context.Context, workerapi.WriteTurnOutputRequest) (workerapi.WriteOutputResponse, error)
	WriteSessionOutput(context.Context, workerapi.WriteSessionOutputRequest) (workerapi.WriteOutputResponse, error)
	TurnMessagesReady(context.Context, workerapi.TurnExecutionRequest) (workerapi.TurnCommandResponse, error)
	BeginTurnSettlement(context.Context, workerapi.TurnExecutionRequest) (workerapi.TurnCommandResponse, error)
	ClaimTurnMessage(context.Context, workerapi.ClaimTurnMessageRequest) (workerapi.ClaimTurnMessageResponse, error)
	CompleteTurnMessage(context.Context, workerapi.CompleteTurnMessageRequest) (workerapi.TurnCommandResponse, error)
	ReadSessionControl(context.Context, workerapi.SessionControlRequest) (workerapi.SessionControlResponse, error)
}

func validateSessionExecution(execution *programv0.SessionExecution, lease workerapi.RunLeaseAssignment) error {
	if execution == nil || api.ValidateSessionID(execution.GetSessionId()) != nil || execution.GetRunId() != lease.RunID || execution.GetAttemptNumber() != uint32(lease.AttemptNumber) || execution.GetRunGeneration() <= 0 {
		return errors.New("Session execution does not match Run authority")
	}
	return nil
}

func (task *guestRunLeaseTask) validateExecution(execution *programv0.SessionExecution) error {
	task.mu.Lock()
	defer task.mu.Unlock()
	if err := validateSessionExecution(execution, task.lease); err != nil {
		return err
	}
	if task.program.execution == nil || !proto.Equal(execution, task.program.execution) {
		return errors.New("Session execution generation mismatch")
	}
	return nil
}

func (task *guestRunLeaseTask) turnRequest(correlation string, execution *programv0.TurnExecution) (workerapi.TurnExecutionRequest, error) {
	if ids.Validate(correlation) != nil || execution == nil || ids.Validate(execution.GetTurnId()) != nil {
		return workerapi.TurnExecutionRequest{}, errors.New("Turn command scope is invalid")
	}
	if err := task.validateExecution(execution.GetSession()); err != nil {
		return workerapi.TurnExecutionRequest{}, err
	}
	return workerapi.TurnExecutionRequest{CorrelationID: correlation, TurnID: execution.GetTurnId(), RunGeneration: execution.GetSession().GetRunGeneration()}, nil
}

func (task *guestRunLeaseTask) writeRuntimeResult(correlation string, completed any, failed *workerapi.RuntimeOperationFailure) error {
	kind := "completed"
	if failed != nil {
		if failed.Code == "" || failed.Message == "" {
			return errors.New("runtime failure receipt is invalid")
		}
		kind = "failed"
		completed = failed
	}
	data, err := json.Marshal(completed)
	if err != nil {
		return err
	}
	return wire.WriteResumeDecision(task.programStream(), &programv0.ResumeDecision{CorrelationId: correlation, Kind: kind, DataJson: string(data)})
}

func (task *guestRunLeaseTask) handleTurnCommand(ctx context.Context, event *programv0.RunEvent) error {
	cp, ok := task.controlPlane.(SessionExecutionControlPlane)
	if !ok {
		return errors.New("Session execution control plane is required")
	}
	var execution *programv0.TurnExecution
	var correlation string
	switch v := event.Event.(type) {
	case *programv0.RunEvent_TurnReadyRequested:
		execution = v.TurnReadyRequested.GetExecution()
		correlation = v.TurnReadyRequested.GetCorrelationId()
	case *programv0.RunEvent_TurnSettlementBeginRequested:
		execution = v.TurnSettlementBeginRequested.GetExecution()
		correlation = v.TurnSettlementBeginRequested.GetCorrelationId()
	case *programv0.RunEvent_TurnMessageClaimRequested:
		execution = v.TurnMessageClaimRequested.GetExecution()
		correlation = v.TurnMessageClaimRequested.GetCorrelationId()
	case *programv0.RunEvent_TurnMessageCompleteRequested:
		execution = v.TurnMessageCompleteRequested.GetExecution()
		correlation = v.TurnMessageCompleteRequested.GetCorrelationId()
	default:
		return errors.New("unsupported Turn command")
	}
	request, err := task.turnRequest(correlation, execution)
	if err != nil {
		return err
	}
	var response workerapi.TurnCommandResponse
	var claimed *workerapi.ClaimTurnMessageResponse
	err = task.callRunSourceRuntime(ctx, func(callCtx context.Context, lease workerapi.RunLeaseAssignment) error {
		request.Lease = lease.Fence()
		var callErr error
		switch v := event.Event.(type) {
		case *programv0.RunEvent_TurnReadyRequested:
			response, callErr = cp.TurnMessagesReady(callCtx, request)
		case *programv0.RunEvent_TurnSettlementBeginRequested:
			response, callErr = cp.BeginTurnSettlement(callCtx, request)
		case *programv0.RunEvent_TurnMessageClaimRequested:
			if ids.Validate(v.TurnMessageClaimRequested.GetDeliveryId()) != nil {
				return errors.New("message delivery identity is invalid")
			}
			result, e := cp.ClaimTurnMessage(callCtx, workerapi.ClaimTurnMessageRequest{TurnExecutionRequest: request, DeliveryID: v.TurnMessageClaimRequested.GetDeliveryId()})
			claimed = &result
			callErr = e
		case *programv0.RunEvent_TurnMessageCompleteRequested:
			r := v.TurnMessageCompleteRequested
			if ids.Validate(r.GetMessageId()) != nil || ids.Validate(r.GetDeliveryId()) != nil {
				return errors.New("message completion identity is invalid")
			}
			var details json.RawMessage
			if r.DetailsJson != nil {
				details = json.RawMessage(r.GetDetailsJson())
				if !json.Valid(details) {
					return errors.New("message completion details are invalid")
				}
			}
			response, callErr = cp.CompleteTurnMessage(callCtx, workerapi.CompleteTurnMessageRequest{TurnExecutionRequest: request, MessageID: r.GetMessageId(), DeliveryID: r.GetDeliveryId(), Status: r.GetStatus(), Code: r.GetCode(), Details: details})
		}
		return callErr
	})
	if err != nil {
		return fmt.Errorf("Turn command: %w", err)
	}
	if claimed != nil {
		if claimed.CorrelationID != correlation {
			return errors.New("message claim correlation mismatch")
		}
		if d := claimed.Delivery; d != nil {
			if claimed.Failed != nil || d.TurnID != request.TurnID || d.DeliveryID != event.GetTurnMessageClaimRequested().GetDeliveryId() || ids.Validate(d.MessageID) != nil || d.Sequence <= 0 || !json.Valid(d.Data) {
				return errors.New("message delivery receipt mismatch")
			}
		}
		return task.writeRuntimeResult(correlation, struct {
			Delivery *workerapi.TurnMessageDelivery `json:"delivery"`
		}{claimed.Delivery}, claimed.Failed)
	}
	if response.CorrelationID != correlation || response.Accepted == (response.Failed != nil) {
		return errors.New("Turn command receipt mismatch")
	}
	return task.writeRuntimeResult(correlation, struct{}{}, response.Failed)
}

func (task *guestRunLeaseTask) validateWaitScope(execution *programv0.SessionExecution, turnID *string) error {
	if task.program.execution == nil {
		if execution != nil || turnID != nil {
			return errors.New("Task wait carries Actor authority")
		}
		return nil
	}
	if err := task.validateExecution(execution); err != nil {
		return err
	}
	if turnID != nil && ids.Validate(*turnID) != nil {
		return errors.New("wait Turn identity is invalid")
	}
	return nil
}
