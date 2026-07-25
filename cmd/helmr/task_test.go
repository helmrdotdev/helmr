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

func TestTaskStartCreatesTaskRun(t *testing.T) {
	var request api.StartTaskRequest
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost || r.URL.Path != "/api/tasks/deploy/start" {
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
		if _, ok := raw["source"]; ok {
			t.Fatalf("request JSON included source: %s", body)
		}
		if _, ok := raw["payload"]; !ok {
			t.Fatalf("request JSON omitted explicit payload: %s", body)
		}
		if err := json.Unmarshal(body, &request); err != nil {
			t.Fatal(err)
		}
		w.WriteHeader(http.StatusCreated)
		_ = json.NewEncoder(w).Encode(api.StartTaskResponse{RunID: "run-1"})
	}))
	defer server.Close()
	t.Setenv(helmrAPIURLEnv, server.URL)
	t.Setenv(helmrAPIKeyEnv, "test-key")

	var out bytes.Buffer
	cmd := newRootCommand()
	cmd.SetOut(&out)
	cmd.SetErr(&bytes.Buffer{})
	cmd.SetArgs([]string{
		"task", "start", "deploy",
		"--workspace", "ws_1",
		"--payload", "env=prod",
		"--idempotency-key", "start-1",
		"--metadata-json", `{"source":"cli"}`,
		"--tag", "deploy",
		"--tag", "prod",
		"--retry-json", `{"max_attempts":3}`,
	})
	if err := cmd.Execute(); err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"run_id: run-1"} {
		if !strings.Contains(out.String(), want) {
			t.Fatalf("output = %q, missing %q", out.String(), want)
		}
	}
	if request.Workspace.ID == nil || *request.Workspace.ID != "ws_1" ||
		request.IdempotencyKey != "start-1" {
		t.Fatalf("request = %+v", request)
	}
	if string(request.Metadata) != `{"source":"cli"}` ||
		request.Retry == nil || request.Retry.MaxAttempts == nil ||
		*request.Retry.MaxAttempts != 3 {
		t.Fatalf("run request = %+v", request)
	}
	if strings.Join(request.Tags, ",") != "deploy,prod" {
		t.Fatalf("tags = %+v", request.Tags)
	}
	if string(request.Payload) != `{"env":"prod"}` {
		t.Fatalf("payload = %s", request.Payload)
	}
}

func TestTaskStartOmitsPayloadWhenNotSpecified(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost || r.URL.Path != "/api/tasks/deploy/start" {
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
		if _, ok := raw["payload"]; ok {
			t.Fatalf("request JSON included omitted payload: %s", body)
		}
		w.WriteHeader(http.StatusCreated)
		_ = json.NewEncoder(w).Encode(api.StartTaskResponse{RunID: "run-1"})
	}))
	defer server.Close()
	t.Setenv(helmrAPIURLEnv, server.URL)
	t.Setenv(helmrAPIKeyEnv, "test-key")

	cmd := newRootCommand()
	cmd.SetOut(&bytes.Buffer{})
	cmd.SetErr(&bytes.Buffer{})
	cmd.SetArgs([]string{"task", "start", "deploy", "--workspace", "ws_1"})
	if err := cmd.Execute(); err != nil {
		t.Fatal(err)
	}
}

func TestTaskCommandReadsPayloadFile(t *testing.T) {
	var request api.StartTaskRequest
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost || r.URL.Path != "/api/tasks/deploy/start" {
			t.Fatalf("%s %s", r.Method, r.URL.Path)
		}
		if err := json.NewDecoder(r.Body).Decode(&request); err != nil {
			t.Fatal(err)
		}
		w.WriteHeader(http.StatusCreated)
		_ = json.NewEncoder(w).Encode(api.StartTaskResponse{RunID: "run-1"})
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
	cmd.SetArgs([]string{"task", "start", "deploy", "--workspace", "ws_1", "--payload-file", payloadPath})
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

func TestTaskStartRequiresExistingWorkspace(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		t.Fatalf("server must not be called: %s %s", r.Method, r.URL.Path)
	}))
	defer server.Close()
	t.Setenv(helmrAPIURLEnv, server.URL)
	t.Setenv(helmrAPIKeyEnv, "test-key")

	cmd := newRootCommand()
	cmd.SetOut(&bytes.Buffer{})
	cmd.SetErr(&bytes.Buffer{})
	cmd.SetArgs([]string{"task", "start", "deploy"})
	if err := cmd.Execute(); err == nil || !strings.Contains(err.Error(), "--workspace is required") {
		t.Fatalf("err = %v", err)
	}
}

func TestTaskStartWaitWaitsForRun(t *testing.T) {
	getRunCalls := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodPost && r.URL.Path == "/api/tasks/deploy/start":
			w.WriteHeader(http.StatusCreated)
			_ = json.NewEncoder(w).Encode(api.StartTaskResponse{RunID: "run-1"})
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
	cmd.SetArgs([]string{"task", "start", "deploy", "--workspace", "ws_1", "--wait", "--timeout", "1500ms"})
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

func TestTaskStartWaitPollsRunSnapshot(t *testing.T) {
	getRunCalls := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodPost && r.URL.Path == "/api/tasks/deploy/start":
			w.WriteHeader(http.StatusCreated)
			_ = json.NewEncoder(w).Encode(api.StartTaskResponse{RunID: "run-1"})
		case r.Method == http.MethodGet && r.URL.Path == "/api/runs/run-1":
			getRunCalls++
			status := api.RunStatusQueued
			if getRunCalls > 1 {
				status = api.RunStatusSucceeded
			}
			_ = json.NewEncoder(w).Encode(api.RunResponse{ID: "run-1", TaskID: "deploy", Status: status})
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
	cmd.SetArgs([]string{"task", "start", "deploy", "--workspace", "ws_1", "--wait", "--timeout", "1500ms"})
	if err := cmd.Execute(); err != nil {
		t.Fatal(err)
	}
	if getRunCalls != 2 {
		t.Fatalf("snapshot calls = %d, want 2", getRunCalls)
	}
	if !strings.Contains(out.String(), "run_status: succeeded") {
		t.Fatalf("output = %q", out.String())
	}
}

func TestTaskStartRejectsJSONFollow(t *testing.T) {
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
	cmd.SetArgs([]string{"task", "start", "deploy", "--workspace", "ws_1", "--json", "--follow"})
	err := cmd.Execute()
	if err == nil || !strings.Contains(err.Error(), "--json cannot be combined with --follow") {
		t.Fatalf("err = %v", err)
	}
	if called {
		t.Fatal("server was called")
	}
}

func TestTaskStartFollowTimeoutReturnsError(t *testing.T) {
	oldPollInterval := runFollowPollInterval
	runFollowPollInterval = 10 * time.Millisecond
	t.Cleanup(func() {
		runFollowPollInterval = oldPollInterval
	})
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodPost && r.URL.Path == "/api/tasks/deploy/start":
			w.WriteHeader(http.StatusCreated)
			_ = json.NewEncoder(w).Encode(api.StartTaskResponse{RunID: "run-1"})
		case r.Method == http.MethodGet && r.URL.Path == "/api/runs/run-1/logs":
			_ = json.NewEncoder(w).Encode(api.RunLogPage{})
		case r.Method == http.MethodGet && r.URL.Path == "/api/runs/run-1":
			_ = json.NewEncoder(w).Encode(api.RunResponse{
				ID: "run-1", Status: api.RunStatusQueued,
			})
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
	cmd.SetArgs([]string{"task", "start", "deploy", "--workspace", "ws_1", "--follow", "--timeout", "1s"})
	err := cmd.Execute()
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("err = %v", err)
	}
}

func TestTaskCommandRejectsPayloadFileCombinations(t *testing.T) {
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
		{"task", "start", "deploy", "--workspace", "ws_1", "--payload-file", payloadPath, "--payload-json", `{"env":"prod"}`},
		{"task", "start", "deploy", "--workspace", "ws_1", "--payload-file", payloadPath, "--payload", "env=prod"},
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

func TestTaskCommandDoesNotExposeInputFlagAliases(t *testing.T) {
	for _, args := range [][]string{
		{"task", "start", "deploy", "--workspace", "ws_1", "--input-json", `{"env":"prod"}`},
		{"task", "start", "deploy", "--workspace", "ws_1", "--input-file", "payload.json"},
		{"task", "start", "deploy", "--workspace", "ws_1", "--input", "env=prod"},
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

func TestTaskCommandRejectsProjectFlagThatLooksLikePayload(t *testing.T) {
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
	cmd.SetArgs([]string{"task", "start", "deploy", "--workspace", "ws_1", "-p", "env=prod"})
	err := cmd.Execute()
	if err == nil || !strings.Contains(err.Error(), "--project must be a project slug or ID") {
		t.Fatalf("err = %v", err)
	}
	if called {
		t.Fatal("server was called")
	}
}

func TestTaskCommandRejectsInvalidTaskIDBeforeRequest(t *testing.T) {
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
	cmd.SetArgs([]string{"task", "start", "bad task", "--workspace", "ws_1"})
	err := cmd.Execute()
	if err == nil || !strings.Contains(err.Error(), "task_id") {
		t.Fatalf("err = %v", err)
	}
	if called {
		t.Fatal("server was called")
	}
}
