package controlplane

import (
	"encoding/json"
	"net/http"
	"testing"
	"uuid"
)

// A browser whose sign-in is older than the recent-proof window gets the code
// that opens the Console confirmation dialog for actions that add authority,
// and is not asked to confirm actions that only take authority away.
func TestRecentProofHTTPAddsAuthorityOnly(t *testing.T) {
	f := newHTTPPostgresFixture(t)
	orgID, owner := f.organizationOwner(t, "recent-proof")
	member := f.member(t, orgID, f.user(t, "Member"), "member")
	var group struct {
		ID string `json:"id"`
	}
	created := f.request(t, http.MethodPost, "/api/groups", owner, `{"name":"Team"}`)
	if created.Code != http.StatusCreated {
		t.Fatalf("create group: %d %s", created.Code, created.Body.String())
	}
	if err := json.Unmarshal(created.Body.Bytes(), &group); err != nil {
		t.Fatal(err)
	}
	if _, err := f.pool.Exec(t.Context(), `UPDATE login_sessions SET authenticated_at=clock_timestamp()-interval '3 hours' WHERE channel='browser'`); err != nil {
		t.Fatal(err)
	}

	for _, tc := range []struct {
		name, method, path, body string
		status                   int
	}{
		{"create role", http.MethodPost, "/api/roles", `{"name":"Readers","permissions":["members.read"]}`, http.StatusForbidden},
		{"suspend member", http.MethodPost, "/api/members/" + member.String() + "/suspend", "", http.StatusNoContent},
		{"resume member", http.MethodPost, "/api/members/" + member.String() + "/resume", "", http.StatusForbidden},
		{"revoke member sessions", http.MethodPost, "/api/members/" + member.String() + "/revoke-sessions", "", http.StatusNoContent},
		{"delete group", http.MethodDelete, "/api/groups/" + group.ID, "", http.StatusNoContent},
		{"remove member", http.MethodDelete, "/api/members/" + member.String(), "", http.StatusNoContent},
	} {
		got := f.request(t, tc.method, tc.path, owner, tc.body)
		if got.Code != tc.status {
			t.Fatalf("%s: %d %s", tc.name, got.Code, got.Body.String())
		}
		if tc.status != http.StatusForbidden {
			continue
		}
		var response struct {
			Error struct {
				Code string `json:"code"`
			} `json:"error"`
		}
		if err := json.Unmarshal(got.Body.Bytes(), &response); err != nil || response.Error.Code != "recent_authentication_required" {
			t.Fatalf("%s: code %q %v", tc.name, response.Error.Code, err)
		}
	}
}

// Creating an organization uses the current browser sign-in.
func TestOrganizationCreationNeedsNoRecentProof(t *testing.T) {
	f := newHTTPPostgresFixture(t, func(cfg *ServerConfig) { cfg.DeploymentMode = deploymentModeManagedCloud })
	f.organizationOwner(t, "existing")
	user := f.user(t, "User")
	browser := f.session(t, user, uuid.Nil())
	if _, err := f.pool.Exec(t.Context(), `UPDATE login_sessions SET authenticated_at=clock_timestamp()-interval '3 hours' WHERE channel='browser'`); err != nil {
		t.Fatal(err)
	}
	if got := f.request(t, http.MethodPost, "/api/organizations", browser, `{"slug":"browser","name":"Browser"}`); got.Code != http.StatusCreated {
		t.Fatalf("browser creation: %d %s", got.Code, got.Body.String())
	}
}
