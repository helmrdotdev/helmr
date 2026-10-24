package controlplane

import (
	"encoding/json"
	"net/http"
	"testing"
	"uuid"

	"github.com/helmrdotdev/helmr/internal/api"
	"github.com/helmrdotdev/helmr/internal/db"
)

func TestOrganizationCreationAdminOnly(t *testing.T) {
	for _, tc := range []struct {
		name      string
		adminOnly bool
		admin     bool
		role      db.OrgMemberRole
		want      int
	}{
		{name: "ordinary signup", want: http.StatusCreated},
		{name: "unaffiliated user", adminOnly: true, want: http.StatusForbidden},
		{name: "organization owner", adminOnly: true, role: db.OrgMemberRoleOwner, want: http.StatusForbidden},
		{name: "organization admin", adminOnly: true, role: db.OrgMemberRoleAdmin, want: http.StatusForbidden},
		{name: "developer", adminOnly: true, role: db.OrgMemberRoleDeveloper, want: http.StatusForbidden},
		{name: "viewer", adminOnly: true, role: db.OrgMemberRoleViewer, want: http.StatusForbidden},
		{name: "installation admin", adminOnly: true, admin: true, want: http.StatusCreated},
	} {
		t.Run(tc.name, func(t *testing.T) {
			fixture := newHTTPPostgresFixture(t, func(cfg *ServerConfig) {
				cfg.DeploymentMode = deploymentModeManagedCloud
				cfg.OrganizationCreationAdminOnly = tc.adminOnly
			})
			user := fixture.user(t, "User")
			if tc.admin {
				if _, err := fixture.pool.Exec(t.Context(), "UPDATE users SET admin = true WHERE id = $1", user); err != nil {
					t.Fatal(err)
				}
			}
			orgID, _ := fixture.organizationOwner(t, "existing")
			if tc.role != "" {
				fixture.member(t, orgID, user, tc.role)
			}
			token := fixture.session(t, user, uuid.Nil())
			me := fixture.request(t, http.MethodGet, "/api/me", token, "")
			var account api.MeResponse
			if me.Code != http.StatusOK || json.Unmarshal(me.Body.Bytes(), &account) != nil {
				t.Fatalf("me: %d %s", me.Code, me.Body.String())
			}
			if tc.role == "" {
				blocked := tc.adminOnly && !tc.admin
				if account.AccessRequired != blocked || account.OrganizationRequired == blocked || account.SetupTokenRequired {
					t.Fatalf("onboarding: %+v", account)
				}
				// Installation authority does not grant access to another organization's members.
				if w := fixture.request(t, http.MethodGet, "/api/members", token, ""); w.Code != http.StatusForbidden {
					t.Fatalf("unaffiliated member access: %d %s", w.Code, w.Body.String())
				}
			} else if account.OrgID != orgID.String() || account.Role != string(tc.role) || account.AccessRequired || account.OrganizationRequired {
				t.Fatalf("existing member: %+v", account)
			}

			w := fixture.request(t, http.MethodPost, "/api/organizations", token, `{"slug":"new","name":"New"}`)
			if w.Code != tc.want {
				t.Fatalf("create: %d %s, want %d", w.Code, w.Body.String(), tc.want)
			}
			var count int
			if err := fixture.pool.QueryRow(t.Context(), "SELECT count(*) FROM organizations").Scan(&count); err != nil {
				t.Fatal(err)
			}
			if tc.want == http.StatusForbidden {
				if count != 1 {
					t.Fatalf("denied creation wrote organization: count=%d", count)
				}
				return
			}
			var organization api.OrganizationSummary
			if count != 2 || json.Unmarshal(w.Body.Bytes(), &organization) != nil {
				t.Fatalf("created organization: count=%d body=%s", count, w.Body.String())
			}
			var role string
			if err := fixture.pool.QueryRow(t.Context(), "SELECT role FROM org_members WHERE org_id = $1 AND user_id = $2", organization.ID, user).Scan(&role); err != nil || role != "owner" {
				t.Fatalf("created owner: role=%s error=%v", role, err)
			}
		})
	}
}

func TestOrganizationCreationAdminOnlyPreservesSelfHostedSetup(t *testing.T) {
	fixture := newHTTPPostgresFixture(t, func(cfg *ServerConfig) {
		cfg.DeploymentMode = deploymentModeSelfHosted
		cfg.OrganizationCreationAdminOnly = true
		cfg.SetupToken = "setup-token"
	})
	user := fixture.user(t, "First owner")
	token := fixture.session(t, user, uuid.Nil())
	if w := fixture.request(t, http.MethodPost, "/api/organizations", token, `{"slug":"first","name":"First"}`); w.Code != http.StatusForbidden {
		t.Fatalf("missing setup token: %d %s", w.Code, w.Body.String())
	}
	if w := fixture.request(t, http.MethodPost, "/api/organizations", token, `{"slug":"first","name":"First","setup_token":"setup-token"}`); w.Code != http.StatusCreated {
		t.Fatalf("initial setup: %d %s", w.Code, w.Body.String())
	}
	if w := fixture.request(t, http.MethodPost, "/api/organizations", token, `{"slug":"second","name":"Second","setup_token":"setup-token"}`); w.Code != http.StatusConflict {
		t.Fatalf("second organization: %d %s", w.Code, w.Body.String())
	}
}
