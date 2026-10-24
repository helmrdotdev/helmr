package controlplane

import (
	"errors"
	"github.com/helmrdotdev/helmr/internal/access"
	"net/http"
)

func (s *Server) memberPermissions(w http.ResponseWriter, r *http.Request) {
	user, err := permissionPathID(r, "memberID")
	if err != nil {
		writeError(w, err)
		return
	}
	q := r.URL.Query()
	filter := access.MemberPermissionFilter{Project: q.Get("project"), Environment: q.Get("environment"), Agent: q.Get("agent")}
	if filter.Environment != "" && filter.Project == "" || filter.Agent != "" && (filter.Project == "" || filter.Environment == "") {
		writeError(w, badRequest(errors.New("environment requires project; agent requires project and environment")))
		return
	}
	result, err := access.MemberPermissions(r.Context(), s.tx, principalFromContext(r.Context()), user, filter)
	if err != nil {
		switch {
		case errors.Is(err, access.ErrDenied):
			err = forbidden(err)
		case errors.Is(err, access.ErrNotFound):
			err = notFound(err)
		}
		writeError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, result)
}
