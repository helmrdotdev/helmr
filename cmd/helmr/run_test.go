package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
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
	var request api.SessionStartRequest
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost || r.URL.Path != "/api/sessions" {
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
		_ = json.NewEncoder(w).Encode(sessionStartResponseFixture())
	}))
	defer server.Close()
	t.Setenv(helmrAPIURLEnv, server.URL)
	t.Setenv(helmrAPIKeyEnv, "test-key")

	var out bytes.Buffer
	cmd := newRootCommand()
	cmd.SetOut(&out)
	cmd.SetErr(&bytes.Buffer{})
	cmd.SetArgs([]string{
		"session", "start", "deploy",
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
	for _, want := range []string{
		"session_id: session-1",
		"run_id: run-1",
		"workspace_id: workspace-1",
	} {
		if !strings.Contains(out.String(), want) {
			t.Fatalf("output = %q, missing %q", out.String(), want)
		}
	}
	if strings.TrimSpace(out.String()) == "session-1" {
		t.Fatalf("output = %q", out.String())
	}
	if request.Options.MaxDurationSeconds != 60 {
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
	cmd.SetArgs([]string{"run", "cancel", "run-1", "--reason", "cleanup", "--force", "--idempotency-key", "cancel-1"})
	if err := cmd.Execute(); err != nil {
		t.Fatal(err)
	}
	expected := strings.Join([]string{
		"run_id: run-1",
		"run_status: cancelled",
		"operation_id: op-1",
		"operation_status: applied",
		"",
	}, "\n")
	if out.String() != expected {
		t.Fatalf("output = %q, want %q", out.String(), expected)
	}
	if request.Reason != "cleanup" || !request.Force || request.IdempotencyKey != "cancel-1" {
		t.Fatalf("request = %+v", request)
	}
}

func TestRunListFiltersBySession(t *testing.T) {
	var gotQuery string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet || r.URL.Path != "/api/runs" {
			t.Fatalf("%s %s", r.Method, r.URL.Path)
		}
		gotQuery = r.URL.RawQuery
		_ = json.NewEncoder(w).Encode(api.ListRunsResponse{Runs: []api.RunResponse{{
			ID:     "run-1",
			TaskID: "deploy",
			Status: "queued",
		}}})
	}))
	defer server.Close()
	t.Setenv(helmrAPIURLEnv, server.URL)
	t.Setenv(helmrAPIKeyEnv, "test-key")

	var out bytes.Buffer
	cmd := newRootCommand()
	cmd.SetOut(&out)
	cmd.SetErr(&bytes.Buffer{})
	cmd.SetArgs([]string{"run", "list", "--session", "session-1", "--json"})
	if err := cmd.Execute(); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(gotQuery, "session_id=session-1") {
		t.Fatalf("query = %s", gotQuery)
	}
	if !strings.Contains(out.String(), `"run-1"`) {
		t.Fatalf("output = %q", out.String())
	}
}

func TestRunCommandReadsPayloadFile(t *testing.T) {
	var request api.SessionStartRequest
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost || r.URL.Path != "/api/sessions" {
			t.Fatalf("%s %s", r.Method, r.URL.Path)
		}
		if err := json.NewDecoder(r.Body).Decode(&request); err != nil {
			t.Fatal(err)
		}
		_ = json.NewEncoder(w).Encode(sessionStartResponseFixture())
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
	cmd.SetArgs([]string{"session", "start", "deploy", "--payload-file", payloadPath})
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

func TestSessionStartAttachesExistingWorkspace(t *testing.T) {
	var request api.SessionStartRequest
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost || r.URL.Path != "/api/sessions" {
			t.Fatalf("%s %s", r.Method, r.URL.Path)
		}
		if err := json.NewDecoder(r.Body).Decode(&request); err != nil {
			t.Fatal(err)
		}
		_ = json.NewEncoder(w).Encode(sessionStartResponseFixture())
	}))
	defer server.Close()
	t.Setenv(helmrAPIURLEnv, server.URL)
	t.Setenv(helmrAPIKeyEnv, "test-key")

	cmd := newRootCommand()
	cmd.SetOut(&bytes.Buffer{})
	cmd.SetErr(&bytes.Buffer{})
	cmd.SetArgs([]string{"session", "start", "deploy", "--workspace", "workspace-1"})
	if err := cmd.Execute(); err != nil {
		t.Fatal(err)
	}
	if request.Options.WorkspaceID != "workspace-1" {
		t.Fatalf("workspace id = %q", request.Options.WorkspaceID)
	}
}

func TestSessionStartWaitWaitsForInitialRun(t *testing.T) {
	getRunCalls := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodPost && r.URL.Path == "/api/sessions":
			_ = json.NewEncoder(w).Encode(sessionStartResponseFixture())
		case r.Method == http.MethodGet && r.URL.Path == "/api/runs/run-1":
			getRunCalls++
			_ = json.NewEncoder(w).Encode(api.RunResponse{ID: "run-1", TaskID: "deploy", Status: api.RunStatusSucceeded})
		default:
			t.Fatalf("%s %s", r.Method, r.URL.Path)
		}
	}))
	defer server.Close()
	t.Setenv(helmrAPIURLEnv, server.URL)
	t.Setenv(helmrAPIKeyEnv, "test-key")

	var out bytes.Buffer
	cmd := newRootCommand()
	cmd.SetOut(&out)
	cmd.SetErr(&bytes.Buffer{})
	cmd.SetArgs([]string{"session", "start", "deploy", "--wait", "--timeout", "1500ms"})
	if err := cmd.Execute(); err != nil {
		t.Fatal(err)
	}
	if getRunCalls != 1 {
		t.Fatalf("get run calls = %d", getRunCalls)
	}
	if !strings.Contains(out.String(), "run_status: succeeded") {
		t.Fatalf("output = %q", out.String())
	}
}

func TestSessionStartRejectsJSONFollow(t *testing.T) {
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
	cmd.SetArgs([]string{"session", "start", "deploy", "--json", "--follow"})
	err := cmd.Execute()
	if err == nil || !strings.Contains(err.Error(), "--json cannot be combined with --follow") {
		t.Fatalf("err = %v", err)
	}
	if called {
		t.Fatal("server was called")
	}
}

func TestSessionStartFollowTimeoutReturnsError(t *testing.T) {
	oldReconnectDelay := runEventReconnectDelay
	runEventReconnectDelay = time.Millisecond
	t.Cleanup(func() {
		runEventReconnectDelay = oldReconnectDelay
	})
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodPost && r.URL.Path == "/api/sessions":
			_ = json.NewEncoder(w).Encode(sessionStartResponseFixture())
		case r.Method == http.MethodGet && r.URL.Path == "/api/runs/run-1/logs" && r.URL.Query().Get("follow") == "1":
			w.Header().Set("content-type", "text/event-stream")
			flusher, _ := w.(http.Flusher)
			if flusher != nil {
				flusher.Flush()
			}
			<-r.Context().Done()
		case r.Method == http.MethodGet && r.URL.Path == "/api/sessions/session-1":
			_ = json.NewEncoder(w).Encode(api.SessionResponse{ID: "session-1", Status: "open"})
		default:
			t.Fatalf("%s %s", r.Method, r.URL.RequestURI())
		}
	}))
	defer server.Close()
	t.Setenv(helmrAPIURLEnv, server.URL)
	t.Setenv(helmrAPIKeyEnv, "test-key")

	var out bytes.Buffer
	cmd := newRootCommand()
	cmd.SetOut(&out)
	cmd.SetErr(&bytes.Buffer{})
	cmd.SetArgs([]string{"session", "start", "deploy", "--follow", "--timeout", "1s"})
	err := cmd.Execute()
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("err = %v", err)
	}
	if !strings.Contains(out.String(), "session_status: open") {
		t.Fatalf("output = %q", out.String())
	}
}

func sessionStartResponseFixture() api.SessionStartResponse {
	now := time.Unix(0, 0).UTC()
	return api.SessionStartResponse{
		Session: api.SessionResponse{
			ID:                  "session-1",
			ProjectID:           "project-1",
			EnvironmentID:       "env-1",
			TaskID:              "deploy",
			InitialDeploymentID: "deployment-1",
			ActiveDeploymentID:  "deployment-1",
			Status:              "open",
			CurrentRunID:        "run-1",
			WorkspaceID:         "workspace-1",
			CreatedAt:           now,
			UpdatedAt:           now,
		},
		Run: api.RunResponse{
			ID:        "run-1",
			TaskID:    "deploy",
			Status:    "queued",
			CreatedAt: now,
			UpdatedAt: now,
		},
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
		{"session", "start", "deploy", "--payload-file", payloadPath, "--payload-json", `{"env":"prod"}`},
		{"session", "start", "deploy", "--payload-file", payloadPath, "--payload", "env=prod"},
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

func TestRunCommandDoesNotExposeInputFlagAliases(t *testing.T) {
	for _, args := range [][]string{
		{"session", "start", "deploy", "--input-json", `{"env":"prod"}`},
		{"session", "start", "deploy", "--input-file", "payload.json"},
		{"session", "start", "deploy", "--input", "env=prod"},
	} {
		cmd := newRootCommand()
		cmd.SetOut(&bytes.Buffer{})
		cmd.SetErr(&bytes.Buffer{})
		cmd.SetArgs(args)
		err := cmd.Execute()
		if err == nil || !strings.Contains(err.Error(), "unknown flag") {
			t.Fatalf("args %v err = %v", args, err)
		}
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
	cmd.SetArgs([]string{"session", "start", "deploy", "-p", "env=prod"})
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
	cmd.SetArgs([]string{"session", "start", "bad task"})
	err := cmd.Execute()
	if err == nil || !strings.Contains(err.Error(), "task_id") {
		t.Fatalf("err = %v", err)
	}
	if called {
		t.Fatal("server was called")
	}
}
