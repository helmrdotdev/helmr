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

func TestTokenCreateSendsDurationMetadataTagsAndIdempotency(t *testing.T) {
	var request api.CreateTokenRequest
	timeoutAt := time.Date(2026, 7, 1, 0, 0, 0, 0, time.UTC)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost || r.URL.Path != "/api/tokens" {
			t.Fatalf("%s %s", r.Method, r.URL.Path)
		}
		if err := json.NewDecoder(r.Body).Decode(&request); err != nil {
			t.Fatal(err)
		}
		_ = json.NewEncoder(w).Encode(api.TokenResponse{
			ID:          "token-1",
			Status:      "pending",
			CallbackURL: "https://api.example.test/api/v1/tokens/token-1/callback/secret",
			TimeoutAt:   &timeoutAt,
		})
	}))
	defer server.Close()
	t.Setenv(helmrAPIURLEnv, server.URL)
	t.Setenv(helmrAPIKeyEnv, "test-key")

	var out bytes.Buffer
	cmd := newRootCommand()
	cmd.SetOut(&out)
	cmd.SetErr(&bytes.Buffer{})
	cmd.SetArgs([]string{"token", "create", "--timeout", "7d", "--metadata-json", `{"release":"v1"}`, "--tag", "release", "--idempotency-key", "approval-1"})
	if err := cmd.Execute(); err != nil {
		t.Fatal(err)
	}
	if string(request.Timeout) != `"7d"` || string(request.Metadata) != `{"release":"v1"}` || request.IdempotencyKey != "approval-1" {
		t.Fatalf("request = %+v", request)
	}
	if len(request.Tags) != 1 || request.Tags[0] != "release" {
		t.Fatalf("tags = %#v", request.Tags)
	}
	if !strings.Contains(out.String(), "Token:       token-1") || !strings.Contains(out.String(), "Callback:") {
		t.Fatalf("output = %q", out.String())
	}
}

func TestTokenCreateRejectsUnexpectedArgs(t *testing.T) {
	cmd := newRootCommand()
	cmd.SetOut(&bytes.Buffer{})
	cmd.SetErr(&bytes.Buffer{})
	cmd.SetArgs([]string{"token", "create", "approval"})
	err := cmd.Execute()
	if err == nil || !strings.Contains(err.Error(), `unknown command`) && !strings.Contains(err.Error(), `accepts 0 arg`) {
		t.Fatalf("err = %v", err)
	}
}

func TestTokenCreateInvalidMetadataNamesActualFlag(t *testing.T) {
	cmd := newRootCommand()
	cmd.SetOut(&bytes.Buffer{})
	cmd.SetErr(&bytes.Buffer{})
	cmd.SetArgs([]string{"token", "create", "--metadata-json", `{bad`})
	err := cmd.Execute()
	if err == nil || !strings.Contains(err.Error(), "--metadata-json must be valid JSON") {
		t.Fatalf("err = %v", err)
	}
}

func TestTokenGetUsesCanonicalCommand(t *testing.T) {
	timeoutAt := time.Date(2026, 7, 1, 0, 0, 0, 0, time.UTC)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet || r.URL.Path != "/api/tokens/token-1" {
			t.Fatalf("%s %s", r.Method, r.URL.Path)
		}
		_ = json.NewEncoder(w).Encode(api.TokenResponse{ID: "token-1", Status: "pending", TimeoutAt: &timeoutAt})
	}))
	defer server.Close()
	t.Setenv(helmrAPIURLEnv, server.URL)
	t.Setenv(helmrAPIKeyEnv, "test-key")

	var out bytes.Buffer
	cmd := newRootCommand()
	cmd.SetOut(&out)
	cmd.SetErr(&bytes.Buffer{})
	cmd.SetArgs([]string{"token", "get", "token-1", "--json"})
	if err := cmd.Execute(); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out.String(), `"id":"token-1"`) {
		t.Fatalf("output = %q", out.String())
	}
}

func TestTokenCompleteSendsData(t *testing.T) {
	var request api.CompleteTokenRequest
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost || r.URL.Path != "/api/tokens/token-1/complete" {
			t.Fatalf("%s %s", r.Method, r.URL.Path)
		}
		if err := json.NewDecoder(r.Body).Decode(&request); err != nil {
			t.Fatal(err)
		}
		w.WriteHeader(http.StatusNoContent)
	}))
	defer server.Close()
	t.Setenv(helmrAPIURLEnv, server.URL)
	t.Setenv(helmrAPIKeyEnv, "test-key")

	var out bytes.Buffer
	cmd := newRootCommand()
	cmd.SetOut(&out)
	cmd.SetErr(&bytes.Buffer{})
	cmd.SetArgs([]string{"token", "complete", "token-1", "--data-json", `{"approved":true}`})
	if err := cmd.Execute(); err != nil {
		t.Fatal(err)
	}
	if string(request.Data) != `{"approved":true}` {
		t.Fatalf("request = %+v", request)
	}
	if got := strings.TrimSpace(out.String()); got != "token-1 completed" {
		t.Fatalf("output = %q", out.String())
	}
}

func TestTokenCancelUsesCanonicalCommand(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost || r.URL.Path != "/api/tokens/token-1/cancel" {
			t.Fatalf("%s %s", r.Method, r.URL.Path)
		}
		_ = json.NewEncoder(w).Encode(api.TokenResponse{ID: "token-1", Status: "cancelled"})
	}))
	defer server.Close()
	t.Setenv(helmrAPIURLEnv, server.URL)
	t.Setenv(helmrAPIKeyEnv, "test-key")

	var out bytes.Buffer
	cmd := newRootCommand()
	cmd.SetOut(&out)
	cmd.SetErr(&bytes.Buffer{})
	cmd.SetArgs([]string{"token", "cancel", "token-1"})
	if err := cmd.Execute(); err != nil {
		t.Fatal(err)
	}
	if got := strings.TrimSpace(out.String()); got != "token-1 cancelled" {
		t.Fatalf("output = %q", out.String())
	}
}
