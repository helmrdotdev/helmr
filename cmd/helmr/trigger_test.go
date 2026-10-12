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

func TestTriggerListAndGet(t *testing.T) {
	const triggerID = "019c10d5-a6f7-7af1-8f5f-bb97bcc0dc36"
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/v1/triggers":
			if r.URL.Query().Get("cursor") != "cursor-current" ||
				r.URL.Query().Get("limit") != "25" {
				t.Fatalf("query = %q", r.URL.RawQuery)
			}
			_ = json.NewEncoder(w).Encode(api.ListTriggersResponse{
				Triggers: []api.TriggerResponse{{
					ID: triggerID, Agent: "nightly", TriggerKey: "daily",
				}},
				NextCursor: "cursor-next",
			})
		case "/v1/triggers/" + triggerID:
			_ = json.NewEncoder(w).Encode(api.TriggerResponse{
				ID: triggerID, Agent: "nightly", TriggerKey: "daily",
			})
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
	cmd.SetArgs([]string{"trigger", "list", "--cursor", "cursor-current", "--limit", "25"})
	if err := cmd.Execute(); err != nil {
		t.Fatal(err)
	}
	if out.String() != triggerID+"\tnightly\tdaily\nnext_cursor: cursor-next\n" {
		t.Fatalf("list output = %q", out.String())
	}

	out.Reset()
	cmd = newRootCommand()
	cmd.SetOut(&out)
	cmd.SetErr(&bytes.Buffer{})
	cmd.SetArgs([]string{"trigger", "get", triggerID})
	if err := cmd.Execute(); err != nil {
		t.Fatal(err)
	}
	for _, expected := range []string{
		"trigger_id: " + triggerID,
		"agent: nightly",
		"trigger: daily",
	} {
		if !strings.Contains(out.String(), expected) {
			t.Fatalf("get output = %q, missing %q", out.String(), expected)
		}
	}
}

func TestTriggerListRejectsExplicitZeroLimit(t *testing.T) {
	t.Setenv(helmrAPIURLEnv, "http://127.0.0.1")
	t.Setenv(helmrAPIKeyEnv, "test-key")
	cmd := newRootCommand()
	cmd.SetOut(&bytes.Buffer{})
	cmd.SetErr(&bytes.Buffer{})
	cmd.SetArgs([]string{"trigger", "list", "--limit", "0"})
	err := cmd.Execute()
	if err == nil || !strings.Contains(err.Error(), "--limit must be in [1,100]") {
		t.Fatalf("error = %v", err)
	}
}

func TestAgentUUIDSelectorIsNotPublic(t *testing.T) {
	for _, resource := range []string{"session", "trigger"} {
		cmd := newRootCommand()
		cmd.SetOut(&bytes.Buffer{})
		cmd.SetErr(&bytes.Buffer{})
		cmd.SetArgs([]string{resource, "list", "--agent-id", "019c10d5-a6f7-7af1-8f5f-bb97bcc0dc37"})
		if err := cmd.Execute(); err == nil || !strings.Contains(err.Error(), "unknown flag: --agent-id") {
			t.Fatalf("%s: %v", resource, err)
		}
	}
}
