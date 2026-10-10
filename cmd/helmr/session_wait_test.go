package main

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
)

func TestSessionTurnWaitObservesWithoutMutations(t *testing.T) {
	const turn = "019c10d5-a6f7-7af1-8f5f-bb97bcc0dc34"
	for _, tc := range []struct {
		name, status, timeout, want string
		httpStatus                  int
	}{
		{"success", "completed", "2s", `"status":"settled"`, 200},
		{"failure", "failed", "2s", `"status":"failed"`, 200},
		{"timeout", "finalizing", "100ms", `"status":"timeout"`, 200},
		{"forbidden", "", "2s", "forbidden", 403},
		{"unknown", "future", "2s", "unknown Turn status", 200},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var calls atomic.Int32
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
				_ = json.NewEncoder(w).Encode(map[string]any{"id": turn, "session_id": testSessionID, "status": tc.status, "result": nil, "payload_expired_at": "2026-10-01T00:00:00Z"})
			}))
			defer server.Close()
			t.Setenv(helmrAPIURLEnv, server.URL)
			t.Setenv(helmrAPIKeyEnv, "test-key")
			var out bytes.Buffer
			cmd := newRootCommand()
			cmd.SetOut(&out)
			cmd.SetErr(&bytes.Buffer{})
			cmd.SetArgs([]string{"session", "turn", "wait", testSessionID, turn, "--timeout", tc.timeout, "--json"})
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
			if tc.status == "completed" && (!strings.Contains(out.String(), `"result":null`) || !strings.Contains(out.String(), `"payload_expired_at"`)) {
				t.Fatalf("payload metadata lost: %s", out.String())
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
			cmd.SetArgs([]string{"session", "turn", "wait", testSessionID, turn, "--timeout", "2s", "--json"})
			if err := cmd.ExecuteContext(ctx); err == nil {
				t.Fatal("invalid observation succeeded")
			}
		})
	}
}

func TestSessionTurnWaitIncludesFinalizingBeforeSettlement(t *testing.T) {
	const turn = "019c10d5-a6f7-7af1-8f5f-bb97bcc0dc34"
	var calls atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != "GET" {
			t.Errorf("unexpected mutation: %s", r.Method)
		}
		status := "completed"
		switch calls.Add(1) {
		case 1:
			status = "queued"
		case 2:
			status = "finalizing"
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"id": turn, "session_id": testSessionID, "status": status, "result": map[string]any{"done": true}})
	}))
	defer server.Close()
	t.Setenv(helmrAPIURLEnv, server.URL)
	t.Setenv(helmrAPIKeyEnv, "test-key")
	var out bytes.Buffer
	cmd := newRootCommand()
	cmd.SetOut(&out)
	cmd.SetErr(&bytes.Buffer{})
	cmd.SetArgs([]string{"session", "turn", "wait", testSessionID, turn, "--timeout", "3s", "--json"})
	if err := cmd.Execute(); err != nil {
		t.Fatal(err)
	}
	if calls.Load() != 3 || !strings.Contains(out.String(), `"status":"settled"`) || !strings.Contains(out.String(), `"done":true`) {
		t.Fatalf("calls=%d out=%s", calls.Load(), out.String())
	}
}
