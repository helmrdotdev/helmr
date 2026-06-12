package control

import (
	"errors"
	"net/http"

	"github.com/google/uuid"
	"github.com/helmrdotdev/helmr/internal/api"
	"github.com/helmrdotdev/helmr/internal/auth"
	"github.com/helmrdotdev/helmr/internal/ids"
)

var uuidNil uuid.UUID

func (s *Server) me(w http.ResponseWriter, r *http.Request) {
	actor := actorFromContext(r.Context())
	if actor.UserID == uuidNil {
		writeError(w, unauthorized(errors.New("session authentication is required")))
		return
	}
	state, err := s.db.GetUserOnboardingState(r.Context(), ids.ToPG(actor.UserID))
	if err != nil {
		if isNoRows(err) {
			writeError(w, unauthorized(errors.New("authentication is required")))
			return
		}
		writeError(w, errors.New("load current user"))
		return
	}
	response := api.MeResponse{
		UserID:          actor.UserID.String(),
		DisplayName:     state.DisplayName,
		ProfileImageURL: state.ProfileImageUrl.String,
		Permissions:     []string{},
		ProjectRequired: state.OrgID.Valid && !state.HasProjects,
	}
	if state.OrgID.Valid {
		orgID, err := ids.FromPG(state.OrgID)
		if err != nil {
			writeError(w, errors.New("load current organization"))
			return
		}
		response.OrgID = orgID.String()
		response.OrgName = state.OrgName.String
		response.OrgSlug = state.OrgSlug.String
		response.Role = state.Role
		response.Permissions = sessionPermissions(auth.Role(state.Role))
	} else {
		orgIDs, err := s.db.ListOrganizationIDs(r.Context(), 1)
		if err != nil {
			writeError(w, errors.New("load current organization"))
			return
		}
		orgExists := len(orgIDs) > 0
		if s.selfHostedMode() {
			response.OrganizationRequired = !orgExists
			response.AccessRequired = orgExists
			response.SetupTokenRequired = !orgExists
		} else {
			response.OrganizationRequired = true
		}
	}
	writeJSON(w, http.StatusOK, response)
}

func sessionPermissions(role auth.Role) []string {
	all := []auth.Permission{
		auth.PermissionAPIKeysManage,
		auth.PermissionMembersManage,
		auth.PermissionProjectsManage,
		auth.PermissionRunsCreate,
		auth.PermissionRunsManage,
		auth.PermissionRunsRead,
		auth.PermissionSecretsWrite,
		auth.PermissionTasksDeploy,
		auth.PermissionWaitpointPolicies,
		auth.PermissionWaitpointsRespond,
	}
	permissions := make([]string, 0, len(all))
	for _, permission := range all {
		if auth.RoleAllows(role, permission) {
			permissions = append(permissions, string(permission))
		}
	}
	return permissions
}

func (s *Server) logout(w http.ResponseWriter, r *http.Request) {
	if err := s.userAuthConfigured(); err != nil {
		writeError(w, unavailable(err))
		return
	}
	if cookie, err := r.Cookie(sessionCookieName(r)); err == nil {
		s.revokeSessionToken(r, cookie.Value)
	}
	if token, ok := bearerToken(r.Header.Get("authorization")); ok {
		s.revokeSessionToken(r, token)
	}
	clearSessionCookie(w, r)
	w.WriteHeader(http.StatusNoContent)
}

func (s *Server) revokeSessionToken(r *http.Request, raw string) {
	tokenHash, err := auth.HashToken(s.authSecret, raw)
	if err != nil {
		return
	}
	_, _ = s.db.RevokeSessionByTokenHash(r.Context(), tokenHash)
}
