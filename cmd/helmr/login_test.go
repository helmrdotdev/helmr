package main

import (
	"bytes"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/helmrdotdev/helmr/internal/api"
	"github.com/helmrdotdev/helmr/internal/clistate"
)

func TestLoginCommandStoresDeviceToken(t *testing.T) {
	state := installTestCLIConfig(t)
	var sawStart bool
	var sawToken bool
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if got := r.Header.Get("authorization"); got != "" {
			t.Fatalf("auth = %s", got)
		}
		switch r.URL.Path {
		case "/api/auth/device/start":
			sawStart = true
			_ = json.NewEncoder(w).Encode(api.DeviceStartResponse{
				DeviceCode:              "device-token",
				UserCode:                "ABCD-EFGH",
				VerificationURI:         "https://helmr.example.test/auth/device",
				VerificationURIComplete: "https://helmr.example.test/auth/device?code=ABCD-EFGH",
				ExpiresInSeconds:        60,
				IntervalSeconds:         1,
			})
		case "/api/auth/device/token":
			sawToken = true
			var request api.DeviceTokenRequest
			if err := json.NewDecoder(r.Body).Decode(&request); err != nil {
				t.Fatal(err)
			}
			if request.DeviceCode != "device-token" {
				t.Fatalf("request = %+v", request)
			}
			_ = json.NewEncoder(w).Encode(api.DeviceTokenResponse{
				AccessToken: "session_test",
			})
		default:
			t.Fatalf("unexpected path %s", r.URL.Path)
		}
	}))
	defer server.Close()
	t.Setenv(helmrAPIURLEnv, "https://ignored.example.test")

	var out bytes.Buffer
	cmd := newRootCommand()
	cmd.SetOut(&out)
	cmd.SetErr(&bytes.Buffer{})
	cmd.SetArgs([]string{"login", "--no-browser", server.URL})
	if err := cmd.Execute(); err != nil {
		t.Fatal(err)
	}
	if !sawStart || !sawToken {
		t.Fatalf("sawStart=%v sawToken=%v", sawStart, sawToken)
	}
	cfg, err := state.Load()
	if err != nil {
		t.Fatal(err)
	}
	if cfg.DefaultHost != server.URL {
		t.Fatalf("default host = %q", cfg.DefaultHost)
	}
	token, err := state.Token(server.URL)
	if err != nil {
		t.Fatal(err)
	}
	if token != "session_test" {
		t.Fatalf("token = %q", token)
	}
	if !strings.Contains(out.String(), "Code: ABCD-EFGH") || !strings.Contains(out.String(), "Logged in to "+server.URL) {
		t.Fatalf("output = %q", out.String())
	}
}

func TestLogoutCommandRevokesAndDeletesStoredToken(t *testing.T) {
	state := installTestCLIConfig(t)
	var sawLogout bool
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost || r.URL.Path != "/api/auth/logout" {
			t.Fatalf("%s %s", r.Method, r.URL.Path)
		}
		if got := r.Header.Get("authorization"); got != "Bearer session_test" {
			t.Fatalf("auth = %s", got)
		}
		sawLogout = true
		w.WriteHeader(http.StatusNoContent)
	}))
	defer server.Close()
	if err := state.SaveLogin(server.URL, "session_test"); err != nil {
		t.Fatal(err)
	}
	t.Setenv(helmrAPIURLEnv, "https://ignored.example.test")

	var out bytes.Buffer
	cmd := newRootCommand()
	cmd.SetOut(&out)
	cmd.SetErr(&bytes.Buffer{})
	cmd.SetArgs([]string{"logout", server.URL})
	if err := cmd.Execute(); err != nil {
		t.Fatal(err)
	}
	if !sawLogout {
		t.Fatal("logout endpoint was not called")
	}
	if _, err := state.Token(server.URL); !errors.Is(err, clistate.ErrNotFound) {
		t.Fatalf("token after logout error = %v, want ErrNotFound", err)
	}
	if !strings.Contains(out.String(), "Logged out from "+server.URL) {
		t.Fatalf("output = %q", out.String())
	}
}

func TestCommandUsesSavedLoginWhenEnvIsUnset(t *testing.T) {
	state := installTestCLIConfig(t)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if got := r.Header.Get("authorization"); got != "Bearer stored-key" {
			t.Fatalf("auth = %s", got)
		}
		switch {
		case r.Method == http.MethodGet && r.URL.Path == "/api/projects/project-1":
			_ = json.NewEncoder(w).Encode(api.ProjectSummary{
				Project: "project-1",
				Environments: []api.EnvironmentSummary{{
					Environment: "env-1",
				}},
			})
		case r.Method == http.MethodGet && r.URL.Path == "/api/projects/project-1/environments/env-1/sessions/019c10d5-a6f7-7af1-8f5f-bb97bcc0dc31":
			_ = json.NewEncoder(w).Encode(api.AgentSession{ID: "019c10d5-a6f7-7af1-8f5f-bb97bcc0dc31", Status: "open"})
		default:
			t.Fatalf("%s %s", r.Method, r.URL.Path)
		}
	}))
	defer server.Close()
	if err := state.SaveLogin(server.URL, "stored-key"); err != nil {
		t.Fatal(err)
	}

	run := func(args ...string) {
		t.Helper()
		var out bytes.Buffer
		cmd := newRootCommand()
		cmd.SetOut(&out)
		cmd.SetErr(&bytes.Buffer{})
		cmd.SetArgs(append([]string{"session", "get", "019c10d5-a6f7-7af1-8f5f-bb97bcc0dc31"}, args...))
		if err := cmd.Execute(); err != nil {
			t.Fatal(err)
		}
		if !strings.Contains(out.String(), "session_status: open") {
			t.Fatalf("output = %q", out.String())
		}
	}
	run("--project", "project-1", "--env", "env-1")
	t.Setenv("HELMR_PROJECT", "project-1")
	t.Setenv("HELMR_ENV", "env-1")
	run()
	t.Setenv("HELMR_PROJECT", "other-project")
	run("--project", "project-1")
}

func TestSessionCommandWithSavedLoginRequiresExplicitScope(t *testing.T) {
	state := installTestCLIConfig(t)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		t.Fatalf("unexpected request %s %s", r.Method, r.URL.Path)
	}))
	defer server.Close()
	if err := state.SaveLogin(server.URL, "stored-key"); err != nil {
		t.Fatal(err)
	}

	cmd := newRootCommand()
	cmd.SetOut(&bytes.Buffer{})
	cmd.SetErr(&bytes.Buffer{})
	cmd.SetArgs([]string{"session", "get", "019c10d5-a6f7-7af1-8f5f-bb97bcc0dc31"})
	err := cmd.Execute()
	if err == nil || !strings.Contains(err.Error(), "--project and --env are required with helmr login") {
		t.Fatalf("err = %v", err)
	}
}

func TestSessionCommandWithAPIKeySendsExpectedScope(t *testing.T) {
	var project, environment string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/sessions/019c10d5-a6f7-7af1-8f5f-bb97bcc0dc31" {
			t.Fatalf("unexpected request %s %s", r.Method, r.URL.Path)
		}
		project, environment = r.Header.Get("X-Helmr-Project"), r.Header.Get("X-Helmr-Environment")
		_ = json.NewEncoder(w).Encode(api.AgentSession{ID: "019c10d5-a6f7-7af1-8f5f-bb97bcc0dc31", Status: "open"})
	}))
	defer server.Close()
	t.Setenv(helmrAPIURLEnv, server.URL)
	t.Setenv(helmrAPIKeyEnv, "test-key")
	run := func(args ...string) {
		t.Helper()
		cmd := newRootCommand()
		cmd.SetOut(&bytes.Buffer{})
		cmd.SetErr(&bytes.Buffer{})
		cmd.SetArgs(append([]string{"session", "get", "019c10d5-a6f7-7af1-8f5f-bb97bcc0dc31"}, args...))
		if err := cmd.Execute(); err != nil {
			t.Fatal(err)
		}
	}
	run()
	if project != "" || environment != "" {
		t.Fatalf("unselected scope sent: %q %q", project, environment)
	}
	run("--project", "project-1", "--env", "env-1")
	if project != "project-1" || environment != "env-1" {
		t.Fatalf("flag scope = %q %q", project, environment)
	}
	t.Setenv("HELMR_PROJECT", "env-project")
	t.Setenv("HELMR_ENV", "env-environment")
	run("-e", "env-1")
	if project != "env-project" || environment != "env-1" {
		t.Fatalf("environment-variable scope = %q %q", project, environment)
	}
}
