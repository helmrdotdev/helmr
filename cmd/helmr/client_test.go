package main

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/helmrdotdev/helmr/internal/api"
)

func TestAPIURLFlagOverridesEnvironmentURL(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet || r.URL.Path != "/v1/sessions/019c10d5-a6f7-7af1-8f5f-bb97bcc0dc31" {
			t.Fatalf("%s %s", r.Method, r.URL.Path)
		}
		if got := r.Header.Get("authorization"); got != "Bearer env-key" {
			t.Fatalf("auth = %s", got)
		}
		_ = json.NewEncoder(w).Encode(api.AgentSession{ID: "019c10d5-a6f7-7af1-8f5f-bb97bcc0dc31", Status: "open"})
	}))
	defer server.Close()
	t.Setenv(helmrAPIURLEnv, "https://ignored.example.test")
	t.Setenv(helmrAPIKeyEnv, "env-key")

	var out bytes.Buffer
	cmd := newRootCommand()
	cmd.SetOut(&out)
	cmd.SetErr(&bytes.Buffer{})
	cmd.SetArgs([]string{"--api-url", server.URL, "session", "get", "019c10d5-a6f7-7af1-8f5f-bb97bcc0dc31"})
	if err := cmd.Execute(); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out.String(), "session_status: open") {
		t.Fatalf("output = %q", out.String())
	}
}

func TestControlPlaneClientRejectsPlainHTTPNonLoopback(t *testing.T) {
	t.Setenv(helmrAPIURLEnv, "http://helmr.example")
	t.Setenv(helmrAPIKeyEnv, "test-key")

	_, err := controlPlaneClient(nil)
	if err == nil || !strings.Contains(err.Error(), "plaintext non-loopback") {
		t.Fatalf("err = %v", err)
	}
}

func TestControlPlaneClientRejectsAPIKeyWhitespace(t *testing.T) {
	t.Setenv(helmrAPIURLEnv, "https://helmr.example")
	t.Setenv(helmrAPIKeyEnv, " test-key")

	_, err := controlPlaneClient(nil)
	if err == nil || !strings.Contains(err.Error(), "must not have surrounding whitespace") {
		t.Fatalf("err = %v", err)
	}
}

func TestControlPlaneClientRejectsURLQueryAndFragment(t *testing.T) {
	t.Setenv(helmrAPIKeyEnv, "test-key")
	for _, raw := range []string{"https://helmr.example?x=1", "https://helmr.example/#fragment"} {
		t.Setenv(helmrAPIURLEnv, raw)
		_, err := controlPlaneClient(nil)
		if err == nil || !strings.Contains(err.Error(), "must be an origin without credentials, path, query, or fragment") {
			t.Fatalf("controlPlaneClient(%q) err = %v", raw, err)
		}
	}
}
