package executor

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"

	"github.com/google/uuid"
	"github.com/helmrdotdev/helmr/internal/api"
	runv0 "github.com/helmrdotdev/helmr/internal/proto/run/v0"
	"github.com/helmrdotdev/helmr/internal/wire"
)

func (task *guestRunLeaseTask) handleActorOutputAppend(
	ctx context.Context,
	requested *runv0.ActorOutputAppendRequested,
) error {
	request, err := workerActorOutputAppendRequest(requested)
	if err != nil {
		return err
	}
	task.mu.Lock()
	defer task.mu.Unlock()
	if task.finished || task.finalizingKind != "" {
		return errors.New("run lease task cannot append actor output")
	}
	request.Lease = task.lease
	var response api.WorkerAppendActorOutputResponse
	requestCtx, cancel, err := runLeaseLogContext(ctx, task.lease.ExpiresAt)
	if err != nil {
		return err
	}
	defer cancel()
	if err := retryRunLeaseRequest(requestCtx, func(retryCtx context.Context) error {
		var requestErr error
		response, requestErr = task.control.AppendActorOutput(retryCtx, request)
		return requestErr
	}); err != nil {
		return fmt.Errorf("append Actor output: %w", err)
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
		return fmt.Errorf("encode Actor output append decision: %w", err)
	}
	if err := wire.WriteResumeDecision(task.program.session.Stream(), &runv0.ResumeDecision{
		CorrelationId: request.CorrelationID,
		Kind:          kind,
		DataJson:      string(data),
	}); err != nil {
		return fmt.Errorf("write Actor output append decision: %w", err)
	}
	return nil
}

func workerActorOutputAppendRequest(
	requested *runv0.ActorOutputAppendRequested,
) (api.WorkerAppendActorOutputRequest, error) {
	if requested == nil {
		return api.WorkerAppendActorOutputRequest{}, errors.New("actor output append request is required")
	}
	correlationID, err := uuid.Parse(requested.GetCorrelationId())
	if err != nil || correlationID == uuid.Nil ||
		correlationID.String() != requested.GetCorrelationId() {
		return api.WorkerAppendActorOutputRequest{}, errors.New("actor output append correlation ID is invalid")
	}
	data := json.RawMessage(requested.GetDataJson())
	if len(data) == 0 || !json.Valid(data) {
		return api.WorkerAppendActorOutputRequest{}, errors.New("actor output append data must be valid JSON")
	}
	contentType := strings.TrimSpace(requested.GetContentType())
	if contentType == "" {
		return api.WorkerAppendActorOutputRequest{}, errors.New("actor output append content type is required")
	}
	return api.WorkerAppendActorOutputRequest{
		CorrelationID:  requested.GetCorrelationId(),
		Data:           data,
		ContentType:    contentType,
		IdempotencyKey: requested.GetIdempotencyKey(),
	}, nil
}
