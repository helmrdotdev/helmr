package main

import (
	"bytes"
	"encoding/json"
	"errors"
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
	"github.com/helmrdotdev/helmr/internal/sha256sum"
)

func TestDeployCommandUploadsCurrentDirectoryTaskArtifact(t *testing.T) {
	root := t.TempDir()
	if err := os.WriteFile(filepath.Join(root, "helmr.config.ts"), []byte(`export default { dirs: ["tasks"] }`), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "package.json"), []byte(`{"private":true,"type":"module","packageManager":"bun@1.3.10","devEngines":{"runtime":{"name":"node","version":"24.16.0","onFail":"error"}},"dependencies":{"@helmr/sdk":"latest"}}`), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "bun.lock"), []byte("{}\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(root, "node_modules", "@helmr", "sdk"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "node_modules", "@helmr", "sdk", "package.json"), []byte(`{"name":"@helmr/sdk"}`), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(root, "tasks"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "tasks", "deploy.ts"), []byte(`export const deploy = task("deploy", async () => {})`), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(root, "secrets"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "secrets", "token.txt"), []byte("secret"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "tasks", ".env.local"), []byte("TOKEN=secret"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(
		filepath.Join(root, ".helmrignore"),
		[]byte("node_modules/\nsecrets/\ntasks/.env.local\n"),
		0o644,
	); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "tasks", "generated.ts"), []byte("generated"), 0o644); err != nil {
		t.Fatal(err)
	}
	oldTemp := deployArchiveTempDir
	deployArchiveTempDir = t.TempDir()
	t.Cleanup(func() {
		deployArchiveTempDir = oldTemp
	})

	var metadata api.CreateDeploymentRequest
	var metadataFields map[string]json.RawMessage
	var uploaded []byte
	requests := []string{}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests = append(requests, r.Method+" "+r.URL.Path)
		switch {
		case r.Method == http.MethodPost && r.URL.Path == "/v1/deployments":
			if got := r.Header.Get("authorization"); got != "Bearer test-key" {
				t.Fatalf("auth = %s", got)
			}
			if err := r.ParseMultipartForm(32 << 20); err != nil {
				t.Fatal(err)
			}
			encodedMetadata := []byte(r.FormValue("metadata"))
			if err := json.Unmarshal(encodedMetadata, &metadata); err != nil {
				t.Fatal(err)
			}
			if err := json.Unmarshal(encodedMetadata, &metadataFields); err != nil {
				t.Fatal(err)
			}
			file, _, err := r.FormFile("deployment_source")
			if err != nil {
				t.Fatal(err)
			}
			defer file.Close()
			uploaded, err = io.ReadAll(file)
			if err != nil {
				t.Fatal(err)
			}
			_ = json.NewEncoder(w).Encode(api.DeploymentResponse{ID: "019c10d5-a6f7-7af1-8f5f-bb97bcc0dc35", ProjectID: "project-resolved", EnvironmentID: "environment-resolved", Status: "queued"})
		case r.Method == http.MethodGet && r.URL.Path == "/v1/deployments/019c10d5-a6f7-7af1-8f5f-bb97bcc0dc35/events":
			writeDeploymentEventSSE(t, w, r, "deployment.deployed")
		case r.Method == http.MethodGet && r.URL.Path == "/v1/deployments/019c10d5-a6f7-7af1-8f5f-bb97bcc0dc35":
			if r.URL.RawQuery != "" {
				t.Fatalf("deployment query = %s", r.URL.RawQuery)
			}
			_ = json.NewEncoder(w).Encode(api.DeploymentResponse{ID: "019c10d5-a6f7-7af1-8f5f-bb97bcc0dc35", Version: "20260101.1", Status: "deployed"})
		case r.Method == http.MethodPost && r.URL.Path == "/v1/deployments/019c10d5-a6f7-7af1-8f5f-bb97bcc0dc35/promote":
			var request api.PromoteDeploymentRequest
			if err := json.NewDecoder(r.Body).Decode(&request); err != nil {
				t.Fatal(err)
			}
			if request.Reason != "deploy" {
				t.Fatalf("promotion request = %+v", request)
			}
			_ = json.NewEncoder(w).Encode(api.DeploymentResponse{ID: "019c10d5-a6f7-7af1-8f5f-bb97bcc0dc35", Version: "20260101.1", Status: "deployed"})
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
	cmd.SetArgs([]string{"deploy", root, "--idempotency-key", "deploy-1"})
	if err := cmd.Execute(); err != nil {
		t.Fatal(err)
	}
	if metadata.IdempotencyKey != "deploy-1" {
		t.Fatalf("deployment idempotency key = %q", metadata.IdempotencyKey)
	}
	if metadata.ImageCacheMode != "prefer" {
		t.Fatalf("deployment image cache mode = %q", metadata.ImageCacheMode)
	}
	if strings.TrimSpace(out.String()) != "20260101.1" {
		t.Fatalf("output = %q", out.String())
	}
	if got := strings.Join(requests, ","); got != "POST /v1/deployments,GET /v1/deployments/019c10d5-a6f7-7af1-8f5f-bb97bcc0dc35/events,GET /v1/deployments/019c10d5-a6f7-7af1-8f5f-bb97bcc0dc35,POST /v1/deployments/019c10d5-a6f7-7af1-8f5f-bb97bcc0dc35/promote" {
		t.Fatalf("requests = %s", got)
	}
	if _, present := metadataFields["project_id"]; present {
		t.Fatalf("metadata contains project_id: %s", metadataFields["project_id"])
	}
	if _, present := metadataFields["environment_id"]; present {
		t.Fatalf("metadata contains environment_id: %s", metadataFields["environment_id"])
	}
	if metadata.ContentHash == "" || metadata.ContentHash != sha256sum.DigestBytes(uploaded) {
		t.Fatalf("content hash = %q, uploaded digest = %q", metadata.ContentHash, sha256sum.DigestBytes(uploaded))
	}
	if !bytes.Contains(uploaded, []byte("helmr.config.ts")) || !bytes.Contains(uploaded, []byte("package.json")) || !bytes.Contains(uploaded, []byte("tasks/deploy.ts")) {
		t.Fatalf("uploaded archive does not include expected files")
	}
	uploadedEntries := readTarEntries(t, uploaded)
	if uploadedEntries["secrets/token.txt"] || uploadedEntries["tasks/.env.local"] {
		t.Fatalf("uploaded archive includes ignored file: %+v", uploadedEntries)
	}
	if !uploadedEntries[".helmrignore"] || !uploadedEntries["tasks/generated.ts"] {
		t.Fatalf("source selection reused declaration ignorePatterns: %+v", uploadedEntries)
	}
}

func TestDeployCommandDoesNotExecuteOrMutateSource(t *testing.T) {
	root, _ := deployCommandFixture(t)
	if err := os.RemoveAll(filepath.Join(root, "node_modules")); err != nil {
		t.Fatal(err)
	}
	configPath := filepath.Join(root, "helmr.config.ts")
	config := []byte("throw new Error(\"must not execute locally\")\nexport default { dirs: [\"./tasks\"] }\n")
	if err := os.WriteFile(configPath, config, 0o644); err != nil {
		t.Fatal(err)
	}
	lockPath := filepath.Join(root, "bun.lock")
	lockBefore, err := os.ReadFile(lockPath)
	if err != nil {
		t.Fatal(err)
	}

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost || r.URL.Path != "/v1/deployments" {
			t.Fatalf("%s %s", r.Method, r.URL.Path)
		}
		_ = json.NewEncoder(w).Encode(api.DeploymentResponse{ID: "019c10d5-a6f7-7af1-8f5f-bb97bcc0dc35", Status: "queued"})
	}))
	defer server.Close()
	t.Setenv(helmrAPIURLEnv, server.URL)
	t.Setenv(helmrAPIKeyEnv, "test-key")

	cmd := newRootCommand()
	cmd.SetOut(&bytes.Buffer{})
	cmd.SetErr(&bytes.Buffer{})
	cmd.SetArgs([]string{"deploy", root, "--detach"})
	if err := cmd.Execute(); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(root, "node_modules")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("node_modules was created: %v", err)
	}
	lockAfter, err := os.ReadFile(lockPath)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(lockAfter, lockBefore) {
		t.Fatal("lockfile was mutated")
	}
	configAfter, err := os.ReadFile(configPath)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(configAfter, config) {
		t.Fatal("config was mutated")
	}
}

func TestDeployCommandWaitsWithResolvedConfiguredScope(t *testing.T) {
	root, _ := deployCommandFixture(t)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodPost && r.URL.Path == "/v1/deployments":
			_ = json.NewEncoder(w).Encode(api.DeploymentResponse{
				ID:            "019c10d5-a6f7-7af1-8f5f-bb97bcc0dc35",
				ProjectID:     "project-resolved",
				EnvironmentID: "environment-resolved",
				Status:        "queued",
			})
		case r.Method == http.MethodGet && r.URL.Path == "/v1/deployments/019c10d5-a6f7-7af1-8f5f-bb97bcc0dc35/events":
			writeDeploymentEventSSE(t, w, r, "deployment.deployed")
		case r.Method == http.MethodGet && r.URL.Path == "/v1/deployments/019c10d5-a6f7-7af1-8f5f-bb97bcc0dc35":
			if r.URL.RawQuery != "" {
				t.Fatalf("deployment query = %s", r.URL.RawQuery)
			}
			_ = json.NewEncoder(w).Encode(api.DeploymentResponse{ID: "019c10d5-a6f7-7af1-8f5f-bb97bcc0dc35", Status: "deployed"})
		case r.Method == http.MethodPost && r.URL.Path == "/v1/deployments/019c10d5-a6f7-7af1-8f5f-bb97bcc0dc35/promote":
			_ = json.NewEncoder(w).Encode(api.DeploymentResponse{ID: "019c10d5-a6f7-7af1-8f5f-bb97bcc0dc35", Status: "deployed"})
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
	cmd.SetArgs([]string{"deploy", root})
	if err := cmd.Execute(); err != nil {
		t.Fatal(err)
	}
	if strings.TrimSpace(out.String()) != "019c10d5-a6f7-7af1-8f5f-bb97bcc0dc35" {
		t.Fatalf("output = %q", out.String())
	}
}

func TestDeployCommandReconnectsDeploymentEventsUntilTerminal(t *testing.T) {
	root, _ := deployCommandFixture(t)
	oldReconnectDelay := deployEventReconnectDelay
	deployEventReconnectDelay = time.Millisecond
	t.Cleanup(func() { deployEventReconnectDelay = oldReconnectDelay })
	eventRequests := 0
	deploymentRequests := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodPost && r.URL.Path == "/v1/deployments":
			_ = json.NewEncoder(w).Encode(api.DeploymentResponse{
				ID:            "019c10d5-a6f7-7af1-8f5f-bb97bcc0dc35",
				ProjectID:     "project-resolved",
				EnvironmentID: "environment-resolved",
				Status:        "queued",
			})
		case r.Method == http.MethodGet && r.URL.Path == "/v1/deployments/019c10d5-a6f7-7af1-8f5f-bb97bcc0dc35/events":
			eventRequests++
			if r.URL.Query().Get("follow") != "1" {
				t.Fatalf("events query = %s", r.URL.RawQuery)
			}
			w.Header().Set("Content-Type", "text/event-stream")
			if eventRequests == 1 {
				_, _ = fmt.Fprint(w, "id: tc1.eyJzIjoxfQ\nevent: deployment_event\ndata: {\"id\":\"tc1.eyJzIjoxfQ\",\"deployment_id\":\"019c10d5-a6f7-7af1-8f5f-bb97bcc0dc35\",\"kind\":\"deployment.building\",\"message\":\"Deployment build started\"}\n\n")
				return
			}
			if got := r.Header.Get("Last-Event-ID"); got != "tc1.eyJzIjoxfQ" {
				t.Fatalf("last event id = %q", got)
			}
			_, _ = fmt.Fprint(w, "id: tc1.eyJzIjoyfQ\nevent: deployment_event\ndata: {\"id\":\"tc1.eyJzIjoyfQ\",\"deployment_id\":\"019c10d5-a6f7-7af1-8f5f-bb97bcc0dc35\",\"kind\":\"deployment.deployed\",\"message\":\"Deployment build completed\"}\n\n")
		case r.Method == http.MethodGet && r.URL.Path == "/v1/deployments/019c10d5-a6f7-7af1-8f5f-bb97bcc0dc35":
			deploymentRequests++
			status := "queued"
			if eventRequests >= 2 {
				status = "deployed"
			}
			_ = json.NewEncoder(w).Encode(api.DeploymentResponse{ID: "019c10d5-a6f7-7af1-8f5f-bb97bcc0dc35", Status: api.DeploymentStatus(status)})
		case r.Method == http.MethodPost && r.URL.Path == "/v1/deployments/019c10d5-a6f7-7af1-8f5f-bb97bcc0dc35/promote":
			_ = json.NewEncoder(w).Encode(api.DeploymentResponse{ID: "019c10d5-a6f7-7af1-8f5f-bb97bcc0dc35", Status: "deployed"})
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
	cmd.SetArgs([]string{"deploy", root})
	if err := cmd.Execute(); err != nil {
		t.Fatal(err)
	}
	if eventRequests != 2 {
		t.Fatalf("event requests = %d", eventRequests)
	}
	if deploymentRequests < 2 {
		t.Fatalf("deployment requests = %d", deploymentRequests)
	}
}

func TestDeployCommandDetachReturnsQueuedDeploymentID(t *testing.T) {
	root, _ := deployCommandFixture(t)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost || r.URL.Path != "/v1/deployments" {
			t.Fatalf("%s %s", r.Method, r.URL.Path)
		}
		_ = json.NewEncoder(w).Encode(api.DeploymentResponse{ID: "019c10d5-a6f7-7af1-8f5f-bb97bcc0dc35", Status: "queued"})
	}))
	defer server.Close()
	t.Setenv(helmrAPIURLEnv, server.URL)
	t.Setenv(helmrAPIKeyEnv, "test-key")

	var out bytes.Buffer
	cmd := newRootCommand()
	cmd.SetOut(&out)
	cmd.SetErr(&bytes.Buffer{})
	cmd.SetArgs([]string{"deploy", root, "--detach"})
	if err := cmd.Execute(); err != nil {
		t.Fatal(err)
	}
	if strings.TrimSpace(out.String()) != "019c10d5-a6f7-7af1-8f5f-bb97bcc0dc35" {
		t.Fatalf("output = %q", out.String())
	}
}

func TestFailingBuildFixtureReachesDeploymentCreation(t *testing.T) {
	if os.Getenv("HELMR_TEST_PREPARED_FAILING_BUILD_FIXTURE") != "1" {
		t.Skip("pre-AWS fixture contract is exercised by check-pre-aws.sh")
	}
	root, err := filepath.Abs("../../dev/workflows-failing-build")
	if err != nil {
		t.Fatal(err)
	}
	for _, path := range []string{
		"bun.lock",
		"vendor/helmr-sdk/package.json",
		"vendor/helmr-proto/package.json",
		"node_modules/@helmr/sdk/package.json",
	} {
		if info, err := os.Stat(filepath.Join(root, path)); err != nil || info.IsDir() {
			t.Fatalf("failing-build fixture must be prepared by sync-local-sdk.sh: %s", path)
		}
	}

	oldTemp := deployArchiveTempDir
	deployArchiveTempDir = t.TempDir()
	t.Cleanup(func() {
		deployArchiveTempDir = oldTemp
	})

	var uploaded []byte
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost || r.URL.Path != "/v1/deployments" {
			t.Fatalf("%s %s", r.Method, r.URL.Path)
		}
		if err := r.ParseMultipartForm(32 << 20); err != nil {
			t.Fatal(err)
		}
		file, _, err := r.FormFile("deployment_source")
		if err != nil {
			t.Fatal(err)
		}
		defer file.Close()
		uploaded, err = io.ReadAll(file)
		if err != nil {
			t.Fatal(err)
		}
		_ = json.NewEncoder(w).Encode(api.DeploymentResponse{
			ID:            "019c10d5-a6f7-7af1-8f5f-bb97bcc0dc35",
			ProjectID:     "project-resolved",
			EnvironmentID: "environment-resolved",
			Status:        "queued",
		})
	}))
	defer server.Close()
	t.Setenv(helmrAPIURLEnv, server.URL)
	t.Setenv(helmrAPIKeyEnv, "test-key")

	var out bytes.Buffer
	cmd := newRootCommand()
	cmd.SetOut(&out)
	cmd.SetErr(&bytes.Buffer{})
	cmd.SetArgs([]string{"deploy", root, "--detach", "--json"})
	if err := cmd.Execute(); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out.String(), `"type":"deployment_created"`) {
		t.Fatalf("deployment creation evidence missing: %s", out.String())
	}
	entries := readTarEntries(t, uploaded)
	for _, path := range []string{
		".helmrignore",
		"bun.lock",
		"package.json",
		"tasks/failing-build.ts",
		"vendor/helmr-sdk/package.json",
		"vendor/helmr-proto/package.json",
	} {
		if !entries[path] {
			t.Fatalf("uploaded failing-build source is not self-contained: %s", path)
		}
	}
}

func TestDeployCommandJSONUsesProjectAndEnv(t *testing.T) {
	state := installTestCLIConfig(t)
	root, _ := deployCommandFixture(t)
	var metadata api.CreateDeploymentRequest
	var metadataFields map[string]json.RawMessage
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost || r.URL.Path != "/api/projects/project-override/environments/prod/deployments" {
			t.Fatalf("%s %s", r.Method, r.URL.Path)
		}
		if got := r.Header.Get("authorization"); got != "Bearer session-test" {
			t.Fatalf("auth = %s", got)
		}
		if err := r.ParseMultipartForm(32 << 20); err != nil {
			t.Fatal(err)
		}
		encodedMetadata := []byte(r.FormValue("metadata"))
		if err := json.Unmarshal(encodedMetadata, &metadata); err != nil {
			t.Fatal(err)
		}
		if err := json.Unmarshal(encodedMetadata, &metadataFields); err != nil {
			t.Fatal(err)
		}
		_ = json.NewEncoder(w).Encode(api.DeploymentResponse{ID: "019c10d5-a6f7-7af1-8f5f-bb97bcc0dc35", ProjectID: "project-override", EnvironmentID: "prod", Status: "queued"})
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
	cmd.SetArgs([]string{"deploy", root, "--project", "project-override", "--env", "prod", "--detach", "--json", "--no-image-cache"})
	if err := cmd.Execute(); err != nil {
		t.Fatal(err)
	}
	if _, present := metadataFields["project_id"]; present {
		t.Fatalf("metadata contains project_id: %s", metadataFields["project_id"])
	}
	if _, present := metadataFields["environment_id"]; present {
		t.Fatalf("metadata contains environment_id: %s", metadataFields["environment_id"])
	}
	if metadata.ImageCacheMode != "bypass" {
		t.Fatalf("deployment image cache mode = %q", metadata.ImageCacheMode)
	}
	lines := strings.Split(strings.TrimSpace(out.String()), "\n")
	if len(lines) == 0 {
		t.Fatal("expected JSON output")
	}
	for _, line := range lines {
		var decoded struct {
			Type string `json:"type"`
		}
		if err := json.Unmarshal([]byte(line), &decoded); err != nil {
			t.Fatalf("decode JSON line %q: %v\n%s", line, err, out.String())
		}
		if decoded.Type == "" {
			t.Fatalf("JSON line missing type: %q", line)
		}
	}
	var result struct {
		Type       string                 `json:"type"`
		Phase      string                 `json:"phase"`
		Deployment api.DeploymentResponse `json:"deployment"`
	}
	if err := json.Unmarshal([]byte(lines[len(lines)-1]), &result); err != nil {
		t.Fatalf("decode result line: %v\n%s", err, out.String())
	}
	if result.Type != "deployment_result" || result.Phase != "queued" || result.Deployment.ID != "019c10d5-a6f7-7af1-8f5f-bb97bcc0dc35" {
		t.Fatalf("result = %+v", result)
	}
}

func TestDeployCommandSkipPromotionDoesNotPromote(t *testing.T) {
	root, _ := deployCommandFixture(t)
	requests := []string{}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests = append(requests, r.Method+" "+r.URL.Path)
		switch {
		case r.Method == http.MethodPost && r.URL.Path == "/v1/deployments":
			_ = json.NewEncoder(w).Encode(api.DeploymentResponse{
				ID:            "019c10d5-a6f7-7af1-8f5f-bb97bcc0dc35",
				ProjectID:     "project-resolved",
				EnvironmentID: "environment-resolved",
				Status:        "queued",
			})
		case r.Method == http.MethodGet && r.URL.Path == "/v1/deployments/019c10d5-a6f7-7af1-8f5f-bb97bcc0dc35/events":
			writeDeploymentEventSSE(t, w, r, "deployment.deployed")
		case r.Method == http.MethodGet && r.URL.Path == "/v1/deployments/019c10d5-a6f7-7af1-8f5f-bb97bcc0dc35":
			_ = json.NewEncoder(w).Encode(api.DeploymentResponse{ID: "019c10d5-a6f7-7af1-8f5f-bb97bcc0dc35", Version: "20260101.1", Status: "deployed"})
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
	cmd.SetArgs([]string{"deploy", root, "--skip-promotion"})
	if err := cmd.Execute(); err != nil {
		t.Fatal(err)
	}
	if strings.TrimSpace(out.String()) != "20260101.1" {
		t.Fatalf("output = %q", out.String())
	}
	if got := strings.Join(requests, ","); got != "POST /v1/deployments,GET /v1/deployments/019c10d5-a6f7-7af1-8f5f-bb97bcc0dc35/events,GET /v1/deployments/019c10d5-a6f7-7af1-8f5f-bb97bcc0dc35" {
		t.Fatalf("requests = %s", got)
	}
}

func TestDeployCommandReturnsFailedDeploymentError(t *testing.T) {
	root, _ := deployCommandFixture(t)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodPost && r.URL.Path == "/v1/deployments":
			_ = json.NewEncoder(w).Encode(api.DeploymentResponse{
				ID:            "019c10d5-a6f7-7af1-8f5f-bb97bcc0dc35",
				ProjectID:     "project-resolved",
				EnvironmentID: "environment-resolved",
				Status:        "queued",
			})
		case r.Method == http.MethodGet && r.URL.Path == "/v1/deployments/019c10d5-a6f7-7af1-8f5f-bb97bcc0dc35/events":
			writeDeploymentEventSSE(t, w, r, "deployment.failed")
		case r.Method == http.MethodGet && r.URL.Path == "/v1/deployments/019c10d5-a6f7-7af1-8f5f-bb97bcc0dc35":
			_ = json.NewEncoder(w).Encode(api.DeploymentResponse{
				ID:     "019c10d5-a6f7-7af1-8f5f-bb97bcc0dc35",
				Status: "failed",
				Error:  &api.DeploymentErrorResponse{Message: "build failed"},
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
	cmd.SetArgs([]string{"deploy", root})
	err := cmd.Execute()
	if err == nil || !strings.Contains(err.Error(), "deployment 019c10d5-a6f7-7af1-8f5f-bb97bcc0dc35 failed: build failed") {
		t.Fatalf("err = %v", err)
	}
	if strings.TrimSpace(out.String()) != "" {
		t.Fatalf("output = %q", out.String())
	}
}

func TestDeployCommandRequiresResolvedDeploymentScopeWithSession(t *testing.T) {
	state := installTestCLIConfig(t)
	root, _ := deployCommandFixture(t)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost || r.URL.Path != "/api/projects/agents/environments/prod/deployments" {
			t.Fatalf("%s %s", r.Method, r.URL.Path)
		}
		_ = json.NewEncoder(w).Encode(api.DeploymentResponse{ID: "019c10d5-a6f7-7af1-8f5f-bb97bcc0dc35", Status: "queued"})
	}))
	defer server.Close()
	t.Setenv(helmrAPIURLEnv, server.URL)
	if err := state.SaveLogin(server.URL, "session-test"); err != nil {
		t.Fatal(err)
	}

	cmd := newRootCommand()
	cmd.SetOut(&bytes.Buffer{})
	cmd.SetErr(&bytes.Buffer{})
	cmd.SetArgs([]string{"deploy", root, "--project", "agents", "--env", "prod"})
	err := cmd.Execute()
	if err == nil || !strings.Contains(err.Error(), "deployment 019c10d5-a6f7-7af1-8f5f-bb97bcc0dc35 response did not include resolved project_id and environment_id") {
		t.Fatalf("err = %v", err)
	}
}

func TestDeployCommandRequiresExplicitSessionScope(t *testing.T) {
	state := installTestCLIConfig(t)
	root, _ := deployCommandFixture(t)
	server := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {
		t.Fatal("deployment request must not be sent without explicit scope")
	}))
	defer server.Close()
	t.Setenv(helmrAPIURLEnv, server.URL)
	if err := state.SaveLogin(server.URL, "session-test"); err != nil {
		t.Fatal(err)
	}

	tests := []struct {
		name string
		args []string
		want string
	}{
		{name: "project", args: []string{"deploy", root, "--env", "prod"}, want: "--project is required with helmr login"},
		{name: "environment", args: []string{"deploy", root, "--project", "agents"}, want: "--env is required with helmr login"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			cmd := newRootCommand()
			cmd.SetOut(&bytes.Buffer{})
			cmd.SetErr(&bytes.Buffer{})
			cmd.SetArgs(test.args)
			err := cmd.Execute()
			if err == nil || !strings.Contains(err.Error(), test.want) {
				t.Fatalf("err = %v", err)
			}
		})
	}
}

func TestDeployCommandRejectsScopeFlagsWithEnvironmentKey(t *testing.T) {
	root, _ := deployCommandFixture(t)
	t.Setenv(helmrAPIKeyEnv, "test-key")
	t.Setenv(helmrAPIURLEnv, "http://localhost:8080")

	cmd := newRootCommand()
	cmd.SetOut(&bytes.Buffer{})
	cmd.SetErr(&bytes.Buffer{})
	cmd.SetArgs([]string{"deploy", root, "--project", "agents", "--env", "prod"})
	err := cmd.Execute()
	if err == nil || !strings.Contains(err.Error(), "--project and --env require helmr login") {
		t.Fatalf("err = %v", err)
	}
}

func TestDeployCommandRequiresPackageJSON(t *testing.T) {
	root := t.TempDir()
	cmd := newRootCommand()
	cmd.SetOut(&bytes.Buffer{})
	cmd.SetErr(&bytes.Buffer{})
	cmd.SetArgs([]string{"deploy", root})

	err := cmd.Execute()
	if err == nil || !strings.Contains(err.Error(), "submitted source package.json must be a regular file") {
		t.Fatalf("err = %v", err)
	}
}
