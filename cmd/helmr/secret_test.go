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

func TestSecretSetCommand(t *testing.T) {
	state, _ := installTestCLIConfig(t)
	var request api.SetSecretRequest
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPut || r.URL.Path != "/api/projects/project-1/environments/env-1/secrets/github-token" {
			t.Fatalf("%s %s", r.Method, r.URL.Path)
		}
		if got := r.Header.Get("authorization"); got != "Bearer session-test" {
			t.Fatalf("auth = %s", got)
		}
		if err := json.NewDecoder(r.Body).Decode(&request); err != nil {
			t.Fatal(err)
		}
		_ = json.NewEncoder(w).Encode(api.SecretResponse{Name: "github-token"})
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
	cmd.SetArgs([]string{"secret", "set", "github-token", "secret-value", "--project", "project-1", "--env", "env-1"})
	if err := cmd.Execute(); err != nil {
		t.Fatal(err)
	}
	if request.Value != "secret-value" {
		t.Fatalf("request = %+v", request)
	}
	if request.ProjectID != "" || request.EnvironmentID != "" {
		t.Fatalf("scope = %+v", request)
	}
	if strings.TrimSpace(out.String()) != "github-token" {
		t.Fatalf("output = %q", out.String())
	}
}

func TestSecretSetCommandPreservesStdin(t *testing.T) {
	var request api.SetSecretRequest
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if err := json.NewDecoder(r.Body).Decode(&request); err != nil {
			t.Fatal(err)
		}
		_ = json.NewEncoder(w).Encode(api.SecretResponse{Name: "github-token"})
	}))
	defer server.Close()
	t.Setenv(helmrAPIURLEnv, server.URL)
	t.Setenv(helmrAPIKeyEnv, "test-key")

	cmd := newRootCommand()
	cmd.SetOut(&bytes.Buffer{})
	cmd.SetErr(&bytes.Buffer{})
	cmd.SetIn(strings.NewReader("secret-value\nsecond-line\n"))
	cmd.SetArgs([]string{"secret", "set", "github-token"})
	if err := cmd.Execute(); err != nil {
		t.Fatal(err)
	}
	if request.Value != "secret-value\nsecond-line\n" {
		t.Fatalf("request = %+v", request)
	}
}

func TestSecretListCommand(t *testing.T) {
	state, _ := installTestCLIConfig(t)
	secretTime := time.Date(2026, 5, 8, 12, 0, 0, 0, time.UTC)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet || r.URL.Path != "/api/projects/project-1/environments/env-1/secrets" {
			t.Fatalf("%s %s", r.Method, r.URL.Path)
		}
		if r.URL.RawQuery != "" {
			t.Fatalf("query = %s", r.URL.RawQuery)
		}
		_ = json.NewEncoder(w).Encode(api.ListSecretsResponse{Secrets: []api.SecretResponse{{
			ProjectID:     "project-1",
			EnvironmentID: "env-1",
			Name:          "github-token",
			CreatedAt:     secretTime,
			UpdatedAt:     secretTime,
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
	cmd.SetArgs([]string{"secret", "list", "--project", "project-1", "--env", "env-1"})
	if err := cmd.Execute(); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out.String(), "github-token") {
		t.Fatalf("output = %q", out.String())
	}
}

func TestSecretGetCommandReturnsMetadataOnly(t *testing.T) {
	state, _ := installTestCLIConfig(t)
	secretTime := time.Date(2026, 5, 8, 12, 0, 0, 0, time.UTC)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet || r.URL.Path != "/api/projects/project-1/environments/env-1/secrets/github-token" {
			t.Fatalf("%s %s", r.Method, r.URL.Path)
		}
		_ = json.NewEncoder(w).Encode(api.SecretResponse{
			ProjectID:     "project-1",
			EnvironmentID: "env-1",
			Name:          "github-token",
			CreatedAt:     secretTime,
			UpdatedAt:     secretTime,
		})
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
	cmd.SetArgs([]string{"secret", "get", "github-token", "--project", "project-1", "--env", "env-1"})
	if err := cmd.Execute(); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out.String(), "Name: github-token") || strings.Contains(out.String(), "secret-value") {
		t.Fatalf("output = %q", out.String())
	}
}

func TestSecretDeleteCommand(t *testing.T) {
	state, _ := installTestCLIConfig(t)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodDelete || r.URL.Path != "/api/projects/project-1/environments/env-1/secrets/github-token" {
			t.Fatalf("%s %s", r.Method, r.URL.Path)
		}
		if r.URL.RawQuery != "" {
			t.Fatalf("query = %s", r.URL.RawQuery)
		}
		w.WriteHeader(http.StatusNoContent)
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
	cmd.SetArgs([]string{"secret", "delete", "github-token", "--project", "project-1", "--env", "env-1", "--yes"})
	if err := cmd.Execute(); err != nil {
		t.Fatal(err)
	}
	if strings.TrimSpace(out.String()) != "github-token" {
		t.Fatalf("output = %q", out.String())
	}
}

func TestSecretDeleteCommandRequiresYes(t *testing.T) {
	cmd := newRootCommand()
	cmd.SetOut(&bytes.Buffer{})
	cmd.SetErr(&bytes.Buffer{})
	cmd.SetArgs([]string{"secret", "delete", "github-token"})
	err := cmd.Execute()
	if err == nil || !strings.Contains(err.Error(), "secret delete requires --yes") {
		t.Fatalf("err = %v", err)
	}
}
