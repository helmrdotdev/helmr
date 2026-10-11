package controlplane

import (
	"encoding/json"
	"net/http"
	"uuid"

	"github.com/helmrdotdev/helmr/internal/agent"
	"github.com/helmrdotdev/helmr/internal/api"
	agentv1 "github.com/helmrdotdev/helmr/internal/proto/agent/v1"
	"github.com/helmrdotdev/helmr/internal/workerapi"
)

func (s *Server) workerAgentReadSessions(w http.ResponseWriter, r *http.Request, requestID string, caller agent.Caller, method agentv1.Operation_Method, payload json.RawMessage) {
	var value any
	if method == agentv1.Operation_METHOD_INSPECT_SESSION {
		var input struct {
			SessionID string `json:"sessionId"`
		}
		if err := decodeAgentPayload(payload, &input); err != nil {
			s.writeAgentAdmissionError(w, requestID, agent.ErrInvalidInput)
			return
		}
		id, err := uuid.Parse(input.SessionID)
		if err != nil || id == uuid.Nil() {
			s.writeAgentAdmissionError(w, requestID, agent.ErrInvalidInput)
			return
		}
		view, err := agent.RuntimeGetSession(r.Context(), s.tx, caller, id)
		if err != nil {
			s.writeAgentAdmissionError(w, requestID, err)
			return
		}
		value = agentSessionResponse(view)
	} else {
		var input struct {
			Relation string   `json:"relation"`
			Cursor   string   `json:"cursor,omitempty"`
			Status   []string `json:"status,omitempty"`
			Limit    *int     `json:"limit,omitempty"`
		}
		if err := decodeAgentPayload(payload, &input); err != nil {
			s.writeAgentAdmissionError(w, requestID, agent.ErrInvalidInput)
			return
		}
		query := agent.RuntimeSessionListRequest{Relation: input.Relation, Statuses: input.Status, Limit: 50}
		if input.Limit != nil {
			query.Limit = *input.Limit
		}
		if input.Cursor != "" {
			var err error
			query.Before, err = uuid.Parse(input.Cursor)
			if err != nil || query.Before == uuid.Nil() {
				s.writeAgentAdmissionError(w, requestID, agent.ErrInvalidInput)
				return
			}
		}
		page, err := agent.RuntimeListSessions(r.Context(), s.tx, caller, query)
		if err != nil {
			s.writeAgentAdmissionError(w, requestID, err)
			return
		}
		result := api.AgentSessionsPage{Sessions: []api.AgentSession{}}
		for _, view := range page.Sessions {
			result.Sessions = append(result.Sessions, agentSessionResponse(view))
		}
		if page.NextCursor != uuid.Nil() {
			result.NextCursor = page.NextCursor.String()
		}
		value = result
	}
	body, _ := json.Marshal(value)
	writeJSON(w, http.StatusOK, workerapi.AgentOperationResponse{RequestID: requestID, Value: body})
}
