package executor

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"

	"github.com/google/uuid"
	"github.com/helmrdotdev/helmr/internal/httpclient"
	runv0 "github.com/helmrdotdev/helmr/internal/proto/run/v0"
	"github.com/helmrdotdev/helmr/internal/wire"
	"github.com/helmrdotdev/helmr/internal/workerapi"
)

type runObservabilityControlPlane interface {
	UpdateRunMetadata(context.Context, workerapi.UpdateRunMetadataRequest) error
	AppendStructuredRunLog(context.Context, workerapi.StructuredLogRequest) error
}

func requireRunObservabilityControlPlane(value any) (runObservabilityControlPlane, error) {
	controlPlane, ok := value.(runObservabilityControlPlane)
	if !ok {
		return nil, errors.New("Run observability Control Plane is required")
	}
	return controlPlane, nil
}

func updateRunMetadata(
	ctx context.Context,
	controlPlane runObservabilityControlPlane,
	lease workerapi.RunLeaseAssignment,
	requested *runv0.MetadataUpdated,
) error {
	request, err := workerRunMetadataRequest(requested)
	if err != nil {
		return err
	}
	request.Lease = lease.Fence()
	requestCtx, cancel, err := runLeaseLogContext(ctx, lease.ExpiresAt)
	if err != nil {
		return err
	}
	defer cancel()
	return retryRunLeaseRequest(requestCtx, func(retryCtx context.Context) error {
		return sendRunMetadataRequest(retryCtx, controlPlane, request)
	})
}

func sendRunMetadataRequest(
	ctx context.Context,
	controlPlane runObservabilityControlPlane,
	request workerapi.UpdateRunMetadataRequest,
) error {
	if err := controlPlane.UpdateRunMetadata(ctx, request); err != nil {
		return fmt.Errorf("update Run metadata: %w", err)
	}
	return nil
}

func appendStructuredRunLog(
	ctx context.Context,
	controlPlane runObservabilityControlPlane,
	lease workerapi.RunLeaseAssignment,
	sequence uint64,
	requested *runv0.StructuredLogRequested,
) error {
	request, err := workerStructuredLogRequest(requested, sequence)
	if err != nil {
		return err
	}
	request.Lease = lease.Fence()
	requestCtx, cancel, err := runLeaseLogContext(ctx, lease.ExpiresAt)
	if err != nil {
		return err
	}
	defer cancel()
	return retryRunLeaseRequest(requestCtx, func(retryCtx context.Context) error {
		return sendStructuredRunLogRequest(retryCtx, controlPlane, request)
	})
}

func sendStructuredRunLogRequest(
	ctx context.Context,
	controlPlane runObservabilityControlPlane,
	request workerapi.StructuredLogRequest,
) error {
	if err := controlPlane.AppendStructuredRunLog(ctx, request); err != nil {
		return fmt.Errorf("append structured Run log: %w", err)
	}
	return nil
}

func workerRunMetadataRequest(
	requested *runv0.MetadataUpdated,
) (workerapi.UpdateRunMetadataRequest, error) {
	if requested == nil {
		return workerapi.UpdateRunMetadataRequest{}, errors.New("Run metadata request is required")
	}
	if err := validateRuntimeCorrelationID(requested.GetCorrelationId()); err != nil {
		return workerapi.UpdateRunMetadataRequest{}, err
	}
	request := workerapi.UpdateRunMetadataRequest{
		OperationID: requested.GetCorrelationId(),
		Operation:   requested.GetOperation(),
	}
	if requested.Key != nil {
		request.Key = requested.GetKey()
	}
	if requested.ValueJson != nil {
		request.Value = json.RawMessage(requested.GetValueJson())
	}
	if requested.PatchJson != nil {
		request.Patch = json.RawMessage(requested.GetPatchJson())
	}
	if requested.Amount != nil {
		value := requested.GetAmount()
		request.Amount = &value
	}
	return request, nil
}

func workerStructuredLogRequest(
	requested *runv0.StructuredLogRequested,
	sequence uint64,
) (workerapi.StructuredLogRequest, error) {
	if requested == nil {
		return workerapi.StructuredLogRequest{}, errors.New("structured log request is required")
	}
	if err := validateRuntimeCorrelationID(requested.GetCorrelationId()); err != nil {
		return workerapi.StructuredLogRequest{}, err
	}
	return workerapi.StructuredLogRequest{
		ObservedSeq: sequence,
		Level:       requested.GetLevel(),
		Message:     requested.GetMessage(),
		Attributes:  json.RawMessage(requested.GetAttributesJson()),
	}, nil
}

func validateRuntimeCorrelationID(raw string) error {
	value, err := uuid.Parse(raw)
	if err != nil || value == uuid.Nil || value.String() != raw {
		return errors.New("runtime operation correlation ID is invalid")
	}
	return nil
}

func processRunMetadataEvent(
	ctx context.Context,
	events freshProgramEventSink,
	lease workerapi.RunLeaseAssignment,
	stream io.Writer,
	requested *runv0.MetadataUpdated,
) error {
	if requested == nil {
		return errors.New("Run metadata request is required")
	}
	err := events.ApplyRunMetadata(ctx, lease, requested)
	return writeRuntimeOperationDecision(
		stream,
		requested.GetCorrelationId(),
		err,
		"run_metadata_rejected",
		"Run metadata request was rejected",
	)
}

func processStructuredLogEvent(
	ctx context.Context,
	events freshProgramEventSink,
	lease workerapi.RunLeaseAssignment,
	stream io.Writer,
	sequence uint64,
	requested *runv0.StructuredLogRequested,
) error {
	if requested == nil {
		return errors.New("structured log request is required")
	}
	err := events.RecordStructuredRunLog(ctx, lease, sequence, requested)
	return writeRuntimeOperationDecision(
		stream,
		requested.GetCorrelationId(),
		err,
		"structured_log_rejected",
		"Structured log request was rejected",
	)
}

func writeRuntimeOperationDecision(
	stream io.Writer,
	correlationID string,
	operationErr error,
	fallbackCode string,
	fallbackMessage string,
) error {
	decision := &runv0.ResumeDecision{
		CorrelationId: correlationID,
		Kind:          "completed",
		DataJson:      `{}`,
	}
	if operationErr != nil {
		failure, ok := runtimeOperationFailure(
			operationErr,
			fallbackCode,
			fallbackMessage,
		)
		if !ok {
			return operationErr
		}
		data, err := json.Marshal(failure)
		if err != nil {
			return fmt.Errorf("encode runtime operation failure: %w", err)
		}
		decision.Kind = "failed"
		decision.DataJson = string(data)
	}
	if err := wire.WriteResumeDecision(stream, decision); err != nil {
		return fmt.Errorf("write runtime operation decision: %w", err)
	}
	return nil
}

func runtimeOperationFailure(
	err error,
	fallbackCode string,
	fallbackMessage string,
) (workerapi.RuntimeOperationFailure, bool) {
	var httpErr *httpclient.Error
	if !errors.As(err, &httpErr) {
		return workerapi.RuntimeOperationFailure{}, false
	}
	semantic := httpErr.StatusCode == http.StatusBadRequest ||
		httpErr.StatusCode == http.StatusRequestEntityTooLarge ||
		httpErr.StatusCode == http.StatusUnprocessableEntity ||
		(httpErr.StatusCode == http.StatusConflict &&
			strings.TrimSpace(httpErr.Code) != "")
	if !semantic {
		return workerapi.RuntimeOperationFailure{}, false
	}
	code := strings.TrimSpace(httpErr.Code)
	if code == "" {
		code = fallbackCode
	}
	message := strings.TrimSpace(httpErr.Message)
	if message == "" {
		message = fallbackMessage
	}
	return workerapi.RuntimeOperationFailure{
		Code: code, Message: message, Retryable: httpErr.Retryable,
	}, true
}

func isRuntimeOperationRejection(err error) bool {
	if err == nil {
		return false
	}
	_, ok := runtimeOperationFailure(err, "", "")
	return ok
}
