package controlplane

import (
	"net/http"
	"time"
	"uuid"

	"github.com/helmrdotdev/helmr/internal/access"
	"github.com/helmrdotdev/helmr/internal/api"
	"github.com/helmrdotdev/helmr/internal/auth"
	"github.com/helmrdotdev/helmr/internal/identity"
)

func (s *Server) me(w http.ResponseWriter, r *http.Request) {
	p := principalFromContext(r.Context())
	if p.Channel != "browser" {
		writeError(w, forbidden(codedError{code: "authentication_required", message: "ordinary authentication is required"}))
		return
	}
	var response api.MeResponse
	response.UserID = p.UserID.String()
	if err := s.tx.QueryRow(r.Context(), `SELECT display_name,COALESCE(profile_image_url,''),COALESCE(primary_email,'') FROM users WHERE id=$1`, p.UserID).Scan(&response.DisplayName, &response.ProfileImageURL, &response.PrimaryEmail); err != nil {
		writeError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, response)
}
func (s *Server) organizationContext(w http.ResponseWriter, r *http.Request) {
	p := principalFromContext(r.Context())
	if p.MemberID == uuid.Nil() {
		writeError(w, badRequest(codedError{code: "organization_required", message: "Choose an organization."}))
		return
	}
	response := api.OrganizationContextResponse{ID: p.OrgID.String(), CurrentMember: api.CurrentMember{ID: p.MemberID.String(), UserID: p.UserID.String(), IsOwner: p.IsOwner, Permissions: sessionPermissions(p), PermissionScopes: []api.PermissionScope{}}, Authentication: api.AuthenticationContext{AuthenticatedAt: p.AuthenticatedAt.UTC().Format(time.RFC3339Nano)}}
	var projects bool
	if err := s.tx.QueryRow(r.Context(), `SELECT o.name,o.slug,COALESCE(m.display_name,u.display_name),EXISTS(SELECT 1 FROM projects WHERE org_id=o.id) FROM organizations o JOIN org_members m ON m.org_id=o.id JOIN users u ON u.id=m.user_id WHERE o.id=$1 AND m.id=$2`, p.OrgID, p.MemberID).Scan(&response.Name, &response.Slug, &response.CurrentMember.DisplayName, &projects); err != nil {
		writeError(w, err)
		return
	}
	response.Onboarding.ProjectRequired = !projects && p.HasPermission(auth.PermissionProjectsCreate, auth.Scope{OrgID: p.OrgID})
	grants, err := access.MemberGrants(r.Context(), s.tx, p.OrgID, p.MemberID)
	if err != nil {
		writeError(w, err)
		return
	}
	for _, g := range grants {
		if g.EnvironmentID == "" {
			continue
		}
		item := api.PermissionScope{Project: g.ProjectSlug, Environment: g.EnvironmentSlug, Agent: g.AgentKey, Permissions: []string{}}
		scope := auth.Scope{OrgID: p.OrgID, ProjectID: g.ProjectID, EnvironmentID: g.EnvironmentID, AgentID: g.AgentID}
		for _, permission := range auth.AllPermissions() {
			if p.HasPermission(permission, scope) {
				item.Permissions = append(item.Permissions, string(permission))
			}
		}
		response.CurrentMember.PermissionScopes = append(response.CurrentMember.PermissionScopes, item)
	}
	writeJSON(w, http.StatusOK, response)
}
func (s *Server) organizations(w http.ResponseWriter, r *http.Request) {
	p := principalFromContext(r.Context())
	if p.Channel != "browser" {
		writeError(w, forbidden(codedError{code: "authentication_required", message: "ordinary authentication is required"}))
		return
	}
	rows, err := s.tx.Query(r.Context(), `SELECT o.id,o.name,o.slug,m.id FROM org_members m JOIN organizations o ON o.id=m.org_id WHERE m.user_id=$1 AND m.removed_at IS NULL AND m.suspended_at IS NULL ORDER BY o.name,o.id`, p.UserID)
	if err != nil {
		writeError(w, err)
		return
	}
	defer rows.Close()
	response := api.OrganizationsResponse{Organizations: []api.OrganizationMembership{}}
	for rows.Next() {
		var o api.OrganizationMembership
		if err = rows.Scan(&o.ID, &o.Name, &o.Slug, &o.MembershipID); err != nil {
			writeError(w, err)
			return
		}
		o.AuthenticationRequired = "ordinary"
		response.Organizations = append(response.Organizations, o)
	}
	if err = rows.Err(); err != nil {
		writeError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, response)
}

// signInMethods are the account sign-in methods this deployment offers.
// Email codes need email delivery, and GitHub needs its configured provider.
func (s *Server) signInMethods() []string {
	methods := []string{"passkey"}
	if s.emailConfigured() {
		methods = append(methods, "email")
	}
	if s.authProvider != nil {
		methods = append(methods, "github")
	}
	return methods
}

func (s *Server) authConfig(w http.ResponseWriter, r *http.Request) {
	response := api.AuthConfigResponse{DeploymentMode: s.deploymentMode, Methods: s.signInMethods(), EmailDelivery: s.emailConfigured(), AppOrigin: s.appOrigin.String(), SignupEnabled: true, OrganizationCreationEnabled: !s.selfHostedMode() && !s.organizationCreationAdminOnly}
	if err := s.tx.QueryRow(r.Context(), `SELECT NOT EXISTS(SELECT 1 FROM org_members WHERE is_owner AND removed_at IS NULL)`).Scan(&response.SetupRequired); err != nil {
		writeError(w, err)
		return
	}
	if s.selfHostedMode() {
		rows, err := s.db.ListOrganizationIDs(r.Context(), 2)
		if err != nil {
			writeError(w, err)
			return
		}
		if len(rows) == 1 {
			var o api.OrganizationMembership
			if err = s.tx.QueryRow(r.Context(), `SELECT id,name,slug FROM organizations WHERE id=$1`, rows[0]).Scan(&o.ID, &o.Name, &o.Slug); err != nil {
				writeError(w, err)
				return
			}
			response.SingleOrganization = &o
		}
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
	clearBinding(w, r)
	w.WriteHeader(http.StatusNoContent)
}
