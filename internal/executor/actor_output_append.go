package executor

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"

	"github.com/helmrdotdev/helmr/internal/ids"
	runv0 "github.com/helmrdotdev/helmr/internal/proto/run/v0"
	"github.com/helmrdotdev/helmr/internal/wire"
	"github.com/helmrdotdev/helmr/internal/workerapi"
)

func (task *guestRunLeaseTask) handleActorOutputAppend(
	ctx context.Context,
	requested *runv0.ActorOutputAppendRequested,
) error {
	request, err := workerActorOutputAppendRequest(requested)
	if err != nil {
		return err
	}
	var response workerapi.AppendActorOutputResponse
	if err := task.callRunSourceRuntime(ctx, func(
		callCtx context.Context,
		lease workerapi.RunLeaseAssignment,
	) error {
		request.Lease = lease.Fence()
		var requestErr error
		response, requestErr = task.controlPlane.AppendActorOutput(callCtx, request)
		return requestErr
	}); err != nil {
		return fmt.Errorf("append actor output: %w", err)
	}
	if response.CorrelationID != request.CorrelationID ||
		(response.Completed == nil) == (response.Failed == nil) {
		return errors.New("actor output append response did not match the request")
	}
	var kind string
	var data []byte
	if response.Completed != nil {
		if response.Completed.Sequence <= 0 ||
			response.Completed.Sequence > maxJavaScriptSafeInteger ||
			strings.TrimSpace(response.Completed.ID) == "" ||
			strings.TrimSpace(response.Completed.ContentType) == "" ||
			!json.Valid(response.Completed.Data) {
			return errors.New("actor output append response is invalid")
		}
		kind = "completed"
		data, err = json.Marshal(response.Completed)
	} else {
		if strings.TrimSpace(response.Failed.Code) == "" ||
			strings.TrimSpace(response.Failed.Message) == "" {
			return errors.New("actor output append failure is invalid")
		}
		kind = "failed"
		data, err = json.Marshal(response.Failed)
	}
	if err != nil {
		return fmt.Errorf("encode actor output append decision: %w", err)
	}
	if err := wire.WriteResumeDecision(task.program.session.Stream(), &runv0.ResumeDecision{
		CorrelationId: request.CorrelationID,
		Kind:          kind,
		DataJson:      string(data),
	}); err != nil {
		return fmt.Errorf("write actor output append decision: %w", err)
	}
	return nil
}

func workerActorOutputAppendRequest(
	requested *runv0.ActorOutputAppendRequested,
) (workerapi.AppendActorOutputRequest, error) {
	if requested == nil {
		return workerapi.AppendActorOutputRequest{}, errors.New("actor output append request is required")
	}
	if err := ids.Validate(requested.GetCorrelationId()); err != nil {
		return workerapi.AppendActorOutputRequest{}, errors.New("actor output append correlation ID is invalid")
	}
	data := json.RawMessage(requested.GetDataJson())
	if len(data) == 0 || !json.Valid(data) {
		return workerapi.AppendActorOutputRequest{}, errors.New("actor output append data must be valid JSON")
	}
	contentType := strings.TrimSpace(requested.GetContentType())
	if contentType == "" {
		return workerapi.AppendActorOutputRequest{}, errors.New("actor output append content type is required")
	}
	return workerapi.AppendActorOutputRequest{
		CorrelationID:  requested.GetCorrelationId(),
		Data:           data,
		ContentType:    contentType,
		IdempotencyKey: requested.GetIdempotencyKey(),
	}, nil
}
