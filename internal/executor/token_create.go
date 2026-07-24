package executor

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"net/http"
	"strings"

	"github.com/google/uuid"
	"github.com/helmrdotdev/helmr/internal/api"
	"github.com/helmrdotdev/helmr/internal/client"
	runv0 "github.com/helmrdotdev/helmr/internal/proto/run/v0"
	"github.com/helmrdotdev/helmr/internal/wire"
)

func (task *guestRunLeaseTask) handleTokenCreate(
	ctx context.Context,
	requested *runv0.TokenCreateRequested,
) error {
	request, err := workerTokenCreateRequest(requested)
	if err != nil {
		return err
	}
	task.mu.Lock()
	defer task.mu.Unlock()
	if task.finished || task.finalizingKind != "" {
		return errors.New("Run Lease Task cannot create a Token")
	}
	request.Lease = task.lease
	var response api.TokenResponse
	createCtx, cancel, err := runLeaseLogContext(ctx, task.lease.ExpiresAt)
	if err != nil {
		return err
	}
	defer cancel()
	if err := retryRunLeaseRequest(createCtx, func(requestCtx context.Context) error {
		var requestErr error
		response, requestErr = task.control.CreateRuntimeToken(requestCtx, request)
		return requestErr
	}); err != nil {
		if failure, ok := tokenCreateFailure(err); ok {
			data, marshalErr := json.Marshal(failure)
			if marshalErr != nil {
				return fmt.Errorf("encode Token create failure: %w", marshalErr)
			}
			if writeErr := wire.WriteResumeDecision(task.program.session.Stream(), &runv0.ResumeDecision{
				CorrelationId: request.CorrelationID,
				Kind:          "failed",
				DataJson:      string(data),
			}); writeErr != nil {
				return fmt.Errorf("write Token create failure: %w", writeErr)
			}
			return nil
		}
		return fmt.Errorf("create Token: %w", err)
	}
	data, err := json.Marshal(response)
	if err != nil {
		return fmt.Errorf("encode Token create decision: %w", err)
	}
	if err := wire.WriteResumeDecision(task.program.session.Stream(), &runv0.ResumeDecision{
		CorrelationId: request.CorrelationID,
		Kind:          "completed",
		DataJson:      string(data),
	}); err != nil {
		return fmt.Errorf("write Token create decision: %w", err)
	}
	return nil
}

func tokenCreateFailure(err error) (api.WorkerRuntimeOperationFailure, bool) {
	var httpErr *client.HTTPError
	if !errors.As(err, &httpErr) {
		return api.WorkerRuntimeOperationFailure{}, false
	}
	semantic := httpErr.StatusCode == http.StatusBadRequest ||
		httpErr.StatusCode == http.StatusRequestEntityTooLarge ||
		httpErr.StatusCode == http.StatusUnprocessableEntity ||
		(httpErr.StatusCode == http.StatusConflict && strings.TrimSpace(httpErr.Code) != "")
	if !semantic {
		return api.WorkerRuntimeOperationFailure{}, false
	}
	code := strings.TrimSpace(httpErr.Code)
	if code == "" {
		code = "token_create_rejected"
	}
	message := strings.TrimSpace(httpErr.Message)
	if message == "" {
		message = "Token create request was rejected"
	}
	return api.WorkerRuntimeOperationFailure{
		Code: code, Message: message, Retryable: httpErr.Retryable,
	}, true
}

func workerTokenCreateRequest(
	requested *runv0.TokenCreateRequested,
) (api.WorkerCreateTokenRequest, error) {
	if requested == nil {
		return api.WorkerCreateTokenRequest{}, errors.New("Token create request is required")
	}
	correlationID, err := uuid.Parse(requested.GetCorrelationId())
	if err != nil || correlationID == uuid.Nil ||
		correlationID.String() != requested.GetCorrelationId() {
		return api.WorkerCreateTokenRequest{}, errors.New("Token create correlation ID is invalid")
	}
	request := api.WorkerCreateTokenRequest{
		CorrelationID: requested.GetCorrelationId(),
		Tags:          append([]string(nil), requested.GetTags()...),
		Metadata:      json.RawMessage(`{}`),
	}
	if requested.TimeoutMs != nil {
		if requested.GetTimeoutMs() > math.MaxInt64 {
			return api.WorkerCreateTokenRequest{}, errors.New("Token timeout exceeds int64 milliseconds")
		}
		timeoutMS := int64(requested.GetTimeoutMs())
		request.TimeoutMS = &timeoutMS
	}
	if requested.MetadataJson != nil {
		request.Metadata = json.RawMessage(requested.GetMetadataJson())
	}
	if requested.IdempotencyKey != nil {
		request.IdempotencyKey = requested.GetIdempotencyKey()
	}
	return request, nil
}
