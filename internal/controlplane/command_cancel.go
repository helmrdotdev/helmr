package controlplane

import (
	"errors"
	"github.com/helmrdotdev/helmr/internal/auth"
	"net/http"

	"github.com/go-chi/chi/v5"
	"github.com/helmrdotdev/helmr/internal/api"
	"github.com/helmrdotdev/helmr/internal/command"
	"github.com/helmrdotdev/helmr/internal/ids"
	"github.com/helmrdotdev/helmr/internal/pgvalue"
)

func (s *Server) cancelCommandHTTP(w http.ResponseWriter, r *http.Request) {
	commandID, err := ids.Parse(chi.URLParam(r, "commandID"))
	if err != nil {
		writeError(w, badRequest(errors.New("command ID is invalid")))
		return
	}
	principal := principalFromContext(r.Context())
	scope, projectID, environmentID, err := s.requestEnvironmentScopeFromRequest(r, principal)
	if err != nil {
		writeError(w, badRequest(err))
		return
	}
	if !principal.HasPermission(auth.PermissionComputersCancelCommand, scope) {
		writeError(w, forbidden(codedError{code: "permission_required", message: errPermissionRequired.Error()}))
		return
	}
	receipt, err := command.Cancel(r.Context(), s.tx, principal, command.Ref{
		OrgID: principal.OrgID, ProjectID: pgvalue.MustUUIDValue(projectID),
		EnvironmentID: pgvalue.MustUUIDValue(environmentID), CommandID: commandID,
	})
	if err != nil {
		s.writeCommandError(w, err, commandCancelOperation)
		return
	}
	writeJSON(w, http.StatusAccepted, api.CommandCancelReceipt(receipt))
}
