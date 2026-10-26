package controlplane

import (
	"errors"
	"net/http"
	"uuid"

	"github.com/helmrdotdev/helmr/internal/access"
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
		s.writeIdentityError(w, r, err)
		return
	}
	hasOrg := principal.OrgID != uuid.Nil()
	response := api.MeResponse{
		UserID:          principal.UserID.String(),
		DisplayName:     account.DisplayName,
		ProfileImageURL: account.ProfileImageURL,
		AppOrigin:       s.appOrigin.String(),
		Admin:           principal.Admin,
		Permissions:     []string{},
		ProjectRequired: hasOrg && !account.HasProjects && principal.HasPermission(auth.PermissionProjectsCreate, auth.Scope{OrgID: principal.OrgID}),
		EmailDelivery:   s.emailConfigured(),
	}
	switch {
	case hasOrg:
		response.OrgID = principal.OrgID.String()
		response.OrgName = account.OrgName
		response.OrgSlug = account.OrgSlug
		response.MembershipID = principal.MemberID.String()
		response.IsOwner = principal.IsOwner
		response.Permissions = sessionPermissions(principal)
		grants, err := access.MemberGrants(r.Context(), s.tx, principal.OrgID, principal.MemberID)
		if err != nil {
			writeError(w, err)
			return
		}
		response.PermissionScopes = []api.PermissionScope{}
		for _, g := range grants {
			if g.EnvironmentID == "" {
				continue
			}
			item := api.PermissionScope{Project: g.ProjectSlug, Environment: g.EnvironmentSlug, Agent: g.AgentKey, Permissions: []string{}}
			scope := auth.Scope{OrgID: principal.OrgID, ProjectID: g.ProjectID, EnvironmentID: g.EnvironmentID, AgentID: g.AgentID}
			for _, operation := range auth.AllPermissions() {
				if principal.HasPermission(operation, scope) {
					item.Permissions = append(item.Permissions, string(operation))
				}
			}

			response.PermissionScopes = append(response.PermissionScopes, item)
		}
	case s.selfHostedMode():
		response.OrganizationRequired = !account.OrganizationExists
		response.AccessRequired = account.OrganizationExists
		response.SetupTokenRequired = !account.OrganizationExists
	case s.organizationCreationAdminOnly && !principal.Admin:
		response.AccessRequired = true
	default:
		response.OrganizationRequired = true
	}
	writeJSON(w, http.StatusOK, response)
}

func sessionPermissions(principal auth.Principal) []string {
	permissions := []string{}
	scope := auth.Scope{OrgID: principal.OrgID}
	for _, permission := range auth.AllPermissions() {
		if principal.HasPermission(permission, scope) {
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
		if err := identity.RevokeLoginSession(r.Context(), s.tx, s.identity, cookie.Value); err != nil {
			writeError(w, unavailable(err))
			return
		}
	}
	if token, ok := bearerToken(r.Header.Get("authorization")); ok {
		if err := identity.RevokeLoginSession(r.Context(), s.tx, s.identity, token); err != nil {
			writeError(w, unavailable(err))
			return
		}
	}
	clearSessionCookie(w, r)
	w.WriteHeader(http.StatusNoContent)
}
