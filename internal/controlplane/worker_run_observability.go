package controlplane

import (
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"net/http"
	"unicode/utf8"

	"github.com/helmrdotdev/helmr/internal/run"
	"github.com/helmrdotdev/helmr/internal/telemetry"
	"github.com/helmrdotdev/helmr/internal/workerapi"
	"github.com/helmrdotdev/helmr/internal/workergroup"
)

const (
	maxStructuredMessageBytes   = 4 << 10
	maxStructuredAttributeBytes = 16 << 10
)

func (s *Server) workerUpdateRunMetadata(w http.ResponseWriter, r *http.Request) {
	var request workerapi.UpdateRunMetadataRequest
	if err := decodeRequestJSON(r, &request); err != nil {
		writeError(w, fmt.Errorf("invalid worker run metadata request JSON: %w", err))
		return
	}
	operationID, err := parseCanonicalUUID("operation_id", request.OperationID)
	if err != nil {
		writeError(w, badRequest(err))
		return
	}
	mutation, err := run.NewMetadataMutation(request.Operation, request.Key, request.Value, request.Patch, request.Amount)
	if err != nil {
		writeError(w, badRequest(publicJSONDecodeError(err)))
		return
	}
	parsed, worker, err := s.parseWorkerRunMutation(r, request.Lease)
	if err != nil {
		writeError(w, err)
		return
	}
	leaseFenceFingerprint, err := runLeaseFenceFingerprint(request.Lease)
	if err != nil {
		writeError(w, badRequest(err))
		return
	}
	err = run.UpdateMetadata(r.Context(), s.tx, run.MetadataUpdate{
		Fence:            workerExecutionFence(worker, parsed, request.Lease),
		OperationID:      operationID,
		Mutation:         mutation,
		FenceFingerprint: leaseFenceFingerprint,
		Event: func() (json.RawMessage, error) {
			payload, err := json.Marshal(map[string]any{
				"operation":    mutation.Operation(),
				"operation_id": operationID.String(),
				"key":          mutation.Key(),
			})
			if err != nil {
				return nil, err
			}
			return payload, telemetry.ValidateEvent("Run metadata updated", payload)
		},
	})
	if err != nil {
		s.writeRunError(w, err, runMetadataOperation, worker, request.Lease)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (s *Server) workerAppendStructuredLog(w http.ResponseWriter, r *http.Request) {
	var request workerapi.StructuredLogRequest
	if err := decodeRequestJSON(r, &request); err != nil {
		writeError(w, fmt.Errorf("invalid worker structured log request JSON: %w", err))
		return
	}
	if request.ObservedSeq > uint64(math.MaxInt64) {
		writeError(w, badRequest(errors.New("observed_seq is too large")))
		return
	}
	switch request.Level {
	case "debug", "info", "warn", "error":
	default:
		writeError(w, badRequest(errors.New("level must be debug, info, warn, or error")))
		return
	}
	if !utf8.ValidString(request.Message) ||
		len([]byte(request.Message)) > maxStructuredMessageBytes {
		writeError(w, badRequest(fmt.Errorf(
			"message must be valid UTF-8 no larger than %d bytes",
			maxStructuredMessageBytes,
		)))
		return
	}
	attributes, err := run.NormalizeMetadata(
		request.Attributes,
		maxStructuredAttributeBytes,
		"structured log attributes",
	)
	if err != nil {
		writeError(w, badRequest(err))
		return
	}
	content, err := canonicalJSON(mustJSON(map[string]any{
		"level": request.Level, "message": request.Message,
		"attributes": json.RawMessage(attributes),
	}))
	if err != nil {
		writeError(w, badRequest(err))
		return
	}
	if err := telemetry.ValidateRunLog(content); err != nil {
		writeError(w, badRequest(err))
		return
	}
	parsed, worker, err := s.parseWorkerRunMutation(r, request.Lease)
	if err != nil {
		writeError(w, err)
		return
	}
	payload, err := json.Marshal(map[string]any{
		"level": request.Level, "message": request.Message,
		"attributes":   json.RawMessage(attributes),
		"observed_seq": request.ObservedSeq,
	})
	if err != nil {
		writeError(w, errors.New("encode structured log"))
		return
	}
	fenceFingerprint, err := runLeaseFenceFingerprint(request.Lease)
	if err != nil {
		s.writeRunError(w, err, runStructuredLogAppendOperation, worker, request.Lease)
		return
	}
	if err := run.AppendLog(r.Context(), s.db, run.LogChunk{
		Fence:            workerExecutionFence(worker, parsed, request.Lease),
		FenceFingerprint: fenceFingerprint,
		Kind:             "log.structured", Payload: payload, Severity: request.Level,
		Stream:      string(workerapi.LogStreamStructured),
		ObservedSeq: int64(request.ObservedSeq), Content: content,
	}); err != nil {
		s.writeRunError(w, err, runStructuredLogAppendOperation, worker, request.Lease)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (s *Server) parseWorkerRunMutation(
	r *http.Request,
	lease workerapi.RunLeaseFence,
) (parsedRunLeaseFence, workergroup.HostPrincipal, error) {
	parsed, err := parseRunLeaseFence(lease)
	if err != nil {
		return parsedRunLeaseFence{}, workergroup.HostPrincipal{}, badRequest(err)
	}
	worker := workerFromContext(r.Context())
	return parsed, worker, nil
}

func mustJSON(value any) []byte {
	raw, err := json.Marshal(value)
	if err != nil {
		panic(err)
	}
	return raw
}
