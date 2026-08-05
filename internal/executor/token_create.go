package executor

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"strings"

	"github.com/helmrdotdev/helmr/internal/api"
	"github.com/helmrdotdev/helmr/internal/httpclient"
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
		response, requestErr = task.controlPlane.CreateRuntimeToken(callCtx, request)
		return requestErr
	}); err != nil {
		if failure, ok := tokenCreateFailure(err); ok {
			data, marshalErr := json.Marshal(failure)
			if marshalErr != nil {
				return fmt.Errorf("encode token create failure: %w", marshalErr)
			}
			if writeErr := wire.WriteResumeDecision(task.program.session.Stream(), &runv0.ResumeDecision{
				CorrelationId: request.CorrelationID,
				Kind:          "failed",
				DataJson:      string(data),
			}); writeErr != nil {
				return fmt.Errorf("write token create failure: %w", writeErr)
			}
			return nil
		}
		return fmt.Errorf("create token: %w", err)
	}
	data, err := json.Marshal(response)
	if err != nil {
		return fmt.Errorf("encode token create decision: %w", err)
	}
	if err := wire.WriteResumeDecision(task.program.session.Stream(), &runv0.ResumeDecision{
		CorrelationId: request.CorrelationID,
		Kind:          "completed",
		DataJson:      string(data),
	}); err != nil {
		return fmt.Errorf("write token create decision: %w", err)
	}
	return nil
}

func tokenCreateFailure(err error) (workerapi.RuntimeOperationFailure, bool) {
	var httpErr *httpclient.Error
	if !errors.As(err, &httpErr) || !semanticRuntimeHTTPError(httpErr) {
		return workerapi.RuntimeOperationFailure{}, false
	}
	code := runtimeOperationCode(httpErr, "token_create_rejected")
	message := strings.TrimSpace(httpErr.Message)
	if message == "" {
		message = "Token create request was rejected"
	}
	return workerapi.RuntimeOperationFailure{
		Code: code, Message: message, Retryable: runtimeOperationRetryable(code),
	}, true
}

func workerTokenCreateRequest(
	requested *runv0.TokenCreateRequested,
) (workerapi.CreateTokenRequest, error) {
	if requested == nil {
		return workerapi.CreateTokenRequest{}, errors.New("token create request is required")
	}
	if err := ids.Validate(requested.GetCorrelationId()); err != nil {
		return workerapi.CreateTokenRequest{}, errors.New("token create correlation ID is invalid")
	}
	request := workerapi.CreateTokenRequest{
		CorrelationID: requested.GetCorrelationId(),
		Tags:          append([]string(nil), requested.GetTags()...),
		Metadata:      json.RawMessage(`{}`),
	}
	if requested.TimeoutMs != nil {
		if requested.GetTimeoutMs() > math.MaxInt64 {
			return workerapi.CreateTokenRequest{}, errors.New("token timeout exceeds int64 milliseconds")
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
