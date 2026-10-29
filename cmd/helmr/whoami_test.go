package main

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestWhoamiWithAPIKeyDoesNotCallMe(t *testing.T) {
	called := false
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		called = true
	}))
	defer server.Close()
	t.Setenv(helmrAPIURLEnv, server.URL)
	t.Setenv(helmrAPIKeyEnv, "test-key")

	var out bytes.Buffer
	cmd := newRootCommand()
	cmd.SetOut(&out)
	cmd.SetErr(&bytes.Buffer{})
	cmd.SetArgs([]string{"whoami"})
	if err := cmd.Execute(); err != nil {
		t.Fatal(err)
	}
	if called {
		t.Fatal("server was called")
	}
	if got := strings.TrimSpace(out.String()); got != "api_key "+server.URL {
		t.Fatalf("output = %q", out.String())
	}
}

func TestWhoamiSavedLoginShowsAccountAndOrganization(t *testing.T) {
	for _, asJSON := range []bool{false, true} {
		t.Run(map[bool]string{false: "text", true: "json"}[asJSON], func(t *testing.T) {
			state := installTestCLIConfig(t)
			t.Setenv(helmrAPIKeyEnv, "")
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.URL.Path != "/api/organization" || r.Header.Get("Authorization") != "Bearer saved" {
					t.Errorf("unexpected identity request: %s", r.URL.Path)
					w.WriteHeader(401)
					return
				}
				w.Header().Set("Content-Type", "application/json")
				w.Write([]byte(`{"id":"org-1","name":"Example Corp","current_member":{"id":"member-1","user_id":"user-1","display_name":"Alice"}}`))
			}))
			defer server.Close()
			t.Setenv(helmrAPIURLEnv, server.URL)
			if err := state.SaveLogin(server.URL, "saved"); err != nil {
				t.Fatal(err)
			}
			var out bytes.Buffer
			cmd := newRootCommand()
			cmd.SetOut(&out)
			cmd.SetErr(&bytes.Buffer{})
			args := []string{"whoami"}
			if asJSON {
				args = append(args, "--json")
			}
			cmd.SetArgs(args)
			if err := cmd.Execute(); err != nil {
				t.Fatal(err)
			}
			if asJSON {
				var response whoamiResponse
				if err := json.Unmarshal(out.Bytes(), &response); err != nil {
					t.Fatal(err)
				}
				if response.Organization == nil || response.Organization.CurrentMember.UserID != "user-1" || response.Organization.ID != "org-1" {
					t.Fatalf("unexpected JSON: %s", out.String())
				}
			} else {
				for _, want := range []string{"login " + server.URL, "Account: Alice (user-1)", "Organization: Example Corp (org-1)"} {
					if !strings.Contains(out.String(), want) {
						t.Fatalf("missing %q: %s", want, out.String())
					}
				}
			}
		})
	}
}
