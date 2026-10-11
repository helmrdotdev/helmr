package main

import (
	"bytes"
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func TestCommandObservation(t *testing.T) {
	const id = "019c10d5-a6f7-7af1-8f5f-bb97bcc0dc36"
	for _, tc := range []struct {
		name        string
		args        []string
		want        string
		unavailable bool
	}{
		{"get", []string{"get", id, "--json"}, `"status":"exited"`, false},
		{"wait", []string{"wait", id, "--timeout", "1s", "--json"}, `"status":"settled"`, false},
		{"timeout", []string{"wait", id, "--timeout", "10ms", "--json"}, `"status":"timeout"`, false},
		{"page", []string{"logs", id, "--json", "--limit", "1", "--cursor", "previous"}, `"output_state":"closed"`, false},
		{"follow", []string{"logs", id, "--follow"}, "hi", false},
		{"unavailable", []string{"logs", id, "--follow"}, "hi", true},
		{"cancel", []string{"cancel", id, "--json"}, `"status":"accepted"`, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			reads := 0
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				reads++
				if r.Header.Get("Authorization") != "Bearer test-key" {
					t.Error("missing authority")
				}
				w.Header().Set("Content-Type", "application/json")
				if tc.name == "timeout" {
					<-r.Context().Done()
					return
				}
				switch {
				case strings.HasSuffix(r.URL.Path, "/logs"):
					if r.Method != "GET" {
						t.Error("logs mutated state")
					}
					if tc.name == "page" && (r.URL.Query().Get("limit") != "1" || r.URL.Query().Get("cursor") != "previous") {
						t.Error("lost query")
					}
					state := "closed"
					if tc.unavailable {
						state = "unavailable"
					}
					if reads == 1 {
						fmt.Fprintf(w, `{"output_state":%q,"logs":[{"kind":"output","stream":"stdout","cursor":"next","content_base64":"aGk="}],"next_cursor":"next"}`, state)
					} else {
						if r.URL.Query().Get("cursor") != "next" {
							t.Error("lost continuation")
						}
						fmt.Fprintf(w, `{"output_state":%q,"logs":[]}`, state)
					}
				case strings.HasSuffix(r.URL.Path, "/cancel"):
					if r.Method != "POST" {
						t.Error("cancel method")
					}
					fmt.Fprintf(w, `{"id":%q,"target_id":%q,"status":"accepted"}`, id, id)
				default:
					if r.Method != "GET" || r.URL.Path != "/v1/commands/"+id {
						t.Errorf("unexpected %s %s", r.Method, r.URL.Path)
					}
					fmt.Fprintf(w, `{"id":%q,"status":"exited","outcome":{"command_id":%q,"kind":"exited","exit_code":7,"terminal_at":"2026-10-08T00:00:00Z"}}`, id, id)
				}
			}))
			defer server.Close()
			t.Setenv(helmrAPIURLEnv, server.URL)
			t.Setenv(helmrAPIKeyEnv, "test-key")
			cmd := newRootCommand()
			var out, stderr bytes.Buffer
			cmd.SetOut(&out)
			cmd.SetErr(&stderr)
			cmd.SetArgs(append([]string{"command"}, tc.args...))
			ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
			defer cancel()
			err := cmd.ExecuteContext(ctx)
			if tc.unavailable {
				if err == nil || !strings.Contains(err.Error(), "unavailable") {
					t.Fatalf("error=%v", err)
				}
			} else if err != nil {
				t.Fatal(err)
			}
			if !strings.Contains(out.String(), tc.want) {
				t.Fatalf("output=%s", out.String())
			}
		})
	}
}

func TestDurationGrammar(t *testing.T) {
	for _, value := range []string{"1ms", "30s", "2m", "1h", "1d"} {
		if _, err := parseDuration(value); err != nil {
			t.Fatal(value, err)
		}
	}
	for _, value := range []string{"0s", "01s", "-1s", "1.5s", "1h30m", "1", " 1s", "999999999999999999999d"} {
		if _, err := parseDuration(value); err == nil {
			t.Fatal("accepted", value)
		}
	}
}

func TestTurnParentsAreRequiredFlags(t *testing.T) {
	for _, args := range [][]string{
		{"turn", "get", testSessionID}, {"turn", "list"}, {"turn", "wait", testSessionID, "--timeout", "1s"},
		{"turn", "ask", "get", testSessionID, "--session", testSessionID},
		{"turn", "ask", "list", "--turn", testSessionID},
		{"turn", "get", testSessionID, testSessionID},
	} {
		cmd := newRootCommand()
		cmd.SetOut(&bytes.Buffer{})
		cmd.SetErr(&bytes.Buffer{})
		cmd.SetArgs(append([]string{"session"}, args...))
		if err := cmd.Execute(); err == nil {
			t.Fatalf("accepted missing or positional parent: %v", args)
		}
	}
}
