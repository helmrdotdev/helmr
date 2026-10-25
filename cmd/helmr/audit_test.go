package main

import (
	"net/http"
	"net/http/httptest"
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
	if _, _, err = runPermissionCLI(append([]string{"installation", "audit"}, flags...)...); err != nil {
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
