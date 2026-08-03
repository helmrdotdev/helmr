package executor

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"net/http"
	"strings"

	"github.com/helmrdotdev/helmr/internal/api"
	"github.com/helmrdotdev/helmr/internal/client"
	"github.com/helmrdotdev/helmr/internal/ids"
	runv0 "github.com/helmrdotdev/helmr/internal/proto/run/v0"
	"github.com/helmrdotdev/helmr/internal/wire"
	"github.com/helmrdotdev/helmr/internal/workerapi"
)

func (task *guestRunLeaseTask) handleTokenCreate(
	ctx context.Context,
	requested *runv0.TokenCreateRequested,
) error {
	request, err := workerTokenCreateRequest(requested)
	if err != nil {
		return err
	}
	var response api.TokenResponse
	if err := task.callRunSourceRuntime(ctx, func(
		callCtx context.Context,
		lease workerapi.RunLeaseAssignment,
	) error {
		request.Lease = lease.Fence()
		var requestErr error
		response, requestErr = task.control.CreateRuntimeToken(callCtx, request)
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

func tokenCreateFailure(err error) (workerapi.RuntimeOperationFailure, bool) {
	var httpErr *client.HTTPError
	if !errors.As(err, &httpErr) {
		return workerapi.RuntimeOperationFailure{}, false
	}
	semantic := httpErr.StatusCode == http.StatusBadRequest ||
		httpErr.StatusCode == http.StatusRequestEntityTooLarge ||
		httpErr.StatusCode == http.StatusUnprocessableEntity ||
		(httpErr.StatusCode == http.StatusConflict && strings.TrimSpace(httpErr.Code) != "")
	if !semantic {
		return workerapi.RuntimeOperationFailure{}, false
	}
	code := strings.TrimSpace(httpErr.Code)
	if code == "" {
		code = "token_create_rejected"
	}
	message := strings.TrimSpace(httpErr.Message)
	if message == "" {
		message = "Token create request was rejected"
	}
	return workerapi.RuntimeOperationFailure{
		Code: code, Message: message, Retryable: httpErr.Retryable,
	}, true
}

func workerTokenCreateRequest(
	requested *runv0.TokenCreateRequested,
) (workerapi.CreateTokenRequest, error) {
	if requested == nil {
		return workerapi.CreateTokenRequest{}, errors.New("Token create request is required")
	}
	if err := ids.Validate(requested.GetCorrelationId()); err != nil {
		return workerapi.CreateTokenRequest{}, errors.New("Token create correlation ID is invalid")
	}
	request := workerapi.CreateTokenRequest{
		CorrelationID: requested.GetCorrelationId(),
		Tags:          append([]string(nil), requested.GetTags()...),
		Metadata:      json.RawMessage(`{}`),
	}
	if requested.TimeoutMs != nil {
		if requested.GetTimeoutMs() > math.MaxInt64 {
			return workerapi.CreateTokenRequest{}, errors.New("Token timeout exceeds int64 milliseconds")
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
