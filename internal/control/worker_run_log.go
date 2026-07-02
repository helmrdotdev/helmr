package control

import (
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"

	"github.com/helmrdotdev/helmr/internal/api"
	"github.com/helmrdotdev/helmr/internal/db"
	"github.com/helmrdotdev/helmr/internal/pgvalue"
)

func (s *Server) workerAppendLogs(w http.ResponseWriter, r *http.Request) {
	var request api.WorkerAppendLogRequest
	decoder := json.NewDecoder(r.Body)
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&request); err != nil {
		writeError(w, badRequest(fmt.Errorf("invalid worker log request JSON: %w", err)))
		return
	}
	content, err := base64.StdEncoding.DecodeString(request.ContentBase64)
	if err != nil {
		writeError(w, badRequest(errors.New("log content is not valid base64")))
		return
	}
	kind := "log.stdout"
	switch request.Stream {
	case api.WorkerLogStreamStdout:
	case api.WorkerLogStreamStderr:
		kind = "log.stderr"
	default:
		writeError(w, badRequest(errors.New("stream must be stdout or stderr")))
		return
	}
	if request.ObservedSeq > uint64(^uint64(0)>>1) {
		writeError(w, badRequest(errors.New("observed_seq is too large")))
		return
	}
	worker, leaseIDs, ok := s.workerRunLeaseForWrite(w, r, request.Lease)
	if !ok {
		return
	}
	payload, err := json.Marshal(workerLogChunkPayload{
		RunID:       request.Lease.RunID,
		Stream:      request.Stream,
		ObservedSeq: request.ObservedSeq,
		Bytes:       len(content),
	})
	if err != nil {
		writeError(w, errors.New("encode worker log event"))
		return
	}
	_, err = s.db.AppendRunLogChunk(r.Context(), db.AppendRunLogChunkParams{
		OrgID:            pgvalue.UUID(leaseIDs.orgID),
		RunID:            pgvalue.UUID(leaseIDs.runID),
		RunLeaseID:       pgvalue.UUID(leaseIDs.runLeaseID),
		WorkerInstanceID: pgvalue.UUID(worker.WorkerInstanceID),
		Stream:           db.RunLogStream(request.Stream),
		ObservedSeq:      int64(request.ObservedSeq),
		Content:          content,
		Kind:             kind,
		Payload:          payload,
	})
	if isNoRows(err) {
		writeError(w, conflict(errors.New("worker run lease is stale")))
		return
	}
	if err != nil {
		s.log.Error("append worker logs failed", "run_id", request.Lease.RunID, "error", err)
		writeError(w, errors.New("append worker logs"))
		return
	}
	writeJSON(w, http.StatusOK, api.WorkerEventResponse{RunID: request.Lease.RunID})
}

func (s *Server) workerRecordLogEntry(w http.ResponseWriter, r *http.Request) {
	var request api.WorkerRecordLogEntryRequest
	decoder := json.NewDecoder(r.Body)
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&request); err != nil {
		writeError(w, badRequest(fmt.Errorf("invalid worker log entry request JSON: %w", err)))
		return
	}
	payload, err := json.Marshal(workerMessagePayload{Message: request.Entry})
	if err != nil {
		writeError(w, errors.New("encode worker log entry"))
		return
	}
	s.appendWorkerEvent(w, r, request.Lease, "log", payload)
}

type workerLogChunkPayload struct {
	Bytes       int                 `json:"bytes"`
	ObservedSeq uint64              `json:"observed_seq"`
	RunID       string              `json:"run_id"`
	Stream      api.WorkerLogStream `json:"stream"`
}
