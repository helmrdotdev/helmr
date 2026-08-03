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
)

const maxJavaScriptSafeInteger = int64(9007199254740991)

func (task *guestRunLeaseTask) handleActorInputSend(
	ctx context.Context,
	requested *runv0.ActorInputSendRequested,
) error {
	request, err := workerActorInputSendRequest(requested)
	if err != nil {
		return err
	}
	var response api.WorkerSendActorInputResponse
	if err := task.callRunSourceRuntime(ctx, func(
		callCtx context.Context,
		lease api.WorkerRunLeaseAssignment,
	) error {
		request.Lease = lease.Fence()
		var requestErr error
		response, requestErr = task.control.SendRunActorInput(callCtx, request)
		return requestErr
	}); err != nil {
		return fmt.Errorf("send Actor input: %w", err)
	}
	if response.CorrelationID != request.CorrelationID ||
		(response.Completed == nil) == (response.Failed == nil) {
		return errors.New("Actor input send response did not match the request")
	}
	var kind string
	var data []byte
	if response.Completed != nil {
		if response.Completed.Sequence <= 0 || response.Completed.Sequence > maxJavaScriptSafeInteger {
			return errors.New("Actor input send response sequence is invalid")
		}
		kind = "completed"
		data, err = json.Marshal(response.Completed)
	} else {
		if strings.TrimSpace(response.Failed.Code) == "" ||
			strings.TrimSpace(response.Failed.Message) == "" {
			return errors.New("Actor input send failure is invalid")
		}
		kind = "failed"
		data, err = json.Marshal(response.Failed)
	}
	if err != nil {
		return fmt.Errorf("encode Actor input send decision: %w", err)
	}
	if err := wire.WriteResumeDecision(task.program.session.Stream(), &runv0.ResumeDecision{
		CorrelationId: request.CorrelationID,
		Kind:          kind,
		DataJson:      string(data),
	}); err != nil {
		return fmt.Errorf("write Actor input send decision: %w", err)
	}
	return nil
}

func workerActorInputSendRequest(
	requested *runv0.ActorInputSendRequested,
) (api.WorkerSendActorInputRequest, error) {
	if requested == nil {
		return api.WorkerSendActorInputRequest{}, errors.New("Actor input send request is required")
	}
	if err := ids.Validate(requested.GetCorrelationId()); err != nil {
		return api.WorkerSendActorInputRequest{}, errors.New("Actor input send correlation ID is invalid")
	}
	request := api.WorkerSendActorInputRequest{
		CorrelationID:   requested.GetCorrelationId(),
		ActorDeclaredID: requested.GetDeclaredId(),
		Input:           json.RawMessage(requested.GetDataJson()),
		IdempotencyKey:  requested.GetIdempotencyKey(),
	}
	switch address := requested.GetAddress().(type) {
	case *runv0.ActorInputSendRequested_ActorId:
		request.ActorID = address.ActorId
	case *runv0.ActorInputSendRequested_ActorKey:
		request.ActorKey = address.ActorKey
	default:
		return api.WorkerSendActorInputRequest{}, errors.New("Actor input send address is required")
	}
	if err := api.ValidateActorDeclaredID(request.ActorDeclaredID); err != nil {
		return api.WorkerSendActorInputRequest{}, err
	}
	if err := api.ValidateSendActorInputRequest(api.SendActorInputRequest{
		ActorID: request.ActorID, ActorKey: request.ActorKey,
		Input: request.Input, IdempotencyKey: request.IdempotencyKey,
	}); err != nil {
		return api.WorkerSendActorInputRequest{}, err
	}
	return request, nil
}
