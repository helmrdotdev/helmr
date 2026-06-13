package main

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/helmrdotdev/helmr/internal/api"
)

func TestPolicyListCommandPrintsPolicyNames(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet || r.URL.Path != "/api/waitpoint-policies" {
			t.Fatalf("%s %s", r.Method, r.URL.Path)
		}
		if got := r.Header.Get("authorization"); got != "Bearer test-key" {
			t.Fatalf("auth = %s", got)
		}
		_ = json.NewEncoder(w).Encode(api.ListWaitpointPoliciesResponse{Policies: []api.WaitpointPolicyResponse{
			{Name: "deploy-prod"},
			{Name: "customer-approval"},
		}})
	}))
	defer server.Close()
	t.Setenv(helmrAPIURLEnv, server.URL)
	t.Setenv(helmrAPIKeyEnv, "test-key")

	var out bytes.Buffer
	cmd := newRootCommand()
	cmd.SetOut(&out)
	cmd.SetErr(&bytes.Buffer{})
	cmd.SetArgs([]string{"policy", "list"})
	if err := cmd.Execute(); err != nil {
		t.Fatal(err)
	}
	if got := strings.TrimSpace(out.String()); got != "deploy-prod\ncustomer-approval" {
		t.Fatalf("output = %q", out.String())
	}
}

func TestPolicyGetCommandPrintsPolicyDetails(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet || r.URL.Path != "/api/waitpoint-policies/deploy-prod" {
			t.Fatalf("%s %s", r.Method, r.URL.Path)
		}
		if got := r.Header.Get("authorization"); got != "Bearer test-key" {
			t.Fatalf("auth = %s", got)
		}
		_ = json.NewEncoder(w).Encode(api.WaitpointPolicyResponse{
			ID:     "policy-1",
			Name:   "deploy-prod",
			Label:  "Production deploy",
			Config: json.RawMessage(`{"deliveries":[{"type":"email","to":["sre@example.test"]}],"resolution":{"type":"any","count":1}}`),
		})
	}))
	defer server.Close()
	t.Setenv(helmrAPIURLEnv, server.URL)
	t.Setenv(helmrAPIKeyEnv, "test-key")

	var out bytes.Buffer
	cmd := newRootCommand()
	cmd.SetOut(&out)
	cmd.SetErr(&bytes.Buffer{})
	cmd.SetArgs([]string{"policy", "get", "deploy-prod"})
	if err := cmd.Execute(); err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{
		"Name: deploy-prod",
		"Label: Production deploy",
		`"type": "email"`,
		`"sre@example.test"`,
	} {
		if !strings.Contains(out.String(), want) {
			t.Fatalf("output missing %q: %s", want, out.String())
		}
	}
}

func TestPolicyApplyEmailCreatesWhenMissing(t *testing.T) {
	var methods []string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		methods = append(methods, r.Method+" "+r.URL.Path)
		if got := r.Header.Get("authorization"); got != "Bearer test-key" {
			t.Fatalf("auth = %s", got)
		}
		switch {
		case r.Method == http.MethodPatch && r.URL.Path == "/api/waitpoint-policies/deploy-prod":
			var request api.UpdateWaitpointPolicyRequest
			if err := json.NewDecoder(r.Body).Decode(&request); err != nil {
				t.Fatal(err)
			}
			assertWaitpointPolicyRequest(t, request.Label, request.Config, "Production deploy", []string{"sre@example.test"})
			http.Error(w, `{"error":"waitpoint policy not found"}`, http.StatusNotFound)
		case r.Method == http.MethodPost && r.URL.Path == "/api/waitpoint-policies":
			var request api.CreateWaitpointPolicyRequest
			if err := json.NewDecoder(r.Body).Decode(&request); err != nil {
				t.Fatal(err)
			}
			if request.Name != "deploy-prod" {
				t.Fatalf("name = %q", request.Name)
			}
			assertWaitpointPolicyRequest(t, request.Label, request.Config, "Production deploy", []string{"sre@example.test"})
			_ = json.NewEncoder(w).Encode(api.WaitpointPolicyResponse{
				ID:        "policy-1",
				Name:      request.Name,
				Label:     request.Label,
				Config:    request.Config,
				CreatedAt: time.Unix(0, 0).UTC(),
				UpdatedAt: time.Unix(0, 0).UTC(),
			})
		default:
			t.Fatalf("%s %s", r.Method, r.URL.Path)
		}
	}))
	defer server.Close()
	t.Setenv(helmrAPIURLEnv, server.URL)
	t.Setenv(helmrAPIKeyEnv, "test-key")

	var out bytes.Buffer
	cmd := newRootCommand()
	cmd.SetOut(&out)
	cmd.SetErr(&bytes.Buffer{})
	cmd.SetArgs([]string{"policy", "apply", "deploy-prod", "--label", "Production deploy", "--email", "sre@example.test", "--json"})
	if err := cmd.Execute(); err != nil {
		t.Fatal(err)
	}
	if got := strings.Join(methods, ","); got != "PATCH /api/waitpoint-policies/deploy-prod,POST /api/waitpoint-policies" {
		t.Fatalf("methods = %s", got)
	}
	var response api.WaitpointPolicyResponse
	if err := json.Unmarshal(out.Bytes(), &response); err != nil {
		t.Fatal(err)
	}
	if response.Name != "deploy-prod" || response.Label != "Production deploy" {
		t.Fatalf("response = %+v", response)
	}
}

func TestPolicyApplyUsesSessionScopedRoute(t *testing.T) {
	const projectID = "00000000-0000-0000-0000-000000000101"
	const environmentID = "00000000-0000-0000-0000-000000000202"
	state, _ := installTestCLIConfig(t)
	var methods []string
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
		case r.Method == http.MethodPatch && r.URL.Path == "/api/projects/"+projectID+"/environments/"+environmentID+"/waitpoint-policies/deploy-prod":
			var request api.UpdateWaitpointPolicyRequest
			if err := json.NewDecoder(r.Body).Decode(&request); err != nil {
				t.Fatal(err)
			}
			assertWaitpointPolicyRequest(t, request.Label, request.Config, "Production deploy", []string{"sre@example.test"})
			_ = json.NewEncoder(w).Encode(api.WaitpointPolicyResponse{
				ID:        "policy-1",
				Name:      "deploy-prod",
				Label:     request.Label,
				Config:    request.Config,
				CreatedAt: time.Unix(0, 0).UTC(),
				UpdatedAt: time.Unix(0, 0).UTC(),
			})
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
	cmd.SetArgs([]string{"policy", "apply", "deploy-prod", "--project", "prod", "--env", "qa", "--label", "Production deploy", "--email", "sre@example.test"})
	if err := cmd.Execute(); err != nil {
		t.Fatal(err)
	}
	wantMethods := "GET /api/projects,PATCH /api/projects/" + projectID + "/environments/" + environmentID + "/waitpoint-policies/deploy-prod"
	if got := strings.Join(methods, ","); got != wantMethods {
		t.Fatalf("methods = %s", got)
	}
}

func TestPolicyListCommandRequiresScopeWithSessionAuth(t *testing.T) {
	state, _ := installTestCLIConfig(t)
	if err := state.SaveLogin("https://control.example.test", "session-test"); err != nil {
		t.Fatal(err)
	}

	cmd := newRootCommand()
	cmd.SetOut(&bytes.Buffer{})
	cmd.SetErr(&bytes.Buffer{})
	cmd.SetArgs([]string{"policy", "list"})
	err := cmd.Execute()
	if err == nil || !strings.Contains(err.Error(), "--project and --env are required with helmr login") {
		t.Fatalf("err = %v", err)
	}
}

func TestPolicyApplyStdinUpdatesPolicy(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPatch || r.URL.Path != "/api/waitpoint-policies/customer-approval" {
			t.Fatalf("%s %s", r.Method, r.URL.Path)
		}
		if got := r.Header.Get("authorization"); got != "Bearer test-key" {
			t.Fatalf("auth = %s", got)
		}
		var request api.UpdateWaitpointPolicyRequest
		if err := json.NewDecoder(r.Body).Decode(&request); err != nil {
			t.Fatal(err)
		}
		assertWaitpointPolicyRequest(t, request.Label, request.Config, "Customer approval", []string{"customer@example.test"})
		_ = json.NewEncoder(w).Encode(api.WaitpointPolicyResponse{
			ID:        "policy-1",
			Name:      "customer-approval",
			Label:     request.Label,
			Config:    request.Config,
			CreatedAt: time.Unix(0, 0).UTC(),
			UpdatedAt: time.Unix(0, 0).UTC(),
		})
	}))
	defer server.Close()
	t.Setenv(helmrAPIURLEnv, server.URL)
	t.Setenv(helmrAPIKeyEnv, "test-key")

	var out bytes.Buffer
	cmd := newRootCommand()
	cmd.SetIn(strings.NewReader(`{
		"label": "Customer approval",
		"deliveries": [{"type": "email", "to": ["customer@example.test"]}],
		"resolution": {"type": "any", "count": 1}
	}`))
	cmd.SetOut(&out)
	cmd.SetErr(&bytes.Buffer{})
	cmd.SetArgs([]string{"policy", "apply", "customer-approval", "--stdin"})
	if err := cmd.Execute(); err != nil {
		t.Fatal(err)
	}
	if strings.TrimSpace(out.String()) != "customer-approval" {
		t.Fatalf("output = %q", out.String())
	}
}
