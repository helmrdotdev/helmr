package main

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/helmrdotdev/helmr/internal/api"
	"github.com/helmrdotdev/helmr/internal/cli/session"
)

func TestLoginCommandStoresDeviceToken(t *testing.T) {
	state, _ := installTestCLIConfig(t)
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
				TokenType:   "bearer",
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
	state, _ := installTestCLIConfig(t)
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
	if _, err := state.Token(server.URL); !errors.Is(err, session.ErrNotFound) {
		t.Fatalf("token after logout error = %v, want ErrNotFound", err)
	}
	if !strings.Contains(out.String(), "Logged out from "+server.URL) {
		t.Fatalf("output = %q", out.String())
	}
}

func TestCommandUsesSavedLoginWhenEnvIsUnset(t *testing.T) {
	state, _ := installTestCLIConfig(t)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if got := r.Header.Get("authorization"); got != "Bearer stored-key" {
			t.Fatalf("auth = %s", got)
		}
		switch {
		case r.Method == http.MethodGet && r.URL.Path == "/api/projects/project-1/environments/env-1/runs/run-1/logs":
			_ = json.NewEncoder(w).Encode(api.LogSnapshotResponse{
				StdoutBase64: base64.StdEncoding.EncodeToString([]byte("hello\n")),
				StderrBase64: "",
			})
		default:
			t.Fatalf("%s %s", r.Method, r.URL.Path)
		}
	}))
	defer server.Close()
	if err := state.SaveLogin(server.URL, "stored-key"); err != nil {
		t.Fatal(err)
	}

	var out bytes.Buffer
	cmd := newRootCommand()
	cmd.SetOut(&out)
	cmd.SetErr(&bytes.Buffer{})
	cmd.SetArgs([]string{"run", "logs", "run-1", "--project", "project-1", "--env", "env-1"})
	if err := cmd.Execute(); err != nil {
		t.Fatal(err)
	}
	if out.String() != "hello\n" {
		t.Fatalf("output = %q", out.String())
	}
}

func TestRunCommandWithSavedLoginRequiresExplicitScope(t *testing.T) {
	state, _ := installTestCLIConfig(t)
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
	cmd.SetArgs([]string{"run", "logs", "run-1"})
	err := cmd.Execute()
	if err == nil || !strings.Contains(err.Error(), "--project and --env are required with helmr login") {
		t.Fatalf("err = %v", err)
	}
}

func TestRunCommandWithAPIKeyRejectsExplicitScope(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		t.Fatalf("unexpected request %s %s", r.Method, r.URL.Path)
	}))
	defer server.Close()
	t.Setenv(helmrAPIURLEnv, server.URL)
	t.Setenv(helmrAPIKeyEnv, "test-key")

	cmd := newRootCommand()
	cmd.SetOut(&bytes.Buffer{})
	cmd.SetErr(&bytes.Buffer{})
	cmd.SetArgs([]string{"run", "get", "run-1", "--project", "project-1", "--env", "env-1"})
	err := cmd.Execute()
	if err == nil || !strings.Contains(err.Error(), "API keys are already environment scoped") {
		t.Fatalf("err = %v", err)
	}
}
