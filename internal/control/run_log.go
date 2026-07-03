package control

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/helmrdotdev/helmr/internal/api"
	"github.com/helmrdotdev/helmr/internal/auth"
	"github.com/helmrdotdev/helmr/internal/db"
	"github.com/helmrdotdev/helmr/internal/pgvalue"
	"github.com/helmrdotdev/helmr/internal/telemetry"
)

func (s *Server) getRunLogs(w http.ResponseWriter, r *http.Request) {
	if s.db == nil {
		writeError(w, unavailable(errors.New("run storage is not configured")))
		return
	}
	runID, err := parseUUIDParam(r, "id")
	if err != nil {
		writeError(w, badRequest(err))
		return
	}
	actor := actorFromContext(r.Context())
	run, err := s.db.GetRunSummary(r.Context(), db.GetRunSummaryParams{OrgID: pgvalue.UUID(actor.OrgID), ID: pgvalue.UUID(runID)})
	if isNoRows(err) {
		writeError(w, notFound(errors.New("run not found")))
		return
	} else if err != nil {
		s.log.Error("get run before logs failed", "run_id", runID.String(), "error", err)
		writeError(w, errors.New("get run logs"))
		return
	}
	summary := getRunSummary(run)
	scope := auth.Scope{
		OrgID:         actor.OrgID,
		ProjectID:     pgvalue.MustUUIDValue(summary.ProjectID).String(),
		EnvironmentID: pgvalue.MustUUIDValue(summary.EnvironmentID).String(),
	}
	if err := s.requireActorScopeForRecord(r, actor, summary.ProjectID, summary.EnvironmentID); err != nil {
		if isNoRows(err) {
			writeError(w, notFound(errors.New("run not found")))
			return
		}
		writeError(w, badRequest(err))
		return
	}
	if !actor.HasPermission(auth.PermissionRunsRead, scope) {
		writeError(w, forbidden(errors.New("permission is required")))
		return
	}
	if s.rejectRunFromWrongCell(w, summary.CellID) {
		return
	}
	if r.URL.Query().Get("follow") == "1" || strings.Contains(r.Header.Get("accept"), "text/event-stream") {
		cursor, err := eventCursor(r)
		if err != nil {
			writeError(w, badRequest(err))
			return
		}
		s.followRunLogs(w, r, actor.OrgID, runID, cursor)
		return
	}
	logs, err := s.telemetryReader.GetRunLogSnapshot(r.Context(), telemetry.RunLogSnapshotQuery{
		StdoutLimit: maxRunLogSnapshotBytes,
		StderrLimit: maxRunLogSnapshotBytes,
		OrgID:       actor.OrgID,
		CellID:      s.cellID,
		RunID:       runID,
	})
	if err != nil {
		s.log.Error("get run logs failed", "run_id", runID.String(), "error", err)
		writeRunTelemetryError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, api.LogSnapshotResponse{
		StdoutBase64: base64.StdEncoding.EncodeToString(logs.Stdout),
		StderrBase64: base64.StdEncoding.EncodeToString(logs.Stderr),
		Cursor:       telemetryCursor(logs.Cursor),
		StdoutBytes:  logs.StdoutBytes,
		StderrBytes:  logs.StderrBytes,
		Truncated:    logs.Truncated,
	})
}

func (s *Server) followRunLogs(w http.ResponseWriter, r *http.Request, orgID uuid.UUID, runID uuid.UUID, cursor int64) {
	flusher, _ := w.(http.Flusher)
	w.Header().Set("content-type", "text/event-stream")
	w.Header().Set("cache-control", "no-cache")
	w.WriteHeader(http.StatusOK)
	encoder := json.NewEncoder(w)
	ctx, cancel := context.WithTimeout(r.Context(), runLogStreamFollowMaxDuration)
	defer cancel()
	ticker := time.NewTicker(runLogStreamPollInterval)
	defer ticker.Stop()
	for {
		nextCursor, rowCount, err := s.writeRunLogChunksAfter(ctx, w, flusher, encoder, orgID, runID, cursor)
		if err != nil {
			if !errors.Is(err, context.Canceled) && !errors.Is(err, context.DeadlineExceeded) {
				s.log.Warn("follow run logs failed", "run_id", runID.String(), "error", err)
			}
			return
		}
		cursor = nextCursor
		if rowCount == int(runLogStreamBatchSize) {
			continue
		}
		run, err := s.db.GetRunSummary(ctx, db.GetRunSummaryParams{OrgID: pgvalue.UUID(orgID), ID: pgvalue.UUID(runID)})
		if isNoRows(err) || (err == nil && api.RunStatusIsTerminal(string(run.Status))) {
			for {
				nextCursor, rowCount, err := s.writeRunLogChunksAfter(ctx, w, flusher, encoder, orgID, runID, cursor)
				if err != nil {
					if !errors.Is(err, context.Canceled) && !errors.Is(err, context.DeadlineExceeded) {
						s.log.Warn("drain terminal run logs failed", "run_id", runID.String(), "error", err)
					}
					return
				}
				cursor = nextCursor
				if rowCount < int(runLogStreamBatchSize) {
					return
				}
			}
		}
		if err != nil && !errors.Is(err, context.Canceled) && !errors.Is(err, context.DeadlineExceeded) {
			s.log.Warn("read run status while following logs failed", "run_id", runID.String(), "error", err)
		}
		_, _ = fmt.Fprint(w, ": keep-alive\n\n")
		if flusher != nil {
			flusher.Flush()
		}
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}
	}
}

func (s *Server) writeRunLogChunksAfter(ctx context.Context, w http.ResponseWriter, flusher http.Flusher, encoder *json.Encoder, orgID uuid.UUID, runID uuid.UUID, cursor int64) (int64, int, error) {
	page, err := s.telemetryReader.ListRunLogChunks(ctx, telemetry.RunLogChunkQuery{
		OrgID:    orgID,
		CellID:   s.cellID,
		RunID:    runID,
		AfterSeq: cursor,
		Limit:    runLogStreamBatchSize,
	})
	if err != nil {
		return cursor, 0, err
	}
	for _, chunk := range page.Chunks {
		_, _ = fmt.Fprintf(w, "id: %s\n", chunk.ID)
		_, _ = fmt.Fprint(w, "event: run_log\n")
		_, _ = fmt.Fprint(w, "data: ")
		if err := encoder.Encode(chunk); err != nil {
			return cursor, 0, err
		}
		_, _ = fmt.Fprint(w, "\n")
		if flusher != nil {
			flusher.Flush()
		}
		next, err := telemetry.ParseCursor(chunk.ID)
		if err != nil {
			return cursor, 0, err
		}
		cursor = next
	}
	return cursor, len(page.Chunks), nil
}

func runLogChunkResponse(chunk db.RunLogHotChunk) api.RunLogChunk {
	return api.RunLogChunk{
		ID:            telemetryCursor(chunk.Seq),
		RunID:         pgvalue.MustUUIDValue(chunk.RunID).String(),
		RunLeaseID:    pgvalue.MustUUIDValue(chunk.RunLeaseID).String(),
		AttemptNumber: chunk.AttemptNumber,
		Stream:        string(chunk.Stream),
		ContentBase64: base64.StdEncoding.EncodeToString(chunk.Content),
		Bytes:         chunk.SizeBytes,
		ObservedSeq:   chunk.ObservedSeq,
		At:            pgvalue.Time(chunk.CreatedAt),
	}
}
