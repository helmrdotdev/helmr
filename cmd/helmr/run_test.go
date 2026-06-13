package main

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/helmrdotdev/helmr/internal/api"
)

func TestRunCommandCreatesGitHubRun(t *testing.T) {
	var request api.CreateRunRequest
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost || r.URL.Path != "/api/runs" {
			t.Fatalf("%s %s", r.Method, r.URL.Path)
		}
		if got := r.Header.Get("authorization"); got != "Bearer test-key" {
			t.Fatalf("auth = %s", got)
		}
		body, err := io.ReadAll(r.Body)
		if err != nil {
			t.Fatal(err)
		}
		var raw map[string]json.RawMessage
		if err := json.Unmarshal(body, &raw); err != nil {
			t.Fatal(err)
		}
		if _, ok := raw["workspace"]; ok {
			t.Fatalf("request JSON included workspace: %s", body)
		}
		if _, ok := raw["source"]; ok {
			t.Fatalf("request JSON included source: %s", body)
		}
		if err := json.Unmarshal(body, &request); err != nil {
			t.Fatal(err)
		}
		_ = json.NewEncoder(w).Encode(api.RunResponse{
			ID:        "run-1",
			TaskID:    request.TaskID,
			Status:    "queued",
			CreatedAt: time.Unix(0, 0).UTC(),
			UpdatedAt: time.Unix(0, 0).UTC(),
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
		"run", "deploy",
		"--payload", "env=prod",
		"--max-duration-seconds", "60",
		"--metadata-json", `{"source":"cli"}`,
		"--tag", "deploy",
		"--tag", "prod",
		"--retry-json", `{"maxAttempts":3}`,
		"--idempotency-key", "deploy-prod",
		"--idempotency-key-ttl", "24h",
	})
	if err := cmd.Execute(); err != nil {
		t.Fatal(err)
	}
	if strings.TrimSpace(out.String()) != "run-1" {
		t.Fatalf("output = %q", out.String())
	}
	if request.TaskID != "deploy" || request.Options.MaxDurationSeconds != 60 {
		t.Fatalf("request = %+v", request)
	}
	if request.ProjectID != "" || request.EnvironmentID != "" {
		t.Fatalf("scope = %s/%s", request.ProjectID, request.EnvironmentID)
	}
	if request.Options.IdempotencyKey != "deploy-prod" || request.Options.IdempotencyKeyTTL != "24h" {
		t.Fatalf("idempotency options = %+v", request.Options)
	}
	if string(request.Options.Metadata) != `{"source":"cli"}` || string(request.Options.Retry) != `{"maxAttempts":3}` {
		t.Fatalf("run options JSON = %+v", request.Options)
	}
	if strings.Join(request.Options.Tags, ",") != "deploy,prod" {
		t.Fatalf("tags = %+v", request.Options.Tags)
	}
	if string(request.Payload) != `{"env":"prod"}` {
		t.Fatalf("payload = %s", request.Payload)
	}
}

func TestCancelCommandCancelsRun(t *testing.T) {
	var request api.CancelRunRequest
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost || r.URL.Path != "/api/runs/run-1/cancel" {
			t.Fatalf("%s %s", r.Method, r.URL.Path)
		}
		if err := json.NewDecoder(r.Body).Decode(&request); err != nil {
			t.Fatal(err)
		}
		_ = json.NewEncoder(w).Encode(api.CancelRunResponse{
			Run:       api.RunResponse{ID: "run-1", Status: "cancelled"},
			Operation: api.RunOperationResponse{ID: "op-1", RunID: "run-1", Kind: "cancel", Status: "applied"},
		})
	}))
	defer server.Close()
	t.Setenv(helmrAPIURLEnv, server.URL)
	t.Setenv(helmrAPIKeyEnv, "test-key")

	var out bytes.Buffer
	cmd := newRootCommand()
	cmd.SetOut(&out)
	cmd.SetErr(&bytes.Buffer{})
	cmd.SetArgs([]string{"cancel", "run-1", "--reason", "cleanup", "--force", "--idempotency-key", "cancel-1"})
	if err := cmd.Execute(); err != nil {
		t.Fatal(err)
	}
	if strings.TrimSpace(out.String()) != "run-1 cancelled" {
		t.Fatalf("output = %q", out.String())
	}
	if request.Reason != "cleanup" || !request.Force || request.IdempotencyKey != "cancel-1" {
		t.Fatalf("request = %+v", request)
	}
}

func TestReplayCommandReplaysRun(t *testing.T) {
	var request api.ReplayRunRequest
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost || r.URL.Path != "/api/runs/run-1/replay" {
			t.Fatalf("%s %s", r.Method, r.URL.Path)
		}
		if err := json.NewDecoder(r.Body).Decode(&request); err != nil {
			t.Fatal(err)
		}
		_ = json.NewEncoder(w).Encode(api.ReplayRunResponse{
			Run:       api.RunResponse{ID: "run-2", Status: "queued"},
			Operation: api.RunOperationResponse{ID: "op-1", RunID: "run-1", Kind: "replay", Status: "applied"},
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
		"replay", "run-1",
		"--version", "latest",
		"--payload", "env=prod",
		"--metadata-json", `{"reason":"manual"}`,
		"--tag", "manual",
		"--reason", "retry deploy",
		"--idempotency-key", "replay-1",
	})
	if err := cmd.Execute(); err != nil {
		t.Fatal(err)
	}
	if strings.TrimSpace(out.String()) != "run-2" {
		t.Fatalf("output = %q", out.String())
	}
	if request.Version != "latest" || request.Reason != "retry deploy" || request.IdempotencyKey != "replay-1" {
		t.Fatalf("request = %+v", request)
	}
	if string(request.Payload) != `{"env":"prod"}` || string(request.Metadata) != `{"reason":"manual"}` {
		t.Fatalf("request JSON = %+v", request)
	}
	if strings.Join(request.Tags, ",") != "manual" {
		t.Fatalf("tags = %+v", request.Tags)
	}
}

func TestReplayCommandDoesNotUseProjectShorthandForPayload(t *testing.T) {
	cmd := newRootCommand()
	cmd.SetOut(&bytes.Buffer{})
	cmd.SetErr(&bytes.Buffer{})
	cmd.SetArgs([]string{"replay", "run-1", "-p", "env=prod"})
	err := cmd.Execute()
	if err == nil || !strings.Contains(err.Error(), "unknown shorthand flag: 'p'") {
		t.Fatalf("err = %v", err)
	}
}

func TestReplayCommandOmitsPayloadMetadataAndTagsWhenNotOverridden(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost || r.URL.Path != "/api/runs/run-1/replay" {
			t.Fatalf("%s %s", r.Method, r.URL.Path)
		}
		body, err := io.ReadAll(r.Body)
		if err != nil {
			t.Fatal(err)
		}
		var raw map[string]json.RawMessage
		if err := json.Unmarshal(body, &raw); err != nil {
			t.Fatal(err)
		}
		for _, key := range []string{"payload", "metadata", "tags"} {
			if _, ok := raw[key]; ok {
				t.Fatalf("request included %s override: %s", key, body)
			}
		}
		_ = json.NewEncoder(w).Encode(api.ReplayRunResponse{Run: api.RunResponse{ID: "run-2", Status: "queued"}})
	}))
	defer server.Close()
	t.Setenv(helmrAPIURLEnv, server.URL)
	t.Setenv(helmrAPIKeyEnv, "test-key")

	var out bytes.Buffer
	cmd := newRootCommand()
	cmd.SetOut(&out)
	cmd.SetErr(&bytes.Buffer{})
	cmd.SetArgs([]string{"replay", "run-1"})
	if err := cmd.Execute(); err != nil {
		t.Fatal(err)
	}
	if strings.TrimSpace(out.String()) != "run-2" {
		t.Fatalf("output = %q", out.String())
	}
}

func TestRunCommandReadsPayloadFile(t *testing.T) {
	var request api.CreateRunRequest
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost || r.URL.Path != "/api/runs" {
			t.Fatalf("%s %s", r.Method, r.URL.Path)
		}
		if err := json.NewDecoder(r.Body).Decode(&request); err != nil {
			t.Fatal(err)
		}
		_ = json.NewEncoder(w).Encode(api.RunResponse{
			ID:        "run-1",
			TaskID:    request.TaskID,
			Status:    "queued",
			CreatedAt: time.Unix(0, 0).UTC(),
			UpdatedAt: time.Unix(0, 0).UTC(),
		})
	}))
	defer server.Close()
	t.Setenv(helmrAPIURLEnv, server.URL)
	t.Setenv(helmrAPIKeyEnv, "test-key")

	payloadPath := filepath.Join(t.TempDir(), "payload.json")
	if err := os.WriteFile(payloadPath, []byte(`{"env":"prod","count":2}`), 0o644); err != nil {
		t.Fatal(err)
	}
	cmd := newRootCommand()
	cmd.SetOut(&bytes.Buffer{})
	cmd.SetErr(&bytes.Buffer{})
	cmd.SetArgs([]string{"run", "deploy", "--payload-file", payloadPath})
	if err := cmd.Execute(); err != nil {
		t.Fatal(err)
	}
	var payload map[string]any
	if err := json.Unmarshal(request.Payload, &payload); err != nil {
		t.Fatal(err)
	}
	if payload["env"] != "prod" || payload["count"] != float64(2) {
		t.Fatalf("payload = %s", request.Payload)
	}
}

func TestRunCommandRejectsPayloadFileCombinations(t *testing.T) {
	called := false
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		called = true
	}))
	defer server.Close()
	t.Setenv(helmrAPIURLEnv, server.URL)
	t.Setenv(helmrAPIKeyEnv, "test-key")

	payloadPath := filepath.Join(t.TempDir(), "payload.json")
	if err := os.WriteFile(payloadPath, []byte(`{"env":"prod"}`), 0o644); err != nil {
		t.Fatal(err)
	}
	for _, args := range [][]string{
		{"run", "deploy", "--payload-file", payloadPath, "--payload-json", `{"env":"prod"}`},
		{"run", "deploy", "--payload-file", payloadPath, "--payload", "env=prod"},
	} {
		cmd := newRootCommand()
		cmd.SetOut(&bytes.Buffer{})
		cmd.SetErr(&bytes.Buffer{})
		cmd.SetArgs(args)
		err := cmd.Execute()
		if err == nil || !strings.Contains(err.Error(), "--payload-file cannot be combined") {
			t.Fatalf("args %v err = %v", args, err)
		}
	}
	if called {
		t.Fatal("server was called")
	}
}

func TestRunCommandRejectsProjectFlagThatLooksLikePayload(t *testing.T) {
	called := false
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		called = true
	}))
	defer server.Close()
	t.Setenv(helmrAPIURLEnv, server.URL)
	t.Setenv(helmrAPIKeyEnv, "test-key")

	cmd := newRootCommand()
	cmd.SetOut(&bytes.Buffer{})
	cmd.SetErr(&bytes.Buffer{})
	cmd.SetArgs([]string{"run", "deploy", "-p", "env=prod"})
	err := cmd.Execute()
	if err == nil || !strings.Contains(err.Error(), "--project must be a project slug or ID") {
		t.Fatalf("err = %v", err)
	}
	if called {
		t.Fatal("server was called")
	}
}

func TestRunCommandRejectsInvalidTaskIDBeforeRequest(t *testing.T) {
	called := false
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		called = true
	}))
	defer server.Close()
	t.Setenv(helmrAPIURLEnv, server.URL)
	t.Setenv(helmrAPIKeyEnv, "test-key")

	cmd := newRootCommand()
	cmd.SetOut(&bytes.Buffer{})
	cmd.SetErr(&bytes.Buffer{})
	cmd.SetArgs([]string{"run", "bad task"})
	err := cmd.Execute()
	if err == nil || !strings.Contains(err.Error(), "task_id") {
		t.Fatalf("err = %v", err)
	}
	if called {
		t.Fatal("server was called")
	}
}
