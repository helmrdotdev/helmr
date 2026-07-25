package control

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"

	"github.com/helmrdotdev/helmr/internal/api"
	"github.com/helmrdotdev/helmr/internal/db"
	"github.com/helmrdotdev/helmr/internal/jsoncanon"
	"github.com/helmrdotdev/helmr/internal/pgvalue"
	"github.com/jackc/pgx/v5"
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
	row, err := s.appendReceiptRunLog(r.Context(), request.Lease, parsed, db.AppendReceiptRunLogChunkParams{
		Kind:        kind,
		Payload:     payload,
		Severity:    "info",
		Stream:      string(request.Stream),
		ObservedSeq: int64(request.ObservedSeq),
		Content:     content,
	})
	if isNoRows(err) || errors.Is(err, errStaleRunLeaseClaim) {
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

func (s *Server) appendReceiptRunLog(
	ctx context.Context,
	lease api.WorkerRunLeaseReceipt,
	parsed parsedRunLeaseReceipt,
	input db.AppendReceiptRunLogChunkParams,
) (db.AppendReceiptRunLogChunkRow, error) {
	receiptFingerprint, err := runLeaseReceiptFingerprint(lease)
	if err != nil {
		return db.AppendReceiptRunLogChunkRow{}, err
	}
	replay, err := s.db.GetRunLogChunkReplay(ctx, db.GetRunLogChunkReplayParams{
		RunID:      pgvalue.UUID(parsed.runID),
		RunLeaseID: pgvalue.UUID(parsed.leaseID),
		AttemptNumber: pgtype.Int4{
			Int32: lease.AttemptNumber,
			Valid: true,
		},
		Stream: input.Stream,
		ObservedSeq: pgtype.Int8{
			Int64: input.ObservedSeq,
			Valid: true,
		},
	})
	switch {
	case err == nil:
		payloadMatches, compareErr := equalJSON(
			[]byte(replay.EventPayload),
			input.Payload,
		)
		if compareErr != nil {
			return db.AppendReceiptRunLogChunkRow{}, compareErr
		}
		return db.AppendReceiptRunLogChunkRow{
			OrgID: replay.OrgID, RunID: replay.RunID,
			RunLeaseID: replay.RunLeaseID, AttemptNumber: replay.AttemptNumber,
			Stream: replay.Stream, Seq: replay.Seq, ObservedSeq: replay.ObservedSeq,
			Content: replay.Content, SizeBytes: replay.SizeBytes, CreatedAt: replay.CreatedAt,
			ReplayMatches: bytes.Equal(replay.Content, input.Content) &&
				payloadMatches &&
				replay.ReceiptFingerprint == receiptFingerprint,
		}, nil
	case !errors.Is(err, pgx.ErrNoRows):
		return db.AppendReceiptRunLogChunkRow{}, err
	}
	input.WorkspaceMountID = pgvalue.UUID(parsed.workspaceMountID)
	input.WorkspaceLeaseID = pgvalue.UUID(parsed.workspaceLeaseID)
	input.RunLeaseID = pgvalue.UUID(parsed.leaseID)
	input.RunID = pgvalue.UUID(parsed.runID)
	input.AttemptNumber = lease.AttemptNumber
	input.LeaseSequence = lease.LeaseSequence
	input.WorkerGroupID = lease.WorkerGroupID
	input.WorkerInstanceID = pgvalue.UUID(parsed.workerInstanceID)
	input.WorkerEpoch = lease.WorkerEpoch
	input.WorkerProtocolVersion = lease.WorkerProtocolVersion
	input.RuntimeInstanceID = pgvalue.UUID(parsed.runtimeInstanceID)
	input.RuntimeIdentityID = lease.RuntimeIdentityID
	input.NetworkSlotID = pgvalue.UUID(parsed.networkSlotID)
	input.NetworkSlotGeneration = lease.NetworkSlotGeneration
	input.WorkspaceID = pgvalue.UUID(parsed.workspaceID)
	input.BaseWorkspaceVersionID = pgvalue.UUID(parsed.baseWorkspaceVersionID)
	input.MountFencingGeneration = lease.MountFencingGeneration
	input.OwnershipGeneration = lease.OwnershipGeneration
	input.WriterGeneration = lease.WriterGeneration
	input.RequestedCpuMillis = lease.RequestedCPUMillis
	input.RequestedMemoryBytes = lease.RequestedMemoryBytes
	input.RequestedWorkloadDiskBytes = lease.RequestedWorkloadDiskBytes
	input.RequestedScratchBytes = lease.RequestedScratchBytes
	input.RequestedExecutionSlots = lease.RequestedExecutionSlots
	input.MaxActiveDurationMs = lease.MaxActiveDurationMs
	input.ActiveElapsedMs = lease.ActiveElapsedMs
	input.TraceID = pgtype.Text{String: lease.Trace.TraceID, Valid: true}
	input.SpanID = pgtype.Text{String: lease.Trace.SpanID, Valid: true}
	input.Traceparent = pgtype.Text{String: lease.Trace.Traceparent, Valid: true}
	input.StartDeadlineAt = pgtype.Timestamptz{Time: lease.StartDeadlineAt, Valid: true}
	input.ExpiresAt = pgtype.Timestamptz{Time: lease.ExpiresAt, Valid: true}
	input.ReceiptFingerprint = receiptFingerprint
	return s.db.AppendReceiptRunLogChunk(ctx, input)
}

func runMetadataClaimScopeParams(
	lease api.WorkerRunLeaseReceipt,
	parsed parsedRunLeaseReceipt,
) db.GetRunMetadataClaimScopeParams {
	return db.GetRunMetadataClaimScopeParams{
		RunLeaseID: pgvalue.UUID(parsed.leaseID), RunID: pgvalue.UUID(parsed.runID),
		AttemptNumber: lease.AttemptNumber, LeaseSequence: lease.LeaseSequence,
		WorkerGroupID:         lease.WorkerGroupID,
		WorkerInstanceID:      pgvalue.UUID(parsed.workerInstanceID),
		WorkerEpoch:           lease.WorkerEpoch,
		WorkerProtocolVersion: lease.WorkerProtocolVersion,
	}
}

func runLeaseReceiptFingerprint(lease api.WorkerRunLeaseReceipt) (string, error) {
	canonical, err := jsoncanon.Transform(mustJSON(lease))
	if err != nil {
		return "", fmt.Errorf("canonicalize Run Lease receipt: %w", err)
	}
	digest := sha256.Sum256(canonical)
	return hex.EncodeToString(digest[:]), nil
}

func equalJSON(left, right []byte) (bool, error) {
	leftCanonical, err := jsoncanon.Transform(left)
	if err != nil {
		return false, fmt.Errorf("canonicalize stored Run log payload: %w", err)
	}
	rightCanonical, err := jsoncanon.Transform(right)
	if err != nil {
		return false, fmt.Errorf("canonicalize Run log payload: %w", err)
	}
	return bytes.Equal(leftCanonical, rightCanonical), nil
}

type workerLogChunkPayload struct {
	Bytes       int                 `json:"bytes"`
	ObservedSeq uint64              `json:"observed_seq"`
	RunID       string              `json:"run_id"`
	Stream      api.WorkerLogStream `json:"stream"`
}
