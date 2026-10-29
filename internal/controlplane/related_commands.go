package controlplane

import (
	"errors"
	"github.com/helmrdotdev/helmr/internal/access"
	"github.com/helmrdotdev/helmr/internal/command"
	"github.com/helmrdotdev/helmr/internal/ids"
	"net/http"
	"uuid"
)

func (s *Server) listRelatedCommandsHTTP(w http.ResponseWriter, r *http.Request) {
	principal := principalFromContext(r.Context())
	scope, _, _, err := s.requestEnvironmentScopeFromRequest(r, principal)
	if err != nil {
		writeError(w, badRequest(err))
		return
	}
	user, err := ids.Parse(r.URL.Query().Get("user"))
	if err != nil || user == uuid.Nil() {
		writeError(w, badRequest(errors.New("invalid related-command selector")))
		return
	}
	var before uuid.UUID
	if raw := r.URL.Query().Get("cursor"); raw != "" {
		before, err = ids.Parse(raw)
		if err != nil {
			writeError(w, badRequest(errors.New("invalid related-command selector")))
			return
		}
	}
	result, err := command.ListRelated(r.Context(), s.tx, principal, scope, user, before)
	if err != nil {
		if errors.Is(err, access.ErrDenied) {
			s.writeOperationError(w, r, forbidden(err))
		} else {
			s.writeCommandError(w, r, err, commandGetOperation)
		}
		return
	}
	writeJSON(w, http.StatusOK, result)
}
