package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/helmrdotdev/helmr/internal/api"
	"github.com/helmrdotdev/helmr/internal/deployment"
	"github.com/helmrdotdev/helmr/internal/sha256sum"
)

func writeSessionScopeProjects(t *testing.T, w http.ResponseWriter, projectSlug, environmentSlug string) {
	t.Helper()
	if err := json.NewEncoder(w).Encode(api.ListProjectsResponse{Projects: []api.ProjectSummary{{
		ID:   "019c10d5-a6f7-7af1-8f5f-bb97bcc0dc30",
		Slug: projectSlug,
		Environments: []api.EnvironmentSummary{{
			ID:   "019c10d5-a6f7-7af2-8f5f-bb97bcc0dc32",
			Slug: environmentSlug,
		}},
	}}}); err != nil {
		t.Fatal(err)
	}
}

func TestDeployBundleUsesUploadFinalizePromoteFlow(t *testing.T) {
	directory, raw, digest, objectDigest := writeDeployTestBundle(t)
	const deploymentID = "019c10d5-a6f7-7af1-8f5f-bb97bcc0dc31"
	requests := make([]string, 0, 4)
	var server *httptest.Server
	server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests = append(requests, r.Method+" "+r.URL.Path)
		switch r.URL.Path {
		case "/v1/deployment-bundles/upload-plan":
			if r.Header.Get("Content-Type") != deployment.DeploymentBundleMediaType {
				t.Fatalf("content type = %q", r.Header.Get("Content-Type"))
			}
			body, err := io.ReadAll(r.Body)
			if err != nil {
				t.Fatal(err)
			}
			if !bytes.Equal(body, raw) {
				t.Fatal("upload plan body differs from bundle.json")
			}
			_ = json.NewEncoder(w).Encode(api.DeploymentBundleUploadPlanResponse{
				BundleDigest: digest,
				Uploads: []api.DeploymentBundleUpload{{
					Digest: objectDigest, Method: http.MethodPut, URL: server.URL + "/upload",
					Headers: map[string]string{
						"Content-Length": "7",
						"Content-Type":   deployment.ProgramArtifactMediaType,
					},
				}},
			})
		case "/upload":
			if r.Header.Get("Authorization") != "" {
				t.Fatal("Control Plane credential leaked to object upload")
			}
			if r.ContentLength != 7 {
				t.Fatalf("upload content length = %d", r.ContentLength)
			}
			body, err := io.ReadAll(r.Body)
			if err != nil || string(body) != "program" {
				t.Fatalf("upload body = %q, err = %v", body, err)
			}
			w.WriteHeader(http.StatusNoContent)
		case "/v1/deployment-bundles/finalize":
			var request api.FinalizeDeploymentBundleRequest
			if err := json.NewDecoder(r.Body).Decode(&request); err != nil {
				t.Fatal(err)
			}
			if request.BundleDigest != digest || request.IdempotencyKey != "deploy-test" {
				t.Fatalf("finalize request = %+v", request)
			}
			writeDeploymentFinalizeTestStream(t, w, objectDigest, api.DeploymentResponse{
				ID: deploymentID, Version: "v-test", BundleDigest: digest, CreatedAt: time.Now(),
			})
		case "/v1/deployments/" + deploymentID + "/promote":
			_ = json.NewEncoder(w).Encode(api.DeploymentResponse{
				ID: deploymentID, Version: "v-test", BundleDigest: digest, CreatedAt: time.Now(),
			})
		default:
			t.Fatalf("unexpected request %s %s", r.Method, r.URL.Path)
		}
	}))
	defer server.Close()
	t.Setenv(helmrAPIURLEnv, server.URL)
	t.Setenv(helmrAPIKeyEnv, "test-key")

	var out bytes.Buffer
	cmd := newRootCommand()
	cmd.SetOut(&out)
	cmd.SetErr(&bytes.Buffer{})
	cmd.SetArgs([]string{"deploy", "--bundle", directory, "--idempotency-key", "deploy-test"})
	if err := cmd.Execute(); err != nil {
		t.Fatal(err)
	}
	if out.String() != "v-test\n" {
		t.Fatalf("output = %q", out.String())
	}
	want := []string{
		"POST /v1/deployment-bundles/upload-plan",
		"PUT /upload",
		"POST /v1/deployment-bundles/finalize",
		"POST /v1/deployments/" + deploymentID + "/promote",
	}
	if strings.Join(requests, "\n") != strings.Join(want, "\n") {
		t.Fatalf("requests = %v", requests)
	}
}

func TestDeployReporterEmitsObjectTransferProgress(t *testing.T) {
	const digest = "sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
	command := newRootCommand()
	var jsonOutput bytes.Buffer
	command.SetOut(&jsonOutput)
	reporter := newDeployReporter(command, true)
	if err := reporter.DeploymentObjectUploadStarted(digest, 1, 2); err != nil {
		t.Fatal(err)
	}
	if err := reporter.DeploymentObjectUploadProgress(digest, 1, 2, 3, 7); err != nil {
		t.Fatal(err)
	}
	if err := reporter.DeploymentObjectUploaded(digest, 1, 2); err != nil {
		t.Fatal(err)
	}
	lines := strings.Split(strings.TrimSpace(jsonOutput.String()), "\n")
	if len(lines) != 3 {
		t.Fatalf("JSON lines = %q", jsonOutput.String())
	}
	wantTypes := []string{
		"deployment_object_upload_started",
		"deployment_object_upload_progress",
		"deployment_object_uploaded",
	}
	for index, raw := range lines {
		var line cliDeployLine
		if err := json.Unmarshal([]byte(raw), &line); err != nil {
			t.Fatal(err)
		}
		if line.Type != wantTypes[index] || line.Digest != digest || line.Index != 1 || line.Count != 2 {
			t.Fatalf("line %d = %+v", index, line)
		}
		if index == 1 && (line.BytesRead != 3 || line.TotalBytes != 7) {
			t.Fatalf("progress line = %+v", line)
		}
	}

	var textOutput bytes.Buffer
	command.SetErr(&textOutput)
	textReporter := newDeployReporter(command, false)
	if err := textReporter.DeploymentObjectUploadStarted(digest, 1, 2); err != nil {
		t.Fatal(err)
	}
	if err := textReporter.DeploymentObjectUploadProgress(digest, 1, 2, 3, 7); err != nil {
		t.Fatal(err)
	}
	if err := textReporter.DeploymentObjectUploaded(digest, 1, 2); err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"Uploading deployment object", "3/7 bytes", "uploaded (1/2)"} {
		if !strings.Contains(textOutput.String(), want) {
			t.Fatalf("text output = %q, want %q", textOutput.String(), want)
		}
	}
}

func TestDeployJSONEmitsPromotedDeploymentResult(t *testing.T) {
	directory, _, digest, objectDigest := writeDeployTestBundle(t)
	const deploymentID = "019c10d5-a6f7-7af1-8f5f-bb97bcc0dc31"
	createdAt := time.Date(2026, 8, 31, 12, 0, 0, 0, time.UTC)
	var server *httptest.Server
	server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/v1/deployment-bundles/upload-plan":
			_ = json.NewEncoder(w).Encode(api.DeploymentBundleUploadPlanResponse{
				BundleDigest: digest,
				Uploads:      []api.DeploymentBundleUpload{deployTestUpload(server.URL, objectDigest)},
			})
		case "/upload":
			_, _ = io.Copy(io.Discard, r.Body)
			w.WriteHeader(http.StatusNoContent)
		case "/v1/deployment-bundles/finalize":
			writeDeploymentFinalizeTestStream(t, w, objectDigest, api.DeploymentResponse{
				ID: deploymentID, Version: "v-test", BundleDigest: digest, CreatedAt: createdAt,
			})
		case "/v1/deployments/" + deploymentID + "/promote":
			_ = json.NewEncoder(w).Encode(api.DeploymentResponse{
				ID: deploymentID, Version: "v-test", BundleDigest: digest, CreatedAt: createdAt,
			})
		default:
			t.Fatalf("unexpected request %s %s", r.Method, r.URL.Path)
		}
	}))
	defer server.Close()
	t.Setenv(helmrAPIURLEnv, server.URL)
	t.Setenv(helmrAPIKeyEnv, "test-key")

	var out bytes.Buffer
	cmd := newRootCommand()
	cmd.SetOut(&out)
	cmd.SetErr(io.Discard)
	cmd.SetArgs([]string{"deploy", "--bundle", directory, "--json", "--idempotency-key", "deploy-test"})
	if err := cmd.Execute(); err != nil {
		t.Fatal(err)
	}
	var result cliDeployLine
	for _, raw := range strings.Split(strings.TrimSpace(out.String()), "\n") {
		var line cliDeployLine
		if err := json.Unmarshal([]byte(raw), &line); err != nil {
			t.Fatal(err)
		}
		if line.Type == "deployment_result" {
			result = line
		}
	}
	if result.Type != "deployment_result" || result.Phase != "promoted" ||
		result.Deployment == nil || result.Deployment.ID != deploymentID {
		t.Fatalf("deployment_result = %+v", result)
	}
}

func TestDeploySkipPromotionDoesNotPromote(t *testing.T) {
	directory, _, digest, objectDigest := writeDeployTestBundle(t)
	const deploymentID = "019c10d5-a6f7-7af1-8f5f-bb97bcc0dc31"
	requests := make([]string, 0, 3)
	var server *httptest.Server
	server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests = append(requests, r.Method+" "+r.URL.Path)
		switch r.URL.Path {
		case "/v1/deployment-bundles/upload-plan":
			_ = json.NewEncoder(w).Encode(api.DeploymentBundleUploadPlanResponse{
				BundleDigest: digest,
				Uploads:      []api.DeploymentBundleUpload{deployTestUpload(server.URL, objectDigest)},
			})
		case "/upload":
			_, _ = io.Copy(io.Discard, r.Body)
			w.WriteHeader(http.StatusNoContent)
		case "/v1/deployment-bundles/finalize":
			writeDeploymentFinalizeTestStream(t, w, objectDigest, api.DeploymentResponse{
				ID: deploymentID, Version: "v-test", BundleDigest: digest, CreatedAt: time.Now(),
			})
		default:
			t.Fatalf("unexpected request %s %s", r.Method, r.URL.Path)
		}
	}))
	defer server.Close()
	t.Setenv(helmrAPIURLEnv, server.URL)
	t.Setenv(helmrAPIKeyEnv, "test-key")

	var out bytes.Buffer
	cmd := newRootCommand()
	cmd.SetOut(&out)
	cmd.SetErr(io.Discard)
	cmd.SetArgs([]string{
		"deploy", "--bundle", directory, "--skip-promotion", "--json", "--idempotency-key", "deploy-test",
	})
	if err := cmd.Execute(); err != nil {
		t.Fatal(err)
	}
	want := []string{
		"POST /v1/deployment-bundles/upload-plan",
		"PUT /upload",
		"POST /v1/deployment-bundles/finalize",
	}
	if strings.Join(requests, "\n") != strings.Join(want, "\n") {
		t.Fatalf("requests = %v", requests)
	}
	var result cliDeployLine
	for _, raw := range strings.Split(strings.TrimSpace(out.String()), "\n") {
		var line cliDeployLine
		if err := json.Unmarshal([]byte(raw), &line); err != nil {
			t.Fatal(err)
		}
		if line.Type == "deployment_result" {
			result = line
		}
	}
	if result.Phase != "finalized" || result.Deployment == nil || result.Deployment.ID != deploymentID {
		t.Fatalf("skip result = %+v", result)
	}
}

func TestDeploymentPromoteSendsEmptyRequestAndRejectsReasonFlag(t *testing.T) {
	const deploymentID = "019c10d5-a6f7-7af1-8f5f-bb97bcc0dc31"
	var promoteBody string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost || r.URL.Path != "/v1/deployments/"+deploymentID+"/promote" {
			t.Fatalf("unexpected request %s %s", r.Method, r.URL.Path)
		}
		body, err := io.ReadAll(r.Body)
		if err != nil {
			t.Fatal(err)
		}
		promoteBody = strings.TrimSpace(string(body))
		_ = json.NewEncoder(w).Encode(api.DeploymentResponse{
			ID: deploymentID, Version: "v-test", CreatedAt: time.Now(),
		})
	}))
	defer server.Close()
	t.Setenv(helmrAPIURLEnv, server.URL)
	t.Setenv(helmrAPIKeyEnv, "test-key")

	var out bytes.Buffer
	cmd := newRootCommand()
	cmd.SetOut(&out)
	cmd.SetErr(io.Discard)
	cmd.SetArgs([]string{"deployment", "promote", deploymentID})
	if err := cmd.Execute(); err != nil {
		t.Fatal(err)
	}
	if promoteBody != "{}" {
		t.Fatalf("promote body = %q, want {}", promoteBody)
	}
	if out.String() != "v-test\n" {
		t.Fatalf("output = %q", out.String())
	}

	rejected := newRootCommand()
	rejected.SetOut(io.Discard)
	rejected.SetErr(io.Discard)
	rejected.SetArgs([]string{"deployment", "promote", deploymentID, "--reason", "rollback"})
	err := rejected.Execute()
	if err == nil || !strings.Contains(err.Error(), "unknown flag: --reason") {
		t.Fatalf("error = %v, want unknown --reason flag", err)
	}
}

func TestDeployBundleReconcilesAcceptedUploadWithLostResponse(t *testing.T) {
	directory, _, digest, objectDigest := writeDeployTestBundle(t)
	const deploymentID = "019c10d5-a6f7-7af1-8f5f-bb97bcc0dc31"
	planRequests := 0
	uploadRequests := 0
	finalizeRequests := 0
	var server *httptest.Server
	server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/v1/deployment-bundles/upload-plan":
			planRequests++
			response := api.DeploymentBundleUploadPlanResponse{BundleDigest: digest}
			if planRequests == 1 {
				response.Uploads = []api.DeploymentBundleUpload{deployTestUpload(server.URL, objectDigest)}
			}
			_ = json.NewEncoder(w).Encode(response)
		case "/upload":
			uploadRequests++
			body, err := io.ReadAll(r.Body)
			if err != nil || string(body) != "program" {
				t.Fatalf("upload body = %q, err = %v", body, err)
			}
			connection, _, err := w.(http.Hijacker).Hijack()
			if err != nil {
				t.Fatal(err)
			}
			_ = connection.Close()
		case "/v1/deployment-bundles/finalize":
			finalizeRequests++
			writeDeploymentFinalizeTestStream(t, w, objectDigest, api.DeploymentResponse{
				ID: deploymentID, Version: "v-test", BundleDigest: digest, CreatedAt: time.Now(),
			})
		case "/v1/deployments/" + deploymentID + "/promote":
			_ = json.NewEncoder(w).Encode(api.DeploymentResponse{
				ID: deploymentID, Version: "v-test", BundleDigest: digest, CreatedAt: time.Now(),
			})
		default:
			t.Fatalf("unexpected request %s %s", r.Method, r.URL.Path)
		}
	}))
	defer server.Close()
	t.Setenv(helmrAPIURLEnv, server.URL)
	t.Setenv(helmrAPIKeyEnv, "test-key")

	cmd := newRootCommand()
	cmd.SetOut(io.Discard)
	cmd.SetErr(io.Discard)
	cmd.SetArgs([]string{"deploy", "--bundle", directory, "--idempotency-key", "deploy-test"})
	if err := cmd.Execute(); err != nil {
		t.Fatal(err)
	}
	if planRequests != 2 || uploadRequests != 1 || finalizeRequests != 1 {
		t.Fatalf(
			"requests = plans:%d uploads:%d finalize:%d",
			planRequests, uploadRequests, finalizeRequests,
		)
	}
}

func TestDeployBundlePreservesUploadFailureWhenObjectRemainsMissing(t *testing.T) {
	directory, _, digest, objectDigest := writeDeployTestBundle(t)
	planRequests := 0
	uploadRequests := 0
	var server *httptest.Server
	server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/v1/deployment-bundles/upload-plan":
			planRequests++
			_ = json.NewEncoder(w).Encode(api.DeploymentBundleUploadPlanResponse{
				BundleDigest: digest,
				Uploads:      []api.DeploymentBundleUpload{deployTestUpload(server.URL, objectDigest)},
			})
		case "/upload":
			uploadRequests++
			_, _ = io.Copy(io.Discard, r.Body)
			connection, _, err := w.(http.Hijacker).Hijack()
			if err != nil {
				t.Fatal(err)
			}
			_ = connection.Close()
		default:
			t.Fatalf("unexpected request %s %s", r.Method, r.URL.Path)
		}
	}))
	defer server.Close()
	t.Setenv(helmrAPIURLEnv, server.URL)
	t.Setenv(helmrAPIKeyEnv, "test-key")

	cmd := newRootCommand()
	cmd.SetOut(io.Discard)
	cmd.SetErr(io.Discard)
	cmd.SetArgs([]string{"deploy", "--bundle", directory})
	err := cmd.Execute()
	if err == nil || !strings.Contains(err.Error(), "deployment object upload transport failed") {
		t.Fatalf("error = %v", err)
	}
	if planRequests != 2 || uploadRequests != 1 {
		t.Fatalf("requests = plans:%d uploads:%d", planRequests, uploadRequests)
	}
}

func TestDeployBundleDoesNotReconcileUnattemptedUpload(t *testing.T) {
	directory, _, digest, objectDigest := writeDeployTestBundle(t)
	planRequests := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		planRequests++
		_ = json.NewEncoder(w).Encode(api.DeploymentBundleUploadPlanResponse{
			BundleDigest: digest,
			Uploads: []api.DeploymentBundleUpload{{
				Digest: objectDigest, Method: http.MethodGet, URL: "https://upload.invalid/object",
			}},
		})
	}))
	defer server.Close()
	t.Setenv(helmrAPIURLEnv, server.URL)
	t.Setenv(helmrAPIKeyEnv, "test-key")

	cmd := newRootCommand()
	cmd.SetOut(io.Discard)
	cmd.SetErr(io.Discard)
	cmd.SetArgs([]string{"deploy", "--bundle", directory})
	err := cmd.Execute()
	if err == nil || !strings.Contains(err.Error(), "upload was not attempted") {
		t.Fatalf("error = %v", err)
	}
	if planRequests != 1 {
		t.Fatalf("plan requests = %d", planRequests)
	}
}

func TestDeployBundleRejectsInvalidReconciliationPlan(t *testing.T) {
	directory, _, digest, objectDigest := writeDeployTestBundle(t)
	planRequests := 0
	uploadRequests := 0
	var server *httptest.Server
	server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/v1/deployment-bundles/upload-plan":
			planRequests++
			response := api.DeploymentBundleUploadPlanResponse{BundleDigest: digest}
			if planRequests == 1 {
				response.Uploads = []api.DeploymentBundleUpload{deployTestUpload(server.URL, objectDigest)}
			} else {
				response.BundleDigest = "sha256:" + strings.Repeat("a", 64)
			}
			_ = json.NewEncoder(w).Encode(response)
		case "/upload":
			uploadRequests++
			_, _ = io.Copy(io.Discard, r.Body)
			connection, _, err := w.(http.Hijacker).Hijack()
			if err != nil {
				t.Fatal(err)
			}
			_ = connection.Close()
		default:
			t.Fatalf("unexpected request %s %s", r.Method, r.URL.Path)
		}
	}))
	defer server.Close()
	t.Setenv(helmrAPIURLEnv, server.URL)
	t.Setenv(helmrAPIKeyEnv, "test-key")

	cmd := newRootCommand()
	cmd.SetOut(io.Discard)
	cmd.SetErr(io.Discard)
	cmd.SetArgs([]string{"deploy", "--bundle", directory})
	err := cmd.Execute()
	if err == nil ||
		!strings.Contains(err.Error(), "deployment object upload transport failed") ||
		!strings.Contains(err.Error(), "different bundle digest") {
		t.Fatalf("error = %v", err)
	}
	if planRequests != 2 || uploadRequests != 1 {
		t.Fatalf("requests = plans:%d uploads:%d", planRequests, uploadRequests)
	}
}

func deployTestUpload(serverURL string, objectDigest string) api.DeploymentBundleUpload {
	return api.DeploymentBundleUpload{
		Digest: objectDigest, Method: http.MethodPut, URL: serverURL + "/upload",
		Headers: map[string]string{
			"Content-Length": "7",
			"Content-Type":   deployment.ProgramArtifactMediaType,
		},
	}
}

func writeDeploymentFinalizeTestStream(
	t *testing.T,
	w http.ResponseWriter,
	objectDigest string,
	response api.DeploymentResponse,
) {
	t.Helper()
	w.Header().Set("Content-Type", "text/event-stream")
	started, err := json.Marshal(api.DeploymentBundleFinalizeStarted{BundleDigest: response.BundleDigest})
	if err != nil {
		t.Fatal(err)
	}
	object, err := json.Marshal(api.DeploymentBundleFinalizeObject{Digest: objectDigest})
	if err != nil {
		t.Fatal(err)
	}
	complete, err := json.Marshal(response)
	if err != nil {
		t.Fatal(err)
	}
	_, err = fmt.Fprintf(w, "event: started\ndata: %s\n\nevent: object_verified\ndata: %s\n\nevent: complete\ndata: %s\n\n", started, object, complete)
	if err != nil {
		t.Fatal(err)
	}
}

func TestDeployBundleRejectsUploadOutsideClosure(t *testing.T) {
	directory, _, digest, _ := writeDeployTestBundle(t)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/deployment-bundles/upload-plan" {
			t.Fatalf("unexpected request %s", r.URL.Path)
		}
		_ = json.NewEncoder(w).Encode(api.DeploymentBundleUploadPlanResponse{
			BundleDigest: digest,
			Uploads: []api.DeploymentBundleUpload{{
				Digest: "sha256:" + strings.Repeat("e", 64), Method: http.MethodPut,
				URL: "https://upload.invalid/object",
			}},
		})
	}))
	defer server.Close()
	t.Setenv(helmrAPIURLEnv, server.URL)
	t.Setenv(helmrAPIKeyEnv, "test-key")
	cmd := newRootCommand()
	cmd.SetOut(io.Discard)
	cmd.SetErr(io.Discard)
	cmd.SetArgs([]string{"deploy", "--bundle", directory})
	if err := cmd.Execute(); err == nil || !strings.Contains(err.Error(), "outside the bundle") {
		t.Fatalf("error = %v", err)
	}
}

func writeDeployTestBundle(t *testing.T) (string, []byte, string, string) {
	t.Helper()
	program := []byte("program")
	programDigest := sha256sum.DigestBytes(program)
	runtimeDigest := "sha256:" + strings.Repeat("f", 64)
	declaration := deployment.ProgramIndexDeclaration{
		Kind: deployment.DefinitionKindTask, DeclaredID: "hello",
		Task: &deployment.TaskManifest{
			Payload: deployment.SchemaManifest{Kind: deployment.SchemaKindNone},
			Run: deployment.RunManifest{Queue: "default", MaxDurationMs: 5000,
				Retry: deployment.RetryManifest{Enabled: false}},
		},
		Locator: &deployment.ProgramLocator{
			ExportName: "hello", ModulePath: ".helmr/modules/" + strings.Repeat("d", 64) + ".mjs",
			Slot: deployment.DeclarationSlotHandler,
		},
	}
	queues := []deployment.QueueInput{{Name: "default"}}
	plan := deployment.DeploymentPlan{FormatVersion: deployment.DeploymentPlanFormatVersion,
		Definitions: []deployment.ProgramIndexDeclaration{declaration}, Queues: queues}
	bundle := deployment.DeploymentBundle{
		Contract: deployment.DeploymentBundleContract,
		Platform: deployment.DeploymentBundlePlatform{Architecture: deployment.ArchitectureX8664,
			OS: deployment.DeploymentBundleTargetOS},
		Plan: plan,
		Runtime: deployment.DeploymentBundleRuntime{Contract: deployment.RuntimeContract,
			Artifact: deployment.BundleObject{Digest: runtimeDigest, SizeBytes: 4096,
				MediaType: deployment.RuntimeArtifactMediaType}},
		Program: deployment.ProgramOutput{
			Artifact: deployment.ProgramDescriptor{Digest: programDigest, SizeBytes: int64(len(program)),
				MediaType: deployment.ProgramArtifactMediaType},
			Index: deployment.ProgramIndex{Architecture: deployment.ArchitectureX8664,
				ConfigResultDigest: "sha256:" + strings.Repeat("c", 64),
				Declarations:       []deployment.ProgramIndexDeclaration{declaration}, Queues: queues,
				RuntimeContract: deployment.RuntimeContract, RuntimeDigest: runtimeDigest},
		},
		WorkspaceImages: []deployment.BundleWorkspaceImage{},
		Objects: []deployment.BundleObject{{Digest: programDigest, SizeBytes: int64(len(program)),
			MediaType: deployment.ProgramArtifactMediaType}},
	}
	raw, err := deployment.CanonicalDeploymentBundle(bundle)
	if err != nil {
		t.Fatal(err)
	}
	digest, err := deployment.DeploymentBundleDigest(raw)
	if err != nil {
		t.Fatal(err)
	}
	root := t.TempDir()
	objects := filepath.Join(root, "objects", "sha256")
	if err := os.MkdirAll(objects, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "bundle.json"), raw, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(objects, strings.TrimPrefix(programDigest, "sha256:")), program, 0o600); err != nil {
		t.Fatal(err)
	}
	return root, raw, digest, programDigest
}
