package controlplane

import (
	"errors"
	"net/http"
	"uuid"

	"github.com/helmrdotdev/helmr/internal/api"
	"github.com/helmrdotdev/helmr/internal/auth"
	"github.com/helmrdotdev/helmr/internal/identity"
)

func (s *Server) me(w http.ResponseWriter, r *http.Request) {
	principal := principalFromContext(r.Context())
	if principal.UserID == uuid.Nil() {
		writeError(w, unauthorized(errors.New("session authentication is required")))
		return
	}
	account, err := identity.LoadAccount(r.Context(), s.db, principal)
	if err != nil {
		writeError(w, identityError(err))
		return
	}
	hasOrg := principal.OrgID != uuid.Nil()
	response := api.MeResponse{
		UserID:          principal.UserID.String(),
		DisplayName:     account.DisplayName,
		ProfileImageURL: account.ProfileImageURL,
		PublicURL:       s.publicURL.String(),
		Admin:           principal.Admin,
		Permissions:     []string{},
		ProjectRequired: hasOrg && !account.HasProjects,
	}
	switch {
	case hasOrg:
		response.OrgID = principal.OrgID.String()
		response.OrgName = account.OrgName
		response.OrgSlug = account.OrgSlug
		response.Role = string(principal.Role)
		response.Permissions = sessionPermissions(principal.Role)
	case s.selfHostedMode():
		response.OrganizationRequired = !account.OrganizationExists
		response.AccessRequired = account.OrganizationExists
		response.SetupTokenRequired = !account.OrganizationExists
	default:
		response.OrganizationRequired = true
	}
	writeJSON(w, http.StatusOK, response)
}

func sessionPermissions(role auth.Role) []string {
	all := auth.AllPermissions()
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
		s.revokeLoginSession(r, cookie.Value)
	}
	if token, ok := bearerToken(r.Header.Get("authorization")); ok {
		s.revokeLoginSession(r, token)
	}
	clearSessionCookie(w, r)
	w.WriteHeader(http.StatusNoContent)
}

// revokeLoginSession revokes a presented login session. Logout succeeds
// whether or not revocation does, so a failure is only logged.
func (s *Server) revokeLoginSession(r *http.Request, raw string) {
	if err := identity.RevokeLoginSession(r.Context(), s.db, s.identity, raw); err != nil {
		s.log.Warn("revoke login session failed", "error", err)
	}
}
