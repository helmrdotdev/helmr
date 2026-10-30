package main

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"testing"
	"uuid"
)

func TestAuditCLIUsesSelectedCredentialsAndBoundedNavigation(t *testing.T) {
	var routes []string
	next := uuid.NewV7().String()
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		routes = append(routes, r.URL.Path)
		expected := "Bearer human-session"
		if r.URL.Path == "/v1/audit-events" {
			expected = "Bearer workload-key"
		}
		if r.Header.Get("Authorization") != expected {
			t.Errorf("incorrect selected credential")
		}
		q := r.URL.Query()
		if q.Get("from") != "2026-10-01T00:00:00Z" || q.Get("to") != "2026-10-02T00:00:00Z" || q.Get("limit") != "5" {
			t.Errorf("incorrect bounded query: %s", r.URL.RawQuery)
		}
		if r.URL.Path == "/v1/audit-events" {
			w.WriteHeader(403)
			w.Write([]byte(`{"error":{"code":"forbidden","message":"denied"}}`))
			return
		}
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(`{"events":[],"next_before":{"recorded_at":"2026-10-01T12:00:00Z","id":"` + next + `"}}`))
	}))
	defer server.Close()
	state := installTestCLIConfig(t)
	if err := state.SaveLogin(server.URL, "human-session"); err != nil {
		t.Fatal(err)
	}
	t.Setenv(helmrAPIURLEnv, server.URL)
	t.Setenv(helmrAPIKeyEnv, "")
	flags := []string{"list", "--from", "2026-10-01T00:00:00Z", "--to", "2026-10-02T00:00:00Z", "--limit", "5", "--json"}
	out, _, err := runPermissionCLI(append([]string{"audit"}, flags...)...)
	if err != nil || !strings.Contains(out, next) {
		t.Fatal("human page", out, err)
	}
	t.Setenv(helmrAPIKeyEnv, "workload-key")
	if out, _, err = runPermissionCLI(append([]string{"audit"}, flags...)...); err == nil || out != "" {
		t.Fatal("denied workload response", out, err)
	}
	if len(routes) != 2 || routes[1] != "/v1/audit-events" {
		t.Fatal("tenant refusal escalated credentials", routes)
	}
	if _, _, err = runPermissionCLI(append([]string{"platform", "audit"}, flags...)...); err != nil {
		t.Fatal(err)
	}
	if routes[2] != "/admin/api/v1/audit-events" {
		t.Fatal(routes)
	}
	before := len(routes)
	if _, _, err = runPermissionCLI("audit", "list", "--from", "2026-10-01T00:00:00Z", "--to", "2026-12-01T00:00:00Z"); err == nil {
		t.Fatal("unbounded range admitted")
	}
	if _, _, err = runPermissionCLI(append(append([]string{"audit"}, flags...), "--before-id", next)...); err == nil {
		t.Fatal("partial cursor admitted")
	}
	if len(routes) != before {
		t.Fatal("invalid query sent request")
	}
}

func TestAuditCLIPreservesPublicAndAdminEvidence(t *testing.T) {
	id, host := uuid.NewV7().String(), uuid.NewV7().String()
	public := `{"id":"` + id + `","recorded_at":"2026-10-01T12:00:00Z","action":"session.content_read","outcome":"succeeded","actor_kind":"platform_service","restricted":["actor_identity","authorization"],"authorization":{"decision":"allowed","checks":[{"type":"permission","subject_user_id":"` + id + `","permission":"sessions.read","scope":{"org_id":"` + id + `","environment_id":"` + id + `","agent_id":"` + id + `"},"result":"passed"}],"platform_authority":false},"details":{"sequence":4}}`
	admin := `{"id":"` + id + `","recorded_at":"2026-10-01T12:00:00Z","action":"session.content_read","outcome":"succeeded","actor_worker_host_id":"` + host + `","source_ip":"10.20.30.40","runtime_generation":17,"authorization":{"decision":"allowed","checks":[{"type":"host_assignment","worker_host_id":"` + host + `","result":"passed"}]},"details":{"sequence":4,"generation":17}}`
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		event := public
		if r.URL.Path == "/admin/api/v1/audit-events" {
			event = admin
		} else if r.URL.Path != "/api/audit-events" {
			t.Errorf("unexpected route %s", r.URL.Path)
		}
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(`{"events":[` + event + `]}`))
	}))
	defer server.Close()
	state := installTestCLIConfig(t)
	if err := state.SaveLogin(server.URL, "human-session"); err != nil {
		t.Fatal(err)
	}
	t.Setenv(helmrAPIURLEnv, server.URL)
	t.Setenv(helmrAPIKeyEnv, "")
	for _, tc := range []struct {
		prefix       []string
		event, label string
	}{
		{[]string{"audit"}, public, "Platform service"}, {[]string{"platform", "audit"}, admin, host},
	} {
		args := append(tc.prefix, "list", "--from", "2026-10-01T00:00:00Z", "--to", "2026-10-02T00:00:00Z")
		out, _, err := runPermissionCLI(append(args, "--json")...)
		if err != nil {
			t.Fatal(err)
		}
		var expected map[string]any
		var actual struct {
			Events []map[string]any `json:"events"`
		}
		if err := json.Unmarshal([]byte(tc.event), &expected); err != nil {
			t.Fatal(err)
		}
		if err := json.Unmarshal([]byte(out), &actual); err != nil {
			t.Fatal(err)
		}
		if len(actual.Events) != 1 {
			t.Fatal(out)
		}
		for key, value := range expected {
			if !reflect.DeepEqual(actual.Events[0][key], value) {
				t.Fatalf("%s changed: got %#v want %#v", key, actual.Events[0][key], value)
			}
		}
		if tc.label == "Platform service" && strings.Contains(out, "actor_worker_host_id") {
			t.Fatal("public decoding includes raw Admin fields", out)
		}
		out, _, err = runPermissionCLI(args...)
		if err != nil || !strings.Contains(out, tc.label) {
			t.Fatal("actor label", out, err)
		}
	}
}
