package controlplane

import (
	"github.com/helmrdotdev/helmr/internal/identity"
	"net/http"
)

func (s *Server) accountActivity(w http.ResponseWriter, r *http.Request) {
	if err := identity.RecordHumanActivity(r.Context(), s.tx, principalFromContext(r.Context())); err != nil {
		s.writeIdentityError(w, r, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}
