package controlplane

import (
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"

	"github.com/go-chi/chi/v5"
	"github.com/helmrdotdev/helmr/internal/api"
	"github.com/helmrdotdev/helmr/internal/deployment"
)

// Agent operational reads are independent of any selected definition snapshot.
type retainedAgent struct {
	Agent               string  `json:"agent"`
	CurrentDeploymentID *string `json:"current_deployment_id"`
	DefinedInCurrent    bool    `json:"defined_in_current"`
}
type retainedAgentCursor struct {
	Project     string `json:"project"`
	Environment string `json:"environment"`
	After       string `json:"after"`
}

func (s *Server) listManagementAgents(w http.ResponseWriter, r *http.Request) {
	if !r.URL.Query().Has("view") {
		s.listAgentDefinitions(w, r)
		return
	}
	s.readRetainedAgents(w, r, false)
}
func (s *Server) getAgentStatus(w http.ResponseWriter, r *http.Request) {
	s.readRetainedAgents(w, r, true)
}
func (s *Server) readRetainedAgents(w http.ResponseWriter, r *http.Request, single bool) {
	principal := principalFromContext(r.Context())
	scope, _, _, err := s.requestEnvironmentScopeFromRequest(r, principal)
	if err != nil {
		writeError(w, badRequest(err))
		return
	}
	values := r.URL.Query()
	for name, entries := range values {
		if single || (name != "view" && name != "cursor" && name != "limit") || len(entries) != 1 || entries[0] == "" {
			writeError(w, badRequest(fmt.Errorf("query parameter %q is not supported", name)))
			return
		}
	}
	if !single && values.Get("view") != "all" {
		writeError(w, badRequest(errors.New("view must be all")))
		return
	}
	limit, err := agentPageLimit(r)
	if err != nil {
		writeError(w, badRequest(err))
		return
	}
	after := ""
	if raw := values.Get("cursor"); raw != "" {
		cursor, err := decodeRetainedAgentCursor(raw)
		if err != nil || cursor.Project != scope.ProjectID || cursor.Environment != scope.EnvironmentID {
			writeError(w, badRequest(errors.New("invalid Agent cursor")))
			return
		}
		after = cursor.After
	}
	key := ""
	if single {
		key = chi.URLParam(r, "agent")
		if err := api.ValidateDefinitionID(key); err != nil {
			writeError(w, badRequest(err))
			return
		}
	}
	records, err := deployment.ListRetainedAgents(r.Context(), s.tx, principal, scope, key, after, limit)
	if err != nil {
		s.writeDeploymentError(w, err)
		return
	}
	items := make([]retainedAgent, 0, len(records))
	for _, record := range records {
		items = append(items, retainedAgent{Agent: record.Agent, CurrentDeploymentID: record.CurrentDeploymentID, DefinedInCurrent: record.DefinedInCurrent})
	}
	if single {
		if len(items) == 0 {
			writeError(w, notFound(errors.New("Agent not found")))
			return
		}
		writeJSON(w, http.StatusOK, items[0])
		return
	}
	next := ""
	if len(items) > limit {
		items = items[:limit]
		raw, _ := json.Marshal(retainedAgentCursor{Project: scope.ProjectID, Environment: scope.EnvironmentID, After: items[len(items)-1].Agent})
		next = base64.RawURLEncoding.EncodeToString(raw)
	}
	writeJSON(w, http.StatusOK, struct {
		Agents     []retainedAgent `json:"agents"`
		NextCursor string          `json:"next_cursor,omitempty"`
	}{items, next})
}

func decodeRetainedAgentCursor(raw string) (retainedAgentCursor, error) {
	var cursor retainedAgentCursor
	encoded, err := base64.RawURLEncoding.DecodeString(raw)
	if err != nil || json.Unmarshal(encoded, &cursor) != nil || cursor.Project == "" || cursor.Environment == "" || api.ValidateDefinitionID(cursor.After) != nil {
		return retainedAgentCursor{}, errors.New("invalid Agent cursor")
	}
	return cursor, nil
}
