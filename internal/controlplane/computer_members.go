package controlplane

import (
	"errors"
	"fmt"
	"net/http"
	"strconv"

	"github.com/go-chi/chi/v5"
	"github.com/helmrdotdev/helmr/internal/api"
	"github.com/helmrdotdev/helmr/internal/auth"
	"github.com/helmrdotdev/helmr/internal/computer"
	"github.com/helmrdotdev/helmr/internal/ids"
	"github.com/helmrdotdev/helmr/internal/pgvalue"
)

func (s *Server) listComputerSessionsHTTP(w http.ResponseWriter, r *http.Request) {
	s.listComputerUsageHTTP(w, r, "session")
}
func (s *Server) listComputerCommandsHTTP(w http.ResponseWriter, r *http.Request) {
	s.listComputerUsageHTTP(w, r, "command")
}
func (s *Server) listComputerUsageHTTP(w http.ResponseWriter, r *http.Request, kind string) {
	principal := principalFromContext(r.Context())
	scope, projectID, environmentID, err := s.requestEnvironmentScopeFromRequest(r, principal)
	if err != nil {
		writeError(w, badRequest(codedError{code: "invalid_computer_reference", message: err.Error()}))
		return
	}
	if !principal.HasPermission(auth.PermissionComputersRead, scope) {
		writeError(w, forbidden(codedError{code: "permission_required", message: errPermissionRequired.Error()}))
		return
	}
	computerID, err := ids.Parse(chi.URLParam(r, "computerID"))
	if err != nil {
		writeError(w, badRequest(codedError{code: "invalid_computer_reference", message: "computer ID is invalid"}))
		return
	}
	query := computer.MembersQuery{Kind: kind}
	for name, values := range r.URL.Query() {
		if len(values) != 1 || values[0] == "" {
			err = fmt.Errorf("%s must appear once with a value", name)
			break
		}
		switch name {
		case "cursor":
			query.Cursor = values[0]
		case "limit":
			limit, parseErr := strconv.ParseInt(values[0], 10, 32)
			if parseErr != nil || limit < 1 || limit > int64(computer.MaxListLimit) {
				err = errors.New("limit must be an integer in [1,100]")
			}
			query.Limit = int32(limit)
		default:
			err = fmt.Errorf("query parameter %q is not supported", name)
		}
		if err != nil {
			break
		}
	}
	if err == nil {
		// A malformed query is rejected before the Computer is addressed.
		err = computer.ValidateMembersQuery(pgvalue.MustUUIDValue(environmentID), computerID, query)
	}
	if err != nil {
		writeError(w, badRequest(codedError{code: "invalid_computer_reference", message: err.Error()}))
		return
	}
	page, err := computer.ListMembers(r.Context(), s.tx, computerScope(principal.OrgID, projectID, environmentID), computerID, query)
	if err != nil {
		s.writeComputerError(w, err, computerReadOperation, "list Computer usage failed")
		return
	}
	items := make([]api.ComputerUsage, 0, len(page.Members))
	for _, member := range page.Members {
		items = append(items, api.ComputerUsage{ID: member.ID, UsageState: member.State, CreatedAt: member.CreatedAt})
	}
	if kind == "session" {
		writeJSON(w, http.StatusOK, api.ListComputerSessionsResponse{Sessions: items, NextCursor: page.NextCursor})
	} else {
		writeJSON(w, http.StatusOK, api.ListComputerCommandsResponse{Commands: items, NextCursor: page.NextCursor})
	}
}
