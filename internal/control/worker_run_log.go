package control

import (
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"

	"github.com/helmrdotdev/helmr/internal/api"
	"github.com/helmrdotdev/helmr/internal/db"
	"github.com/helmrdotdev/helmr/internal/pgvalue"
	"github.com/jackc/pgx/v5/pgtype"
)

func (s *Server) workerAppendRunLogs(w http.ResponseWriter, r *http.Request) {
	if s.db == nil {
		writeError(w, unavailable(errors.New("run storage is not configured")))
		return
	}
	var request api.WorkerRunLogAppendRequest
	decoder := json.NewDecoder(r.Body)
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&request); err != nil {
		if errors.Is(err, io.EOF) {
			err = errors.New("request body is required")
		}
		writeError(w, badRequest(fmt.Errorf("invalid worker log request JSON: %w", err)))
		return
	}
	var trailing any
	if err := decoder.Decode(&trailing); !errors.Is(err, io.EOF) {
		writeError(w, badRequest(errors.New("invalid worker log request JSON: trailing value")))
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
	parsed, err := parseRunLeaseReceipt(request.Lease)
	if err != nil {
		writeError(w, badRequest(err))
		return
	}
	worker := workerFromContext(r.Context())
	if request.Lease.WorkerGroupID != worker.WorkerGroupID ||
		parsed.workerInstanceID != worker.WorkerInstanceID ||
		request.Lease.WorkerEpoch != worker.WorkerEpoch ||
		request.Lease.WorkerProtocolVersion != worker.ProtocolVersion {
		writeError(w, forbidden(errors.New("worker Run Lease receipt belongs to another worker epoch")))
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
	row, err := s.db.AppendReceiptRunLogChunk(r.Context(), db.AppendReceiptRunLogChunkParams{
		Kind:                       kind,
		Payload:                    payload,
		WorkspaceMountID:           pgvalue.UUID(parsed.workspaceMountID),
		WorkspaceLeaseID:           pgvalue.UUID(parsed.workspaceLeaseID),
		RunLeaseID:                 pgvalue.UUID(parsed.leaseID),
		RunID:                      pgvalue.UUID(parsed.runID),
		AttemptNumber:              request.Lease.AttemptNumber,
		LeaseSequence:              request.Lease.LeaseSequence,
		WorkerGroupID:              request.Lease.WorkerGroupID,
		WorkerInstanceID:           pgvalue.UUID(parsed.workerInstanceID),
		WorkerEpoch:                request.Lease.WorkerEpoch,
		WorkerProtocolVersion:      request.Lease.WorkerProtocolVersion,
		RuntimeInstanceID:          pgvalue.UUID(parsed.runtimeInstanceID),
		RuntimeIdentityID:          request.Lease.RuntimeIdentityID,
		NetworkSlotID:              pgvalue.UUID(parsed.networkSlotID),
		NetworkSlotGeneration:      request.Lease.NetworkSlotGeneration,
		WorkspaceID:                pgvalue.UUID(parsed.workspaceID),
		BaseWorkspaceVersionID:     pgvalue.UUID(parsed.baseWorkspaceVersionID),
		MountFencingGeneration:     request.Lease.MountFencingGeneration,
		OwnershipGeneration:        request.Lease.OwnershipGeneration,
		WriterGeneration:           request.Lease.WriterGeneration,
		RequestedCpuMillis:         request.Lease.RequestedCPUMillis,
		RequestedMemoryBytes:       request.Lease.RequestedMemoryBytes,
		RequestedWorkloadDiskBytes: request.Lease.RequestedWorkloadDiskBytes,
		RequestedScratchBytes:      request.Lease.RequestedScratchBytes,
		RequestedExecutionSlots:    request.Lease.RequestedExecutionSlots,
		MaxActiveDurationMs:        request.Lease.MaxActiveDurationMs,
		ActiveElapsedMs:            request.Lease.ActiveElapsedMs,
		TraceID:                    pgtype.Text{String: request.Lease.Trace.TraceID, Valid: true},
		SpanID:                     pgtype.Text{String: request.Lease.Trace.SpanID, Valid: true},
		Traceparent:                pgtype.Text{String: request.Lease.Trace.Traceparent, Valid: true},
		StartDeadlineAt:            pgtype.Timestamptz{Time: request.Lease.StartDeadlineAt, Valid: true},
		ExpiresAt:                  pgtype.Timestamptz{Time: request.Lease.ExpiresAt, Valid: true},
		Stream:                     string(request.Stream),
		ObservedSeq:                int64(request.ObservedSeq),
		Content:                    content,
	})
	if isNoRows(err) {
		writeError(w, conflict(errors.New("worker run lease is stale or the log chunk sequence contains different content")))
		return
	}
	if err != nil {
		s.log.Error("append worker logs failed", "run_id", request.Lease.RunID, "error", err)
		writeError(w, errors.New("append worker logs"))
		return
	}
	if !row.ReplayMatches {
		writeError(w, conflict(errors.New("worker log chunk sequence already contains different content")))
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

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
	row, err := s.db.AppendRunLogChunk(r.Context(), db.AppendRunLogChunkParams{
		OrgID:            pgvalue.UUID(leaseIDs.orgID),
		RunID:            pgvalue.UUID(leaseIDs.runID),
		RunLeaseID:       pgvalue.UUID(leaseIDs.runLeaseID),
		WorkerInstanceID: pgvalue.UUID(worker.WorkerInstanceID),
		Stream:           string(request.Stream),
		ObservedSeq:      int64(request.ObservedSeq),
		Content:          content,
		Kind:             kind,
		Payload:          payload,
	})
	if isNoRows(err) {
		writeError(w, conflict(errors.New("worker run lease is stale or the log chunk sequence contains different content")))
		return
	}
	if err != nil {
		s.log.Error("append worker logs failed", "run_id", request.Lease.RunID, "error", err)
		writeError(w, errors.New("append worker logs"))
		return
	}
	if !row.ReplayMatches {
		writeError(w, conflict(errors.New("worker log chunk sequence already contains different content")))
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
