package control

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/helmrdotdev/helmr/internal/api"
	"github.com/helmrdotdev/helmr/internal/auth"
	"github.com/helmrdotdev/helmr/internal/db"
	"github.com/helmrdotdev/helmr/internal/ids"
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
	run, err := s.db.GetRunSummary(r.Context(), db.GetRunSummaryParams{OrgID: ids.ToPG(actor.OrgID), ID: ids.ToPG(runID)})
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
		ProjectID:     ids.MustFromPG(summary.ProjectID).String(),
		EnvironmentID: ids.MustFromPG(summary.EnvironmentID).String(),
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
	if r.URL.Query().Get("follow") == "1" || strings.Contains(r.Header.Get("accept"), "text/event-stream") {
		cursor, err := eventCursor(r)
		if err != nil {
			writeError(w, badRequest(err))
			return
		}
		s.followRunLogs(w, r, actor.OrgID, runID, cursor)
		return
	}
	logs, err := s.db.GetRunLogSnapshot(r.Context(), db.GetRunLogSnapshotParams{
		StdoutLimit: maxRunLogSnapshotBytes,
		StderrLimit: maxRunLogSnapshotBytes,
		OrgID:       ids.ToPG(actor.OrgID),
		RunID:       ids.ToPG(runID),
	})
	if isNoRows(err) {
		writeJSON(w, http.StatusOK, api.LogSnapshotResponse{Cursor: "0"})
		return
	}
	if err != nil {
		s.log.Error("get run logs failed", "run_id", runID.String(), "error", err)
		writeError(w, errors.New("get run logs"))
		return
	}
	writeJSON(w, http.StatusOK, api.LogSnapshotResponse{
		StdoutBase64: base64.StdEncoding.EncodeToString(logs.Stdout),
		StderrBase64: base64.StdEncoding.EncodeToString(logs.Stderr),
		Cursor:       strconv.FormatInt(logs.Cursor, 10),
		StdoutBytes:  logs.StdoutBytes,
		StderrBytes:  logs.StderrBytes,
		Truncated:    logs.Truncated.Bool,
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
		run, err := s.db.GetRunSummary(ctx, db.GetRunSummaryParams{OrgID: ids.ToPG(orgID), ID: ids.ToPG(runID)})
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
	rows, err := s.db.ListRunLogChunksAfter(ctx, db.ListRunLogChunksAfterParams{
		OrgID:    ids.ToPG(orgID),
		RunID:    ids.ToPG(runID),
		Seq:      cursor,
		RowLimit: runLogStreamBatchSize,
	})
	if err != nil {
		return cursor, 0, err
	}
	for _, row := range rows {
		chunk := runLogChunkResponse(row)
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
		cursor = row.Seq
	}
	return cursor, len(rows), nil
}

func runLogChunkResponse(chunk db.RunLogChunk) api.RunLogChunk {
	return api.RunLogChunk{
		ID:            strconv.FormatInt(chunk.Seq, 10),
		RunID:         ids.MustFromPG(chunk.RunID).String(),
		SessionID:     ids.MustFromPG(chunk.SessionID).String(),
		AttemptNumber: chunk.AttemptNumber,
		Stream:        string(chunk.Stream),
		ContentBase64: base64.StdEncoding.EncodeToString(chunk.Content),
		Bytes:         chunk.SizeBytes,
		ObservedSeq:   chunk.ObservedSeq,
		At:            pgTime(chunk.CreatedAt),
	}
}
