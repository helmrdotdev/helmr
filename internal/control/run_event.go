package control

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strconv"
	"strings"

	"github.com/google/uuid"
	"github.com/helmrdotdev/helmr/internal/api"
	"github.com/helmrdotdev/helmr/internal/auth"
	"github.com/helmrdotdev/helmr/internal/db"
	"github.com/helmrdotdev/helmr/internal/pgvalue"
	"github.com/jackc/pgx/v5/pgtype"
)

func (s *Server) getRunEvents(w http.ResponseWriter, r *http.Request) {
	if s.db == nil {
		writeError(w, unavailable(errors.New("run storage is not configured")))
		return
	}
	runID, err := parseUUIDParam(r, "id")
	if err != nil {
		writeError(w, badRequest(err))
		return
	}
	cursor, err := eventCursor(r)
	if err != nil {
		writeError(w, badRequest(err))
		return
	}
	limit, err := eventLimit(r)
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
		s.log.Error("get run before events failed", "run_id", runID.String(), "error", err)
		writeError(w, errors.New("list run events"))
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
		s.followRunEvents(w, r, actor.OrgID, runID, cursor)
		return
	}
	rows, err := s.listRunEvents(r, pgvalue.UUID(actor.OrgID), pgvalue.UUID(runID), cursor, limit)
	if err != nil {
		s.log.Error("list run events failed", "run_id", runID.String(), "error", err)
		writeError(w, errors.New("list run events"))
		return
	}
	hasNext := len(rows) > int(limit)
	if hasNext {
		rows = rows[:limit]
	}
	events := make([]api.RunEvent, 0, len(rows))
	for _, row := range rows {
		events = append(events, runEventResponse(row))
	}
	var nextCursor *string
	if hasNext {
		value := telemetryCursor(rows[len(rows)-1].Seq)
		nextCursor = &value
	}
	writeJSON(w, http.StatusOK, api.RunEventPage{Events: events, Cursor: telemetryCursor(cursor), NextCursor: nextCursor})
}

func (s *Server) listRunEvents(r *http.Request, orgID pgtype.UUID, runID pgtype.UUID, cursor int64, limit int32) ([]db.EventHotPayload, error) {
	return s.db.ListSubjectEvents(r.Context(), db.ListSubjectEventsParams{
		OrgID:       orgID,
		SubjectType: db.EventSubjectTypeRun,
		SubjectID:   runID,
		Seq:         cursor,
		RowLimit:    limit + 1,
	})
}

func eventCursor(r *http.Request) (int64, error) {
	value := strings.TrimSpace(r.Header.Get("Last-Event-ID"))
	if value == "" {
		value = strings.TrimSpace(r.URL.Query().Get("cursor"))
	}
	if value == "" {
		return 0, nil
	}
	return parseTelemetryCursor(value)
}

func eventLimit(r *http.Request) (int32, error) {
	value := strings.TrimSpace(r.URL.Query().Get("limit"))
	if value == "" {
		return runEventsPageSize, nil
	}
	parsed, err := strconv.ParseInt(value, 10, 32)
	if err != nil || parsed < 1 || parsed > int64(runEventsPageSize) {
		return 0, fmt.Errorf("limit must be an integer between 1 and %d", runEventsPageSize)
	}
	return int32(parsed), nil
}

func (s *Server) followRunEvents(w http.ResponseWriter, r *http.Request, orgID uuid.UUID, runID uuid.UUID, cursor int64) {
	if s.eventStream == nil {
		writeError(w, unavailable(errors.New("event stream is not configured")))
		return
	}
	flusher, _ := w.(http.Flusher)
	w.Header().Set("content-type", "text/event-stream")
	w.Header().Set("cache-control", "no-cache")
	w.WriteHeader(http.StatusOK)
	encoder := json.NewEncoder(w)
	ctx, cancel := context.WithTimeout(r.Context(), runEventsFollowMaxDuration)
	defer cancel()
	err := s.eventStream.ReadSubject(ctx, orgID, db.EventSubjectTypeRun, runID, cursor, func(event api.RunEvent) error {
		_, _ = fmt.Fprintf(w, "id: %s\n", event.ID)
		_, _ = fmt.Fprint(w, "event: run_event\n")
		_, _ = fmt.Fprint(w, "data: ")
		if err := encoder.Encode(event); err != nil {
			return err
		}
		_, _ = fmt.Fprint(w, "\n")
		if flusher != nil {
			flusher.Flush()
		}
		if api.RunEventKindIsTerminal(event.Kind) {
			cancel()
		}
		return nil
	}, func() error {
		_, _ = fmt.Fprint(w, ": keep-alive\n\n")
		if flusher != nil {
			flusher.Flush()
		}
		return nil
	})
	if err != nil && !errors.Is(err, context.Canceled) && !errors.Is(err, context.DeadlineExceeded) {
		s.log.Warn("follow run events failed", "error", err)
	}
}

func runEventResponse(event db.EventHotPayload) api.RunEvent {
	return eventResponseFromRecord(event)
}
