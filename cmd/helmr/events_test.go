package main

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/helmrdotdev/helmr/internal/api"
)

func TestWaitCommandFollowsEventsUntilTerminal(t *testing.T) {
	requests := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet {
			t.Fatalf("%s %s", r.Method, r.URL.Path)
		}
		switch r.URL.Path {
		case "/api/runs/run-1":
			requests++
			status := "running"
			if requests > 1 {
				status = "succeeded"
			}
			_ = json.NewEncoder(w).Encode(api.RunResponse{ID: "run-1", Status: status})
		case "/api/runs/run-1/events":
			if r.URL.Query().Get("follow") != "1" {
				t.Fatalf("query = %s", r.URL.RawQuery)
			}
			w.Header().Set("Content-Type", "text/event-stream")
			_, _ = w.Write([]byte("id: 1\nevent: run_event\ndata: {\"id\":\"1\",\"kind\":\"run.completed\"}\n\n"))
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
	cmd.SetArgs([]string{"run", "wait", "run-1", "--timeout", "1s"})
	if err := cmd.Execute(); err != nil {
		t.Fatal(err)
	}
	if strings.TrimSpace(out.String()) != "run-1 succeeded" {
		t.Fatalf("output = %q", out.String())
	}
	if requests != 2 {
		t.Fatalf("requests = %d", requests)
	}
}

func TestRunEventsFollowStopsAfterTerminalEvent(t *testing.T) {
	eventRequests := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet || r.URL.Path != "/api/runs/run-1/events" {
			t.Fatalf("%s %s", r.Method, r.URL.Path)
		}
		if r.URL.Query().Get("follow") != "1" {
			t.Fatalf("query = %s", r.URL.RawQuery)
		}
		eventRequests++
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = w.Write([]byte("id: 7\nevent: run_event\ndata: {\"id\":\"7\",\"kind\":\"run.completed\",\"message\":\"run.completed\"}\n\n"))
	}))
	defer server.Close()
	t.Setenv(helmrAPIURLEnv, server.URL)
	t.Setenv(helmrAPIKeyEnv, "test-key")

	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	var out bytes.Buffer
	cmd := newRootCommand()
	cmd.SetContext(ctx)
	cmd.SetOut(&out)
	cmd.SetErr(&bytes.Buffer{})
	cmd.SetArgs([]string{"run", "events", "run-1", "--follow"})
	if err := cmd.Execute(); err != nil {
		t.Fatal(err)
	}
	if eventRequests != 1 {
		t.Fatalf("event requests = %d, want one", eventRequests)
	}
	if !strings.Contains(out.String(), `"kind":"run.completed"`) {
		t.Fatalf("output = %q", out.String())
	}
}

func TestWaitCommandChecksStatusAfterStreamDisconnect(t *testing.T) {
	requests := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet {
			t.Fatalf("%s %s", r.Method, r.URL.Path)
		}
		switch r.URL.Path {
		case "/api/runs/run-1":
			requests++
			status := "running"
			if requests > 1 {
				status = "succeeded"
			}
			_ = json.NewEncoder(w).Encode(api.RunResponse{ID: "run-1", Status: status})
		case "/api/runs/run-1/events":
			w.Header().Set("Content-Type", "text/event-stream")
			_, _ = w.Write([]byte("id: 1\nevent: run_event\ndata: {\"id\":\"1\",\"kind\":\"run.created\"}\n\n"))
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
	cmd.SetArgs([]string{"run", "wait", "run-1", "--timeout", "1s"})
	if err := cmd.Execute(); err != nil {
		t.Fatal(err)
	}
	if strings.TrimSpace(out.String()) != "run-1 succeeded" {
		t.Fatalf("output = %q", out.String())
	}
	if requests != 2 {
		t.Fatalf("requests = %d", requests)
	}
}

func TestWaitCommandReconnectsAfterTransientEventStreamError(t *testing.T) {
	oldReconnectDelay := runEventReconnectDelay
	runEventReconnectDelay = time.Millisecond
	t.Cleanup(func() { runEventReconnectDelay = oldReconnectDelay })

	runRequests := 0
	eventRequests := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet {
			t.Fatalf("%s %s", r.Method, r.URL.Path)
		}
		switch r.URL.Path {
		case "/api/runs/run-1":
			runRequests++
			status := "running"
			if runRequests > 2 {
				status = "succeeded"
			}
			_ = json.NewEncoder(w).Encode(api.RunResponse{ID: "run-1", Status: status})
		case "/api/runs/run-1/events":
			eventRequests++
			if eventRequests == 1 {
				http.Error(w, "temporary", http.StatusBadGateway)
				return
			}
			w.Header().Set("Content-Type", "text/event-stream")
			_, _ = w.Write([]byte("id: 2\nevent: run_event\ndata: {\"id\":\"2\",\"kind\":\"run.completed\"}\n\n"))
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
	cmd.SetArgs([]string{"run", "wait", "run-1", "--timeout", "1s"})
	if err := cmd.Execute(); err != nil {
		t.Fatal(err)
	}
	if strings.TrimSpace(out.String()) != "run-1 succeeded" {
		t.Fatalf("output = %q", out.String())
	}
	if eventRequests != 2 {
		t.Fatalf("eventRequests = %d", eventRequests)
	}
}

func TestEventsCommandFollowsRunEvents(t *testing.T) {
	oldReconnectDelay := runEventReconnectDelay
	runEventReconnectDelay = time.Millisecond
	t.Cleanup(func() { runEventReconnectDelay = oldReconnectDelay })
	var requests int32
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet || r.URL.Path != "/api/runs/run-1/events" {
			t.Fatalf("%s %s", r.Method, r.URL.Path)
		}
		request := atomic.AddInt32(&requests, 1)
		if r.URL.Query().Get("follow") != "1" {
			t.Fatalf("query = %s", r.URL.RawQuery)
		}
		w.Header().Set("Content-Type", "text/event-stream")
		if request == 1 {
			_, _ = w.Write([]byte("id: 1\nevent: run_event\ndata: {\"id\":\"1\",\"kind\":\"run.created\"}\n\n"))
			return
		}
		if request > 2 {
			<-r.Context().Done()
			return
		}
		if got := r.Header.Get("Last-Event-ID"); got != "1" {
			t.Fatalf("last event id = %q", got)
		}
		_, _ = w.Write([]byte("id: 2\nevent: run_event\ndata: {\"id\":\"2\",\"kind\":\"run.completed\"}\n\n"))
		time.AfterFunc(10*time.Millisecond, cancel)
	}))
	defer server.Close()
	t.Setenv(helmrAPIURLEnv, server.URL)
	t.Setenv(helmrAPIKeyEnv, "test-key")

	var out bytes.Buffer
	cmd := newRootCommand()
	cmd.SetContext(ctx)
	cmd.SetOut(&out)
	cmd.SetErr(&bytes.Buffer{})
	cmd.SetArgs([]string{"run", "events", "run-1", "--follow"})
	if err := cmd.Execute(); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out.String(), `"id":"1"`) || !strings.Contains(out.String(), `"id":"2"`) {
		t.Fatalf("output = %q", out.String())
	}
	if requests < 2 {
		t.Fatalf("requests = %d", requests)
	}
}

func TestLogsCommandFollowsRunLogs(t *testing.T) {
	var requests []string
	var followRequests int
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests = append(requests, r.Method+" "+r.URL.RequestURI())
		switch {
		case r.Method == http.MethodGet && r.URL.Path == "/api/runs/run-1/logs" && r.URL.Query().Get("follow") == "":
			_ = json.NewEncoder(w).Encode(api.LogSnapshotResponse{
				StdoutBase64: base64.StdEncoding.EncodeToString([]byte("old\n")),
				StderrBase64: "",
				Cursor:       "7",
			})
		case r.Method == http.MethodGet && r.URL.Path == "/api/runs/run-1/logs" && r.URL.Query().Get("follow") == "1":
			followRequests++
			wantCursor := "7"
			if followRequests == 2 {
				wantCursor = "9"
			}
			if got := r.Header.Get("Last-Event-ID"); got != wantCursor {
				t.Fatalf("last event id = %q", got)
			}
			w.Header().Set("content-type", "text/event-stream")
			if followRequests == 2 {
				return
			}
			_, _ = io.WriteString(w, "id: 8\nevent: run_log\ndata: ")
			_ = json.NewEncoder(w).Encode(api.RunLogChunk{
				ID:            "8",
				RunID:         "run-1",
				RunLeaseID:    "run-lease-1",
				AttemptNumber: 1,
				Stream:        "stdout",
				ContentBase64: base64.StdEncoding.EncodeToString([]byte("new\n")),
				Bytes:         4,
				ObservedSeq:   8,
			})
			_, _ = io.WriteString(w, "\n")
			_, _ = io.WriteString(w, "id: 9\nevent: run_log\ndata: ")
			_ = json.NewEncoder(w).Encode(api.RunLogChunk{
				ID:            "9",
				RunID:         "run-1",
				RunLeaseID:    "run-lease-1",
				AttemptNumber: 1,
				Stream:        "stderr",
				ContentBase64: base64.StdEncoding.EncodeToString([]byte("warn\n")),
				Bytes:         5,
				ObservedSeq:   9,
			})
			_, _ = io.WriteString(w, "\n")
		case r.Method == http.MethodGet && r.URL.Path == "/api/runs/run-1":
			_ = json.NewEncoder(w).Encode(api.RunResponse{ID: "run-1", Status: api.RunStatusSucceeded})
		default:
			t.Fatalf("unexpected request %s %s", r.Method, r.URL.RequestURI())
		}
	}))
	defer server.Close()
	t.Setenv(helmrAPIURLEnv, server.URL)
	t.Setenv(helmrAPIKeyEnv, "test-key")

	var out bytes.Buffer
	var errOut bytes.Buffer
	cmd := newRootCommand()
	cmd.SetOut(&out)
	cmd.SetErr(&errOut)
	cmd.SetArgs([]string{"run", "logs", "run-1", "--follow"})
	if err := cmd.Execute(); err != nil {
		t.Fatal(err)
	}
	if out.String() != "old\nnew\n" {
		t.Fatalf("stdout = %q", out.String())
	}
	if errOut.String() != "warn\n" {
		t.Fatalf("stderr = %q", errOut.String())
	}
	if got := strings.Join(requests, ","); got != "GET /api/runs/run-1/logs,GET /api/runs/run-1/logs?follow=1,GET /api/runs/run-1,GET /api/runs/run-1/logs?follow=1" {
		t.Fatalf("requests = %s", got)
	}
}

func TestLogsCommandPrintsStreams(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet || r.URL.Path != "/api/runs/run-1/logs" {
			t.Fatalf("%s %s", r.Method, r.URL.Path)
		}
		_ = json.NewEncoder(w).Encode(api.LogSnapshotResponse{
			StdoutBase64: base64.StdEncoding.EncodeToString([]byte("hello\n")),
			StderrBase64: base64.StdEncoding.EncodeToString([]byte("warn\n")),
		})
	}))
	defer server.Close()
	t.Setenv(helmrAPIURLEnv, server.URL)
	t.Setenv(helmrAPIKeyEnv, "test-key")

	var out, stderr bytes.Buffer
	cmd := newRootCommand()
	cmd.SetOut(&out)
	cmd.SetErr(&stderr)
	cmd.SetArgs([]string{"run", "logs", "run-1"})
	if err := cmd.Execute(); err != nil {
		t.Fatal(err)
	}
	if out.String() != "hello\n" || stderr.String() != "warn\n" {
		t.Fatalf("stdout=%q stderr=%q", out.String(), stderr.String())
	}
}

func TestEventsCommandPrintsJSONLines(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet || r.URL.Path != "/api/runs/run-1/events" || r.URL.Query().Get("cursor") != "4" || r.URL.Query().Get("limit") != "2" {
			t.Fatalf("%s %s?%s", r.Method, r.URL.Path, r.URL.RawQuery)
		}
		_ = json.NewEncoder(w).Encode(api.RunEventPage{
			Events: []api.RunEvent{{ID: "5", Kind: "run.started"}},
			Cursor: 5,
		})
	}))
	defer server.Close()
	t.Setenv(helmrAPIURLEnv, server.URL)
	t.Setenv(helmrAPIKeyEnv, "test-key")

	var out bytes.Buffer
	cmd := newRootCommand()
	cmd.SetOut(&out)
	cmd.SetErr(&bytes.Buffer{})
	cmd.SetArgs([]string{"run", "events", "run-1", "--cursor", "4", "--limit", "2"})
	if err := cmd.Execute(); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out.String(), `"kind":"run.started"`) {
		t.Fatalf("output = %q", out.String())
	}
}
