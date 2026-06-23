package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/helmrdotdev/helmr/internal/api"
)

func TestSessionWaitPassesTimeout(t *testing.T) {
	var request api.TaskWaitRequest
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost || r.URL.Path != "/api/sessions/session-1/wait" {
			t.Fatalf("%s %s", r.Method, r.URL.Path)
		}
		if err := json.NewDecoder(r.Body).Decode(&request); err != nil {
			t.Fatal(err)
		}
		_ = json.NewEncoder(w).Encode(api.TaskSessionResponse{ID: "session-1", Status: "completed"})
	}))
	defer server.Close()
	t.Setenv(helmrAPIURLEnv, server.URL)
	t.Setenv(helmrAPIKeyEnv, "test-key")

	cmd := newRootCommand()
	cmd.SetOut(&bytes.Buffer{})
	cmd.SetErr(&bytes.Buffer{})
	cmd.SetArgs([]string{"session", "wait", "session-1", "--timeout", "1500ms"})
	if err := cmd.Execute(); err != nil {
		t.Fatal(err)
	}
	if request.TimeoutSeconds != 2 {
		t.Fatalf("timeout seconds = %d", request.TimeoutSeconds)
	}
}

func TestSessionWaitContinuesAfterServerLongPollTimeout(t *testing.T) {
	waitCalls := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost || r.URL.Path != "/api/sessions/session-1/wait" {
			t.Fatalf("%s %s", r.Method, r.URL.Path)
		}
		waitCalls++
		if waitCalls == 1 {
			_ = json.NewEncoder(w).Encode(api.TaskSessionResponse{ID: "session-1", Status: "open", TimedOut: true})
			return
		}
		_ = json.NewEncoder(w).Encode(api.TaskSessionResponse{ID: "session-1", Status: "completed"})
	}))
	defer server.Close()
	t.Setenv(helmrAPIURLEnv, server.URL)
	t.Setenv(helmrAPIKeyEnv, "test-key")

	var out bytes.Buffer
	cmd := newRootCommand()
	cmd.SetOut(&out)
	cmd.SetErr(&bytes.Buffer{})
	cmd.SetArgs([]string{"session", "wait", "session-1"})
	if err := cmd.Execute(); err != nil {
		t.Fatal(err)
	}
	if waitCalls != 2 {
		t.Fatalf("wait calls = %d", waitCalls)
	}
	if got := strings.TrimSpace(out.String()); got != "session-1 completed" {
		t.Fatalf("output = %q", out.String())
	}
}

func TestSessionOutputFollowReconnectsUntilTerminalSession(t *testing.T) {
	oldReconnectDelay := runEventReconnectDelay
	runEventReconnectDelay = time.Millisecond
	t.Cleanup(func() {
		runEventReconnectDelay = oldReconnectDelay
	})
	streamCalls := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodGet && r.URL.Path == "/api/sessions/session-1/channels/report/outputs/stream":
			streamCalls++
			w.Header().Set("content-type", "text/event-stream")
			if streamCalls == 1 {
				if got := r.URL.Query().Get("after_sequence"); got != "" {
					t.Fatalf("first cursor = %q", got)
				}
				writeSessionOutputRecord(t, w, 1, `"first"`)
				return
			}
			if got := r.URL.Query().Get("after_sequence"); got != "1" {
				t.Fatalf("second cursor = %q", got)
			}
			writeSessionOutputRecord(t, w, 2, `"second"`)
		case r.Method == http.MethodGet && r.URL.Path == "/api/sessions/session-1":
			status := "open"
			if streamCalls >= 2 {
				status = "completed"
			}
			_ = json.NewEncoder(w).Encode(api.TaskSessionResponse{ID: "session-1", Status: status})
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
	cmd.SetArgs([]string{"session", "output", "follow", "session-1", "report"})
	if err := cmd.Execute(); err != nil {
		t.Fatal(err)
	}
	if streamCalls != 2 {
		t.Fatalf("stream calls = %d", streamCalls)
	}
	if got := strings.TrimSpace(out.String()); got != `"first"`+"\n"+`"second"` {
		t.Fatalf("output = %q", out.String())
	}
}

func writeSessionOutputRecord(t *testing.T, w http.ResponseWriter, sequence int64, data string) {
	t.Helper()
	record := api.ChannelRecordResponse{
		ID:          fmt.Sprintf("record-%d", sequence),
		ChannelID:   "channel-1",
		Sequence:    sequence,
		Data:        json.RawMessage(data),
		ContentType: "application/json",
	}
	payload, err := json.Marshal(record)
	if err != nil {
		t.Fatal(err)
	}
	_, _ = fmt.Fprintf(w, "id: %d\nevent: channel_output\ndata: %s\n\n", sequence, payload)
}
