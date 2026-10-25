package main

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestAgentChangesUseHumanLoginEvenWithAPIKey(t *testing.T) {
	for _, action := range []string{"delete", "restore"} {
		t.Run(action, func(t *testing.T) {
			state := installTestCLIConfig(t)
			t.Setenv(helmrAPIKeyEnv, "machine-key")
			calls := 0
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.Header.Get("Authorization") != "Bearer human-session" {
					t.Errorf("wrong identity %s", r.Header.Get("Authorization"))
				}
				var result any
				switch r.URL.Path {
				case "/api/projects/demo":
					result = map[string]any{"project": "demo", "environments": []any{map[string]any{"environment": "test"}}}
				case "/api/projects/demo/environments/test/agents/worker/status":
					result = map[string]any{"agent": "worker", "revision": 7}
				default:
					wantPath := "/api/projects/demo/environments/test/agents/worker"
					wantMethod := "DELETE"
					if action == "restore" {
						wantPath += "/restore"
						wantMethod = "POST"
					}
					if r.URL.Path != wantPath || r.Method != wantMethod || r.Header.Get("If-Match") != `"7"` {
						t.Errorf("unexpected mutation %s %s %s", r.Method, r.URL.Path, r.Header.Get("If-Match"))
					}
					calls++
					result = map[string]any{"agent": "worker", "revision": 8, "changed": true, "can_apply": true}
				}
				_ = json.NewEncoder(w).Encode(result)
			}))
			defer server.Close()
			if err := state.SaveLogin(server.URL, "human-session"); err != nil {
				t.Fatal(err)
			}
			cmd := newRootCommand()
			cmd.SetOut(&bytes.Buffer{})
			cmd.SetErr(&bytes.Buffer{})
			cmd.SetArgs([]string{"agent", action, "worker", "--project", "demo", "--env", "test", "--json"})
			if err := cmd.Execute(); err != nil {
				t.Fatal(err)
			}
			if calls != 1 {
				t.Fatalf("mutations %d", calls)
			}
		})
	}
}
