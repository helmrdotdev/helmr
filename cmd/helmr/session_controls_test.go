package main

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestSessionControlsPreserveExactTargetsAndReceipts(t *testing.T) {
	const turnID = "019c10d5-a6f7-7af1-8f5f-bb97bcc0dc34"
	const holdID = "019c10d5-a6f7-7af1-8f5f-bb97bcc0dc35"
	for _, tc := range []struct {
		name                     string
		args                     []string
		method, suffix, response string
		body                     map[string]any
		output                   []string
	}{
		{"enqueue", []string{"enqueue", testSessionID, "--text", "Inspect issue 42", "--idempotency-key", "enqueue-1"}, "POST", "/enqueue", `{"session_id":"` + testSessionID + `","turn_id":"` + turnID + `","sequence":2}`, map[string]any{"input": []any{map[string]any{"type": "text", "text": "Inspect issue 42"}}, "idempotency_key": "enqueue-1"}, []string{"sequence: 2", "turn_id: " + turnID}},
		{"message", []string{"turn", "send", testSessionID, turnID, "--text", "Please continue", "--idempotency-key", "message-1"}, "POST", "/turns/" + turnID + "/messages", `{"id":"op-2","turn_id":"` + turnID + `","message_id":"message-1","status":"accepted"}`, map[string]any{"data": []any{map[string]any{"type": "text", "text": "Please continue"}}, "idempotency_key": "message-1"}, []string{"status: accepted", "message_id: message-1"}},
		{"resume", []string{"resume", testSessionID, "--hold", holdID, "--idempotency-key", "resume-1"}, "POST", "/resume", `{"id":"op-4","session_id":"` + testSessionID + `","hold_id":"` + holdID + `","status":"resumed"}`, map[string]any{"hold_id": holdID, "idempotency_key": "resume-1"}, []string{"status: resumed", "hold_id: " + holdID}},
		{"outcome", []string{"turn", "get", testSessionID, turnID}, "GET", "/turns/" + turnID, `{"id":"` + turnID + `","session_id":"` + testSessionID + `","status":"failed","sequence":2,"error":{"code":"failed","message":"tests failed"}}`, nil, []string{"status: failed", "sequence: 2", "error_code: failed", "error_message: tests failed"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			calls := 0
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				calls++
				if r.Method != tc.method || r.URL.Path != "/v1/sessions/"+testSessionID+tc.suffix {
					t.Errorf("unexpected target: %s %s", r.Method, r.URL.Path)
				}
				if tc.body != nil {
					var body map[string]any
					if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
						t.Error(err)
					}
					got, _ := json.Marshal(body)
					want, _ := json.Marshal(tc.body)
					if string(got) != string(want) {
						t.Errorf("body %s, want %s", got, want)
					}
				}
				_, _ = w.Write([]byte(tc.response))
			}))
			defer server.Close()
			t.Setenv(helmrAPIURLEnv, server.URL)
			t.Setenv(helmrAPIKeyEnv, "test-key")
			var out bytes.Buffer
			cmd := newRootCommand()
			cmd.SetOut(&out)
			cmd.SetErr(&bytes.Buffer{})
			cmd.SetArgs(append([]string{"session"}, tc.args...))
			if err := cmd.Execute(); err != nil {
				t.Fatal(err)
			}
			if calls != 1 {
				t.Fatalf("requests = %d", calls)
			}
			for _, want := range tc.output {
				if !strings.Contains(out.String(), want) {
					t.Errorf("output %q missing %q", out.String(), want)
				}
			}
		})
	}
}

func TestSessionResumeRequiresExactHold(t *testing.T) {
	calls := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { calls++; t.Error("unexpected request") }))
	defer server.Close()
	t.Setenv(helmrAPIURLEnv, server.URL)
	t.Setenv(helmrAPIKeyEnv, "test-key")
	for _, args := range [][]string{{"session", "resume", testSessionID}, {"session", "resume", testSessionID, "--hold", ""}} {
		cmd := newRootCommand()
		cmd.SetOut(&bytes.Buffer{})
		cmd.SetErr(&bytes.Buffer{})
		cmd.SetArgs(args)
		if err := cmd.Execute(); err == nil {
			t.Fatal("missing hold accepted")
		}
	}
	if calls != 0 {
		t.Fatalf("requests = %d", calls)
	}
}

func TestSessionGetDisplaysInheritedHoldOwner(t *testing.T) {
	const parent = "019c10d5-a6f7-7af1-8f5f-bb97bcc0dc40"
	const hold = "019c10d5-a6f7-7af1-8f5f-bb97bcc0dc35"
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != "GET" || r.URL.Path != "/v1/sessions/"+testSessionID {
			t.Errorf("unexpected request: %s %s", r.Method, r.URL.Path)
		}
		_, _ = w.Write([]byte(`{"id":"` + testSessionID + `","status":"open","holds":[{"id":"` + hold + `","session_id":"` + parent + `","scope":"subtree","reason":""}]}`))
	}))
	defer server.Close()
	t.Setenv(helmrAPIURLEnv, server.URL)
	t.Setenv(helmrAPIKeyEnv, "test-key")
	var out bytes.Buffer
	cmd := newRootCommand()
	cmd.SetOut(&out)
	cmd.SetErr(&bytes.Buffer{})
	cmd.SetArgs([]string{"session", "get", testSessionID})
	if err := cmd.Execute(); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out.String(), "hold_id: "+hold+"\nhold_session_id: "+parent+"\nhold_scope: subtree") || strings.Contains(out.String(), "hold_reason:") {
		t.Fatalf("inherited hold owner missing: %s", out.String())
	}
}
