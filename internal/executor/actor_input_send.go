package executor

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"

	"github.com/helmrdotdev/helmr/internal/api"
	"github.com/helmrdotdev/helmr/internal/ids"
	runv0 "github.com/helmrdotdev/helmr/internal/proto/run/v0"
	"github.com/helmrdotdev/helmr/internal/wire"
	"github.com/helmrdotdev/helmr/internal/workerapi"
)

const maxJavaScriptSafeInteger = int64(9007199254740991)

func (task *guestRunLeaseTask) handleActorInputSend(
	ctx context.Context,
	requested *runv0.SessionInputSendRequested,
) error {
	request, err := workerActorInputSendRequest(requested)
	if err != nil {
		return err
	}
	var response workerapi.SendActorInputResponse
	if err := task.callRunSourceRuntime(ctx, func(
		callCtx context.Context,
		lease workerapi.RunLeaseAssignment,
	) error {
		request.Lease = lease.Fence()
		var requestErr error
		response, requestErr = task.controlPlane.SendRunActorInput(callCtx, request)
		return requestErr
	}); err != nil {
		return fmt.Errorf("send actor input: %w", err)
	}
	if response.CorrelationID != request.CorrelationID ||
		(response.Completed == nil) == (response.Failed == nil) {
		return errors.New("actor input send response did not match the request")
	}
	var kind string
	var data []byte
	if response.Completed != nil {
		if response.Completed.Sequence <= 0 || response.Completed.Sequence > maxJavaScriptSafeInteger {
			return errors.New("actor input send response sequence is invalid")
		}
		kind = "completed"
		data, err = json.Marshal(response.Completed)
	} else {
		if strings.TrimSpace(response.Failed.Code) == "" ||
			strings.TrimSpace(response.Failed.Message) == "" {
			return errors.New("actor input send failure is invalid")
		}
		kind = "failed"
		data, err = json.Marshal(response.Failed)
	}
	if err != nil {
		return fmt.Errorf("encode actor input send decision: %w", err)
	}
	if err := wire.WriteResumeDecision(task.program.session.Stream(), &runv0.ResumeDecision{
		CorrelationId: request.CorrelationID,
		Kind:          kind,
		DataJson:      string(data),
	}); err != nil {
		return fmt.Errorf("write actor input send decision: %w", err)
	}
	return nil
}

func workerActorInputSendRequest(
	requested *runv0.SessionInputSendRequested,
) (workerapi.SendActorInputRequest, error) {
	if requested == nil {
		return workerapi.SendActorInputRequest{}, errors.New("actor input send request is required")
	}
	if err := ids.Validate(requested.GetCorrelationId()); err != nil {
		return workerapi.SendActorInputRequest{}, errors.New("actor input send correlation ID is invalid")
	}
	request := workerapi.SendActorInputRequest{
		CorrelationID:  requested.GetCorrelationId(),
		SessionID:      requested.GetSessionId(),
		Input:          json.RawMessage(requested.GetDataJson()),
		IdempotencyKey: requested.GetIdempotencyKey(),
	}
	if err := api.ValidateSessionID(request.SessionID); err != nil {
		return workerapi.SendActorInputRequest{}, err
	}
	if err := api.ValidateSendSessionInputRequest(api.SendSessionInputRequest{
		Input: request.Input, IdempotencyKey: request.IdempotencyKey,
	}); err != nil {
		return workerapi.SendActorInputRequest{}, err
	}
	return request, nil
}
