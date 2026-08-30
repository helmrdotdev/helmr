package main

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/helmrdotdev/helmr/internal/api"
)

func TestWaitCommandPollsUntilTerminal(t *testing.T) {
	requests := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet {
			t.Fatalf("%s %s", r.Method, r.URL.Path)
		}
		switch r.URL.Path {
		case "/v1/runs/019c10d5-a6f7-7af1-8f5f-bb97bcc0dc31":
			requests++
			status := "running"
			if requests > 1 {
				status = "succeeded"
			}
			_ = json.NewEncoder(w).Encode(api.RunSnapshotResponse{ID: "019c10d5-a6f7-7af1-8f5f-bb97bcc0dc31", Status: api.RunStatus(status)})
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
	cmd.SetArgs([]string{"run", "wait", "019c10d5-a6f7-7af1-8f5f-bb97bcc0dc31", "--timeout", "1s"})
	if err := cmd.Execute(); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out.String(), "run_id: 019c10d5-a6f7-7af1-8f5f-bb97bcc0dc31") || !strings.Contains(out.String(), "run_status: succeeded") {
		t.Fatalf("output = %q", out.String())
	}
	if requests != 2 {
		t.Fatalf("requests = %d", requests)
	}
}

func TestRunEventsFollowStopsAfterTerminalEvent(t *testing.T) {
	eventRequests := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet || r.URL.Path != "/v1/runs/019c10d5-a6f7-7af1-8f5f-bb97bcc0dc31/events" {
			t.Fatalf("%s %s", r.Method, r.URL.Path)
		}
		eventRequests++
		_ = json.NewEncoder(w).Encode(api.RunEventPage{Events: []api.RunEvent{{
			ID: "7", Kind: api.RunEventKindCompleted, Message: "run.completed",
		}}})
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
	cmd.SetArgs([]string{"run", "events", "019c10d5-a6f7-7af1-8f5f-bb97bcc0dc31", "--follow"})
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

func TestEventsCommandFollowsRunEvents(t *testing.T) {
	oldPollInterval := runFollowPollInterval
	runFollowPollInterval = time.Millisecond
	t.Cleanup(func() { runFollowPollInterval = oldPollInterval })
	var requests atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet || r.URL.Path != "/v1/runs/019c10d5-a6f7-7af1-8f5f-bb97bcc0dc31/events" {
			t.Fatalf("%s %s", r.Method, r.URL.Path)
		}
		request := requests.Add(1)
		if request == 1 {
			next := "cursor-next"
			_ = json.NewEncoder(w).Encode(api.RunEventPage{
				Events:     []api.RunEvent{{ID: "event-1", Kind: "run.created"}},
				NextCursor: &next,
			})
			return
		}
		if r.URL.Query().Get("cursor") != "cursor-next" {
			t.Fatalf("query = %s", r.URL.RawQuery)
		}
		_ = json.NewEncoder(w).Encode(api.RunEventPage{
			Events: []api.RunEvent{{ID: "event-2", Kind: api.RunEventKindCompleted}},
		})
	}))
	defer server.Close()
	t.Setenv(helmrAPIURLEnv, server.URL)
	t.Setenv(helmrAPIKeyEnv, "test-key")

	var out bytes.Buffer
	cmd := newRootCommand()
	cmd.SetOut(&out)
	cmd.SetErr(&bytes.Buffer{})
	cmd.SetArgs([]string{"run", "events", "019c10d5-a6f7-7af1-8f5f-bb97bcc0dc31", "--follow"})
	if err := cmd.Execute(); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out.String(), `"id":"event-1"`) || !strings.Contains(out.String(), `"id":"event-2"`) {
		t.Fatalf("output = %q", out.String())
	}
	if requests := requests.Load(); requests < 2 {
		t.Fatalf("requests = %d", requests)
	}
}

func TestLogsCommandFollowsRunLogs(t *testing.T) {
	var requests []string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests = append(requests, r.Method+" "+r.URL.RequestURI())
		switch {
		case r.Method == http.MethodGet && r.URL.Path == "/v1/runs/019c10d5-a6f7-7af1-8f5f-bb97bcc0dc31/logs" && r.URL.Query().Get("cursor") == "":
			_ = json.NewEncoder(w).Encode(api.RunLogPage{
				Logs: []api.RunLogRecord{{
					Kind: "stdout", ContentBase64: base64.StdEncoding.EncodeToString([]byte("old\n")),
				}},
				NextCursor: "cursor-old",
			})
		case r.Method == http.MethodGet && r.URL.Path == "/v1/runs/019c10d5-a6f7-7af1-8f5f-bb97bcc0dc31/logs" && r.URL.Query().Get("cursor") == "cursor-old":
			_ = json.NewEncoder(w).Encode(api.RunLogPage{
				Logs: []api.RunLogRecord{
					{Kind: "stdout", ContentBase64: base64.StdEncoding.EncodeToString([]byte("new\n"))},
					{Kind: "stderr", ContentBase64: base64.StdEncoding.EncodeToString([]byte("warn\n"))},
				},
				NextCursor: "cursor-new",
			})
		case r.Method == http.MethodGet && r.URL.Path == "/v1/runs/019c10d5-a6f7-7af1-8f5f-bb97bcc0dc31/logs" && r.URL.Query().Get("cursor") == "cursor-new":
			_ = json.NewEncoder(w).Encode(api.RunLogPage{})
		case r.Method == http.MethodGet && r.URL.Path == "/v1/runs/019c10d5-a6f7-7af1-8f5f-bb97bcc0dc31":
			_ = json.NewEncoder(w).Encode(api.RunSnapshotResponse{ID: "019c10d5-a6f7-7af1-8f5f-bb97bcc0dc31", Status: api.RunStatusSucceeded})
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
	cmd.SetArgs([]string{"run", "logs", "019c10d5-a6f7-7af1-8f5f-bb97bcc0dc31", "--follow"})
	if err := cmd.Execute(); err != nil {
		t.Fatal(err)
	}
	if out.String() != "old\nnew\n" {
		t.Fatalf("stdout = %q", out.String())
	}
	if errOut.String() != "warn\n" {
		t.Fatalf("stderr = %q", errOut.String())
	}
	if got := strings.Join(requests, ","); got != "GET /v1/runs/019c10d5-a6f7-7af1-8f5f-bb97bcc0dc31/logs,GET /v1/runs/019c10d5-a6f7-7af1-8f5f-bb97bcc0dc31/logs?cursor=cursor-old,GET /v1/runs/019c10d5-a6f7-7af1-8f5f-bb97bcc0dc31,GET /v1/runs/019c10d5-a6f7-7af1-8f5f-bb97bcc0dc31/logs?cursor=cursor-new" {
		t.Fatalf("requests = %s", got)
	}
}

func TestLogsCommandPrintsStreams(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet || r.URL.Path != "/v1/runs/019c10d5-a6f7-7af1-8f5f-bb97bcc0dc31/logs" {
			t.Fatalf("%s %s", r.Method, r.URL.Path)
		}
		_ = json.NewEncoder(w).Encode(api.RunLogPage{Logs: []api.RunLogRecord{
			{Kind: "stdout", ContentBase64: base64.StdEncoding.EncodeToString([]byte("hello\n"))},
			{Kind: "stderr", ContentBase64: base64.StdEncoding.EncodeToString([]byte("warn\n"))},
		}})
	}))
	defer server.Close()
	t.Setenv(helmrAPIURLEnv, server.URL)
	t.Setenv(helmrAPIKeyEnv, "test-key")

	var out, stderr bytes.Buffer
	cmd := newRootCommand()
	cmd.SetOut(&out)
	cmd.SetErr(&stderr)
	cmd.SetArgs([]string{"run", "logs", "019c10d5-a6f7-7af1-8f5f-bb97bcc0dc31"})
	if err := cmd.Execute(); err != nil {
		t.Fatal(err)
	}
	if out.String() != "hello\n" || stderr.String() != "warn\n" {
		t.Fatalf("stdout=%q stderr=%q", out.String(), stderr.String())
	}
}

func TestEventsCommandPrintsJSONLines(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet || r.URL.Path != "/v1/runs/019c10d5-a6f7-7af1-8f5f-bb97bcc0dc31/events" || r.URL.Query().Get("cursor") != "cursor-current" || r.URL.Query().Get("limit") != "2" {
			t.Fatalf("%s %s?%s", r.Method, r.URL.Path, r.URL.RawQuery)
		}
		_ = json.NewEncoder(w).Encode(api.RunEventPage{
			Events: []api.RunEvent{{ID: "cursor-next", Kind: "run.started"}},
		})
	}))
	defer server.Close()
	t.Setenv(helmrAPIURLEnv, server.URL)
	t.Setenv(helmrAPIKeyEnv, "test-key")

	var out bytes.Buffer
	cmd := newRootCommand()
	cmd.SetOut(&out)
	cmd.SetErr(&bytes.Buffer{})
	cmd.SetArgs([]string{"run", "events", "019c10d5-a6f7-7af1-8f5f-bb97bcc0dc31", "--cursor", "cursor-current", "--limit", "2"})
	if err := cmd.Execute(); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out.String(), `"kind":"run.started"`) {
		t.Fatalf("output = %q", out.String())
	}
}

func TestRunEventsWaitReadyRetriesTelemetryLag(t *testing.T) {
	withFastRunTelemetryPoll(t)
	requests := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests++
		if requests == 1 {
			writeRunTelemetryTestError(w, http.StatusServiceUnavailable, "telemetry_lagging")
			return
		}
		_ = json.NewEncoder(w).Encode(api.RunEventPage{Events: []api.RunEvent{{ID: "ready", Kind: "run.completed"}}})
	}))
	defer server.Close()
	setRunTelemetryTestEnvironment(t, server.URL)

	var out bytes.Buffer
	cmd := newRootCommand()
	cmd.SetOut(&out)
	cmd.SetErr(&bytes.Buffer{})
	cmd.SetArgs([]string{"run", "events", "019c10d5-a6f7-7af1-8f5f-bb97bcc0dc31", "--wait-ready", "1s"})
	if err := cmd.Execute(); err != nil {
		t.Fatal(err)
	}
	if requests != 2 || !strings.Contains(out.String(), `"id":"ready"`) {
		t.Fatalf("requests=%d output=%q", requests, out.String())
	}
}

func TestRunLogsWaitReadyRetriesTelemetryLag(t *testing.T) {
	withFastRunTelemetryPoll(t)
	requests := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests++
		if requests == 1 {
			writeRunTelemetryTestError(w, http.StatusServiceUnavailable, "telemetry_lagging")
			return
		}
		_ = json.NewEncoder(w).Encode(api.RunLogPage{Logs: []api.RunLogRecord{{
			Kind: "stdout", ContentBase64: base64.StdEncoding.EncodeToString([]byte("ready\n")),
		}}})
	}))
	defer server.Close()
	setRunTelemetryTestEnvironment(t, server.URL)

	var out bytes.Buffer
	cmd := newRootCommand()
	cmd.SetOut(&out)
	cmd.SetErr(&bytes.Buffer{})
	cmd.SetArgs([]string{"run", "logs", "019c10d5-a6f7-7af1-8f5f-bb97bcc0dc31", "--wait-ready", "1s"})
	if err := cmd.Execute(); err != nil {
		t.Fatal(err)
	}
	if requests != 2 || out.String() != "ready\n" {
		t.Fatalf("requests=%d output=%q", requests, out.String())
	}
}

func TestRunEventsWaitReadyStopsAtBound(t *testing.T) {
	withFastRunTelemetryPoll(t)
	var requests atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests.Add(1)
		writeRunTelemetryTestError(w, http.StatusServiceUnavailable, "telemetry_lagging")
	}))
	defer server.Close()
	setRunTelemetryTestEnvironment(t, server.URL)

	cmd := newRootCommand()
	cmd.SetOut(&bytes.Buffer{})
	cmd.SetErr(&bytes.Buffer{})
	cmd.SetArgs([]string{"run", "events", "019c10d5-a6f7-7af1-8f5f-bb97bcc0dc31", "--wait-ready", "5ms"})
	err := cmd.Execute()
	if err == nil || !strings.Contains(err.Error(), "wait for run telemetry readiness: context deadline exceeded") {
		t.Fatalf("err=%v", err)
	}
	if requests := requests.Load(); requests < 2 {
		t.Fatalf("requests=%d, want retries", requests)
	}
}

func TestRunEventsWaitReadyRejectsNonLaggingErrorsImmediately(t *testing.T) {
	tests := []struct {
		name   string
		status int
		code   string
	}{
		{name: "telemetry unavailable", status: http.StatusServiceUnavailable, code: "telemetry_unavailable"},
		{name: "generic service unavailable", status: http.StatusServiceUnavailable, code: "service_unavailable"},
		{name: "unauthorized", status: http.StatusUnauthorized, code: "unauthorized"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			requests := 0
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				requests++
				writeRunTelemetryTestError(w, test.status, test.code)
			}))
			defer server.Close()
			setRunTelemetryTestEnvironment(t, server.URL)

			cmd := newRootCommand()
			cmd.SetOut(&bytes.Buffer{})
			cmd.SetErr(&bytes.Buffer{})
			cmd.SetArgs([]string{"run", "events", "019c10d5-a6f7-7af1-8f5f-bb97bcc0dc31", "--wait-ready", "1s"})
			if err := cmd.Execute(); err == nil {
				t.Fatal("expected command failure")
			}
			if requests != 1 {
				t.Fatalf("requests=%d, want one", requests)
			}
		})
	}
}

func TestRunEventsWithoutWaitReadyReadsOnce(t *testing.T) {
	requests := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests++
		writeRunTelemetryTestError(w, http.StatusServiceUnavailable, "telemetry_lagging")
	}))
	defer server.Close()
	setRunTelemetryTestEnvironment(t, server.URL)

	cmd := newRootCommand()
	cmd.SetOut(&bytes.Buffer{})
	cmd.SetErr(&bytes.Buffer{})
	cmd.SetArgs([]string{"run", "events", "019c10d5-a6f7-7af1-8f5f-bb97bcc0dc31"})
	if err := cmd.Execute(); err == nil {
		t.Fatal("expected command failure")
	}
	if requests != 1 {
		t.Fatalf("requests=%d, want one", requests)
	}
}

func TestRunTelemetryWaitReadyFlagValidation(t *testing.T) {
	tests := [][]string{
		{"run", "events", "run-id", "--wait-ready", "0s"},
		{"run", "logs", "run-id", "--wait-ready", "invalid"},
		{"run", "events", "run-id", "--wait-ready="},
		{"run", "logs", "run-id", "--wait-ready=   "},
		{"run", "events", "run-id", "--wait-ready", "1s", "--follow"},
		{"run", "logs", "run-id", "--wait-ready", "1s", "--follow"},
		{"run", "events", "run-id", "--wait-ready=", "--follow"},
		{"run", "logs", "run-id", "--wait-ready=", "--follow"},
	}
	for _, args := range tests {
		cmd := newRootCommand()
		cmd.SetOut(&bytes.Buffer{})
		cmd.SetErr(&bytes.Buffer{})
		cmd.SetArgs(args)
		if err := cmd.Execute(); err == nil {
			t.Fatalf("args=%v unexpectedly succeeded", args)
		}
	}
}

func withFastRunTelemetryPoll(t *testing.T) {
	t.Helper()
	oldPollInterval := runFollowPollInterval
	runFollowPollInterval = time.Millisecond
	t.Cleanup(func() { runFollowPollInterval = oldPollInterval })
}

func setRunTelemetryTestEnvironment(t *testing.T, serverURL string) {
	t.Helper()
	t.Setenv(helmrAPIURLEnv, serverURL)
	t.Setenv(helmrAPIKeyEnv, "test-key")
}

func writeRunTelemetryTestError(w http.ResponseWriter, status int, code string) {
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(map[string]any{
		"error": map[string]any{"code": code, "message": code},
	})
}
