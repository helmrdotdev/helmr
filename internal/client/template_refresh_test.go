package client

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/helmrdotdev/helmr/internal/api"
)

func TestTemplateRefreshWaitKeepsOriginalIdentity(t *testing.T) {
	for _, outcome := range []string{"ready", "failed", "expired", "changed-deployment", "changed-spec", "changed-build", "changed-template"} {
		t.Run(outcome, func(t *testing.T) {
			initial := api.TemplateBuildResponse{TemplateID: "repo", DeploymentID: testTriggerID, ImageSpecID: testTriggerID, ImageBuildID: testTriggerID, Status: "queued", Deadline: time.Now().Add(time.Minute)}
			posts, gets := 0, 0
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				result := initial
				if r.Method == http.MethodPost {
					posts++
					if !strings.HasSuffix(r.URL.Path, "/templates/repo/refresh") {
						t.Errorf("path %s", r.URL.Path)
					}
					var request api.RefreshTemplateRequest
					if err := json.NewDecoder(r.Body).Decode(&request); err != nil || request.IdempotencyKey != "refresh-key" {
						t.Errorf("request: %+v %v", request, err)
					}
				} else if r.Method == http.MethodGet {
					gets++
					if !strings.HasSuffix(r.URL.Path, "/templates/repo/builds/"+initial.ImageBuildID) || r.URL.Query().Get("deployment_id") != initial.DeploymentID {
						t.Errorf("observation escaped original build: %s", r.URL)
					}
					result.Status, result.Ready = "succeeded", true
					switch outcome {
					case "failed":
						result.Status, result.ErrorCode = "failed", "authored_preparation_failed"
					case "expired":
						result.Ready = false
					case "changed-deployment":
						result.DeploymentID = "other"
					case "changed-spec":
						result.ImageSpecID = "other"
					case "changed-build":
						result.ImageBuildID = "other"
					case "changed-template":
						result.TemplateID = "other"
					}
				} else {
					t.Errorf("method %s", r.Method)
				}
				_ = json.NewEncoder(w).Encode(result)
			}))
			defer server.Close()
			client, err := New(server.URL, WithHTTPClient(server.Client()))
			if err != nil {
				t.Fatal(err)
			}
			build, err := client.RefreshTemplate(t.Context(), "repo", "refresh-key", EnvironmentScopeOptions{})
			if err != nil {
				t.Fatal(err)
			}
			_, err = client.WaitTemplateBuild(t.Context(), build, EnvironmentScopeOptions{})
			if (err == nil) != (outcome == "ready") || posts != 1 || gets != 1 {
				t.Fatalf("outcome=%s posts=%d gets=%d err=%v", outcome, posts, gets, err)
			}
		})
	}
}

func TestTemplateRefreshWaitTerminalStatesNeverRequestWork(t *testing.T) {
	for _, state := range []string{"failed", "cancelled", "unknown", "queued"} {
		t.Run(state, func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { t.Error("terminal or expired wait issued HTTP request") }))
			defer server.Close()
			client, err := New(server.URL, WithHTTPClient(server.Client()))
			if err != nil {
				t.Fatal(err)
			}
			_, err = client.WaitTemplateBuild(t.Context(), api.TemplateBuildResponse{TemplateID: "repo", Status: state, Deadline: time.Now().Add(-time.Minute)}, EnvironmentScopeOptions{})
			if err == nil {
				t.Fatal("wait accepted unsuccessful build")
			}
		})
	}
}
