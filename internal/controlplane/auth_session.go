package controlplane

import (
	"errors"
	"github.com/helmrdotdev/helmr/internal/db"
	"github.com/helmrdotdev/helmr/internal/pgvalue"
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
		AppOrigin:       s.appOrigin.String(),
		Admin:           principal.Admin,
		Permissions:     []string{},
		ProjectRequired: hasOrg && !account.HasProjects && principal.HasPermission(auth.CapabilityOrganizationManage, auth.Scope{OrgID: principal.OrgID}),
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
		grants, err := s.db.ListUserPermissionGrants(r.Context(), db.ListUserPermissionGrantsParams{OrgID: pgvalue.UUID(principal.OrgID), UserID: pgvalue.UUID(principal.UserID)})
		if err != nil {
			writeError(w, err)
			return
		}
		response.PermissionScopes = []api.PermissionScope{}
		for _, g := range grants {
			if !g.EnvironmentID.Valid {
				continue
			}
			item := api.PermissionScope{Project: g.ProjectSlug, Environment: g.EnvironmentSlug, Agent: g.AgentKey, Permissions: []string{}}
			scope := auth.Scope{OrgID: principal.OrgID, ProjectID: pgvalue.UUIDString(g.ProjectID), EnvironmentID: pgvalue.UUIDString(g.EnvironmentID), AgentID: pgvalue.UUIDString(g.AgentID)}
			for _, operation := range auth.AllPermissions() {
				if auth.CapabilityAllows(auth.Permission(g.Permission), operation) || (auth.SessionExecutionOperation(operation) && principal.HasPermission(operation, scope)) {
					item.Permissions = append(item.Permissions, string(operation))
				}
			}
			for _, capability := range auth.IncludedCapabilities(auth.Permission(g.Permission)) {
				item.Permissions = append(item.Permissions, string(capability))
			}
			response.PermissionScopes = append(response.PermissionScopes, item)
		}
	case s.selfHostedMode():
		response.OrganizationRequired = !account.OrganizationExists
		response.AccessRequired = account.OrganizationExists
		response.SetupTokenRequired = !account.OrganizationExists
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
	for _, cap := range auth.CapabilityCatalog() {
		if principal.HasPermission(cap.ID, scope) {
			permissions = append(permissions, string(cap.ID))
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
