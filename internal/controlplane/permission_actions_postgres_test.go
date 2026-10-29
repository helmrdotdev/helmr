package controlplane

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"uuid"

	"github.com/helmrdotdev/helmr/internal/api"
	"github.com/helmrdotdev/helmr/internal/db/dbtest"
)

// A mutation must not depend on its neighboring read or management actions.
func TestOrganizationActionsAuthorizeIndependentlyHTTP(t *testing.T) {
	f := newInvitationHTTPFixture(t, &batchInvitationSender{})
	organization, owner := f.organizationOwner(t, "leaf-actions")
	project, environment := deploymentHTTPEnvironment(t, f, organization)
	agent := uuid.NewV7()
	dbtest.MustExec(t, t.Context(), f.pool, `INSERT INTO agents(id,environment_id,key) VALUES($1,$2,'leaf-agent')`, agent, environment)
	request := func(t *testing.T, method, path, token, body, revision string, status int, out any) {
		t.Helper()
		r := httptest.NewRequest(method, path, strings.NewReader(body))
		f.authenticateRequest(r, token)
		if body != "" {
			r.Header.Set("Content-Type", "application/json")
		}
		if revision != "" {
			r.Header.Set("If-Match", `"`+revision+`"`)
		}
		w := httptest.NewRecorder()
		f.handler.ServeHTTP(w, r)
		if w.Code != status {
			t.Fatalf("%s %s: %d %s, want %d", method, path, w.Code, w.Body.String(), status)
		}
		if out != nil {
			if err := json.Unmarshal(w.Body.Bytes(), out); err != nil {
				t.Fatal(err)
			}
		}
	}
	encode := func(value any) string {
		body, err := json.Marshal(value)
		if err != nil {
			t.Fatal(err)
		}
		return string(body)
	}
	for _, permission := range []string{"roles.read", "roles.create", "roles.update", "roles.delete", "groups.read", "groups.create", "groups.update", "groups.delete", "groups.add_member", "groups.remove_member", "assignments.read", "assignments.create", "assignments.update", "assignments.delete", "members.read", "members.remove", "invitations.read", "invitations.create", "invitations.revoke"} {
		t.Run(permission, func(t *testing.T) {
			user := f.user(t, permission)
			f.member(t, organization, user, "member")
			dbtest.GrantMember(t, f.pool, organization, user, nil, permission)
			token := f.session(t, user, organization)
			recipient := f.user(t, "Recipient")
			member := f.member(t, organization, recipient, "member")
			var role api.PermissionRole
			var group api.PermissionGroup
			request(t, "POST", "/api/roles", owner, encode(map[string]any{"name": permission + " role", "permissions": []string{"agents.use"}}), "", 201, &role)
			request(t, "POST", "/api/groups", owner, encode(map[string]any{"name": permission + " group"}), "", 201, &group)
			input := api.RoleGrantInput{Subject: api.GrantSubject{Type: "member", MemberID: new(member.String())}, RoleID: role.ID, ProjectID: new(project.String()), EnvironmentID: new(environment.String()), AgentsMode: "selected", AgentIDs: []string{agent.String()}, ExpectedRoleRevision: role.Revision}
			var grants api.RoleGrantsResponse
			if strings.HasPrefix(permission, "assignments.") && permission != "assignments.create" {
				request(t, "POST", "/api/role-grants", owner, encode(input), "", 201, &grants)
			}
			// Catalog metadata is fixed product information and does not expose Roles.
			request(t, "GET", "/api/permission-catalog", token, "", "", 200, nil)
			denied := "/api/roles"
			if permission == "roles.read" {
				denied = "/api/groups"
			}
			request(t, "GET", denied, token, "", "", 403, nil)
			switch permission {
			case "roles.read":
				request(t, "GET", "/api/roles/"+role.ID, token, "", "", 200, nil)
			case "roles.create":
				request(t, "POST", "/api/roles", token, `{"name":"Created by leaf","permissions":["computers.read"]}`, "", 201, nil)
			case "roles.update":
				request(t, "PATCH", "/api/roles/"+role.ID, token, `{"description":"Updated without read"}`, "1", 200, nil)
			case "roles.delete":
				request(t, "DELETE", "/api/roles/"+role.ID, token, "", "", 204, nil)
			case "groups.read":
				request(t, "GET", "/api/groups/"+group.ID, token, "", "", 200, nil)
			case "groups.create":
				request(t, "POST", "/api/groups", token, `{"name":"Created by leaf"}`, "", 201, nil)
			case "groups.update":
				request(t, "PATCH", "/api/groups/"+group.ID, token, `{"name":"Updated without read"}`, "1", 200, nil)
			case "groups.delete":
				request(t, "DELETE", "/api/groups/"+group.ID, token, "", "", 204, nil)
			case "groups.add_member":
				request(t, "PUT", "/api/groups/"+group.ID+"/members/"+member.String(), token, "", "", 204, nil)
			case "groups.remove_member":
				request(t, "PUT", "/api/groups/"+group.ID+"/members/"+member.String(), owner, "", "", 204, nil)
				request(t, "DELETE", "/api/groups/"+group.ID+"/members/"+member.String(), token, "", "", 204, nil)
			case "assignments.read":
				request(t, "GET", "/api/role-grants/"+grants.Grants[0].ID, token, "", "", 200, nil)
			case "assignments.create":
				request(t, "POST", "/api/role-grants", token, encode(input), "", 201, nil)
			case "assignments.update":
				input.AgentsMode = "all"
				input.AgentIDs = nil
				request(t, "PATCH", "/api/role-grants/"+grants.Grants[0].ID, token, encode(input), "1", 200, nil)
			case "assignments.delete":
				request(t, "DELETE", "/api/role-grants/"+grants.Grants[0].ID, token, "", "1", 204, nil)
			case "members.read":
				request(t, "GET", "/api/members", token, "", "", 200, nil)
			case "members.remove":
				request(t, "DELETE", "/api/members/"+member.String(), token, "", "", 204, nil)
			case "invitations.create":
				request(t, "POST", "/api/invitations", token, `{"emails":["leaf-create@example.test"]}`, "", 200, nil)
				request(t, "GET", "/api/invitations", token, "", "", 403, nil)
			case "invitations.revoke":
				request(t, "POST", "/api/invitations", owner, `{"emails":["leaf-revoke@example.test"]}`, "", 200, nil)
				var invitation uuid.UUID
				if err := f.pool.QueryRow(t.Context(), `SELECT id FROM invitations WHERE invitee_email='leaf-revoke@example.test'`).Scan(&invitation); err != nil {
					t.Fatal(err)
				}
				request(t, "DELETE", "/api/invitations/"+invitation.String(), token, "", "", 204, nil)
				request(t, "POST", "/api/invitations", token, `{"emails":["unauthorized@example.test"]}`, "", 403, nil)
			case "invitations.read":
				request(t, "GET", "/api/invitations", token, "", "", 200, nil)
			}
		})
	}
	request(t, http.MethodGet, "/api/permission-catalog", "", "", "", http.StatusUnauthorized, nil)
}
