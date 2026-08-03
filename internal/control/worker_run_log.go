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

	"github.com/helmrdotdev/helmr/internal/db"
	"github.com/helmrdotdev/helmr/internal/jsoncanon"
	"github.com/helmrdotdev/helmr/internal/pgvalue"
	"github.com/helmrdotdev/helmr/internal/workerapi"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
)

func (s *Server) workerAppendRunLogs(w http.ResponseWriter, r *http.Request) {
	if s.db == nil {
		writeError(w, unavailable(errors.New("run storage is not configured")))
		return
	}
	var request workerapi.RunLogAppendRequest
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
	row, err := s.appendRunLog(r.Context(), worker, request.Lease, parsed, db.AppendRunLogChunkParams{
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
		s.log.Error("append worker logs failed", "run_lease_id", request.Lease.ID, "error", err)
		writeError(w, errors.New("append worker logs"))
		return
	}
	if !row.ReplayMatches {
		writeError(w, conflict(errors.New("worker log chunk sequence already contains different content")))
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (s *Server) appendRunLog(
	ctx context.Context,
	worker workerActor,
	lease workerapi.RunLeaseFence,
	parsed parsedRunLeaseFence,
	input db.AppendRunLogChunkParams,
) (db.AppendRunLogChunkRow, error) {
	fenceFingerprint, err := runLeaseFenceFingerprint(lease)
	if err != nil {
		return db.AppendRunLogChunkRow{}, err
	}
	replay, err := s.db.GetRunLogChunkReplay(ctx, db.GetRunLogChunkReplayParams{
		RunLeaseID: pgvalue.UUID(parsed.leaseID),
		Stream:     input.Stream,
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
			return db.AppendRunLogChunkRow{}, compareErr
		}
		return db.AppendRunLogChunkRow{
			OrgID: replay.OrgID, RunID: replay.RunID,
			RunLeaseID: replay.RunLeaseID, AttemptNumber: replay.AttemptNumber,
			Stream: replay.Stream, Seq: replay.Seq, ObservedSeq: replay.ObservedSeq,
			Content: replay.Content, SizeBytes: replay.SizeBytes, CreatedAt: replay.CreatedAt,
			ReplayMatches: bytes.Equal(replay.Content, input.Content) &&
				payloadMatches &&
				replay.LeaseFenceFingerprint == fenceFingerprint,
		}, nil
	case !errors.Is(err, pgx.ErrNoRows):
		return db.AppendRunLogChunkRow{}, err
	}
	input.RunLeaseID = pgvalue.UUID(parsed.leaseID)
	input.LeaseSequence = lease.LeaseSequence
	input.WorkerGroupID = worker.WorkerGroupID
	input.WorkerInstanceID = pgvalue.UUID(worker.WorkerInstanceID)
	input.WorkerEpoch = worker.WorkerEpoch
	input.WorkerProtocolVersion = worker.ProtocolVersion
	input.LeaseFenceFingerprint = fenceFingerprint
	return s.db.AppendRunLogChunk(ctx, input)
}

func runMetadataClaimScopeParams(
	lease workerapi.RunLeaseFence,
	parsed parsedRunLeaseFence,
	worker workerActor,
) db.GetRunMetadataClaimScopeParams {
	return db.GetRunMetadataClaimScopeParams{
		RunLeaseID: pgvalue.UUID(parsed.leaseID), LeaseSequence: lease.LeaseSequence,
		WorkerGroupID:         worker.WorkerGroupID,
		WorkerInstanceID:      pgvalue.UUID(worker.WorkerInstanceID),
		WorkerEpoch:           worker.WorkerEpoch,
		WorkerProtocolVersion: worker.ProtocolVersion,
	}
}

func runLeaseFenceFingerprint(lease workerapi.RunLeaseFence) (string, error) {
	canonical, err := jsoncanon.Transform(mustJSON(lease))
	if err != nil {
		return "", fmt.Errorf("canonicalize Run Lease fence: %w", err)
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
	Stream      workerapi.LogStream `json:"stream"`
}
