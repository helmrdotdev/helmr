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

func TestProjectCreateCommandGeneratesSlug(t *testing.T) {
	const projectID = "00000000-0000-0000-0000-000000000101"
	state, _ := installTestCLIConfig(t)
	var request api.CreateProjectRequest
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost || r.URL.Path != "/api/projects" {
			t.Fatalf("%s %s", r.Method, r.URL.Path)
		}
		if got := r.Header.Get("authorization"); got != "Bearer session_test" {
			t.Fatalf("auth = %s", got)
		}
		if err := json.NewDecoder(r.Body).Decode(&request); err != nil {
			t.Fatal(err)
		}
		_ = json.NewEncoder(w).Encode(api.ProjectSummary{ID: projectID, Slug: request.Slug, Name: request.Name})
	}))
	defer server.Close()
	if err := state.SaveLogin(server.URL, "session_test"); err != nil {
		t.Fatal(err)
	}

	var out bytes.Buffer
	cmd := newRootCommand()
	cmd.SetOut(&out)
	cmd.SetErr(&bytes.Buffer{})
	cmd.SetArgs([]string{"project", "create", "Production App!"})
	if err := cmd.Execute(); err != nil {
		t.Fatal(err)
	}
	if request.Name != "Production App!" || request.Slug != "production-app" {
		t.Fatalf("request = %+v", request)
	}
	if !strings.Contains(out.String(), projectID+"\tproduction-app\tProduction App!") {
		t.Fatalf("output = %q", out.String())
	}
}

func TestProjectGetCommandResolvesSlug(t *testing.T) {
	const projectID = "00000000-0000-0000-0000-000000000101"
	state, _ := installTestCLIConfig(t)
	methods := []string{}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		methods = append(methods, r.Method+" "+r.URL.Path)
		if got := r.Header.Get("authorization"); got != "Bearer session_test" {
			t.Fatalf("auth = %s", got)
		}
		switch {
		case r.Method == http.MethodGet && r.URL.Path == "/api/projects":
			_ = json.NewEncoder(w).Encode(api.ListProjectsResponse{Projects: []api.ProjectSummary{{
				ID:   projectID,
				Slug: "prod",
				Name: "Production",
			}}})
		case r.Method == http.MethodGet && r.URL.Path == "/api/projects/"+projectID:
			_ = json.NewEncoder(w).Encode(api.ProjectSummary{
				ID:   projectID,
				Slug: "prod",
				Name: "Production",
			})
		default:
			t.Fatalf("%s %s", r.Method, r.URL.Path)
		}
	}))
	defer server.Close()
	if err := state.SaveLogin(server.URL, "session_test"); err != nil {
		t.Fatal(err)
	}

	var out bytes.Buffer
	cmd := newRootCommand()
	cmd.SetOut(&out)
	cmd.SetErr(&bytes.Buffer{})
	cmd.SetArgs([]string{"project", "get", "prod", "--json"})
	if err := cmd.Execute(); err != nil {
		t.Fatal(err)
	}
	if got := strings.Join(methods, ","); got != "GET /api/projects,GET /api/projects/"+projectID {
		t.Fatalf("methods = %s", got)
	}
	var project api.ProjectSummary
	if err := json.Unmarshal(out.Bytes(), &project); err != nil {
		t.Fatal(err)
	}
	if project.ID != projectID || project.Slug != "prod" {
		t.Fatalf("project = %+v", project)
	}
}

func TestProjectUpdateCommandPreservesOmittedName(t *testing.T) {
	const projectID = "00000000-0000-0000-0000-000000000101"
	state, _ := installTestCLIConfig(t)
	var request api.UpdateProjectRequest
	methods := []string{}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		methods = append(methods, r.Method+" "+r.URL.Path)
		if got := r.Header.Get("authorization"); got != "Bearer session_test" {
			t.Fatalf("auth = %s", got)
		}
		switch {
		case r.Method == http.MethodGet && r.URL.Path == "/api/projects":
			_ = json.NewEncoder(w).Encode(api.ListProjectsResponse{Projects: []api.ProjectSummary{{
				ID:   projectID,
				Slug: "prod",
				Name: "Production",
			}}})
		case r.Method == http.MethodGet && r.URL.Path == "/api/projects/"+projectID:
			_ = json.NewEncoder(w).Encode(api.ProjectSummary{
				ID:   projectID,
				Slug: "prod",
				Name: "Production",
			})
		case r.Method == http.MethodPatch && r.URL.Path == "/api/projects/"+projectID:
			if err := json.NewDecoder(r.Body).Decode(&request); err != nil {
				t.Fatal(err)
			}
			_ = json.NewEncoder(w).Encode(api.ProjectSummary{ID: projectID, Slug: request.Slug, Name: request.Name})
		default:
			t.Fatalf("%s %s", r.Method, r.URL.Path)
		}
	}))
	defer server.Close()
	if err := state.SaveLogin(server.URL, "session_test"); err != nil {
		t.Fatal(err)
	}

	cmd := newRootCommand()
	cmd.SetOut(&bytes.Buffer{})
	cmd.SetErr(&bytes.Buffer{})
	cmd.SetArgs([]string{"project", "update", "prod", "--slug", "production"})
	if err := cmd.Execute(); err != nil {
		t.Fatal(err)
	}
	if got := strings.Join(methods, ","); got != "GET /api/projects,GET /api/projects/"+projectID+",PATCH /api/projects/"+projectID {
		t.Fatalf("methods = %s", got)
	}
	if request.Name != "Production" || request.Slug != "production" {
		t.Fatalf("request = %+v", request)
	}
}

func TestEnvCreateCommandResolvesProjectAndGeneratesSlug(t *testing.T) {
	const projectID = "00000000-0000-0000-0000-000000000101"
	const environmentID = "00000000-0000-0000-0000-000000000202"
	state, _ := installTestCLIConfig(t)
	var request api.CreateEnvironmentRequest
	methods := []string{}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		methods = append(methods, r.Method+" "+r.URL.Path)
		if got := r.Header.Get("authorization"); got != "Bearer session_test" {
			t.Fatalf("auth = %s", got)
		}
		switch {
		case r.Method == http.MethodGet && r.URL.Path == "/api/projects":
			_ = json.NewEncoder(w).Encode(api.ListProjectsResponse{Projects: []api.ProjectSummary{{
				ID:   projectID,
				Slug: "prod",
				Name: "Production",
			}}})
		case r.Method == http.MethodPost && r.URL.Path == "/api/projects/"+projectID+"/environments":
			if err := json.NewDecoder(r.Body).Decode(&request); err != nil {
				t.Fatal(err)
			}
			_ = json.NewEncoder(w).Encode(api.EnvironmentSummary{
				ID:        environmentID,
				ProjectID: projectID,
				Slug:      request.Slug,
				Name:      request.Name,
				ColorHex:  request.ColorHex,
			})
		default:
			t.Fatalf("%s %s", r.Method, r.URL.Path)
		}
	}))
	defer server.Close()
	if err := state.SaveLogin(server.URL, "session_test"); err != nil {
		t.Fatal(err)
	}

	cmd := newRootCommand()
	cmd.SetOut(&bytes.Buffer{})
	cmd.SetErr(&bytes.Buffer{})
	cmd.SetArgs([]string{"env", "create", "QA Environment", "--project", "prod"})
	if err := cmd.Execute(); err != nil {
		t.Fatal(err)
	}
	if got := strings.Join(methods, ","); got != "GET /api/projects,POST /api/projects/"+projectID+"/environments" {
		t.Fatalf("methods = %s", got)
	}
	if request.Name != "QA Environment" || request.Slug != "qa-environment" || request.ColorHex != "#F59E0B" {
		t.Fatalf("request = %+v", request)
	}
}

func TestEnvCommandRequiresProjectFlag(t *testing.T) {
	cmd := newRootCommand()
	cmd.SetOut(&bytes.Buffer{})
	cmd.SetErr(&bytes.Buffer{})
	cmd.SetArgs([]string{"env", "list"})

	err := cmd.Execute()

	if err == nil || !strings.Contains(err.Error(), "--project is required") {
		t.Fatalf("err = %v", err)
	}
}

func TestProjectEnvNestedCommandIsNotRegistered(t *testing.T) {
	cmd := newRootCommand()
	cmd.SetOut(&bytes.Buffer{})
	cmd.SetErr(&bytes.Buffer{})
	cmd.SetArgs([]string{"project", "env", "list", "prod"})

	err := cmd.Execute()

	if err == nil || !strings.Contains(err.Error(), "unknown command") {
		t.Fatalf("err = %v", err)
	}
}

func TestEnvUpdateCommandResolvesSlugsAndPreservesOmittedName(t *testing.T) {
	const projectID = "00000000-0000-0000-0000-000000000101"
	const environmentID = "00000000-0000-0000-0000-000000000202"
	state, _ := installTestCLIConfig(t)
	var request api.UpdateEnvironmentRequest
	methods := []string{}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		methods = append(methods, r.Method+" "+r.URL.Path)
		if got := r.Header.Get("authorization"); got != "Bearer session_test" {
			t.Fatalf("auth = %s", got)
		}
		switch {
		case r.Method == http.MethodGet && r.URL.Path == "/api/projects":
			_ = json.NewEncoder(w).Encode(api.ListProjectsResponse{Projects: []api.ProjectSummary{{
				ID:   projectID,
				Slug: "prod",
				Name: "Production",
				Environments: []api.EnvironmentSummary{{
					ID:        environmentID,
					ProjectID: projectID,
					Slug:      "qa",
					Name:      "QA",
					ColorHex:  "#F59E0B",
				}},
			}}})
		case r.Method == http.MethodGet && r.URL.Path == "/api/projects/"+projectID+"/environments/"+environmentID:
			_ = json.NewEncoder(w).Encode(api.EnvironmentSummary{
				ID:        environmentID,
				ProjectID: projectID,
				Slug:      "qa",
				Name:      "QA",
				ColorHex:  "#F59E0B",
			})
		case r.Method == http.MethodPatch && r.URL.Path == "/api/projects/"+projectID+"/environments/"+environmentID:
			if err := json.NewDecoder(r.Body).Decode(&request); err != nil {
				t.Fatal(err)
			}
			_ = json.NewEncoder(w).Encode(api.EnvironmentSummary{
				ID:        environmentID,
				ProjectID: projectID,
				Slug:      request.Slug,
				Name:      request.Name,
				ColorHex:  request.ColorHex,
			})
		default:
			t.Fatalf("%s %s", r.Method, r.URL.Path)
		}
	}))
	defer server.Close()
	if err := state.SaveLogin(server.URL, "session_test"); err != nil {
		t.Fatal(err)
	}

	cmd := newRootCommand()
	cmd.SetOut(&bytes.Buffer{})
	cmd.SetErr(&bytes.Buffer{})
	cmd.SetArgs([]string{"env", "update", "qa", "--project", "prod", "--slug", "staging"})
	if err := cmd.Execute(); err != nil {
		t.Fatal(err)
	}
	wantMethods := "GET /api/projects,GET /api/projects/" + projectID + "/environments/" + environmentID + ",PATCH /api/projects/" + projectID + "/environments/" + environmentID
	if got := strings.Join(methods, ","); got != wantMethods {
		t.Fatalf("methods = %s", got)
	}
	if request.Name != "QA" || request.Slug != "staging" || request.ColorHex != "#F59E0B" {
		t.Fatalf("request = %+v", request)
	}
}

func TestEnvUpdateCommandAllowsColorOnlyUpdate(t *testing.T) {
	const projectID = "00000000-0000-0000-0000-000000000101"
	const environmentID = "00000000-0000-0000-0000-000000000202"
	state, _ := installTestCLIConfig(t)
	var request api.UpdateEnvironmentRequest
	methods := []string{}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		methods = append(methods, r.Method+" "+r.URL.Path)
		if got := r.Header.Get("authorization"); got != "Bearer session_test" {
			t.Fatalf("auth = %s", got)
		}
		switch {
		case r.Method == http.MethodGet && r.URL.Path == "/api/projects":
			_ = json.NewEncoder(w).Encode(api.ListProjectsResponse{Projects: []api.ProjectSummary{{
				ID:   projectID,
				Slug: "prod",
				Name: "Production",
				Environments: []api.EnvironmentSummary{{
					ID:        environmentID,
					ProjectID: projectID,
					Slug:      "qa",
					Name:      "QA",
					ColorHex:  "#F59E0B",
				}},
			}}})
		case r.Method == http.MethodGet && r.URL.Path == "/api/projects/"+projectID+"/environments/"+environmentID:
			_ = json.NewEncoder(w).Encode(api.EnvironmentSummary{
				ID:        environmentID,
				ProjectID: projectID,
				Slug:      "qa",
				Name:      "QA",
				ColorHex:  "#F59E0B",
			})
		case r.Method == http.MethodPatch && r.URL.Path == "/api/projects/"+projectID+"/environments/"+environmentID:
			if err := json.NewDecoder(r.Body).Decode(&request); err != nil {
				t.Fatal(err)
			}
			_ = json.NewEncoder(w).Encode(api.EnvironmentSummary{
				ID:        environmentID,
				ProjectID: projectID,
				Slug:      request.Slug,
				Name:      request.Name,
				ColorHex:  request.ColorHex,
			})
		default:
			t.Fatalf("%s %s", r.Method, r.URL.Path)
		}
	}))
	defer server.Close()
	if err := state.SaveLogin(server.URL, "session_test"); err != nil {
		t.Fatal(err)
	}

	cmd := newRootCommand()
	cmd.SetOut(&bytes.Buffer{})
	cmd.SetErr(&bytes.Buffer{})
	cmd.SetArgs([]string{"env", "update", "qa", "--project", "prod", "--color", "#06b6d4"})
	if err := cmd.Execute(); err != nil {
		t.Fatal(err)
	}
	wantMethods := "GET /api/projects,GET /api/projects/" + projectID + "/environments/" + environmentID + ",PATCH /api/projects/" + projectID + "/environments/" + environmentID
	if got := strings.Join(methods, ","); got != wantMethods {
		t.Fatalf("methods = %s", got)
	}
	if request.Name != "QA" || request.Slug != "qa" || request.ColorHex != "#06B6D4" {
		t.Fatalf("request = %+v", request)
	}
}

func TestDefaultEnvironmentColorHexUsesSemanticAndCustomPalette(t *testing.T) {
	tests := map[string]string{
		"production": "#315FCE",
		"master":     "#315FCE",
		"staging":    "#F59E0B",
		"dev":        "#22C55E",
		"preview":    "#06B6D4",
		"":           "#0EA5E9",
	}
	for slug, want := range tests {
		if got := defaultEnvironmentColorHex(slug); got != want {
			t.Fatalf("defaultEnvironmentColorHex(%q) = %q, want %q", slug, got, want)
		}
	}

	customPalette := map[string]bool{
		"#0EA5E9": true,
		"#8B5CF6": true,
		"#EC4899": true,
		"#F97316": true,
		"#14B8A6": true,
		"#84CC16": true,
		"#6366F1": true,
	}
	first := defaultEnvironmentColorHex("customer-a")
	second := defaultEnvironmentColorHex("customer-a")
	if first != second {
		t.Fatalf("custom color should be stable: %q != %q", first, second)
	}
	if !customPalette[first] {
		t.Fatalf("custom color = %q, want preset palette color", first)
	}
}

func TestEnvDeleteCommandResolvesSlugs(t *testing.T) {
	const projectID = "00000000-0000-0000-0000-000000000101"
	const environmentID = "00000000-0000-0000-0000-000000000202"
	state, _ := installTestCLIConfig(t)
	methods := []string{}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		methods = append(methods, r.Method+" "+r.URL.Path)
		if got := r.Header.Get("authorization"); got != "Bearer session_test" {
			t.Fatalf("auth = %s", got)
		}
		switch {
		case r.Method == http.MethodGet && r.URL.Path == "/api/projects":
			_ = json.NewEncoder(w).Encode(api.ListProjectsResponse{Projects: []api.ProjectSummary{{
				ID:   projectID,
				Slug: "prod",
				Name: "Production",
				Environments: []api.EnvironmentSummary{{
					ID:        environmentID,
					ProjectID: projectID,
					Slug:      "qa",
					Name:      "QA",
				}},
			}}})
		case r.Method == http.MethodDelete && r.URL.Path == "/api/projects/"+projectID+"/environments/"+environmentID:
			w.WriteHeader(http.StatusNoContent)
		default:
			t.Fatalf("%s %s", r.Method, r.URL.Path)
		}
	}))
	defer server.Close()
	if err := state.SaveLogin(server.URL, "session_test"); err != nil {
		t.Fatal(err)
	}

	cmd := newRootCommand()
	cmd.SetOut(&bytes.Buffer{})
	cmd.SetErr(&bytes.Buffer{})
	cmd.SetArgs([]string{"env", "delete", "qa", "--project", "prod", "--yes"})
	if err := cmd.Execute(); err != nil {
		t.Fatal(err)
	}
	wantMethods := "GET /api/projects,DELETE /api/projects/" + projectID + "/environments/" + environmentID
	if got := strings.Join(methods, ","); got != wantMethods {
		t.Fatalf("methods = %s", got)
	}
}
