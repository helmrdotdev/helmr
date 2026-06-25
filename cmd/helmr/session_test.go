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
