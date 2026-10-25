package controlplane

import (
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strconv"

	"github.com/go-chi/chi/v5"
	"github.com/helmrdotdev/helmr/internal/api"
	"github.com/helmrdotdev/helmr/internal/deployment"
)

// Agent operational reads are independent of any selected definition snapshot.
type retainedAgent = api.AgentStatus

type retainedAgentCursor struct {
	View        string `json:"view"`
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
	if !single && values.Get("view") != "all" && values.Get("view") != "deleted" {
		writeError(w, badRequest(errors.New("view must be all or deleted")))
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
		if err != nil || cursor.View != values.Get("view") || cursor.Project != scope.ProjectID || cursor.Environment != scope.EnvironmentID {
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
	records, err := deployment.ListRetainedAgents(r.Context(), s.tx, principal, scope, key, after, limit, values.Get("view") == "deleted")
	if err != nil {
		s.writeDeploymentError(w, err)
		return
	}
	items := make([]retainedAgent, 0, len(records))
	for _, record := range records {
		items = append(items, retainedAgent{Agent: record.Agent, DeletedAt: record.DeletedAt, Revision: record.Revision, CurrentDeploymentID: record.CurrentDeploymentID, DefinedInCurrent: record.DefinedInCurrent})
	}
	if single {
		if len(items) == 0 {
			writeError(w, notFound(errors.New("agent not found")))
			return
		}
		w.Header().Set("ETag", `"`+strconv.FormatInt(items[0].Revision, 10)+`"`)
		writeJSON(w, http.StatusOK, items[0])
		return
	}
	next := ""
	if len(items) > limit {
		items = items[:limit]
		raw, _ := json.Marshal(retainedAgentCursor{View: values.Get("view"), Project: scope.ProjectID, Environment: scope.EnvironmentID, After: items[len(items)-1].Agent})
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
