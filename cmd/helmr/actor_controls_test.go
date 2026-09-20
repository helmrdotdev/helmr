package main

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestActorControlsPreserveExactTargetsAndReceipts(t *testing.T) {
	const turnID = "019c10d5-a6f7-7af1-8f5f-bb97bcc0dc34"
	const holdID = "019c10d5-a6f7-7af1-8f5f-bb97bcc0dc35"
	for _, tc := range []struct {
		name                     string
		args                     []string
		method, suffix, response string
		body                     map[string]any
		output                   []string
	}{
		{"enqueue", []string{"enqueue", testSessionID, "--data-json", `{"issue":42}`, "--idempotency-key", "enqueue-1"}, "POST", "/enqueue", `{"id":"op-1","kind":"enqueued","turn_id":"` + turnID + `"}`, map[string]any{"data": map[string]any{"issue": float64(42)}, "idempotency_key": "enqueue-1"}, []string{"kind: enqueued", "turn_id: " + turnID}},
		{"message", []string{"turn", "send", testSessionID, turnID, "--data-json", `{"type":"answer","value":null}`, "--idempotency-key", "message-1"}, "POST", "/turns/" + turnID + "/messages", `{"id":"op-2","turn_id":"` + turnID + `","message_id":"message-1","status":"accepted"}`, map[string]any{"data": map[string]any{"type": "answer", "value": nil}, "idempotency_key": "message-1"}, []string{"status: accepted", "message_id: message-1"}},
		{"interrupt", []string{"turn", "interrupt", testSessionID, turnID, "--idempotency-key", "stop-1"}, "POST", "/turns/" + turnID + "/interrupt", `{"id":"op-3","session_id":"` + testSessionID + `","turn_id":"` + turnID + `","hold_id":"` + holdID + `","status":"stopping"}`, map[string]any{"idempotency_key": "stop-1"}, []string{"status: stopping", "hold_id: " + holdID}},
		{"resume", []string{"resume", testSessionID, "--hold", holdID, "--idempotency-key", "resume-1"}, "POST", "/resume", `{"id":"op-4","session_id":"` + testSessionID + `","hold_id":"` + holdID + `","status":"resumed"}`, map[string]any{"hold_id": holdID, "idempotency_key": "resume-1"}, []string{"status: resumed", "hold_id: " + holdID}},
		{"recover turn", []string{"recover", testSessionID, holdID, "--turn", turnID, "--workspace-version", testWorkspaceID, "--reconciliation-ref", "incident:42", "--disposition", "interrupted", "--idempotency-key", "recover-1"}, "POST", "/recover", `{"id":"op-5","session_id":"` + testSessionID + `","turn_id":"` + turnID + `","hold_id":"` + holdID + `","status":"recovered"}`, map[string]any{"hold_id": holdID, "turn_id": turnID, "workspace_version_id": testWorkspaceID, "reconciliation_ref": "incident:42", "disposition": "interrupted", "idempotency_key": "recover-1"}, []string{"turn_id: " + turnID, "status: recovered"}},
		{"recover outside turn", []string{"recover", testSessionID, holdID, "--outside-turn", "--workspace-version", testWorkspaceID, "--reconciliation-ref", "incident:43", "--idempotency-key", "recover-2", "--json"}, "POST", "/recover", `{"id":"op-6","session_id":"` + testSessionID + `","turn_id":null,"hold_id":"` + holdID + `","status":"recovered"}`, map[string]any{"hold_id": holdID, "turn_id": nil, "workspace_version_id": testWorkspaceID, "reconciliation_ref": "incident:43", "idempotency_key": "recover-2"}, []string{`"turn_id":null`}},
		{"outcome", []string{"turn", "get", testSessionID, turnID}, "GET", "/turns/" + turnID, `{"id":"` + turnID + `","session_id":"` + testSessionID + `","status":"failed","accepts_messages":false,"interrupt_requested":false,"error":{"message":"tests failed"},"terminal_event_id":"event-1"}`, nil, []string{"status: failed", "accepts_messages: false", `error: {"message":"tests failed"}`, "terminal_event_id: event-1"}},
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
			cmd.SetArgs(append([]string{"actor"}, tc.args...))
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

func TestActorRecoveryRequiresExplicitReconciliation(t *testing.T) {
	const holdID = "019c10d5-a6f7-7af1-8f5f-bb97bcc0dc35"
	for _, flags := range [][]string{
		{},
		{"--outside-turn"},
		{"--outside-turn", "--workspace-version", testWorkspaceID, "--reconciliation-ref", "incident:1", "--disposition", "failed"},
		{"--turn", testSessionID, "--workspace-version", testWorkspaceID, "--reconciliation-ref", "incident:1", "--disposition", "completed"},
		{"--turn", testSessionID, "--workspace-version", testWorkspaceID, "--reconciliation-ref", "incident:1"},
	} {
		cmd := newRootCommand()
		cmd.SetOut(&bytes.Buffer{})
		cmd.SetErr(&bytes.Buffer{})
		cmd.SetArgs(append([]string{"actor", "recover", testSessionID, holdID}, flags...))
		if err := cmd.Execute(); err == nil {
			t.Fatalf("accepted incomplete reconciliation: %v", flags)
		}
	}
}

func TestActorResumePinsObservedHoldWithoutRetargeting(t *testing.T) {
	const holdID = "019c10d5-a6f7-7af1-8f5f-bb97bcc0dc35"
	for _, outcome := range []string{"resumed", "stale", "recovery", "no-hold"} {
		t.Run(outcome, func(t *testing.T) {
			reads, writes := 0, 0
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				switch r.Method + " " + r.URL.Path {
				case "GET /v1/sessions/" + testSessionID:
					reads++
					dispatch := map[string]any{"state": "held", "hold_id": holdID, "reason": "interrupted"}
					if outcome == "recovery" {
						dispatch["reason"] = "recovery_required"
					}
					if outcome == "no-hold" {
						dispatch = map[string]any{"state": "ready"}
					}
					_ = json.NewEncoder(w).Encode(map[string]any{"id": testSessionID, "status": "open", "dispatch": dispatch})
				case "POST /v1/sessions/" + testSessionID + "/resume":
					writes++
					var request map[string]any
					if err := json.NewDecoder(r.Body).Decode(&request); err != nil {
						t.Error(err)
					}
					if request["hold_id"] != holdID {
						t.Errorf("wrong hold: %v", request)
					}
					if outcome == "stale" {
						w.WriteHeader(http.StatusConflict)
						_, _ = w.Write([]byte(`{"error":{"code":"conflict","message":"hold changed"}}`))
						return
					}
					_, _ = w.Write([]byte(`{"id":"op","session_id":"` + testSessionID + `","hold_id":"` + holdID + `","status":"resumed"}`))
				default:
					t.Errorf("unexpected request %s %s", r.Method, r.URL.Path)
					w.WriteHeader(http.StatusNotFound)
				}
			}))
			defer server.Close()
			t.Setenv(helmrAPIURLEnv, server.URL)
			t.Setenv(helmrAPIKeyEnv, "test-key")
			cmd := newRootCommand()
			cmd.SetOut(&bytes.Buffer{})
			cmd.SetErr(&bytes.Buffer{})
			cmd.SetArgs([]string{"actor", "resume", testSessionID})
			err := cmd.Execute()
			if (err == nil) != (outcome == "resumed") {
				t.Fatalf("outcome %s, error %v", outcome, err)
			}
			wantWrites := 1
			if outcome == "recovery" || outcome == "no-hold" {
				wantWrites = 0
			}
			if reads != 1 || writes != wantWrites {
				t.Fatalf("reads=%d writes=%d; expected 1/%d", reads, writes, wantWrites)
			}
		})
	}
}
