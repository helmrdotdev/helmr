package controlplane

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"uuid"

	"github.com/helmrdotdev/helmr/internal/api"
	"github.com/helmrdotdev/helmr/internal/deployment"
	"github.com/helmrdotdev/helmr/internal/pgvalue"
	"github.com/helmrdotdev/helmr/internal/telemetry"
)

func (s *Server) getDeploymentEvents(w http.ResponseWriter, r *http.Request) {
	deploymentID, err := parseUUIDParam(r, "deploymentID")
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
	scope, err := s.requestedRunListScope(r, actor)
	if err != nil {
		writeError(w, badRequest(err))
		return
	}
	record, err := deployment.Get(r.Context(), s.db, actor, scope, deploymentID)
	if err != nil {
		s.writeDeploymentError(w, err)
		return
	}
	if r.URL.Query().Get("follow") == "1" || strings.Contains(r.Header.Get("accept"), "text/event-stream") {
		s.followDeploymentEvents(w, r, actor.OrgID, deploymentID, cursor)
		return
	}
	page, err := s.telemetryReader.ListEvents(r.Context(), telemetry.EventQuery{
		OrgID:       actor.OrgID,
		SubjectType: eventSubjectTypeDeployment,
		SubjectID:   pgvalue.MustUUIDValue(record.ID),
		AfterSeq:    cursor,
		Limit:       limit + 1,
	})
	if err != nil {
		writeError(w, errors.New("list deployment events"))
		return
	}
	rows := page.Events
	hasNext := len(rows) > int(limit)
	if hasNext {
		rows = rows[:limit]
	}
	var nextCursor *string
	if hasNext {
		value := rows[len(rows)-1].ID
		nextCursor = &value
	}
	writeJSON(w, http.StatusOK, api.RunEventPage{Events: rows, NextCursor: nextCursor})
}

func (s *Server) followDeploymentEvents(w http.ResponseWriter, r *http.Request, orgID uuid.UUID, deploymentID uuid.UUID, cursor int64) {
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
	err := s.eventStream.ReadSubject(ctx, orgID, eventSubjectTypeDeployment, deploymentID, cursor, func(event api.RunEvent) error {
		_, _ = fmt.Fprintf(w, "id: %s\n", event.ID)
		_, _ = fmt.Fprint(w, "event: deployment_event\n")
		_, _ = fmt.Fprint(w, "data: ")
		if err := encoder.Encode(event); err != nil {
			return err
		}
		_, _ = fmt.Fprint(w, "\n")
		if flusher != nil {
			flusher.Flush()
		}
		if deploymentEventKindIsTerminal(event.Kind) {
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
		s.log.Warn("follow deployment events failed", "error", err)
	}
}

func deploymentEventKindIsTerminal(kind string) bool {
	switch kind {
	case "deployment.deployed", "deployment.failed":
		return true
	default:
		return false
	}
}
