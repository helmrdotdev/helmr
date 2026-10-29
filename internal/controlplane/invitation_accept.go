package controlplane

import (
	"github.com/helmrdotdev/helmr/internal/identity"
	"net/http"
)

func (s *Server) resolveInvitationSummary(w http.ResponseWriter, r *http.Request) {
	var input struct {
		Token string `json:"token"`
	}
	if err := decodeRequestJSON(r, &input); err != nil {
		writeError(w, badRequest(err))
		return
	}
	result, err := identity.ResolveInvitationSummary(r.Context(), s.tx, s.identity, input.Token)
	if err != nil {
		s.writeIdentityError(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, result)
}
func (s *Server) acceptInvitation(w http.ResponseWriter, r *http.Request) {
	var input struct {
		Token string `json:"token"`
	}
	if err := decodeRequestJSON(r, &input); err != nil {
		writeError(w, badRequest(err))
		return
	}
	result, err := identity.AcceptAccountInvitation(r.Context(), s.tx, s.identity, principalFromContext(r.Context()), input.Token)
	if err != nil {
		s.writeIdentityError(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, result)
}
