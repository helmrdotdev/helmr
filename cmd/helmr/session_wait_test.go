package main

import (
	"bytes"
	"context"
	"encoding/json"
	"github.com/helmrdotdev/helmr/internal/api"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"sync/atomic"
	"testing"
)

func TestSessionTurnWaitObservesWithoutMutations(t *testing.T) {
	const turn = "019c10d5-a6f7-7af1-8f5f-bb97bcc0dc34"
	for _, tc := range []struct {
		name, fixture, timeout, want string
		httpStatus                   int
	}{
		{"success", "completed-null", "", `"status":"settled"`, 200},
		{"expired", "completed-expired", "2s", `"status":"settled"`, 200},
		{"failure", "failed", "2s", `"status":"failed"`, 200},
		{"timeout", "running", "100ms", `"status":"timeout"`, 200},
		{"forbidden", "", "2s", "forbidden", 403},
		{"unknown", "running", "2s", "unknown Turn status", 200},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var calls atomic.Int32
			var payload api.AgentTurn
			if tc.httpStatus == 200 {
				loadSessionWireFixture(t, tc.fixture, &payload)
			}
			if tc.name == "unknown" {
				payload.Status = "future"
			}
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				calls.Add(1)
				if r.Method != "GET" || r.URL.Path != "/v1/sessions/"+testSessionID+"/turns/"+turn {
					t.Errorf("unexpected mutation/target: %s %s", r.Method, r.URL.Path)
				}
				w.WriteHeader(tc.httpStatus)
				if tc.httpStatus != 200 {
					_, _ = w.Write([]byte(`{"error":{"code":"forbidden","message":"forbidden"}}`))
					return
				}
				_ = json.NewEncoder(w).Encode(payload)
			}))
			defer server.Close()
			t.Setenv(helmrAPIURLEnv, server.URL)
			t.Setenv(helmrAPIKeyEnv, "test-key")
			var out bytes.Buffer
			cmd := newRootCommand()
			cmd.SetOut(&out)
			cmd.SetErr(&bytes.Buffer{})
			args := []string{"session", "turn", "wait", turn, "--session", testSessionID, "--json"}
			if tc.timeout != "" {
				args = append(args, "--timeout", tc.timeout)
			}
			cmd.SetArgs(args)
			err := cmd.Execute()
			if tc.name == "forbidden" || tc.name == "unknown" {
				if err == nil || !strings.Contains(err.Error(), tc.want) {
					t.Fatalf("err=%v", err)
				}
			} else if err != nil || !strings.Contains(out.String(), tc.want) {
				t.Fatalf("out=%s err=%v", out.String(), err)
			}
			if calls.Load() != 1 {
				t.Fatalf("reads=%d", calls.Load())
			}
			if tc.fixture == "completed-null" && (!strings.Contains(out.String(), `"result":null`) || strings.Contains(out.String(), `"payload_expired_at"`)) {
				t.Fatalf("explicit null lost: %s", out.String())
			}
			if tc.fixture == "completed-expired" && (strings.Contains(out.String(), `"result"`) || !strings.Contains(out.String(), `"payload_expired_at"`)) {
				t.Fatalf("expired payload fabricated: %s", out.String())
			}

		})
	}
}

func TestSessionTurnWaitCancellationAndIdentity(t *testing.T) {
	const turn = "019c10d5-a6f7-7af1-8f5f-bb97bcc0dc34"
	for _, mode := range []string{"cancel", "identity"} {
		t.Run(mode, func(t *testing.T) {
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if mode == "cancel" {
					cancel()
					<-r.Context().Done()
					return
				}
				_, _ = w.Write([]byte(`{"id":"` + testSessionID + `","session_id":"` + testSessionID + `","status":"completed"}`))
			}))
			defer server.Close()
			t.Setenv(helmrAPIURLEnv, server.URL)
			t.Setenv(helmrAPIKeyEnv, "test-key")
			cmd := newRootCommand()
			cmd.SetOut(&bytes.Buffer{})
			cmd.SetErr(&bytes.Buffer{})
			cmd.SetArgs([]string{"session", "turn", "wait", turn, "--session", testSessionID, "--timeout", "2s", "--json"})
			if err := cmd.ExecuteContext(ctx); err == nil {
				t.Fatal("invalid observation succeeded")
			}
		})
	}
}

func TestSessionTurnWaitObservesQueuedAndRunningBeforeSettlement(t *testing.T) {
	const turn = "019c10d5-a6f7-7af1-8f5f-bb97bcc0dc34"
	var calls atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != "GET" {
			t.Errorf("unexpected mutation: %s", r.Method)
		}
		status := "completed-object"
		switch calls.Add(1) {
		case 1:
			status = "queued"
		case 2:
			status = "running"
		}
		var payload api.AgentTurn
		loadSessionWireFixture(t, status, &payload)
		_ = json.NewEncoder(w).Encode(payload)
	}))
	defer server.Close()
	t.Setenv(helmrAPIURLEnv, server.URL)
	t.Setenv(helmrAPIKeyEnv, "test-key")
	var out bytes.Buffer
	cmd := newRootCommand()
	cmd.SetOut(&out)
	cmd.SetErr(&bytes.Buffer{})
	cmd.SetArgs([]string{"session", "turn", "wait", turn, "--session", testSessionID, "--timeout", "3s", "--json"})
	if err := cmd.Execute(); err != nil {
		t.Fatal(err)
	}
	if calls.Load() != 3 || !strings.Contains(out.String(), `"status":"settled"`) || !strings.Contains(out.String(), `"done":true`) {
		t.Fatalf("calls=%d out=%s", calls.Load(), out.String())
	}
}

func loadSessionWireFixture(t *testing.T, name string, target any) {
	t.Helper()
	raw, err := os.ReadFile("../../tests/fixtures/session-wire.json")
	if err != nil {
		t.Fatal(err)
	}
	var fixtures map[string]json.RawMessage
	if err := json.Unmarshal(raw, &fixtures); err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(fixtures[name], target); err != nil {
		t.Fatal(err)
	}
}
