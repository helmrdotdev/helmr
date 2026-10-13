package controlplane

import (
	"encoding/json"
	"net/http"
	"uuid"

	"github.com/helmrdotdev/helmr/internal/api"
	"github.com/helmrdotdev/helmr/internal/session"
	"github.com/helmrdotdev/helmr/internal/workerapi"
)

func (s *Server) workerAgentInspectSession(w http.ResponseWriter, r *http.Request, request workerapi.AgentOperationRequest, caller session.Caller) {
	var input struct {
		SessionID string `json:"sessionId"`
	}
	if err := decodeAgentPayload(request.Payload, &input); err != nil {
		s.writeAgentAdmissionError(w, request.RequestID, session.ErrInvalidInput)
		return
	}
	id, err := uuid.Parse(input.SessionID)
	if err != nil || id == uuid.Nil() {
		s.writeAgentAdmissionError(w, request.RequestID, session.ErrInvalidInput)
		return
	}
	view, err := session.RuntimeGetSession(r.Context(), s.tx, caller, id)
	if err != nil {
		s.writeAgentAdmissionError(w, request.RequestID, err)
		return
	}
	value := agentSessionResponse(view)
	body, _ := json.Marshal(value)
	writeJSON(w, http.StatusOK, workerapi.AgentOperationResponse{RequestID: request.RequestID, Value: body})
}

func (s *Server) workerAgentListSessions(w http.ResponseWriter, r *http.Request, request workerapi.AgentOperationRequest, caller session.Caller) {
	var input struct {
		Relation string   `json:"relation"`
		Cursor   string   `json:"cursor,omitempty"`
		Status   []string `json:"status,omitempty"`
		Limit    *int     `json:"limit,omitempty"`
	}
	if err := decodeAgentPayload(request.Payload, &input); err != nil {
		s.writeAgentAdmissionError(w, request.RequestID, session.ErrInvalidInput)
		return
	}
	query := session.RuntimeSessionListRequest{Relation: input.Relation, Statuses: input.Status, Limit: 50}
	if input.Limit != nil {
		query.Limit = *input.Limit
	}
	if input.Cursor != "" {
		var err error
		query.Before, err = uuid.Parse(input.Cursor)
		if err != nil || query.Before == uuid.Nil() {
			s.writeAgentAdmissionError(w, request.RequestID, session.ErrInvalidInput)
			return
		}
	}
	page, err := session.RuntimeListSessions(r.Context(), s.tx, caller, query)
	if err != nil {
		s.writeAgentAdmissionError(w, request.RequestID, err)
		return
	}
	result := api.AgentSessionsPage{Sessions: []api.AgentSession{}}
	for _, view := range page.Sessions {
		result.Sessions = append(result.Sessions, agentSessionResponse(view))
	}
	if page.NextCursor != uuid.Nil() {
		result.NextCursor = page.NextCursor.String()
	}
	value := result
	body, _ := json.Marshal(value)
	writeJSON(w, http.StatusOK, workerapi.AgentOperationResponse{RequestID: request.RequestID, Value: body})
}
