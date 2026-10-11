package controlplane

import (
	"encoding/json"
	"net/http"
	"uuid"

	"github.com/helmrdotdev/helmr/internal/agent"
	"github.com/helmrdotdev/helmr/internal/api"
	"github.com/helmrdotdev/helmr/internal/computer"
	"github.com/helmrdotdev/helmr/internal/workerapi"
)

func (s *Server) workerAgentListComputers(w http.ResponseWriter, r *http.Request, requestID string, caller agent.Caller, payload json.RawMessage) {
	var input struct {
		Cursor string `json:"cursor,omitempty"`
	}
	if err := decodeAgentPayload(payload, &input); err != nil {
		writeJSON(w, http.StatusOK, agentOperationFailure(requestID, "invalid_arguments", "invalid Computer list arguments"))
		return
	}
	page := computer.ListPage{Limit: computer.DefaultListLimit}
	if input.Cursor != "" {
		cursor, err := decodeComputerListCursor(input.Cursor)
		if err != nil || cursor.EnvironmentID != caller.Execution.EnvironmentID.String() {
			writeJSON(w, http.StatusOK, agentOperationFailure(requestID, "invalid_arguments", "invalid Computer cursor"))
			return
		}
		page.After = &computer.ListPosition{CreatedAt: cursor.CreatedAt, ID: uuid.MustParse(cursor.ID)}
	}
	listing, scope, err := computer.ListRuntime(r.Context(), s.tx, caller, page)
	if err != nil {
		s.writeAgentAdmissionError(w, requestID, err)
		return
	}
	result := api.ListComputersResponse{Computers: []api.ComputerListItem{}}
	for _, item := range listing.Items {
		result.Computers = append(result.Computers, apiComputerListItem(item))
	}
	if listing.More {
		last := listing.Items[len(listing.Items)-1]
		result.NextCursor, err = encodeComputerListCursor(computerListCursor{ProjectID: scope.ProjectID.String(), EnvironmentID: scope.EnvironmentID.String(), CreatedAt: last.CreatedAt, ID: last.ID})
		if err != nil {
			s.writeAgentAdmissionError(w, requestID, err)
			return
		}
	}
	body, err := json.Marshal(result)
	if err != nil {
		s.writeAgentAdmissionError(w, requestID, err)
		return
	}
	writeJSON(w, http.StatusOK, workerapi.AgentOperationResponse{RequestID: requestID, Value: body})
}
