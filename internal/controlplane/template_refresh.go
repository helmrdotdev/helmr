package controlplane

import (
	"github.com/go-chi/chi/v5"
	"github.com/helmrdotdev/helmr/internal/api"
	"github.com/helmrdotdev/helmr/internal/deployment"
	"net/http"
)

func (s *Server) refreshTemplate(w http.ResponseWriter, r *http.Request) {
	var request api.RefreshTemplateRequest
	if err := decodeRequestJSON(r, &request); err != nil {
		writeError(w, badRequest(err))
		return
	}
	principal := principalFromContext(r.Context())
	scope, _, _, err := s.requestEnvironmentScopeFromRequest(r, principal)
	if err != nil {
		writeError(w, badRequest(err))
		return
	}
	result, err := deployment.RefreshTemplate(r.Context(), s.tx, principal, scope, chi.URLParam(r, "template"), request.IdempotencyKey)
	if err != nil {
		s.writeDeploymentError(w, err)
		return
	}
	writeJSON(w, http.StatusAccepted, result)
}
func (s *Server) getTemplateBuild(w http.ResponseWriter, r *http.Request) {
	build, err := parseUUIDParam(r, "image_build")
	if err != nil {
		writeError(w, badRequest(err))
		return
	}
	principal := principalFromContext(r.Context())
	scope, _, _, err := s.requestEnvironmentScopeFromRequest(r, principal)
	if err != nil {
		writeError(w, badRequest(err))
		return
	}
	selected, err := parseDefinitionItemQuery(r)
	if err != nil {
		writeError(w, badRequest(err))
		return
	}
	result, err := deployment.ReadTemplateBuild(r.Context(), s.tx, principal, scope, chi.URLParam(r, "template"), selected, build)
	if err != nil {
		s.writeDeploymentError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, result)
}
