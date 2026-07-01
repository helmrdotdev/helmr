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

func TestSessionStreamInputSendUsesCanonicalCommand(t *testing.T) {
	var request api.AppendStreamRecordRequest
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost || r.URL.Path != "/api/sessions/session-1/inputs/approval" {
			t.Fatalf("%s %s", r.Method, r.URL.Path)
		}
		if err := json.NewDecoder(r.Body).Decode(&request); err != nil {
			t.Fatal(err)
		}
		_ = json.NewEncoder(w).Encode(api.AppendStreamRecordResponse{
			Record: api.StreamRecordResponse{ID: "record-1", Sequence: 7},
		})
	}))
	defer server.Close()
	t.Setenv(helmrAPIURLEnv, server.URL)
	t.Setenv(helmrAPIKeyEnv, "test-key")

	var out bytes.Buffer
	cmd := newRootCommand()
	cmd.SetOut(&out)
	cmd.SetErr(&bytes.Buffer{})
	cmd.SetArgs([]string{"session", "stream", "input", "send", "session-1", "approval", "--data-json", `{"approved":true}`, "--correlation-id", "thread-1", "--idempotency-key", "click-1"})
	if err := cmd.Execute(); err != nil {
		t.Fatal(err)
	}
	if string(request.Data) != `{"approved":true}` || request.CorrelationID != "thread-1" || request.IdempotencyKey != "click-1" {
		t.Fatalf("request = %+v", request)
	}
	if got := strings.TrimSpace(out.String()); got != "record-1 7" {
		t.Fatalf("output = %q", out.String())
	}
}

func TestSessionListFiltersByExternalID(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet || r.URL.Path != "/api/sessions" {
			t.Fatalf("%s %s", r.Method, r.URL.Path)
		}
		if r.URL.Query().Get("external_id") != "slack:T123:C456" {
			t.Fatalf("query = %s", r.URL.RawQuery)
		}
		_ = json.NewEncoder(w).Encode(api.ListSessionsResponse{
			Sessions: []api.SessionResponse{{ID: "session-1", TaskID: "review", Status: "open", Activity: "idle"}},
		})
	}))
	defer server.Close()
	t.Setenv(helmrAPIURLEnv, server.URL)
	t.Setenv(helmrAPIKeyEnv, "test-key")

	var out bytes.Buffer
	cmd := newRootCommand()
	cmd.SetOut(&out)
	cmd.SetErr(&bytes.Buffer{})
	cmd.SetArgs([]string{"session", "list", "--external-id", "slack:T123:C456"})
	if err := cmd.Execute(); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out.String(), "session-1\treview\topen\tidle") {
		t.Fatalf("output = %q", out.String())
	}
}

func TestSessionGetByExternalIDUsesCollectionFilter(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet || r.URL.Path != "/api/sessions" {
			t.Fatalf("%s %s", r.Method, r.URL.Path)
		}
		if r.URL.Query().Get("external_id") != "slack:T123:C456" || r.URL.Query().Get("limit") != "2" {
			t.Fatalf("query = %s", r.URL.RawQuery)
		}
		_ = json.NewEncoder(w).Encode(api.ListSessionsResponse{
			Sessions: []api.SessionResponse{{ID: "session-1", TaskID: "review", Status: "open", Activity: "idle", CurrentRunID: "run-1", WorkspaceID: "workspace-1"}},
		})
	}))
	defer server.Close()
	t.Setenv(helmrAPIURLEnv, server.URL)
	t.Setenv(helmrAPIKeyEnv, "test-key")

	var out bytes.Buffer
	cmd := newRootCommand()
	cmd.SetOut(&out)
	cmd.SetErr(&bytes.Buffer{})
	cmd.SetArgs([]string{"session", "get", "--external-id", "slack:T123:C456"})
	if err := cmd.Execute(); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out.String(), "Session:   session-1") || !strings.Contains(out.String(), "Run:       run-1") {
		t.Fatalf("output = %q", out.String())
	}
}

func TestSessionCloseUsesCloseEndpoint(t *testing.T) {
	var request api.CloseSessionRequest
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost || r.URL.Path != "/api/sessions/session-1/close" {
			t.Fatalf("%s %s", r.Method, r.URL.Path)
		}
		if err := json.NewDecoder(r.Body).Decode(&request); err != nil {
			t.Fatal(err)
		}
		_ = json.NewEncoder(w).Encode(api.SessionResponse{ID: "session-1", Status: "closed", Activity: "idle", CurrentRunID: "run-1"})
	}))
	defer server.Close()
	t.Setenv(helmrAPIURLEnv, server.URL)
	t.Setenv(helmrAPIKeyEnv, "test-key")

	var out bytes.Buffer
	cmd := newRootCommand()
	cmd.SetOut(&out)
	cmd.SetErr(&bytes.Buffer{})
	cmd.SetArgs([]string{"session", "close", "session-1", "--reason", "done"})
	if err := cmd.Execute(); err != nil {
		t.Fatal(err)
	}
	if request.Reason != "done" {
		t.Fatalf("request = %+v", request)
	}
	for _, want := range []string{
		"operation: close",
		"session_id: session-1",
		"session_status: closed",
		"session_activity: idle",
		"run_id: run-1",
	} {
		if !strings.Contains(out.String(), want) {
			t.Fatalf("output = %q, missing %q", out.String(), want)
		}
	}
}

func TestSessionCloseWithSavedLoginUsesScopedEndpoint(t *testing.T) {
	state, _ := installTestCLIConfig(t)
	var request api.CloseSessionRequest
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if got := r.Header.Get("authorization"); got != "Bearer stored-key" {
			t.Fatalf("auth = %s", got)
		}
		if r.Method != http.MethodPost || r.URL.Path != "/api/projects/project-1/environments/env-1/sessions/session-1/close" {
			t.Fatalf("%s %s", r.Method, r.URL.Path)
		}
		if err := json.NewDecoder(r.Body).Decode(&request); err != nil {
			t.Fatal(err)
		}
		_ = json.NewEncoder(w).Encode(api.SessionResponse{ID: "session-1", Status: "closed", Activity: "idle"})
	}))
	defer server.Close()
	if err := state.SaveLogin(server.URL, "stored-key"); err != nil {
		t.Fatal(err)
	}

	cmd := newRootCommand()
	cmd.SetOut(&bytes.Buffer{})
	cmd.SetErr(&bytes.Buffer{})
	cmd.SetArgs([]string{"session", "close", "session-1", "--project", "project-1", "--env", "env-1", "--reason", "done"})
	if err := cmd.Execute(); err != nil {
		t.Fatal(err)
	}
	if request.Reason != "done" {
		t.Fatalf("request = %+v", request)
	}
}

func TestSessionCancelPrintsLifecycleFields(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost || r.URL.Path != "/api/sessions/session-1/cancel" {
			t.Fatalf("%s %s", r.Method, r.URL.Path)
		}
		_ = json.NewEncoder(w).Encode(api.SessionResponse{ID: "session-1", Status: "cancelled", Activity: "idle", CurrentRunID: "run-1"})
	}))
	defer server.Close()
	t.Setenv(helmrAPIURLEnv, server.URL)
	t.Setenv(helmrAPIKeyEnv, "test-key")

	var out bytes.Buffer
	cmd := newRootCommand()
	cmd.SetOut(&out)
	cmd.SetErr(&bytes.Buffer{})
	cmd.SetArgs([]string{"session", "cancel", "session-1"})
	if err := cmd.Execute(); err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{
		"operation: cancel",
		"session_id: session-1",
		"session_status: cancelled",
		"session_activity: idle",
		"run_id: run-1",
	} {
		if !strings.Contains(out.String(), want) {
			t.Fatalf("output = %q, missing %q", out.String(), want)
		}
	}
}

func TestSessionStreamListUsesCanonicalCommand(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet || r.URL.Path != "/api/sessions/session-1/streams" {
			t.Fatalf("%s %s", r.Method, r.URL.Path)
		}
		_ = json.NewEncoder(w).Encode(api.ListSessionStreamsResponse{
			Streams: []api.StreamResponse{{Name: "approval", Direction: "input", Sequence: 3}},
		})
	}))
	defer server.Close()
	t.Setenv(helmrAPIURLEnv, server.URL)
	t.Setenv(helmrAPIKeyEnv, "test-key")

	var out bytes.Buffer
	cmd := newRootCommand()
	cmd.SetOut(&out)
	cmd.SetErr(&bytes.Buffer{})
	cmd.SetArgs([]string{"session", "stream", "list", "session-1"})
	if err := cmd.Execute(); err != nil {
		t.Fatal(err)
	}
	if got := strings.TrimSpace(out.String()); got != "approval\tinput\t3" {
		t.Fatalf("output = %q", out.String())
	}
}

func TestSessionStreamInputListUsesCanonicalCommand(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet || r.URL.Path != "/api/sessions/session-1/inputs/approval" {
			t.Fatalf("%s %s", r.Method, r.URL.Path)
		}
		if r.URL.Query().Get("after_sequence") != "2" || r.URL.Query().Get("limit") != "5" {
			t.Fatalf("query = %s", r.URL.RawQuery)
		}
		_ = json.NewEncoder(w).Encode(api.ListStreamRecordsResponse{
			Records: []api.StreamRecordResponse{{ID: "record-3", Sequence: 3, Data: json.RawMessage(`{"approved":true}`)}},
		})
	}))
	defer server.Close()
	t.Setenv(helmrAPIURLEnv, server.URL)
	t.Setenv(helmrAPIKeyEnv, "test-key")

	var out bytes.Buffer
	cmd := newRootCommand()
	cmd.SetOut(&out)
	cmd.SetErr(&bytes.Buffer{})
	cmd.SetArgs([]string{"session", "stream", "input", "list", "session-1", "approval", "--cursor", "2", "--limit", "5"})
	if err := cmd.Execute(); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out.String(), `"id":"record-3"`) {
		t.Fatalf("output = %q", out.String())
	}
}

func TestSessionStreamOutputListUsesCanonicalCommand(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet || r.URL.Path != "/api/sessions/session-1/outputs/events" {
			t.Fatalf("%s %s", r.Method, r.URL.Path)
		}
		if r.URL.Query().Get("after_sequence") != "9" || r.URL.Query().Get("limit") != "4" {
			t.Fatalf("query = %s", r.URL.RawQuery)
		}
		_ = json.NewEncoder(w).Encode(api.ListStreamRecordsResponse{
			Records: []api.StreamRecordResponse{{ID: "record-10", Sequence: 10, Data: json.RawMessage(`{"ok":true}`)}},
		})
	}))
	defer server.Close()
	t.Setenv(helmrAPIURLEnv, server.URL)
	t.Setenv(helmrAPIKeyEnv, "test-key")

	var out bytes.Buffer
	cmd := newRootCommand()
	cmd.SetOut(&out)
	cmd.SetErr(&bytes.Buffer{})
	cmd.SetArgs([]string{"session", "stream", "output", "list", "session-1", "events", "--cursor", "9", "--limit", "4"})
	if err := cmd.Execute(); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out.String(), `"id":"record-10"`) {
		t.Fatalf("output = %q", out.String())
	}
}
