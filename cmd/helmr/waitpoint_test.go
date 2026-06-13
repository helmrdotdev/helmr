package main

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/helmrdotdev/helmr/internal/api"
)

func TestWaitpointRespondCommand(t *testing.T) {
	var request api.RespondWaitpointRequest
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost || r.URL.Path != "/api/waitpoints/wait-1/respond" {
			t.Fatalf("%s %s", r.Method, r.URL.Path)
		}
		if got := r.Header.Get("authorization"); got != "Bearer test-key" {
			t.Fatalf("auth = %s", got)
		}
		if err := json.NewDecoder(r.Body).Decode(&request); err != nil {
			t.Fatal(err)
		}
		w.WriteHeader(http.StatusNoContent)
	}))
	defer server.Close()
	t.Setenv(helmrAPIURLEnv, server.URL)
	t.Setenv(helmrAPIKeyEnv, "test-key")

	cmd := newRootCommand()
	cmd.SetOut(&bytes.Buffer{})
	cmd.SetErr(&bytes.Buffer{})
	cmd.SetArgs([]string{"waitpoint", "respond", "wait-1", "--value", `{"action":"approve"}`})
	if err := cmd.Execute(); err != nil {
		t.Fatal(err)
	}
	if string(request.Value) != `{"action":"approve"}` {
		t.Fatalf("request = %+v", request)
	}
}

func TestWaitpointRespondCommandUsesSessionScopedRoute(t *testing.T) {
	const projectID = "00000000-0000-0000-0000-000000000101"
	const environmentID = "00000000-0000-0000-0000-000000000202"
	state, _ := installTestCLIConfig(t)
	var request api.RespondWaitpointRequest
	methods := []string{}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		methods = append(methods, r.Method+" "+r.URL.Path)
		if got := r.Header.Get("authorization"); got != "Bearer session-test" {
			t.Fatalf("auth = %s", got)
		}
		switch {
		case r.Method == http.MethodGet && r.URL.Path == "/api/projects":
			_ = json.NewEncoder(w).Encode(api.ListProjectsResponse{Projects: []api.ProjectSummary{{
				ID:   projectID,
				Slug: "prod",
				Environments: []api.EnvironmentSummary{{
					ID:        environmentID,
					ProjectID: projectID,
					Slug:      "qa",
				}},
			}}})
		case r.Method == http.MethodPost && r.URL.Path == "/api/projects/"+projectID+"/environments/"+environmentID+"/waitpoints/wait-1/respond":
			if err := json.NewDecoder(r.Body).Decode(&request); err != nil {
				t.Fatal(err)
			}
			w.WriteHeader(http.StatusAccepted)
		default:
			t.Fatalf("%s %s", r.Method, r.URL.Path)
		}
	}))
	defer server.Close()
	if err := state.SaveLogin(server.URL, "session-test"); err != nil {
		t.Fatal(err)
	}

	cmd := newRootCommand()
	cmd.SetOut(&bytes.Buffer{})
	cmd.SetErr(&bytes.Buffer{})
	cmd.SetArgs([]string{"waitpoint", "respond", "wait-1", "--project", "prod", "--env", "qa", "--value", `{"action":"approve"}`})
	if err := cmd.Execute(); err != nil {
		t.Fatal(err)
	}
	wantMethods := "GET /api/projects,POST /api/projects/" + projectID + "/environments/" + environmentID + "/waitpoints/wait-1/respond"
	if got := strings.Join(methods, ","); got != wantMethods {
		t.Fatalf("methods = %s", got)
	}
	if string(request.Value) != `{"action":"approve"}` {
		t.Fatalf("request = %+v", request)
	}
}

func TestWaitpointRespondCommandRequiresScopeWithSessionAuth(t *testing.T) {
	state, _ := installTestCLIConfig(t)
	if err := state.SaveLogin("https://control.example.test", "session-test"); err != nil {
		t.Fatal(err)
	}

	cmd := newRootCommand()
	cmd.SetOut(&bytes.Buffer{})
	cmd.SetErr(&bytes.Buffer{})
	cmd.SetArgs([]string{"waitpoint", "respond", "wait-1", "--value", `{"action":"approve"}`})
	err := cmd.Execute()
	if err == nil || !strings.Contains(err.Error(), "--project and --env are required with helmr login") {
		t.Fatalf("err = %v", err)
	}
}

func TestWaitpointRespondCommandReadsValueFile(t *testing.T) {
	var request api.RespondWaitpointRequest
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost || r.URL.Path != "/api/waitpoints/wait-1/respond" {
			t.Fatalf("%s %s", r.Method, r.URL.Path)
		}
		if err := json.NewDecoder(r.Body).Decode(&request); err != nil {
			t.Fatal(err)
		}
		w.WriteHeader(http.StatusNoContent)
	}))
	defer server.Close()
	t.Setenv(helmrAPIURLEnv, server.URL)
	t.Setenv(helmrAPIKeyEnv, "test-key")
	valuePath := filepath.Join(t.TempDir(), "value.json")
	if err := os.WriteFile(valuePath, []byte(`{"text":"Use the smaller rollout."}`), 0o600); err != nil {
		t.Fatal(err)
	}

	cmd := newRootCommand()
	cmd.SetOut(&bytes.Buffer{})
	cmd.SetErr(&bytes.Buffer{})
	cmd.SetArgs([]string{"waitpoint", "respond", "wait-1", "--value-file", valuePath})
	if err := cmd.Execute(); err != nil {
		t.Fatal(err)
	}
	if string(request.Value) != `{"text":"Use the smaller rollout."}` {
		t.Fatalf("request = %+v", request)
	}
}

func TestWaitpointRespondCommandAllowsEmptyValue(t *testing.T) {
	called := false
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		called = true
		w.WriteHeader(http.StatusNoContent)
	}))
	defer server.Close()
	t.Setenv(helmrAPIURLEnv, server.URL)
	t.Setenv(helmrAPIKeyEnv, "test-key")

	cmd := newRootCommand()
	cmd.SetOut(&bytes.Buffer{})
	cmd.SetErr(&bytes.Buffer{})
	cmd.SetArgs([]string{"waitpoint", "respond", "wait-1"})
	if err := cmd.Execute(); err != nil {
		t.Fatalf("err = %v", err)
	}
	if !called {
		t.Fatal("server was not called")
	}
}

func TestWaitpointListCommandPrintsOpenWaitpoints(t *testing.T) {
	state, _ := installTestCLIConfig(t)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet || r.URL.Path != "/api/projects/project-1/environments/env-1/runs" {
			t.Fatalf("%s %s", r.Method, r.URL.Path)
		}
		query := r.URL.Query()
		if query.Get("status") != "waiting" || query.Get("limit") != "25" {
			t.Fatalf("query = %s", r.URL.RawQuery)
		}
		if got := r.Header.Get("authorization"); got != "Bearer session-test" {
			t.Fatalf("auth = %s", got)
		}
		_ = json.NewEncoder(w).Encode(api.ListRunsResponse{Runs: []api.RunResponse{{
			ID:     "run-1",
			TaskID: "deploy-prod",
			Status: "waiting",
			PendingWaitpoint: &api.PendingWaitpoint{
				Kind:        "human",
				WaitpointID: "wait-1",
				DisplayText: "Approve production deployment?",
				Request:     json.RawMessage(`{"message":"approve"}`),
				RequestedAt: time.Date(2026, 6, 6, 1, 2, 3, 0, time.UTC),
			},
		}}})
	}))
	defer server.Close()
	t.Setenv(helmrAPIURLEnv, server.URL)
	if err := state.SaveLogin(server.URL, "session-test"); err != nil {
		t.Fatal(err)
	}

	var out bytes.Buffer
	cmd := newRootCommand()
	cmd.SetOut(&out)
	cmd.SetErr(&bytes.Buffer{})
	cmd.SetArgs([]string{"waitpoint", "list", "--project", "project-1", "--env", "env-1", "--limit", "25", "--json"})
	if err := cmd.Execute(); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out.String(), `"waitpoint_id":"wait-1"`) || !strings.Contains(out.String(), `"run_id":"run-1"`) {
		t.Fatalf("out = %s", out.String())
	}
}

func TestWaitpointListCommandRejectsInvalidLimit(t *testing.T) {
	cmd := newRootCommand()
	cmd.SetOut(&bytes.Buffer{})
	cmd.SetErr(&bytes.Buffer{})
	cmd.SetArgs([]string{"waitpoint", "list", "--limit", "0"})
	err := cmd.Execute()
	if err == nil || !strings.Contains(err.Error(), "--limit must be an integer between 1 and 200") {
		t.Fatalf("err = %v", err)
	}
}
