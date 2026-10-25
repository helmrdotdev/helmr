package main

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"uuid"
)

func runPermissionCLI(args ...string) (string, string, error) {
	root := newRootCommand()
	var out, stderr bytes.Buffer
	root.SetOut(&out)
	root.SetErr(&stderr)
	root.SetArgs(args)
	err := executeCommand(root, args)
	return out.String(), stderr.String(), err
}

func TestPermissionCLIHumanIdentityAndConditionalPatch(t *testing.T) {
	role := uuid.NewV7().String()
	patches := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer human-session" {
			t.Errorf("wrong authority: %q", r.Header.Get("Authorization"))
		}
		w.Header().Set("Content-Type", "application/json")
		switch r.Method + " " + r.URL.Path {
		case "GET /api/roles/" + role:
			json.NewEncoder(w).Encode(map[string]any{"id": role, "name": "Existing", "description": "Preserved", "permissions": []string{"agents.use"}, "revision": 123})
		case "GET /api/role-grants":
			w.Write([]byte(`{"grants":[]}`))
		case "PATCH /api/roles/" + role:
			patches++
			if r.Header.Get("If-Match") != `"123"` {
				t.Errorf("missing revision %q", r.Header.Get("If-Match"))
			}
			var body map[string]any
			json.NewDecoder(r.Body).Decode(&body)
			if len(body) != 1 || body["description"] != "New description" {
				t.Errorf("patch overwrites omitted fields: %v", body)
			}
			w.WriteHeader(412)
			w.Write([]byte(`{"error":{"code":"stale_revision","message":"object changed"}}`))
		default:
			t.Errorf("unexpected %s %s", r.Method, r.URL)
			w.WriteHeader(404)
		}
	}))
	defer server.Close()
	state := installTestCLIConfig(t)
	state.SaveLogin(server.URL, "human-session")
	t.Setenv(helmrAPIKeyEnv, "workload-key")
	t.Setenv(helmrAPIURLEnv, "")
	stdout, _, err := runPermissionCLI("role", "update", "--role-id", role, "--description", "New description", "--json")
	if err == nil || stdout != "" || patches != 1 {
		t.Fatalf("stdout=%q err=%v patches=%d", stdout, err, patches)
	}
	var envelope struct {
		Error struct {
			Code string `json:"code"`
		} `json:"error"`
	}
	if json.Unmarshal([]byte(err.Error()), &envelope) != nil || envelope.Error.Code != "stale_revision" {
		t.Fatalf("error not structured: %v", err)
	}
}

func TestPermissionCLIGrantScopeAndContext(t *testing.T) {
	role, group, project, env, agent, grant := uuid.NewV7().String(), uuid.NewV7().String(), uuid.NewV7().String(), uuid.NewV7().String(), uuid.NewV7().String(), uuid.NewV7().String()
	writes := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch r.Method + " " + r.URL.Path {
		case "GET /api/roles/" + role:
			json.NewEncoder(w).Encode(map[string]any{"id": role, "name": "Users", "permissions": []string{"agents.use"}, "revision": 4})
		case "GET /api/groups/" + group:
			json.NewEncoder(w).Encode(map[string]any{"id": group, "name": "Team"})
		case "GET /api/projects/work":
			w.Write([]byte(`{"project":"work","name":"Work"}`))
		case "GET /api/projects/work/access-targets":
			json.NewEncoder(w).Encode(map[string]any{"project_id": project, "targets": []any{map[string]any{"environment_id": env, "environment": "production", "agents": []any{map[string]any{"id": agent, "key": "invoice"}}}}})
		case "POST /api/role-grants":
			writes++
			var body map[string]any
			json.NewDecoder(r.Body).Decode(&body)
			ids, _ := body["agent_ids"].([]any)
			subject, _ := body["subject"].(map[string]any)
			if body["project_id"] != project || body["environment_id"] != env || subject["group_id"] != group || subject["type"] != "group" || body["role_id"] != role || body["agents_mode"] != "selected" || len(ids) != 1 || ids[0] != agent || body["expected_role_revision"] != float64(4) {
				t.Errorf("incorrect grouped targets: %v", body)
			}
			json.NewEncoder(w).Encode(map[string]any{"grants": []any{map[string]any{"id": grant, "created": false, "project": "work", "environment": "production", "agent_keys": []string{"invoice"}, "agents_mode": "selected"}}})

		case "GET /api/role-grants/" + grant:
			json.NewEncoder(w).Encode(map[string]any{"id": grant, "role_name": "Users", "group_name": "Team", "project": "work", "environment": "production", "agent_key": "invoice"})
		default:
			t.Errorf("unexpected %s %s", r.Method, r.URL)
			w.WriteHeader(404)
		}
	}))
	defer server.Close()
	state := installTestCLIConfig(t)
	state.SaveLogin(server.URL, "human")
	t.Setenv(helmrAPIURLEnv, "")
	common := []string{"role", "grant", "create", "--group-id", group, "--role-id", role, "--project", "work", "--env", "production"}
	if _, _, err := runPermissionCLI(common...); err == nil {
		t.Fatal("omitted scope accepted")
	}
	if writes != 0 {
		t.Fatal("invalid invocation wrote grant")
	}
	stdout, _, err := runPermissionCLI(append(common, "--agent", "invoice", "--json")...)
	if err != nil || !strings.Contains(stdout, `"project":"work"`) || writes != 1 {
		t.Fatalf("%s %v", stdout, err)
	}
	if _, _, err := runPermissionCLI("role", "grant", "create", "--all-users", "--role-id", role, "--project", "work", "--env", "production", "--entire-environment"); err == nil {
		t.Fatal("all users environment accepted")
	}
}

func TestPermissionCLISelfInspectionDoesNotRequireManagementLookup(t *testing.T) {
	user := uuid.NewV7().String()
	calls := []string{}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls = append(calls, r.URL.Path)
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Path {
		case "/api/me":
			json.NewEncoder(w).Encode(map[string]any{"user_id": uuid.NewV7().String(), "membership_id": user})
		case "/api/projects/work":
			w.Write([]byte(`{"project":"work"}`))
		case "/api/members/" + user + "/permissions":
			if r.URL.Query().Get("agent") != "invoice" || r.URL.Query().Get("environment") != "production" {
				t.Errorf("missing filters: %s", r.URL)
			}
			json.NewEncoder(w).Encode(map[string]any{"user_id": user, "status": "active", "scopes": []any{}})
		default:
			t.Errorf("self used privileged lookup %s", r.URL)
			w.WriteHeader(403)
		}
	}))
	defer server.Close()
	state := installTestCLIConfig(t)
	state.SaveLogin(server.URL, "human")
	t.Setenv(helmrAPIURLEnv, "")
	_, _, err := runPermissionCLI("member", "permissions", "--self", "--project", "work", "--env", "production", "--agent", "invoice", "--json")
	if err != nil {
		t.Fatal(err)
	}
	if len(calls) != 3 {
		t.Fatal(calls)
	}
}

func TestPermissionCLIGrantPaginationAndDeleteConfirmation(t *testing.T) {
	group, grant := uuid.NewV7().String(), uuid.NewV7().String()
	deleted := false
	pages := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch r.Method + " " + r.URL.Path {
		case "GET /api/groups/" + group:
			json.NewEncoder(w).Encode(map[string]any{"id": group, "name": "Team"})
		case "GET /api/role-grants":
			pages++
			if r.URL.Query().Get("group_id") != group {
				t.Error("filter lost")
			}
			if r.URL.Query().Get("cursor") == "" {
				json.NewEncoder(w).Encode(map[string]any{"grants": []any{map[string]any{"id": grant}}, "next_cursor": grant})
			} else {
				w.Write([]byte(`{"grants":[]}`))
			}
		case "DELETE /api/groups/" + group:
			deleted = true
			w.WriteHeader(204)
		default:
			t.Errorf("unexpected %s %s", r.Method, r.URL)
			w.WriteHeader(404)
		}
	}))
	defer server.Close()
	state := installTestCLIConfig(t)
	state.SaveLogin(server.URL, "human")
	t.Setenv(helmrAPIURLEnv, "")
	stdout, stderr, err := runPermissionCLI("group", "delete", "--group-id", group, "--json")
	if err == nil || stdout != "" || stderr != "" || deleted || pages != 2 {
		t.Fatalf("%q %q %v %t %d", stdout, stderr, err, deleted, pages)
	}
	if !strings.Contains(err.Error(), grant) || !strings.Contains(err.Error(), "confirmation_required") {
		t.Fatal(err)
	}
	_, _, err = runPermissionCLI("group", "delete", "--group-id", group, "--yes", "--json")
	if err != nil || !deleted {
		t.Fatal(err)
	}
}

func TestPermissionCLIArgumentErrorsAreJSON(t *testing.T) {
	for _, args := range [][]string{
		{"role", "list", "--json", "--unknown"},
		{"role", "list", "--json=1", "--unknown"},
		{"group", "get", "--unknown", "--json"},
		{"member", "permissions", "--json", "--user"},
		{"role", "list", "--json", "extra"},
	} {
		_, _, err := runPermissionCLI(args...)
		if err == nil {
			t.Fatalf("expected error: %v", args)
		}
		var result struct{ Error struct{ Code string } }
		if json.Unmarshal([]byte(err.Error()), &result) != nil || result.Error.Code != "invalid_input" {
			t.Fatalf("%v: %v", args, err)
		}
	}
}

func TestPermissionCLIJSONFalseOverride(t *testing.T) {
	_, _, err := runPermissionCLI("role", "list", "--json", "--json=false", "--unknown")
	if err == nil || !strings.Contains(err.Error(), "unknown flag") || strings.HasPrefix(err.Error(), "{") {
		t.Fatal(err)
	}
}

func TestPermissionCLIRoleUpdateReportsAffectedGrants(t *testing.T) {
	role := uuid.NewV7().String()
	grant := uuid.NewV7().String()
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch r.Method + " " + r.URL.Path {
		case "GET /api/roles/" + role, "PATCH /api/roles/" + role:
			json.NewEncoder(w).Encode(map[string]any{"id": role, "name": "Shared", "revision": 123})
		case "GET /api/role-grants":
			if r.URL.Query().Get("role_id") != role {
				t.Error("missing affected role filter")
			}
			json.NewEncoder(w).Encode(map[string]any{"grants": []map[string]any{{"id": grant, "role_id": role, "group_name": "Business", "project": "demo", "environment": "test", "agent_key": "helper"}}})
		default:
			t.Errorf("unexpected %s %s", r.Method, r.URL)
			w.WriteHeader(404)
		}
	}))
	defer server.Close()
	state := installTestCLIConfig(t)
	state.SaveLogin(server.URL, "human-session")
	out, _, err := runPermissionCLI("role", "update", "--role-id", role, "--description", "Changed", "--json")
	if err != nil {
		t.Fatal(err)
	}
	var result struct {
		ID             string
		AffectedGrants []struct{ ID string } `json:"affected_grants"`
	}
	if json.Unmarshal([]byte(out), &result) != nil || result.ID != role || len(result.AffectedGrants) != 1 || result.AffectedGrants[0].ID != grant {
		t.Fatal(out)
	}
}

func TestPermissionCLIEmailSelectsActiveRejoinedMembership(t *testing.T) {
	active, removed := uuid.NewV7().String(), uuid.NewV7().String()
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Path {
		case "/api/members":
			json.NewEncoder(w).Encode(map[string]any{"members": []any{map[string]any{"membership_id": removed, "email": "person@example.test", "status": "removed"}, map[string]any{"membership_id": active, "email": "person@example.test", "status": "active"}}})
		case "/api/members/" + active + "/permissions":
			json.NewEncoder(w).Encode(map[string]any{"membership_id": active, "status": "active", "scopes": []any{}})
		default:
			t.Errorf("unexpected lookup %s", r.URL)
			w.WriteHeader(404)
		}
	}))
	defer server.Close()
	state := installTestCLIConfig(t)
	state.SaveLogin(server.URL, "human")
	t.Setenv(helmrAPIURLEnv, "")
	if _, _, err := runPermissionCLI("member", "permissions", "--user", "person@example.test", "--json"); err != nil {
		t.Fatal(err)
	}
}

func TestPermissionCLIGrantPreviewUsesInspectedRoleRevision(t *testing.T) {
	role, group, project, env, agent, grant := uuid.NewV7().String(), uuid.NewV7().String(), uuid.NewV7().String(), uuid.NewV7().String(), uuid.NewV7().String(), uuid.NewV7().String()
	writes := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch r.Method + " " + r.URL.Path {
		case "GET /api/role-grants/" + grant:
			json.NewEncoder(w).Encode(map[string]any{"id": grant, "revision": 3, "role_revision": 8, "subject_kind": "group", "group_id": group, "group_name": "Team", "role_id": role, "role_name": "Tools", "project": "work", "environment": "production", "target_kind": "environment", "agents_mode": "selected", "agent_ids": []string{agent}, "agent_keys": []string{"invoice"}, "permissions": []string{"agents.use"}, "targets": []any{map[string]any{"resource_type": "agents", "mode": "selected", "permissions": []string{"agents.use"}}}})
		case "GET /api/roles/" + role:
			// The Role changed after the assignment response was read.
			json.NewEncoder(w).Encode(map[string]any{"id": role, "revision": 9, "name": "Tools", "permissions": []string{"agents.use", "sessions.send", "secrets.create"}})
		case "GET /api/projects/work":
			w.Write([]byte(`{"project":"work","name":"Work"}`))
		case "GET /api/projects/work/access-targets":
			json.NewEncoder(w).Encode(map[string]any{"project_id": project, "targets": []any{map[string]any{"environment_id": env, "environment": "production", "agents": []any{map[string]any{"id": agent, "key": "invoice"}}}}})
		case "PATCH /api/role-grants/" + grant:
			writes++
			var input map[string]any
			json.NewDecoder(r.Body).Decode(&input)
			if r.Header.Get("If-Match") != `"3"` || input["expected_role_revision"] != float64(9) {
				t.Errorf("unpinned write: %v %v", r.Header, input)
			}
			json.NewEncoder(w).Encode(map[string]any{"id": grant, "revision": 4, "role_revision": 9})
		default:
			t.Errorf("unexpected %s %s", r.Method, r.URL)
			w.WriteHeader(404)
		}
	}))
	defer server.Close()
	state := installTestCLIConfig(t)
	state.SaveLogin(server.URL, "human")
	t.Setenv(helmrAPIURLEnv, "")
	args := []string{"role", "grant", "update", "--id", grant, "--agent", "invoice"}
	_, stderr, err := runPermissionCLI(args...)
	if err == nil || writes != 0 || !strings.Contains(stderr, "secrets: environment") || !strings.Contains(stderr, "sessions.send") || !strings.Contains(stderr, "agents.use") || !strings.Contains(stderr, "Role revision: 9") {
		t.Fatalf("incomplete or mutating preview: %s %v writes=%d", stderr, err, writes)
	}
	_, _, err = runPermissionCLI(append(args, "--json")...)
	if err == nil || !strings.Contains(err.Error(), `"role_revision":9`) || !strings.Contains(err.Error(), "secrets.create") {
		t.Fatalf("JSON preview omitted inspected role: %v", err)
	}
	if _, _, err := runPermissionCLI(append(args, "--yes")...); err != nil || writes != 1 {
		t.Fatalf("confirmed update: %v writes=%d", err, writes)
	}
}

func TestPermissionCLIAPIKeyRecipient(t *testing.T) {
	role, key, project, env := uuid.NewV7().String(), uuid.NewV7().String(), uuid.NewV7().String(), uuid.NewV7().String()
	writes, lists := 0, 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch r.Method + " " + r.URL.Path {
		case "GET /api/roles/" + role:
			json.NewEncoder(w).Encode(map[string]any{"id": role, "name": "Reader", "revision": 7, "permissions": []string{"sessions.read"}, "api_key_assignable": true})
		case "GET /api/projects/work":
			w.Write([]byte(`{"project":"work"}`))
		case "GET /api/projects/work/access-targets":
			json.NewEncoder(w).Encode(map[string]any{"project_id": project, "targets": []any{map[string]any{"environment_id": env, "environment": "production"}}})
		case "GET /api/role-grants":
			lists++
			if r.URL.Query().Get("api_key_id") != key {
				t.Error("API key filter lost")
			}
			w.Write([]byte(`{"grants":[]}`))
		case "POST /api/role-grants":
			writes++
			var body map[string]any
			json.NewDecoder(r.Body).Decode(&body)
			subject, _ := body["subject"].(map[string]any)
			if subject["type"] != "api_key" || subject["api_key_id"] != key || body["environment_id"] != env || body["expected_role_revision"] != float64(7) {
				t.Errorf("invalid key assignment: %v", body)
			}
			w.Write([]byte(`{"grants":[]}`))
		default:
			t.Errorf("unexpected request %s %s", r.Method, r.URL)
			w.WriteHeader(404)
		}
	}))
	defer server.Close()
	state := installTestCLIConfig(t)
	state.SaveLogin(server.URL, "human")
	t.Setenv(helmrAPIURLEnv, "")
	if _, _, err := runPermissionCLI("role", "grant", "create", "--api-key-id", key, "--role-id", role, "--project", "work", "--env", "production", "--all-agents", "--json"); err != nil {
		t.Fatal(err)
	}
	if _, _, err := runPermissionCLI("role", "grant", "list", "--api-key-id", key, "--json"); err != nil {
		t.Fatal(err)
	}
	if writes != 1 || lists != 1 {
		t.Fatal(writes, lists)
	}
}
