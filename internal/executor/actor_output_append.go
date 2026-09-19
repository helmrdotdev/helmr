package executor

import (
	"context"
	"encoding/json"
	"errors"
	"github.com/helmrdotdev/helmr/internal/api"
	"github.com/helmrdotdev/helmr/internal/ids"
	programv0 "github.com/helmrdotdev/helmr/internal/proto/program/v0"
	"github.com/helmrdotdev/helmr/internal/workerapi"
)

func (task *guestRunLeaseTask) handleTurnOutput(ctx context.Context, requested *programv0.TurnOutputWriteRequested) error {
	if requested == nil {
		return errors.New("Turn output request is required")
	}
	scope, err := task.turnRequest(requested.GetCorrelationId(), requested.GetExecution())
	if err != nil {
		return err
	}
	request := workerapi.WriteTurnOutputRequest{CorrelationID: scope.CorrelationID, TurnID: scope.TurnID, RunGeneration: scope.RunGeneration, Data: json.RawMessage(requested.GetDataJson()), IdempotencyKey: requested.GetIdempotencyKey(), MessageDeliveryID: requested.MessageDeliveryId}
	if err := api.ValidateSessionDataRequest(api.SessionDataRequest{Data: request.Data, IdempotencyKey: request.IdempotencyKey}); err != nil {
		return err
	}
	if request.MessageDeliveryID != nil && ids.Validate(*request.MessageDeliveryID) != nil {
		return errors.New("output message delivery identity is invalid")
	}
	cp, ok := task.controlPlane.(SessionExecutionControlPlane)
	if !ok {
		return errors.New("Session output control plane is required")
	}
	var response workerapi.WriteOutputResponse
	err = task.callRunSourceRuntime(ctx, func(callCtx context.Context, lease workerapi.RunLeaseAssignment) error {
		request.Lease = lease.Fence()
		var e error
		response, e = cp.WriteTurnOutput(callCtx, request)
		return e
	})
	if err != nil {
		return err
	}
	return task.writeOutputReceipt(scope.CorrelationID, requested.GetExecution().GetSession(), &request.TurnID, response)
}
func (task *guestRunLeaseTask) handleSessionOutput(ctx context.Context, requested *programv0.SessionOutputWriteRequested) error {
	if requested == nil || ids.Validate(requested.GetCorrelationId()) != nil {
		return errors.New("Session output request is invalid")
	}
	if err := task.validateExecution(requested.GetExecution()); err != nil {
		return err
	}
	request := workerapi.WriteSessionOutputRequest{CorrelationID: requested.GetCorrelationId(), RunGeneration: requested.GetExecution().GetRunGeneration(), Data: json.RawMessage(requested.GetDataJson()), IdempotencyKey: requested.GetIdempotencyKey()}
	if err := api.ValidateSessionDataRequest(api.SessionDataRequest{Data: request.Data, IdempotencyKey: request.IdempotencyKey}); err != nil {
		return err
	}
	cp, ok := task.controlPlane.(SessionExecutionControlPlane)
	if !ok {
		return errors.New("Session output control plane is required")
	}
	var response workerapi.WriteOutputResponse
	err := task.callRunSourceRuntime(ctx, func(callCtx context.Context, lease workerapi.RunLeaseAssignment) error {
		request.Lease = lease.Fence()
		var e error
		response, e = cp.WriteSessionOutput(callCtx, request)
		return e
	})
	if err != nil {
		return err
	}
	return task.writeOutputReceipt(request.CorrelationID, requested.GetExecution(), nil, response)
}
func (task *guestRunLeaseTask) writeOutputReceipt(correlation string, execution *programv0.SessionExecution, turnID *string, response workerapi.WriteOutputResponse) error {
	if response.CorrelationID != correlation || (response.Completed == nil) == (response.Failed == nil) {
		return errors.New("output receipt correlation mismatch")
	}
	if response.Failed != nil {
		return task.writeRuntimeResult(correlation, nil, response.Failed)
	}
	event := response.Completed
	p := event.Provenance
	if event.ID == "" || event.Sequence <= 0 || event.Sequence > maxJavaScriptSafeInteger || event.SessionID != execution.GetSessionId() || p == nil || p.RunID != execution.GetRunId() || p.AttemptNumber != int32(execution.GetAttemptNumber()) || p.RunGeneration != execution.GetRunGeneration() || (event.TurnID == nil) != (turnID == nil) || (turnID != nil && *event.TurnID != *turnID) {
		return errors.New("output receipt execution mismatch")
	}
	return task.writeRuntimeResult(correlation, struct {
		ID            string  `json:"id"`
		Sequence      int64   `json:"sequence"`
		SessionID     string  `json:"session_id"`
		TurnID        *string `json:"turn_id"`
		RunID         string  `json:"run_id"`
		AttemptNumber int32   `json:"attempt_number"`
		RunGeneration int64   `json:"run_generation"`
	}{event.ID, event.Sequence, event.SessionID, event.TurnID, p.RunID, p.AttemptNumber, p.RunGeneration}, nil)
}
