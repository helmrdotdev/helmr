package controlplane

import (
	"encoding/json"
	"errors"
	"net/http"
	"strconv"
	"strings"

	"github.com/go-chi/chi/v5"
	"github.com/helmrdotdev/helmr/internal/api"
	"github.com/helmrdotdev/helmr/internal/deployment"
	"github.com/helmrdotdev/helmr/internal/org"
	"github.com/helmrdotdev/helmr/internal/slack"
)

func (s *Server) deleteAgent(w http.ResponseWriter, r *http.Request)  { s.changeAgent(w, r, false) }
func (s *Server) restoreAgent(w http.ResponseWriter, r *http.Request) { s.changeAgent(w, r, true) }
func (s *Server) changeAgent(w http.ResponseWriter, r *http.Request, restore bool) {
	p := principalFromContext(r.Context())
	scope, _, _, err := s.requestEnvironmentScopeFromRequest(r, p)
	if err != nil {
		writeError(w, badRequest(err))
		return
	}
	key := chi.URLParam(r, "agent")
	if err = api.ValidateDefinitionID(key); err != nil {
		writeError(w, badRequest(err))
		return
	}
	dry := false
	for name, values := range r.URL.Query() {
		if restore || name != "dry_run" || len(values) != 1 || (values[0] != "true" && values[0] != "false") {
			writeError(w, badRequest(errors.New("unsupported query parameter")))
			return
		}
		dry = values[0] == "true"
	}
	if restore {
		var input struct {
			DryRun bool `json:"dry_run"`
		}
		if err = decodeOptionalRequestJSON(r, &input); err != nil {
			writeError(w, badRequest(err))
			return
		}
		dry = input.DryRun
	} else if r.ContentLength != 0 {
		writeError(w, badRequest(errors.New("DELETE uses query parameters, not a request body")))
		return
	}
	revision := ""
	if !dry {
		raw := r.Header.Get("If-Match")
		if raw == "" {
			writeError(w, apiError{kind: errPreconditionRequired, err: codedError{code: "revision_required", message: "If-Match from the displayed revision is required"}})
			return
		}
		revision = strings.Trim(raw, `"`)
		n, parseErr := strconv.ParseInt(revision, 10, 64)
		if parseErr != nil || n < 1 || raw != `"`+strconv.FormatInt(n, 10)+`"` {
			writeError(w, badRequest(errors.New("If-Match must contain one quoted revision")))
			return
		}
	}
	result, err := deployment.ChangeAgent(r.Context(), s.tx, p, scope, key, revision, restore, dry)
	if err != nil {
		switch {
		case errors.Is(err, deployment.ErrAgentNotFound):
			writeError(w, notFound(err))
		case errors.Is(err, deployment.ErrPermissionRequired), errors.Is(err, org.ErrMemberManagementRequired):
			writeError(w, forbidden(codedError{code: "permission_required", message: "access.manage and target agents.manage are required"}))
		case errors.Is(err, org.ErrStaleRevision):
			writeError(w, permissionManagementError(err))
		case errors.Is(err, org.ErrAgentDependenciesChanged), errors.Is(err, slack.ErrConnectionUnavailable):
			writeError(w, conflict(codedError{code: "agent_dependencies_changed", message: "Agent dependencies changed; refresh the preview and retry"}))
		case errors.Is(err, deployment.ErrAgentDeletionBlocked):
			raw, _ := json.Marshal(agentChangeResponse(result))
			writeError(w, conflict(codedError{code: "agent_deletion_blocked", message: err.Error(), details: map[string]json.RawMessage{"preview": raw}}))
		default:
			s.log.Error("change Agent failed", "error", err)
			writeError(w, errors.New("change Agent"))
		}
		return
	}
	w.Header().Set("ETag", `"`+strconv.FormatInt(result.Revision, 10)+`"`)
	writeJSON(w, http.StatusOK, agentChangeResponse(result))
}

func agentChangeResponse(result deployment.AgentChange) api.AgentChangeResponse {
	out := api.AgentChangeResponse{AgentStatus: api.AgentStatus{Agent: result.Agent, CurrentDeploymentID: result.CurrentDeploymentID, DefinedInCurrent: result.DefinedInCurrent, DeletedAt: result.DeletedAt, Revision: result.Revision}, CanApply: result.CanApply, Changed: result.Changed, Effects: api.AgentDeletionEffects(result.Effects), Warnings: result.Warnings, Blockers: []api.AgentDeletionBlocker{}}
	for _, b := range result.Blockers {
		out.Blockers = append(out.Blockers, api.AgentDeletionBlocker(b))
	}
	return out
}
