package controlplane

import (
	"net/http"

	"github.com/helmrdotdev/helmr/internal/api"
	"github.com/helmrdotdev/helmr/internal/deployment"
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
	principal := principalFromContext(r.Context())
	scope, err := s.requestedRunListScope(r, principal)
	if err != nil {
		writeError(w, badRequest(err))
		return
	}
	events, err := deployment.ListEvents(r.Context(), s.tx, principal, scope, deploymentID, cursor, limit+1)
	if err != nil {
		s.writeDeploymentError(w, r, err)
		return
	}
	hasNext := len(events) > int(limit)
	if hasNext {
		events = events[:limit]
	}
	rows := make([]api.DeploymentEvent, 0, len(events))
	for _, event := range events {
		rows = append(rows, api.DeploymentEvent{ID: telemetry.Cursor(event.ID), Kind: event.Kind, Message: event.Message, At: event.AcceptedAt, Attributes: event.Payload})
	}
	var nextCursor *string
	if hasNext {
		value := rows[len(rows)-1].ID
		nextCursor = &value
	}
	writeJSON(w, http.StatusOK, api.DeploymentEventPage{Events: rows, NextCursor: nextCursor})
}
