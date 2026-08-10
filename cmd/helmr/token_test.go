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

func TestTokenCreateIsRuntimeOnly(t *testing.T) {
	if commandByPath(newRootCommand(), "token", "create") != nil {
		t.Fatal("token create command is registered")
	}
}

func TestTokenGetUsesCanonicalCommand(t *testing.T) {
	timeoutAt := time.Date(2026, 7, 1, 0, 0, 0, 0, time.UTC)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet || r.URL.Path != "/v1/tokens/019c10d5-a6f7-7af1-8f5f-bb97bcc0dc37" {
			t.Fatalf("%s %s", r.Method, r.URL.Path)
		}
		_ = json.NewEncoder(w).Encode(api.TokenResponse{ID: "019c10d5-a6f7-7af1-8f5f-bb97bcc0dc37", Status: "pending", TimeoutAt: timeoutAt})
	}))
	defer server.Close()
	t.Setenv(helmrAPIURLEnv, server.URL)
	t.Setenv(helmrAPIKeyEnv, "test-key")

	var out bytes.Buffer
	cmd := newRootCommand()
	cmd.SetOut(&out)
	cmd.SetErr(&bytes.Buffer{})
	cmd.SetArgs([]string{"token", "get", "019c10d5-a6f7-7af1-8f5f-bb97bcc0dc37", "--json"})
	if err := cmd.Execute(); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out.String(), `"id":"019c10d5-a6f7-7af1-8f5f-bb97bcc0dc37"`) {
		t.Fatalf("output = %q", out.String())
	}
}

func TestTokenGetResolvesSessionScope(t *testing.T) {
	state := installTestCLIConfig(t)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodGet && r.URL.Path == "/api/projects":
			writeSessionScopeProjects(t, w, "project-1", "env-1")
		case r.Method == http.MethodGet && r.URL.Path == "/api/projects/019c10d5-a6f7-7af1-8f5f-bb97bcc0dc30/environments/019c10d5-a6f7-7af2-8f5f-bb97bcc0dc32/tokens/019c10d5-a6f7-7af1-8f5f-bb97bcc0dc37":
			_ = json.NewEncoder(w).Encode(api.TokenResponse{ID: "019c10d5-a6f7-7af1-8f5f-bb97bcc0dc37", Status: "pending"})
		default:
			t.Fatalf("%s %s", r.Method, r.URL.Path)
		}
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
		"token", "get", "019c10d5-a6f7-7af1-8f5f-bb97bcc0dc37",
		"--project", "project-1", "--env", "env-1",
	})
	if err := cmd.Execute(); err != nil {
		t.Fatal(err)
	}
}

func TestTokenCompleteSendsResultAndIdempotencyKey(t *testing.T) {
	var request api.CompleteTokenRequest
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost || r.URL.Path != "/v1/tokens/019c10d5-a6f7-7af1-8f5f-bb97bcc0dc37/complete" {
			t.Fatalf("%s %s", r.Method, r.URL.Path)
		}
		if err := json.NewDecoder(r.Body).Decode(&request); err != nil {
			t.Fatal(err)
		}
		_ = json.NewEncoder(w).Encode(api.TokenResponse{
			ID: "019c10d5-a6f7-7af1-8f5f-bb97bcc0dc37", Status: api.TokenStatusCompleted,
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
		"token", "complete", "019c10d5-a6f7-7af1-8f5f-bb97bcc0dc37",
		"--data-json", `{"approved":true}`,
		"--idempotency-key", "approval-1",
	})
	if err := cmd.Execute(); err != nil {
		t.Fatal(err)
	}
	if string(request.Result) != `{"approved":true}` || request.IdempotencyKey != "approval-1" {
		t.Fatalf("request = %+v", request)
	}
	for _, want := range []string{
		"Token:       019c10d5-a6f7-7af1-8f5f-bb97bcc0dc37",
		"Status:      completed",
	} {
		if !strings.Contains(out.String(), want) {
			t.Fatalf("output = %q, missing %q", out.String(), want)
		}
	}
}

func TestTokenCompleteJSONEmitsResponse(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost || r.URL.Path != "/v1/tokens/019c10d5-a6f7-7af1-8f5f-bb97bcc0dc37/complete" {
			t.Fatalf("%s %s", r.Method, r.URL.Path)
		}
		_ = json.NewEncoder(w).Encode(api.TokenResponse{
			ID: "019c10d5-a6f7-7af1-8f5f-bb97bcc0dc37", Status: api.TokenStatusCompleted,
		})
	}))
	defer server.Close()
	t.Setenv(helmrAPIURLEnv, server.URL)
	t.Setenv(helmrAPIKeyEnv, "test-key")

	var out bytes.Buffer
	cmd := newRootCommand()
	cmd.SetOut(&out)
	cmd.SetErr(&bytes.Buffer{})
	cmd.SetArgs([]string{"token", "complete", "019c10d5-a6f7-7af1-8f5f-bb97bcc0dc37", "--data-json", `{"approved":true}`, "--json"})
	if err := cmd.Execute(); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out.String(), `"status":"completed"`) || !strings.Contains(out.String(), `"id":"019c10d5-a6f7-7af1-8f5f-bb97bcc0dc37"`) {
		t.Fatalf("output = %q", out.String())
	}
}

func TestTokenCancelUsesCanonicalCommand(t *testing.T) {
	var request api.CancelTokenRequest
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost || r.URL.Path != "/v1/tokens/019c10d5-a6f7-7af1-8f5f-bb97bcc0dc37/cancel" {
			t.Fatalf("%s %s", r.Method, r.URL.Path)
		}
		if err := json.NewDecoder(r.Body).Decode(&request); err != nil {
			t.Fatal(err)
		}
		_ = json.NewEncoder(w).Encode(api.TokenResponse{ID: "019c10d5-a6f7-7af1-8f5f-bb97bcc0dc37", Status: "cancelled"})
	}))
	defer server.Close()
	t.Setenv(helmrAPIURLEnv, server.URL)
	t.Setenv(helmrAPIKeyEnv, "test-key")

	var out bytes.Buffer
	cmd := newRootCommand()
	cmd.SetOut(&out)
	cmd.SetErr(&bytes.Buffer{})
	cmd.SetArgs([]string{"token", "cancel", "019c10d5-a6f7-7af1-8f5f-bb97bcc0dc37", "--idempotency-key", "cancel-1"})
	if err := cmd.Execute(); err != nil {
		t.Fatal(err)
	}
	if got := strings.TrimSpace(out.String()); got != "019c10d5-a6f7-7af1-8f5f-bb97bcc0dc37 cancelled" {
		t.Fatalf("output = %q", out.String())
	}
	if request.IdempotencyKey != "cancel-1" {
		t.Fatalf("request = %+v", request)
	}
}
