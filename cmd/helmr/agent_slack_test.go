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

func TestAgentSlackStart(t *testing.T) {
	const channel = "C0123456789"
	var starts int
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == "POST" && r.URL.Path == "/v1/agents/engineer/start":
			starts++
			var req api.StartAgentRequest
			if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
				t.Fatal(err)
			}
			if string(req.Input) != `[{"text":"inspect","type":"text"}]` || string(req.Slack) != `{"channel_id":"`+channel+`"}` {
				t.Fatalf("start bytes: %+v", req)
			}
			_ = json.NewEncoder(w).Encode(api.StartAgentResponse{TurnAdmission: api.TurnAdmission{SessionID: testSessionID, TurnID: testComputerID, Sequence: 1}, Created: true})
		default:
			t.Errorf("unexpected %s %s", r.Method, r.URL)
			w.WriteHeader(500)
		}
	}))
	defer server.Close()
	t.Setenv(helmrAPIURLEnv, server.URL)
	t.Setenv(helmrAPIKeyEnv, "test-key")
	run := func(args ...string) (string, error) {
		var out bytes.Buffer
		cmd := newRootCommand()
		cmd.SetOut(&out)
		cmd.SetErr(&bytes.Buffer{})
		cmd.SetArgs(args)
		err := cmd.Execute()
		return out.String(), err
	}
	if out, err := run("agent", "start", "engineer", "--input-json", `[{"text":"inspect","type":"text"}]`, "--slack-channel", channel, "--json"); err != nil || !strings.Contains(out, testSessionID) {
		t.Fatalf("start: %s %v", out, err)
	}
	for _, flags := range [][]string{{"--slack-channel", ""}, {"--slack-channel", "bad"}, {"--slack-channel", channel, "--slack-channel", channel}} {
		args := append([]string{"agent", "start", "engineer", "--input-json", "[]"}, flags...)
		if _, err := run(args...); err == nil {
			t.Fatalf("accepted malformed route %v", flags)
		}
	}
	if starts != 1 {
		t.Fatalf("malformed route sent: %d", starts)
	}
}
