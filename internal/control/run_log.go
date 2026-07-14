package control

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strings"

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
	if s.eventStream == nil {
		writeError(w, unavailable(errors.New("event stream is not configured")))
		return
	}
	flusher, _ := w.(http.Flusher)
	w.Header().Set("content-type", "text/event-stream")
	w.Header().Set("cache-control", "no-cache")
	w.WriteHeader(http.StatusOK)
	encoder := json.NewEncoder(w)
	ctx, cancel := context.WithTimeout(r.Context(), runLogStreamFollowMaxDuration)
	defer cancel()
	err := s.eventStream.ReadRunLogs(ctx, orgID, runID, cursor, func(chunk api.RunLogChunk) error {
		_, _ = fmt.Fprintf(w, "id: %s\n", chunk.ID)
		_, _ = fmt.Fprint(w, "event: run_log\n")
		_, _ = fmt.Fprint(w, "data: ")
		if err := encoder.Encode(chunk); err != nil {
			return err
		}
		_, _ = fmt.Fprint(w, "\n")
		if flusher != nil {
			flusher.Flush()
		}
		next, err := telemetry.ParseCursor(chunk.ID)
		if err != nil {
			return err
		}
		cursor = next
		return nil
	}, func() error {
		run, err := s.db.GetRunSummary(ctx, db.GetRunSummaryParams{OrgID: pgvalue.UUID(orgID), ID: pgvalue.UUID(runID)})
		if isNoRows(err) || (err == nil && api.RunStatusIsTerminal(string(run.Status))) {
			pending, pendingErr := s.hasUnpublishedRunLogs(ctx, orgID, runID)
			if pendingErr != nil {
				return pendingErr
			}
			if !pending {
				return errLiveTelemetryFollowComplete
			}
		}
		if err != nil && !errors.Is(err, context.Canceled) && !errors.Is(err, context.DeadlineExceeded) {
			s.log.Warn("read run status while following logs failed", "run_id", runID.String(), "error", err)
		}
		_, _ = fmt.Fprint(w, ": keep-alive\n\n")
		if flusher != nil {
			flusher.Flush()
		}
		return nil
	})
	if err != nil && !errors.Is(err, errLiveTelemetryFollowComplete) && !errors.Is(err, context.Canceled) && !errors.Is(err, context.DeadlineExceeded) {
		s.log.Warn("follow run logs failed", "run_id", runID.String(), "error", err)
	}
}

func (s *Server) hasUnpublishedRunLogs(ctx context.Context, orgID uuid.UUID, runID uuid.UUID) (bool, error) {
	for _, stream := range []api.WorkerLogStream{api.WorkerLogStreamStdout, api.WorkerLogStreamStderr} {
		pending, err := s.db.HasUnpublishedLiveTelemetryOutbox(ctx, db.HasUnpublishedLiveTelemetryOutboxParams{
			OrgID:      pgvalue.UUID(orgID),
			StreamKind: db.TelemetryStreamKindRunLog,
			SourceKind: "run",
			SourceID:   pgvalue.UUID(runID),
			StreamName: string(stream),
		})
		if err != nil {
			return false, err
		}
		if pending {
			return true, nil
		}
	}
	return false, nil
}

func (s *Server) hasUnpublishedTerminalOutput(ctx context.Context, orgID uuid.UUID, resourceKind string, resourceID uuid.UUID, streamName string) (bool, error) {
	return s.db.HasUnpublishedLiveTelemetryOutbox(ctx, db.HasUnpublishedLiveTelemetryOutboxParams{
		OrgID:      pgvalue.UUID(orgID),
		StreamKind: db.TelemetryStreamKindTerminalOutput,
		SourceKind: resourceKind,
		SourceID:   pgvalue.UUID(resourceID),
		StreamName: streamName,
	})
}

func runLogChunkResponse(chunk db.AppendRunLogChunkRow) api.RunLogChunk {
	attemptNumber := int32(0)
	if value := pgvalue.Int4Response(chunk.AttemptNumber); value != nil {
		attemptNumber = *value
	}
	return api.RunLogChunk{
		ID:            telemetryCursor(chunk.Seq),
		RunID:         pgvalue.MustUUIDValue(chunk.RunID).String(),
		AttemptNumber: attemptNumber,
		Stream:        string(chunk.Stream),
		ContentBase64: base64.StdEncoding.EncodeToString(chunk.Content),
		Bytes:         pgvalue.Int8Value(chunk.SizeBytes),
		ObservedSeq:   pgvalue.Int8Value(chunk.ObservedSeq),
		At:            pgvalue.Time(chunk.CreatedAt),
	}
}
