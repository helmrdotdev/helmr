package main

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/helmrdotdev/helmr/internal/api"
)

func TestWaitpointTokenCreateCommand(t *testing.T) {
	var request api.CreateWaitpointTokenRequest
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost || r.URL.Path != "/api/waitpoints/tokens" {
			t.Fatalf("%s %s", r.Method, r.URL.Path)
		}
		if got := r.Header.Get("authorization"); got != "Bearer test-key" {
			t.Fatalf("auth = %s", got)
		}
		if err := json.NewDecoder(r.Body).Decode(&request); err != nil {
			t.Fatal(err)
		}
		_ = json.NewEncoder(w).Encode(api.WaitpointTokenResponse{
			ID:                "token-1",
			CallbackURL:       serverURL(r) + "/api/waitpoints/tokens/token-1/callback/callback-secret",
			PublicAccessToken: "public-token",
			TimeoutAt:         nil,
		})
	}))
	defer server.Close()
	t.Setenv(helmrAPIURLEnv, server.URL)
	t.Setenv(helmrAPIKeyEnv, "test-key")

	var out bytes.Buffer
	cmd := newRootCommand()
	cmd.SetOut(&out)
	cmd.SetErr(&bytes.Buffer{})
	cmd.SetArgs([]string{
		"waitpoint", "token", "create",
		"--timeout-seconds", "3600",
		"--tag", "approval",
		"--metadata", `{"bridge":"slack"}`,
	})
	if err := cmd.Execute(); err != nil {
		t.Fatal(err)
	}
	if request.TimeoutInSeconds == nil || *request.TimeoutInSeconds != 3600 {
		t.Fatalf("timeout = %+v", request.TimeoutInSeconds)
	}
	if strings.Join(request.Tags, ",") != "approval" || string(request.Metadata) != `{"bridge":"slack"}` {
		t.Fatalf("request = %+v metadata=%s", request, request.Metadata)
	}
	if !strings.Contains(out.String(), `"public_access_token":"public-token"`) {
		t.Fatalf("out = %s", out.String())
	}
}

func TestWaitpointTokenCreateCommandUsesProjectEnvironmentPathWithSession(t *testing.T) {
	state, _ := installTestCLIConfig(t)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost || r.URL.Path != "/api/projects/main/environments/production/waitpoints/tokens" {
			t.Fatalf("%s %s", r.Method, r.URL.Path)
		}
		if got := r.Header.Get("authorization"); got != "Bearer session-test" {
			t.Fatalf("auth = %s", got)
		}
		_ = json.NewEncoder(w).Encode(api.WaitpointTokenResponse{ID: "token-1"})
	}))
	defer server.Close()
	t.Setenv(helmrAPIURLEnv, server.URL)
	if err := state.SaveLogin(server.URL, "session-test"); err != nil {
		t.Fatal(err)
	}

	cmd := newRootCommand()
	cmd.SetOut(&bytes.Buffer{})
	cmd.SetErr(&bytes.Buffer{})
	cmd.SetArgs([]string{"waitpoint", "token", "create", "--project", "main", "--env", "production"})
	if err := cmd.Execute(); err != nil {
		t.Fatal(err)
	}
}

func TestWaitpointTokenCompleteCommand(t *testing.T) {
	var request api.CompleteWaitpointTokenRequest
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost || r.URL.Path != "/api/waitpoints/tokens/token-1/complete" {
			t.Fatalf("%s %s", r.Method, r.URL.Path)
		}
		if got := r.Header.Get("authorization"); got != "Bearer test-key" {
			t.Fatalf("auth = %s", got)
		}
		if err := json.NewDecoder(r.Body).Decode(&request); err != nil {
			t.Fatal(err)
		}
		w.WriteHeader(http.StatusNoContent)
	}))
	defer server.Close()
	t.Setenv(helmrAPIURLEnv, server.URL)
	t.Setenv(helmrAPIKeyEnv, "test-key")

	cmd := newRootCommand()
	cmd.SetOut(&bytes.Buffer{})
	cmd.SetErr(&bytes.Buffer{})
	cmd.SetArgs([]string{
		"waitpoint", "token", "complete", "token-1",
		"--data", `{"approved":true}`,
	})
	if err := cmd.Execute(); err != nil {
		t.Fatal(err)
	}
	if string(request.Data) != `{"approved":true}` {
		t.Fatalf("request = %+v data=%s", request, request.Data)
	}
}

func TestWaitpointTokenCompleteCommandUsesTokenRouteWithSession(t *testing.T) {
	state, _ := installTestCLIConfig(t)
	var request api.CompleteWaitpointTokenRequest
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost || r.URL.Path != "/api/waitpoints/tokens/token-1/complete" {
			t.Fatalf("%s %s", r.Method, r.URL.Path)
		}
		if got := r.Header.Get("authorization"); got != "Bearer session-test" {
			t.Fatalf("auth = %s", got)
		}
		if err := json.NewDecoder(r.Body).Decode(&request); err != nil {
			t.Fatal(err)
		}
		w.WriteHeader(http.StatusNoContent)
	}))
	defer server.Close()
	t.Setenv(helmrAPIURLEnv, server.URL)
	if err := state.SaveLogin(server.URL, "session-test"); err != nil {
		t.Fatal(err)
	}

	cmd := newRootCommand()
	cmd.SetOut(&bytes.Buffer{})
	cmd.SetErr(&bytes.Buffer{})
	cmd.SetArgs([]string{
		"waitpoint", "token", "complete", "token-1",
		"--data", `{"approved":true}`,
	})
	if err := cmd.Execute(); err != nil {
		t.Fatal(err)
	}
	if string(request.Data) != `{"approved":true}` {
		t.Fatalf("request data=%s", request.Data)
	}
}

func TestWaitpointTokenCompleteCommandRejectsMetadataFlag(t *testing.T) {
	cmd := newRootCommand()
	cmd.SetOut(&bytes.Buffer{})
	cmd.SetErr(&bytes.Buffer{})
	cmd.SetArgs([]string{
		"waitpoint", "token", "complete", "token-1",
		"--data", `{"approved":true}`,
		"--metadata", `{"actor":"alice"}`,
	})
	err := cmd.Execute()
	if err == nil {
		t.Fatal("expected metadata flag to be rejected")
	}
	if !strings.Contains(err.Error(), "unknown flag") {
		t.Fatalf("error = %v", err)
	}
}

func TestWaitpointTokenListCommandFiltersStatus(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet || r.URL.Path != "/api/waitpoints/tokens" {
			t.Fatalf("%s %s", r.Method, r.URL.Path)
		}
		if r.URL.Query().Get("status") != "waiting" {
			t.Fatalf("query = %s", r.URL.RawQuery)
		}
		_ = json.NewEncoder(w).Encode(api.ListWaitpointTokensResponse{Tokens: []api.WaitpointTokenResponse{{
			ID:     "token-1",
			Status: "waiting",
		}}})
	}))
	defer server.Close()
	t.Setenv(helmrAPIURLEnv, server.URL)
	t.Setenv(helmrAPIKeyEnv, "test-key")

	var out bytes.Buffer
	cmd := newRootCommand()
	cmd.SetOut(&out)
	cmd.SetErr(&bytes.Buffer{})
	cmd.SetArgs([]string{"waitpoint", "token", "list", "--status", "waiting"})
	if err := cmd.Execute(); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out.String(), `"id":"token-1"`) {
		t.Fatalf("out = %s", out.String())
	}
}

func TestWaitpointListCommandPrintsOpenWaitpoints(t *testing.T) {
	state, _ := installTestCLIConfig(t)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet || r.URL.Path != "/api/projects/project-1/environments/env-1/runs" {
			t.Fatalf("%s %s", r.Method, r.URL.Path)
		}
		query := r.URL.Query()
		if query.Get("status") != "waiting" || query.Get("limit") != "25" {
			t.Fatalf("query = %s", r.URL.RawQuery)
		}
		_ = json.NewEncoder(w).Encode(api.ListRunsResponse{Runs: []api.RunResponse{{
			ID:     "run-1",
			TaskID: "deploy-prod",
			Status: "waiting",
			PendingWaitpoint: &api.PendingWaitpoint{
				Kind:      "token",
				ID:        "waitpoint-1",
				Params:    json.RawMessage(`{"token_id":"token-1"}`),
				CreatedAt: time.Date(2026, 6, 6, 1, 2, 3, 0, time.UTC),
			},
		}}})
	}))
	defer server.Close()
	t.Setenv(helmrAPIURLEnv, server.URL)
	if err := state.SaveLogin(server.URL, "session-test"); err != nil {
		t.Fatal(err)
	}

	var out bytes.Buffer
	cmd := newRootCommand()
	cmd.SetOut(&out)
	cmd.SetErr(&bytes.Buffer{})
	cmd.SetArgs([]string{"waitpoint", "list", "--project", "project-1", "--env", "env-1", "--limit", "25"})
	if err := cmd.Execute(); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out.String(), "waitpoint-1") || !strings.Contains(out.String(), "deploy-prod") {
		t.Fatalf("out = %s", out.String())
	}
}

func serverURL(r *http.Request) string {
	scheme := "http"
	if r.TLS != nil {
		scheme = "https"
	}
	return scheme + "://" + r.Host
}
