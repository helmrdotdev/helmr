package main

import (
	"archive/tar"
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/helmrdotdev/helmr/internal/api"
	"github.com/helmrdotdev/helmr/internal/cas"
	"github.com/helmrdotdev/helmr/internal/cli/session"
	"github.com/helmrdotdev/helmr/internal/version"
	"github.com/zalando/go-keyring"
)

func TestRootCommandPrintsVersion(t *testing.T) {
	const testVersion = "v0.0.0-test"
	originalVersion := version.Version
	version.Version = testVersion
	t.Cleanup(func() {
		version.Version = originalVersion
	})

	var out bytes.Buffer
	cmd := newRootCommand()
	cmd.SetOut(&out)
	cmd.SetErr(&bytes.Buffer{})
	cmd.SetArgs([]string{"--version"})
	if err := cmd.Execute(); err != nil {
		t.Fatal(err)
	}
	if strings.TrimSpace(out.String()) != testVersion {
		t.Fatalf("version output = %q", out.String())
	}
}

func TestInitCommandCreatesStarterProject(t *testing.T) {
	root := t.TempDir()
	var out bytes.Buffer
	cmd := newRootCommand()
	cmd.SetOut(&out)
	cmd.SetErr(&bytes.Buffer{})
	cmd.SetArgs([]string{"init", "--dir", root})
	if err := cmd.Execute(); err != nil {
		t.Fatal(err)
	}
	config, err := os.ReadFile(filepath.Join(root, "helmr.config.ts"))
	if err != nil {
		t.Fatal(err)
	}
	task, err := os.ReadFile(filepath.Join(root, "tasks", "hello.ts"))
	if err != nil {
		t.Fatal(err)
	}
	pkg, err := os.ReadFile(filepath.Join(root, "package.json"))
	if err != nil {
		t.Fatal(err)
	}
	if string(config) != starterHelmrConfig {
		t.Fatalf("config = %q", config)
	}
	if string(pkg) != starterPackageJSON() {
		t.Fatalf("package = %q", pkg)
	}
	if string(task) != starterHelloTask {
		t.Fatalf("task = %q", task)
	}
	if !strings.Contains(out.String(), "created helmr.config.ts") || !strings.Contains(out.String(), "created or updated package.json") || !strings.Contains(out.String(), "created tasks/hello.ts") {
		t.Fatalf("output = %q", out.String())
	}
}

func TestInitCommandRejectsExistingFilesWithoutForce(t *testing.T) {
	root := t.TempDir()
	if err := os.WriteFile(filepath.Join(root, "helmr.config.ts"), []byte("custom\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	cmd := newRootCommand()
	cmd.SetOut(&bytes.Buffer{})
	cmd.SetErr(&bytes.Buffer{})
	cmd.SetArgs([]string{"init", "--dir", root})
	err := cmd.Execute()
	if err == nil || !strings.Contains(err.Error(), "already exists; pass --force to overwrite") {
		t.Fatalf("err = %v", err)
	}
	contents, err := os.ReadFile(filepath.Join(root, "helmr.config.ts"))
	if err != nil {
		t.Fatal(err)
	}
	if string(contents) != "custom\n" {
		t.Fatalf("config was overwritten: %q", contents)
	}
}

func TestInitCommandMergesExistingPackageJSONWithoutForce(t *testing.T) {
	root := t.TempDir()
	if err := os.WriteFile(filepath.Join(root, "package.json"), []byte(`{"type":"module","dependencies":{"left-pad":"1.3.0"}}`), 0o644); err != nil {
		t.Fatal(err)
	}
	cmd := newRootCommand()
	cmd.SetOut(&bytes.Buffer{})
	cmd.SetErr(&bytes.Buffer{})
	cmd.SetArgs([]string{"init", "--dir", root})
	if err := cmd.Execute(); err != nil {
		t.Fatal(err)
	}
	packageContents, err := os.ReadFile(filepath.Join(root, "package.json"))
	if err != nil {
		t.Fatal(err)
	}
	var packageJSON map[string]any
	if err := json.Unmarshal(packageContents, &packageJSON); err != nil {
		t.Fatal(err)
	}
	dependencies := packageJSON["dependencies"].(map[string]any)
	if dependencies["left-pad"] != "1.3.0" || dependencies["@helmr/sdk"] == "" {
		t.Fatalf("dependencies were not merged: %s", packageContents)
	}
	if packageJSON["packageManager"] != "bun@1.3.10" {
		t.Fatalf("packageManager was not set: %s", packageContents)
	}
}

func TestInitCommandForceOverwritesStarterFiles(t *testing.T) {
	root := t.TempDir()
	if err := os.WriteFile(filepath.Join(root, "helmr.config.ts"), []byte("custom\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "package.json"), []byte(`{"scripts":{"test":"echo ok"},"dependencies":{"left-pad":"1.3.0"}}`), 0o644); err != nil {
		t.Fatal(err)
	}
	cmd := newRootCommand()
	cmd.SetOut(&bytes.Buffer{})
	cmd.SetErr(&bytes.Buffer{})
	cmd.SetArgs([]string{"init", "--dir", root, "--force"})
	if err := cmd.Execute(); err != nil {
		t.Fatal(err)
	}
	contents, err := os.ReadFile(filepath.Join(root, "helmr.config.ts"))
	if err != nil {
		t.Fatal(err)
	}
	if string(contents) != starterHelmrConfig {
		t.Fatalf("config = %q", contents)
	}
	packageContents, err := os.ReadFile(filepath.Join(root, "package.json"))
	if err != nil {
		t.Fatal(err)
	}
	var packageJSON map[string]any
	if err := json.Unmarshal(packageContents, &packageJSON); err != nil {
		t.Fatal(err)
	}
	if packageJSON["scripts"].(map[string]any)["test"] != "echo ok" {
		t.Fatalf("scripts were not preserved: %s", packageContents)
	}
	dependencies := packageJSON["dependencies"].(map[string]any)
	if dependencies["left-pad"] != "1.3.0" || dependencies["@helmr/sdk"] == "" {
		t.Fatalf("dependencies were not merged: %s", packageContents)
	}
	if packageJSON["packageManager"] != "bun@1.3.10" {
		t.Fatalf("packageManager was not set: %s", packageContents)
	}
}

func TestRunCommandCreatesGitHubRun(t *testing.T) {
	var request api.CreateRunRequest
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost || r.URL.Path != "/api/runs" {
			t.Fatalf("%s %s", r.Method, r.URL.Path)
		}
		if got := r.Header.Get("authorization"); got != "Bearer test-key" {
			t.Fatalf("auth = %s", got)
		}
		body, err := io.ReadAll(r.Body)
		if err != nil {
			t.Fatal(err)
		}
		var raw map[string]json.RawMessage
		if err := json.Unmarshal(body, &raw); err != nil {
			t.Fatal(err)
		}
		if _, ok := raw["workspace"]; !ok {
			t.Fatalf("request JSON missing workspace: %s", body)
		}
		var rawWorkspace map[string]json.RawMessage
		if err := json.Unmarshal(raw["workspace"], &rawWorkspace); err != nil {
			t.Fatal(err)
		}
		if _, ok := rawWorkspace["repository"]; !ok {
			t.Fatalf("request JSON missing workspace.repository: %s", body)
		}
		if _, ok := raw["source"]; ok {
			t.Fatalf("request JSON included source: %s", body)
		}
		if err := json.Unmarshal(body, &request); err != nil {
			t.Fatal(err)
		}
		_ = json.NewEncoder(w).Encode(api.RunResponse{
			ID:        "run-1",
			TaskID:    request.TaskID,
			Status:    "queued",
			CreatedAt: time.Unix(0, 0).UTC(),
			UpdatedAt: time.Unix(0, 0).UTC(),
		})
	}))
	defer server.Close()
	t.Setenv(helmrURLEnv, server.URL)
	t.Setenv(helmrAPIKeyEnv, "test-key")

	var out bytes.Buffer
	cmd := newRootCommand()
	cmd.SetOut(&out)
	cmd.SetErr(&bytes.Buffer{})
	cmd.SetArgs([]string{
		"run", "deploy",
		"--repo", "helmrdotdev/helmr",
		"--ref", "main",
		"--subpath", "apps/api",
		"-p", "env=prod",
		"--secret", "TOKEN=vault:github-token",
		"--project", "project-1",
		"--environment", "env-1",
		"--max-duration-seconds", "60",
		"--idempotency-key", "deploy-prod",
		"--idempotency-key-ttl", "24h",
	})
	if err := cmd.Execute(); err != nil {
		t.Fatal(err)
	}
	if strings.TrimSpace(out.String()) != "run-1" {
		t.Fatalf("output = %q", out.String())
	}
	if request.TaskID != "deploy" || request.Workspace.Repository != "helmrdotdev/helmr" || request.Workspace.Ref != "main" || request.Workspace.Subpath != "apps/api" || request.Options.MaxDurationSeconds != 60 {
		t.Fatalf("request = %+v", request)
	}
	if request.ProjectID != "project-1" || request.EnvironmentID != "env-1" {
		t.Fatalf("scope = %s/%s", request.ProjectID, request.EnvironmentID)
	}
	if request.Options.IdempotencyKey != "deploy-prod" || request.Options.IdempotencyKeyTTL != "24h" {
		t.Fatalf("idempotency options = %+v", request.Options)
	}
	if string(request.Payload) != `{"env":"prod"}` {
		t.Fatalf("payload = %s", request.Payload)
	}
	if request.Secrets["TOKEN"] != "vault:github-token" {
		t.Fatalf("secrets = %+v", request.Secrets)
	}
}

func TestDeployCommandUploadsCurrentDirectoryTaskArtifact(t *testing.T) {
	root := t.TempDir()
	if err := os.WriteFile(filepath.Join(root, "helmr.config.ts"), []byte(`export default { project: "agents", dirs: ["tasks"] }`), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "package.json"), []byte(`{"private":true,"packageManager":"bun@1.3.10","dependencies":{"@helmr/sdk":"latest"}}`), 0o644); err != nil {
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
	adapter := filepath.Join(t.TempDir(), "adapter")
	adapterScript := `#!/bin/sh
if [ "$1" = "--import" ]; then
	shift 2
fi
case "$2" in
	inspect-config)
		printf '%s\n' '{"project":"agents","dirs":["tasks"],"ignorePatterns":["secrets/**"]}'
		;;
	parse)
		printf '%s\n' '{"tasks":{"deploy":{"modulePath":"tasks/deploy.ts","exportName":"deploy","bundle":{"sandbox":{"resources":{"cpu":3,"memory":"4Gi"}}}}}}'
		;;
	*)
		echo "unexpected adapter command: $*" >&2
		exit 1
		;;
esac
`
	if err := os.WriteFile(adapter, []byte(adapterScript), 0o755); err != nil {
		t.Fatal(err)
	}
	oldAdapterRuntime := deployAdapterRuntimePath
	oldAdapter := deployAdapterPath
	oldTemp := deployArchiveTempDir
	deployAdapterRuntimePath = adapter
	deployAdapterPath = "ignored"
	deployArchiveTempDir = t.TempDir()
	registerPath := filepath.Join(t.TempDir(), "register.mjs")
	if err := os.WriteFile(registerPath, []byte(""), 0o644); err != nil {
		t.Fatal(err)
	}
	t.Setenv("HELMR_ADAPTER_PATH", "ignored")
	t.Setenv("HELMR_ADAPTER_REGISTER_PATH", registerPath)
	t.Cleanup(func() {
		deployAdapterRuntimePath = oldAdapterRuntime
		deployAdapterPath = oldAdapter
		deployArchiveTempDir = oldTemp
	})

	var metadata api.CreateDeploymentRequest
	var uploaded []byte
	requests := []string{}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests = append(requests, r.Method+" "+r.URL.Path)
		switch {
		case r.Method == http.MethodPost && r.URL.Path == "/api/deployments":
			if got := r.Header.Get("authorization"); got != "Bearer test-key" {
				t.Fatalf("auth = %s", got)
			}
			if err := r.ParseMultipartForm(32 << 20); err != nil {
				t.Fatal(err)
			}
			if err := json.Unmarshal([]byte(r.FormValue("metadata")), &metadata); err != nil {
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
			_ = json.NewEncoder(w).Encode(api.DeploymentResponse{ID: "deployment-1", ProjectID: "project-resolved", EnvironmentID: "environment-resolved", Status: "queued"})
		case r.Method == http.MethodGet && r.URL.Path == "/api/deployments/deployment-1":
			if r.URL.Query().Get("project_id") != "project-resolved" || r.URL.Query().Get("environment_id") != "environment-resolved" {
				t.Fatalf("deployment query = %s", r.URL.RawQuery)
			}
			_ = json.NewEncoder(w).Encode(api.DeploymentResponse{ID: "deployment-1", Status: "deployed"})
		default:
			t.Fatalf("%s %s", r.Method, r.URL.Path)
		}
	}))
	defer server.Close()
	t.Setenv(helmrURLEnv, server.URL)
	t.Setenv(helmrAPIKeyEnv, "test-key")

	var out bytes.Buffer
	cmd := newRootCommand()
	cmd.SetOut(&out)
	cmd.SetErr(&bytes.Buffer{})
	cmd.SetArgs([]string{"deploy", root, "--environment", "prod"})
	if err := cmd.Execute(); err != nil {
		t.Fatal(err)
	}
	if strings.TrimSpace(out.String()) != "deployment-1" {
		t.Fatalf("output = %q", out.String())
	}
	if got := strings.Join(requests, ","); got != "POST /api/deployments,GET /api/deployments/deployment-1" {
		t.Fatalf("requests = %s", got)
	}
	if metadata.ProjectID != "agents" || metadata.EnvironmentID != "prod" {
		t.Fatalf("metadata = %+v", metadata)
	}
	if metadata.ContentHash == "" || metadata.ContentHash != cas.DigestBytes(uploaded) {
		t.Fatalf("content hash = %q, uploaded digest = %q", metadata.ContentHash, cas.DigestBytes(uploaded))
	}
	if !bytes.Contains(uploaded, []byte("helmr.config.ts")) || !bytes.Contains(uploaded, []byte("package.json")) || !bytes.Contains(uploaded, []byte("tasks/deploy.ts")) {
		t.Fatalf("uploaded archive does not include expected files")
	}
	uploadedEntries := readTarEntries(t, uploaded)
	if uploadedEntries["secrets/token.txt"] || uploadedEntries["tasks/.env.local"] {
		t.Fatalf("uploaded archive includes ignored file: %+v", uploadedEntries)
	}
}

func TestDeployCommandWaitsWithResolvedDefaultScope(t *testing.T) {
	root, _ := deployCommandFixture(t)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodPost && r.URL.Path == "/api/deployments":
			_ = json.NewEncoder(w).Encode(api.DeploymentResponse{
				ID:            "deployment-1",
				ProjectID:     "project-resolved",
				EnvironmentID: "environment-resolved",
				Status:        "queued",
			})
		case r.Method == http.MethodGet && r.URL.Path == "/api/deployments/deployment-1":
			if r.URL.Query().Get("project_id") != "project-resolved" || r.URL.Query().Get("environment_id") != "environment-resolved" {
				t.Fatalf("deployment query = %s", r.URL.RawQuery)
			}
			_ = json.NewEncoder(w).Encode(api.DeploymentResponse{ID: "deployment-1", Status: "deployed"})
		default:
			t.Fatalf("%s %s", r.Method, r.URL.Path)
		}
	}))
	defer server.Close()
	t.Setenv(helmrURLEnv, server.URL)
	t.Setenv(helmrAPIKeyEnv, "test-key")

	var out bytes.Buffer
	cmd := newRootCommand()
	cmd.SetOut(&out)
	cmd.SetErr(&bytes.Buffer{})
	cmd.SetArgs([]string{"deploy", root})
	if err := cmd.Execute(); err != nil {
		t.Fatal(err)
	}
	if strings.TrimSpace(out.String()) != "deployment-1" {
		t.Fatalf("output = %q", out.String())
	}
}

func TestDeployCommandDetachReturnsQueuedDeploymentID(t *testing.T) {
	root, _ := deployCommandFixture(t)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost || r.URL.Path != "/api/deployments" {
			t.Fatalf("%s %s", r.Method, r.URL.Path)
		}
		_ = json.NewEncoder(w).Encode(api.DeploymentResponse{ID: "deployment-1", Status: "queued"})
	}))
	defer server.Close()
	t.Setenv(helmrURLEnv, server.URL)
	t.Setenv(helmrAPIKeyEnv, "test-key")

	var out bytes.Buffer
	cmd := newRootCommand()
	cmd.SetOut(&out)
	cmd.SetErr(&bytes.Buffer{})
	cmd.SetArgs([]string{"deploy", root, "--detach"})
	if err := cmd.Execute(); err != nil {
		t.Fatal(err)
	}
	if strings.TrimSpace(out.String()) != "deployment-1" {
		t.Fatalf("output = %q", out.String())
	}
}

func TestDeployCommandReturnsFailedDeploymentError(t *testing.T) {
	root, _ := deployCommandFixture(t)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodPost && r.URL.Path == "/api/deployments":
			_ = json.NewEncoder(w).Encode(api.DeploymentResponse{
				ID:            "deployment-1",
				ProjectID:     "project-resolved",
				EnvironmentID: "environment-resolved",
				Status:        "queued",
			})
		case r.Method == http.MethodGet && r.URL.Path == "/api/deployments/deployment-1":
			_ = json.NewEncoder(w).Encode(api.DeploymentResponse{
				ID:     "deployment-1",
				Status: "failed",
				Error:  &api.DeploymentErrorResponse{Message: "build failed"},
			})
		default:
			t.Fatalf("%s %s", r.Method, r.URL.Path)
		}
	}))
	defer server.Close()
	t.Setenv(helmrURLEnv, server.URL)
	t.Setenv(helmrAPIKeyEnv, "test-key")

	var out bytes.Buffer
	cmd := newRootCommand()
	cmd.SetOut(&out)
	cmd.SetErr(&bytes.Buffer{})
	cmd.SetArgs([]string{"deploy", root})
	err := cmd.Execute()
	if err == nil || !strings.Contains(err.Error(), "deployment deployment-1 failed: build failed") {
		t.Fatalf("err = %v", err)
	}
	if strings.TrimSpace(out.String()) != "" {
		t.Fatalf("output = %q", out.String())
	}
}

func TestDeployCommandRequiresResolvedDeploymentScope(t *testing.T) {
	root, _ := deployCommandFixture(t)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost || r.URL.Path != "/api/deployments" {
			t.Fatalf("%s %s", r.Method, r.URL.Path)
		}
		_ = json.NewEncoder(w).Encode(api.DeploymentResponse{ID: "deployment-1", Status: "queued"})
	}))
	defer server.Close()
	t.Setenv(helmrURLEnv, server.URL)
	t.Setenv(helmrAPIKeyEnv, "test-key")

	cmd := newRootCommand()
	cmd.SetOut(&bytes.Buffer{})
	cmd.SetErr(&bytes.Buffer{})
	cmd.SetArgs([]string{"deploy", root})
	err := cmd.Execute()
	if err == nil || !strings.Contains(err.Error(), "deployment deployment-1 response did not include resolved project_id and environment_id") {
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
	if err == nil || !strings.Contains(err.Error(), "package.json is required for Helmr task projects") {
		t.Fatalf("err = %v", err)
	}
}

func TestDeployCommandRequiresHelmrSDKDependency(t *testing.T) {
	root := t.TempDir()
	if err := os.WriteFile(filepath.Join(root, "package.json"), []byte(`{"packageManager":"bun@1.3.10","dependencies":{"left-pad":"1.3.0"}}`), 0o644); err != nil {
		t.Fatal(err)
	}
	cmd := newRootCommand()
	cmd.SetOut(&bytes.Buffer{})
	cmd.SetErr(&bytes.Buffer{})
	cmd.SetArgs([]string{"deploy", root})

	err := cmd.Execute()
	if err == nil || !strings.Contains(err.Error(), "package.json must declare @helmr/sdk in dependencies") {
		t.Fatalf("err = %v", err)
	}
}

func TestDeployCommandRejectsProjectFlag(t *testing.T) {
	cmd := newRootCommand()
	cmd.SetOut(&bytes.Buffer{})
	cmd.SetErr(&bytes.Buffer{})
	cmd.SetArgs([]string{"deploy", "--project", "agents"})

	err := cmd.Execute()
	if err == nil || !strings.Contains(err.Error(), "unknown flag: --project") {
		t.Fatalf("err = %v", err)
	}
}

func TestPrepareLocalDeploySourceInstallsFreshTaskProject(t *testing.T) {
	root := t.TempDir()
	if err := os.WriteFile(filepath.Join(root, "package.json"), []byte(`{"private":true,"packageManager":"bun@1.3.10","dependencies":{"@helmr/sdk":"latest"}}`), 0o644); err != nil {
		t.Fatal(err)
	}
	binDir := filepath.Join(t.TempDir(), "bin")
	if err := os.MkdirAll(binDir, 0o755); err != nil {
		t.Fatal(err)
	}
	bun := filepath.Join(binDir, "bun")
	if err := os.WriteFile(bun, []byte(`#!/bin/sh
if [ "$1" != "install" ]; then
  echo "unexpected bun args: $*" >&2
  exit 1
fi
mkdir -p node_modules/@helmr/sdk
printf '{"name":"@helmr/sdk"}' > node_modules/@helmr/sdk/package.json
printf '%s\n' "$*" > bun-invocation.txt
`), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", binDir+string(os.PathListSeparator)+os.Getenv("PATH"))

	if err := prepareLocalDeploySource(context.Background(), root); err != nil {
		t.Fatal(err)
	}
	invocation, err := os.ReadFile(filepath.Join(root, "bun-invocation.txt"))
	if err != nil {
		t.Fatal(err)
	}
	if got := strings.TrimSpace(string(invocation)); got != "install" {
		t.Fatalf("bun invocation = %q", got)
	}
}

func TestDeployAdapterPathFindsPackagedMainJS(t *testing.T) {
	t.Setenv("HELMR_ADAPTER_PATH", "")
	root := t.TempDir()
	binDir := filepath.Join(root, "bin")
	if err := os.MkdirAll(filepath.Join(binDir, "adapter"), 0o755); err != nil {
		t.Fatal(err)
	}
	mainJS := filepath.Join(binDir, "adapter", "main.js")
	if err := os.WriteFile(mainJS, []byte("console.log('adapter')\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	exePath := filepath.Join(binDir, "helmr")
	if err := os.WriteFile(exePath, []byte("binary"), 0o755); err != nil {
		t.Fatal(err)
	}
	oldAdapter := deployAdapterPath
	oldExecutable := deployExecutable
	deployAdapterPath = ""
	deployExecutable = func() (string, error) { return exePath, nil }
	t.Cleanup(func() {
		deployAdapterPath = oldAdapter
		deployExecutable = oldExecutable
	})

	path, err := resolveDeployAdapterPath()
	if err != nil {
		t.Fatal(err)
	}
	if path != mainJS {
		t.Fatalf("adapter path = %q", path)
	}
}

func TestDeployAdapterRegisterPathRequiresHook(t *testing.T) {
	missing := filepath.Join(t.TempDir(), "register.mjs")
	t.Setenv("HELMR_ADAPTER_REGISTER_PATH", missing)

	_, err := resolveDeployAdapterRegisterPath()
	if err == nil || !strings.Contains(err.Error(), "adapter register hook not found") {
		t.Fatalf("err = %v", err)
	}
}

func deployCommandFixture(t *testing.T) (string, func()) {
	t.Helper()
	root := t.TempDir()
	if err := os.WriteFile(filepath.Join(root, "helmr.config.ts"), []byte(`export default { project: "agents", dirs: ["tasks"] }`), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "package.json"), []byte(`{"private":true,"packageManager":"bun@1.3.10","dependencies":{"@helmr/sdk":"latest"}}`), 0o644); err != nil {
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
	adapter := filepath.Join(t.TempDir(), "adapter")
	adapterScript := `#!/bin/sh
if [ "$1" = "--import" ]; then
	shift 2
fi
case "$2" in
	inspect-config)
		printf '%s\n' '{"project":"agents","dirs":["tasks"]}'
		;;
	parse)
		printf '%s\n' '{"tasks":{"deploy":{"modulePath":"tasks/deploy.ts","exportName":"deploy","bundle":{"sandbox":{"resources":{"cpu":3,"memory":"4Gi"}}}}}}'
		;;
	*)
		echo "unexpected adapter command: $*" >&2
		exit 1
		;;
esac
`
	if err := os.WriteFile(adapter, []byte(adapterScript), 0o755); err != nil {
		t.Fatal(err)
	}
	oldAdapterRuntime := deployAdapterRuntimePath
	oldAdapter := deployAdapterPath
	oldTemp := deployArchiveTempDir
	deployAdapterRuntimePath = adapter
	deployAdapterPath = "ignored"
	deployArchiveTempDir = t.TempDir()
	registerPath := filepath.Join(t.TempDir(), "register.mjs")
	if err := os.WriteFile(registerPath, []byte(""), 0o644); err != nil {
		t.Fatal(err)
	}
	t.Setenv("HELMR_ADAPTER_PATH", "ignored")
	t.Setenv("HELMR_ADAPTER_REGISTER_PATH", registerPath)
	cleanup := func() {
		deployAdapterRuntimePath = oldAdapterRuntime
		deployAdapterPath = oldAdapter
		deployArchiveTempDir = oldTemp
	}
	t.Cleanup(cleanup)
	return root, cleanup
}

func readTarEntries(t *testing.T, archive []byte) map[string]bool {
	t.Helper()
	reader := tar.NewReader(bytes.NewReader(archive))
	entries := map[string]bool{}
	for {
		header, err := reader.Next()
		if err == io.EOF {
			return entries
		}
		if err != nil {
			t.Fatal(err)
		}
		entries[header.Name] = true
	}
}

func TestRunCommandReadsPayloadFile(t *testing.T) {
	var request api.CreateRunRequest
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost || r.URL.Path != "/api/runs" {
			t.Fatalf("%s %s", r.Method, r.URL.Path)
		}
		if err := json.NewDecoder(r.Body).Decode(&request); err != nil {
			t.Fatal(err)
		}
		_ = json.NewEncoder(w).Encode(api.RunResponse{
			ID:        "run-1",
			TaskID:    request.TaskID,
			Status:    "queued",
			CreatedAt: time.Unix(0, 0).UTC(),
			UpdatedAt: time.Unix(0, 0).UTC(),
		})
	}))
	defer server.Close()
	t.Setenv(helmrURLEnv, server.URL)
	t.Setenv(helmrAPIKeyEnv, "test-key")

	payloadPath := filepath.Join(t.TempDir(), "payload.json")
	if err := os.WriteFile(payloadPath, []byte(`{"env":"prod","count":2}`), 0o644); err != nil {
		t.Fatal(err)
	}
	cmd := newRootCommand()
	cmd.SetOut(&bytes.Buffer{})
	cmd.SetErr(&bytes.Buffer{})
	cmd.SetArgs([]string{"run", "deploy", "--repo", "helmrdotdev/helmr", "--ref", "main", "--payload-file", payloadPath})
	if err := cmd.Execute(); err != nil {
		t.Fatal(err)
	}
	var payload map[string]any
	if err := json.Unmarshal(request.Payload, &payload); err != nil {
		t.Fatal(err)
	}
	if payload["env"] != "prod" || payload["count"] != float64(2) {
		t.Fatalf("payload = %s", request.Payload)
	}
}

func TestRunCommandRejectsPayloadFileCombinations(t *testing.T) {
	called := false
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		called = true
	}))
	defer server.Close()
	t.Setenv(helmrURLEnv, server.URL)
	t.Setenv(helmrAPIKeyEnv, "test-key")

	payloadPath := filepath.Join(t.TempDir(), "payload.json")
	if err := os.WriteFile(payloadPath, []byte(`{"env":"prod"}`), 0o644); err != nil {
		t.Fatal(err)
	}
	for _, args := range [][]string{
		{"run", "deploy", "--ref", "main", "--payload-file", payloadPath, "--payload-json", `{"env":"prod"}`},
		{"run", "deploy", "--ref", "main", "--payload-file", payloadPath, "-p", "env=prod"},
	} {
		cmd := newRootCommand()
		cmd.SetOut(&bytes.Buffer{})
		cmd.SetErr(&bytes.Buffer{})
		cmd.SetArgs(args)
		err := cmd.Execute()
		if err == nil || !strings.Contains(err.Error(), "--payload-file cannot be combined") {
			t.Fatalf("args %v err = %v", args, err)
		}
	}
	if called {
		t.Fatal("server was called")
	}
}

func TestLoginCommandStoresDeviceToken(t *testing.T) {
	state, _ := installTestCLIConfig(t)
	var sawStart bool
	var sawToken bool
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if got := r.Header.Get("authorization"); got != "" {
			t.Fatalf("auth = %s", got)
		}
		switch r.URL.Path {
		case "/api/auth/device/start":
			sawStart = true
			_ = json.NewEncoder(w).Encode(api.DeviceStartResponse{
				DeviceCode:              "device-token",
				UserCode:                "ABCD-EFGH",
				VerificationURI:         "https://helmr.example.test/auth/device",
				VerificationURIComplete: "https://helmr.example.test/auth/device?code=ABCD-EFGH",
				ExpiresInSeconds:        60,
				IntervalSeconds:         1,
			})
		case "/api/auth/device/token":
			sawToken = true
			var request api.DeviceTokenRequest
			if err := json.NewDecoder(r.Body).Decode(&request); err != nil {
				t.Fatal(err)
			}
			if request.DeviceCode != "device-token" {
				t.Fatalf("request = %+v", request)
			}
			_ = json.NewEncoder(w).Encode(api.DeviceTokenResponse{
				AccessToken: "session_test",
				TokenType:   "bearer",
			})
		default:
			t.Fatalf("unexpected path %s", r.URL.Path)
		}
	}))
	defer server.Close()

	var out bytes.Buffer
	cmd := newRootCommand()
	cmd.SetOut(&out)
	cmd.SetErr(&bytes.Buffer{})
	cmd.SetArgs([]string{"login", "--no-browser", server.URL})
	if err := cmd.Execute(); err != nil {
		t.Fatal(err)
	}
	if !sawStart || !sawToken {
		t.Fatalf("sawStart=%v sawToken=%v", sawStart, sawToken)
	}
	cfg, err := state.Load()
	if err != nil {
		t.Fatal(err)
	}
	if cfg.DefaultHost != server.URL {
		t.Fatalf("default host = %q", cfg.DefaultHost)
	}
	token, err := state.Token(server.URL)
	if err != nil {
		t.Fatal(err)
	}
	if token != "session_test" {
		t.Fatalf("token = %q", token)
	}
	if !strings.Contains(out.String(), "Code: ABCD-EFGH") || !strings.Contains(out.String(), "Logged in to "+server.URL) {
		t.Fatalf("output = %q", out.String())
	}
}

func TestLogoutCommandRevokesAndDeletesStoredToken(t *testing.T) {
	state, _ := installTestCLIConfig(t)
	var sawLogout bool
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost || r.URL.Path != "/api/auth/logout" {
			t.Fatalf("%s %s", r.Method, r.URL.Path)
		}
		if got := r.Header.Get("authorization"); got != "Bearer session_test" {
			t.Fatalf("auth = %s", got)
		}
		sawLogout = true
		w.WriteHeader(http.StatusNoContent)
	}))
	defer server.Close()
	if err := state.SaveLogin(server.URL, "session_test"); err != nil {
		t.Fatal(err)
	}

	var out bytes.Buffer
	cmd := newRootCommand()
	cmd.SetOut(&out)
	cmd.SetErr(&bytes.Buffer{})
	cmd.SetArgs([]string{"logout"})
	if err := cmd.Execute(); err != nil {
		t.Fatal(err)
	}
	if !sawLogout {
		t.Fatal("logout endpoint was not called")
	}
	if _, err := state.Token(server.URL); !errors.Is(err, session.ErrNotFound) {
		t.Fatalf("token after logout error = %v, want ErrNotFound", err)
	}
	if !strings.Contains(out.String(), "Logged out from "+server.URL) {
		t.Fatalf("output = %q", out.String())
	}
}

func TestCommandUsesSavedLoginWhenEnvIsUnset(t *testing.T) {
	state, _ := installTestCLIConfig(t)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet || r.URL.Path != "/api/runs/run-1/logs" {
			t.Fatalf("%s %s", r.Method, r.URL.Path)
		}
		if got := r.Header.Get("authorization"); got != "Bearer stored-key" {
			t.Fatalf("auth = %s", got)
		}
		_ = json.NewEncoder(w).Encode(api.LogSnapshotResponse{
			StdoutBase64: base64.StdEncoding.EncodeToString([]byte("hello\n")),
			StderrBase64: "",
		})
	}))
	defer server.Close()
	if err := state.SaveLogin(server.URL, "stored-key"); err != nil {
		t.Fatal(err)
	}

	var out bytes.Buffer
	cmd := newRootCommand()
	cmd.SetOut(&out)
	cmd.SetErr(&bytes.Buffer{})
	cmd.SetArgs([]string{"logs", "run-1"})
	if err := cmd.Execute(); err != nil {
		t.Fatal(err)
	}
	if out.String() != "hello\n" {
		t.Fatalf("output = %q", out.String())
	}
}

func TestRunCommandRejectsLocalSecretSchemesBeforeRequest(t *testing.T) {
	called := false
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		called = true
	}))
	defer server.Close()
	t.Setenv(helmrURLEnv, server.URL)
	t.Setenv(helmrAPIKeyEnv, "test-key")

	for _, binding := range []string{"TOKEN=env:TOKEN", "TOKEN=file:/tmp/token"} {
		cmd := newRootCommand()
		cmd.SetOut(&bytes.Buffer{})
		cmd.SetErr(&bytes.Buffer{})
		cmd.SetArgs([]string{"run", "deploy", "--ref", "main", "--secret", binding})
		err := cmd.Execute()
		if err == nil || !strings.Contains(err.Error(), "unsupported secret binding scheme") {
			t.Fatalf("binding %q err = %v", binding, err)
		}
	}
	if called {
		t.Fatal("server was called")
	}
}

func installTestCLIConfig(t *testing.T) (*session.Store, *testKeyring) {
	t.Helper()
	keyring := &testKeyring{values: map[string]string{}}
	state := session.NewStore(filepath.Join(t.TempDir(), "helmr"), keyring)
	previous := newSessionStore
	newSessionStore = func() (*session.Store, error) {
		return state, nil
	}
	t.Cleanup(func() {
		newSessionStore = previous
	})
	return state, keyring
}

type testKeyring struct {
	values map[string]string
}

func (k *testKeyring) Set(service, user, password string) error {
	k.values[service+"\x00"+user] = password
	return nil
}

func (k *testKeyring) Get(service, user string) (string, error) {
	value, ok := k.values[service+"\x00"+user]
	if !ok {
		return "", keyring.ErrNotFound
	}
	return value, nil
}

func (k *testKeyring) Delete(service, user string) error {
	key := service + "\x00" + user
	if _, ok := k.values[key]; !ok {
		return keyring.ErrNotFound
	}
	delete(k.values, key)
	return nil
}

func TestRunCommandRejectsInvalidTaskIDBeforeRequest(t *testing.T) {
	called := false
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		called = true
	}))
	defer server.Close()
	t.Setenv(helmrURLEnv, server.URL)
	t.Setenv(helmrAPIKeyEnv, "test-key")

	cmd := newRootCommand()
	cmd.SetOut(&bytes.Buffer{})
	cmd.SetErr(&bytes.Buffer{})
	cmd.SetArgs([]string{"run", "bad task", "--repo", "helmrdotdev/helmr", "--ref", "main"})
	err := cmd.Execute()
	if err == nil || !strings.Contains(err.Error(), "task_id") {
		t.Fatalf("err = %v", err)
	}
	if called {
		t.Fatal("server was called")
	}
}

func TestResumeApproveCommand(t *testing.T) {
	var request api.ResumeApprovalRequest
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost || r.URL.Path != "/api/runs/run-1/waitpoints/wait-1/approve" {
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
	t.Setenv(helmrURLEnv, server.URL)
	t.Setenv(helmrAPIKeyEnv, "test-key")

	cmd := newRootCommand()
	cmd.SetOut(&bytes.Buffer{})
	cmd.SetErr(&bytes.Buffer{})
	cmd.SetArgs([]string{"resume", "approve", "run-1", "wait-1", "--reason", "looks good"})
	if err := cmd.Execute(); err != nil {
		t.Fatal(err)
	}
	if request.Reason != "looks good" {
		t.Fatalf("request = %+v", request)
	}
}

func TestResumeMessageCommandRequiresText(t *testing.T) {
	called := false
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		called = true
	}))
	defer server.Close()
	t.Setenv(helmrURLEnv, server.URL)
	t.Setenv(helmrAPIKeyEnv, "test-key")

	cmd := newRootCommand()
	cmd.SetOut(&bytes.Buffer{})
	cmd.SetErr(&bytes.Buffer{})
	cmd.SetArgs([]string{"resume", "message", "run-1", "wait-1"})
	err := cmd.Execute()
	if err == nil || !strings.Contains(err.Error(), "--text is required") {
		t.Fatalf("err = %v", err)
	}
	if called {
		t.Fatal("server was called")
	}
}

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
	t.Setenv(helmrURLEnv, server.URL)
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
	t.Setenv(helmrURLEnv, server.URL)
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
	t.Setenv(helmrURLEnv, server.URL)
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
	t.Setenv(helmrURLEnv, server.URL)
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

func assertWaitpointPolicyRequest(t *testing.T, label string, configJSON json.RawMessage, wantLabel string, wantEmails []string) {
	t.Helper()
	if label != wantLabel {
		t.Fatalf("label = %q", label)
	}
	var config api.WaitpointPolicyConfig
	if err := json.Unmarshal(configJSON, &config); err != nil {
		t.Fatal(err)
	}
	if len(config.Deliveries) != 1 || config.Deliveries[0].Type != "email" || strings.Join(config.Deliveries[0].To, ",") != strings.Join(wantEmails, ",") {
		t.Fatalf("deliveries = %+v", config.Deliveries)
	}
	if config.Resolution == nil || config.Resolution.Type != "any" || config.Resolution.Count != 1 {
		t.Fatalf("resolution = %+v", config.Resolution)
	}
}

func TestLogsCommandPrintsStreams(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet || r.URL.Path != "/api/runs/run-1/logs" {
			t.Fatalf("%s %s", r.Method, r.URL.Path)
		}
		_ = json.NewEncoder(w).Encode(api.LogSnapshotResponse{
			StdoutBase64: base64.StdEncoding.EncodeToString([]byte("hello\n")),
			StderrBase64: base64.StdEncoding.EncodeToString([]byte("warn\n")),
		})
	}))
	defer server.Close()
	t.Setenv(helmrURLEnv, server.URL)
	t.Setenv(helmrAPIKeyEnv, "test-key")

	var out, stderr bytes.Buffer
	cmd := newRootCommand()
	cmd.SetOut(&out)
	cmd.SetErr(&stderr)
	cmd.SetArgs([]string{"logs", "run-1"})
	if err := cmd.Execute(); err != nil {
		t.Fatal(err)
	}
	if out.String() != "hello\n" || stderr.String() != "warn\n" {
		t.Fatalf("stdout=%q stderr=%q", out.String(), stderr.String())
	}
}

func TestEventsCommandPrintsJSONLines(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet || r.URL.Path != "/api/runs/run-1/events" || r.URL.Query().Get("cursor") != "4" || r.URL.Query().Get("limit") != "2" {
			t.Fatalf("%s %s?%s", r.Method, r.URL.Path, r.URL.RawQuery)
		}
		_ = json.NewEncoder(w).Encode(api.RunEventPage{
			Events: []api.RunEvent{{ID: "5", Kind: "run.started"}},
			Cursor: 5,
		})
	}))
	defer server.Close()
	t.Setenv(helmrURLEnv, server.URL)
	t.Setenv(helmrAPIKeyEnv, "test-key")

	var out bytes.Buffer
	cmd := newRootCommand()
	cmd.SetOut(&out)
	cmd.SetErr(&bytes.Buffer{})
	cmd.SetArgs([]string{"events", "run-1", "--cursor", "4", "--limit", "2"})
	if err := cmd.Execute(); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out.String(), `"kind":"run.started"`) {
		t.Fatalf("output = %q", out.String())
	}
}

func TestSecretSetCommand(t *testing.T) {
	var request api.SetSecretRequest
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPut || r.URL.Path != "/api/secrets/github-token" {
			t.Fatalf("%s %s", r.Method, r.URL.Path)
		}
		if got := r.Header.Get("authorization"); got != "Bearer test-key" {
			t.Fatalf("auth = %s", got)
		}
		if err := json.NewDecoder(r.Body).Decode(&request); err != nil {
			t.Fatal(err)
		}
		_ = json.NewEncoder(w).Encode(api.SecretResponse{Name: "github-token"})
	}))
	defer server.Close()
	t.Setenv(helmrURLEnv, server.URL)
	t.Setenv(helmrAPIKeyEnv, "test-key")

	var out bytes.Buffer
	cmd := newRootCommand()
	cmd.SetOut(&out)
	cmd.SetErr(&bytes.Buffer{})
	cmd.SetArgs([]string{"secret", "set", "github-token", "secret-value", "--project", "project-1", "--environment", "env-1"})
	if err := cmd.Execute(); err != nil {
		t.Fatal(err)
	}
	if request.Value != "secret-value" {
		t.Fatalf("request = %+v", request)
	}
	if request.ProjectID != "project-1" || request.EnvironmentID != "env-1" {
		t.Fatalf("scope = %+v", request)
	}
	if strings.TrimSpace(out.String()) != "github-token" {
		t.Fatalf("output = %q", out.String())
	}
}

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
	if request.Name != "QA Environment" || request.Slug != "qa-environment" {
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
				}},
			}}})
		case r.Method == http.MethodGet && r.URL.Path == "/api/projects/"+projectID+"/environments/"+environmentID:
			_ = json.NewEncoder(w).Encode(api.EnvironmentSummary{
				ID:        environmentID,
				ProjectID: projectID,
				Slug:      "qa",
				Name:      "QA",
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
	if request.Name != "QA" || request.Slug != "staging" {
		t.Fatalf("request = %+v", request)
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

func TestRunCommandPreservesInvalidSubpathForServerValidation(t *testing.T) {
	var request api.CreateRunRequest
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if err := json.NewDecoder(r.Body).Decode(&request); err != nil {
			t.Fatal(err)
		}
		w.WriteHeader(http.StatusBadRequest)
		_ = json.NewEncoder(w).Encode(map[string]string{"error": "workspace.subpath must be relative"})
	}))
	defer server.Close()
	t.Setenv(helmrURLEnv, server.URL)
	t.Setenv(helmrAPIKeyEnv, "test-key")

	cmd := newRootCommand()
	cmd.SetOut(&bytes.Buffer{})
	cmd.SetErr(&bytes.Buffer{})
	cmd.SetArgs([]string{"run", "deploy", "--repo", "helmrdotdev/helmr", "--ref", "main", "--subpath", "/apps/api"})
	err := cmd.Execute()
	if err == nil || !strings.Contains(err.Error(), "workspace.subpath must be relative") {
		t.Fatalf("err = %v", err)
	}
	if request.Workspace.Subpath != "/apps/api" {
		t.Fatalf("subpath = %q", request.Workspace.Subpath)
	}
}

func TestControlClientRejectsPlainHTTPNonLoopback(t *testing.T) {
	t.Setenv(helmrURLEnv, "http://helmr.example")
	t.Setenv(helmrAPIKeyEnv, "test-key")

	_, err := controlClient()
	if err == nil || !strings.Contains(err.Error(), "plaintext non-loopback") {
		t.Fatalf("err = %v", err)
	}
}

func TestControlClientRejectsURLQueryAndFragment(t *testing.T) {
	t.Setenv(helmrAPIKeyEnv, "test-key")
	for _, raw := range []string{"https://helmr.example?x=1", "https://helmr.example/#fragment"} {
		t.Setenv(helmrURLEnv, raw)
		_, err := controlClient()
		if err == nil || !strings.Contains(err.Error(), "must not include query or fragment") {
			t.Fatalf("controlClient(%q) err = %v", raw, err)
		}
	}
}
