package controlplane

import (
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"

	"github.com/helmrdotdev/helmr/internal/run"
	"github.com/helmrdotdev/helmr/internal/telemetry"
	"github.com/helmrdotdev/helmr/internal/workerapi"
)

func (s *Server) workerAppendRunLogs(w http.ResponseWriter, r *http.Request) {
	var request workerapi.RunLogAppendRequest
	if err := decodeRequestJSON(r, &request); err != nil {
		writeError(w, fmt.Errorf("invalid worker log request JSON: %w", err))
		return
	}
	content, err := base64.StdEncoding.DecodeString(request.ContentBase64)
	if err != nil {
		writeError(w, badRequest(errors.New("log content is not valid base64")))
		return
	}
	if err := telemetry.ValidateRunLog(content); err != nil {
		writeError(w, badRequest(err))
		return
	}
	kind := "log.stdout"
	switch request.Stream {
	case workerapi.LogStreamStdout:
	case workerapi.LogStreamStderr:
		kind = "log.stderr"
	default:
		writeError(w, badRequest(errors.New("stream must be stdout or stderr")))
		return
	}
	if request.ObservedSeq > uint64(^uint64(0)>>1) {
		writeError(w, badRequest(errors.New("observed_seq is too large")))
		return
	}
	parsed, err := parseRunLeaseFence(request.Lease)
	if err != nil {
		writeError(w, badRequest(err))
		return
	}
	worker := workerFromContext(r.Context())
	payload, err := json.Marshal(workerLogChunkPayload{
		Stream:      request.Stream,
		ObservedSeq: request.ObservedSeq,
		Bytes:       len(content),
	})
	if err != nil {
		writeError(w, errors.New("encode worker log event"))
		return
	}
	fenceFingerprint, err := runLeaseFenceFingerprint(request.Lease)
	if err != nil {
		s.writeRunError(w, err, runLogAppendOperation, worker, request.Lease)
		return
	}
	if err := run.AppendLog(r.Context(), s.db, run.LogChunk{
		Fence:            workerExecutionFence(worker, parsed, request.Lease),
		FenceFingerprint: fenceFingerprint,
		Kind:             kind,
		Payload:          payload,
		Severity:         "info",
		Stream:           string(request.Stream),
		ObservedSeq:      int64(request.ObservedSeq),
		Content:          content,
	}); err != nil {
		s.writeRunError(w, err, runLogAppendOperation, worker, request.Lease)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func runLeaseFenceFingerprint(lease workerapi.RunLeaseFence) (string, error) {
	canonical, err := canonicalJSON(mustJSON(lease))
	if err != nil {
		return "", fmt.Errorf("canonicalize run lease fence: %w", err)
	}
	digest := sha256.Sum256(canonical)
	return hex.EncodeToString(digest[:]), nil
}

type workerLogChunkPayload struct {
	Bytes       int                 `json:"bytes"`
	ObservedSeq uint64              `json:"observed_seq"`
	Stream      workerapi.LogStream `json:"stream"`
}
