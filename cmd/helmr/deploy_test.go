package main

import (
	"bytes"
	"context"
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
	"github.com/helmrdotdev/helmr/internal/cas"
)

func TestDeployCommandUploadsCurrentDirectoryTaskArtifact(t *testing.T) {
	root := t.TempDir()
	if err := os.WriteFile(filepath.Join(root, "helmr.config.ts"), []byte(`export default { dirs: ["tasks"] }`), 0o644); err != nil {
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
if [ "$1" = "-e" ]; then
	exit 0
fi
if [ "$1" = "--import" ]; then
	shift 2
fi
case "$2" in
	inspect-config)
			printf '%s\n' '{"dirs":["tasks"],"ignorePatterns":["secrets/**"]}'
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
	oldTemp := deployArchiveTempDir
	deployAdapterRuntimePath = adapter
	deployArchiveTempDir = t.TempDir()
	adapterDir := t.TempDir()
	adapterPath := filepath.Join(adapterDir, "main.js")
	registerPath := filepath.Join(adapterDir, "register.mjs")
	if err := os.WriteFile(adapterPath, []byte(""), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(registerPath, []byte(""), 0o644); err != nil {
		t.Fatal(err)
	}
	t.Setenv("HELMR_ADAPTER_PATH", adapterPath)
	t.Setenv("HELMR_ADAPTER_REGISTER_PATH", registerPath)
	t.Cleanup(func() {
		deployAdapterRuntimePath = oldAdapterRuntime
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
		case r.Method == http.MethodGet && r.URL.Path == "/api/deployments/deployment-1/events":
			writeDeploymentEventSSE(t, w, r, "deployment.deployed")
		case r.Method == http.MethodGet && r.URL.Path == "/api/deployments/deployment-1":
			if r.URL.RawQuery != "" {
				t.Fatalf("deployment query = %s", r.URL.RawQuery)
			}
			_ = json.NewEncoder(w).Encode(api.DeploymentResponse{ID: "deployment-1", Version: "20260101.1", Status: "deployed"})
		case r.Method == http.MethodPost && r.URL.Path == "/api/deployments/deployment-1/promote":
			var request api.PromoteDeploymentRequest
			if err := json.NewDecoder(r.Body).Decode(&request); err != nil {
				t.Fatal(err)
			}
			if request.ProjectID != "" || request.EnvironmentID != "" || request.Reason != "deploy" {
				t.Fatalf("promotion request = %+v", request)
			}
			_ = json.NewEncoder(w).Encode(api.DeploymentResponse{ID: "deployment-1", Version: "20260101.1", Status: "deployed"})
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
	if strings.TrimSpace(out.String()) != "20260101.1" {
		t.Fatalf("output = %q", out.String())
	}
	if got := strings.Join(requests, ","); got != "POST /api/deployments,GET /api/deployments/deployment-1/events,GET /api/deployments/deployment-1,POST /api/deployments/deployment-1/promote" {
		t.Fatalf("requests = %s", got)
	}
	if metadata.ProjectID != "" || metadata.EnvironmentID != "" {
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

func TestDeployCommandWaitsWithResolvedConfiguredScope(t *testing.T) {
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
		case r.Method == http.MethodGet && r.URL.Path == "/api/deployments/deployment-1/events":
			writeDeploymentEventSSE(t, w, r, "deployment.deployed")
		case r.Method == http.MethodGet && r.URL.Path == "/api/deployments/deployment-1":
			if r.URL.RawQuery != "" {
				t.Fatalf("deployment query = %s", r.URL.RawQuery)
			}
			_ = json.NewEncoder(w).Encode(api.DeploymentResponse{ID: "deployment-1", Status: "deployed"})
		case r.Method == http.MethodPost && r.URL.Path == "/api/deployments/deployment-1/promote":
			_ = json.NewEncoder(w).Encode(api.DeploymentResponse{ID: "deployment-1", Status: "deployed"})
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
	if strings.TrimSpace(out.String()) != "deployment-1" {
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
		case r.Method == http.MethodPost && r.URL.Path == "/api/deployments":
			_ = json.NewEncoder(w).Encode(api.DeploymentResponse{
				ID:            "deployment-1",
				ProjectID:     "project-resolved",
				EnvironmentID: "environment-resolved",
				Status:        "queued",
			})
		case r.Method == http.MethodGet && r.URL.Path == "/api/deployments/deployment-1/events":
			eventRequests++
			if r.URL.Query().Get("follow") != "1" {
				t.Fatalf("events query = %s", r.URL.RawQuery)
			}
			w.Header().Set("Content-Type", "text/event-stream")
			if eventRequests == 1 {
				_, _ = fmt.Fprint(w, "id: 1\nevent: deployment_event\ndata: {\"id\":\"1\",\"deployment_id\":\"deployment-1\",\"kind\":\"deployment.building\",\"message\":\"Deployment build started\"}\n\n")
				return
			}
			if got := r.Header.Get("Last-Event-ID"); got != "1" {
				t.Fatalf("last event id = %q", got)
			}
			_, _ = fmt.Fprint(w, "id: 2\nevent: deployment_event\ndata: {\"id\":\"2\",\"deployment_id\":\"deployment-1\",\"kind\":\"deployment.deployed\",\"message\":\"Deployment build completed\"}\n\n")
		case r.Method == http.MethodGet && r.URL.Path == "/api/deployments/deployment-1":
			deploymentRequests++
			status := "queued"
			if eventRequests >= 2 {
				status = "deployed"
			}
			_ = json.NewEncoder(w).Encode(api.DeploymentResponse{ID: "deployment-1", Status: status})
		case r.Method == http.MethodPost && r.URL.Path == "/api/deployments/deployment-1/promote":
			_ = json.NewEncoder(w).Encode(api.DeploymentResponse{ID: "deployment-1", Status: "deployed"})
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
		if r.Method != http.MethodPost || r.URL.Path != "/api/deployments" {
			t.Fatalf("%s %s", r.Method, r.URL.Path)
		}
		_ = json.NewEncoder(w).Encode(api.DeploymentResponse{ID: "deployment-1", Status: "queued"})
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
	if strings.TrimSpace(out.String()) != "deployment-1" {
		t.Fatalf("output = %q", out.String())
	}
}

func TestDeployCommandJSONUsesProjectAndEnv(t *testing.T) {
	state, _ := installTestCLIConfig(t)
	root, _ := deployCommandFixture(t)
	var metadata api.CreateDeploymentRequest
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
		if err := json.Unmarshal([]byte(r.FormValue("metadata")), &metadata); err != nil {
			t.Fatal(err)
		}
		_ = json.NewEncoder(w).Encode(api.DeploymentResponse{ID: "deployment-1", ProjectID: "project-override", EnvironmentID: "prod", Status: "queued"})
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
	cmd.SetArgs([]string{"deploy", root, "--project", "project-override", "--env", "prod", "--detach", "--json"})
	if err := cmd.Execute(); err != nil {
		t.Fatal(err)
	}
	if metadata.ProjectID != "" || metadata.EnvironmentID != "" {
		t.Fatalf("metadata = %+v", metadata)
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
	if result.Type != "deployment_result" || result.Phase != "queued" || result.Deployment.ID != "deployment-1" {
		t.Fatalf("result = %+v", result)
	}
}

func TestLoadEnvFileDoesNotOverrideExistingEnv(t *testing.T) {
	t.Setenv("APP_EXISTING", "ambient")
	path := filepath.Join(t.TempDir(), "deploy.env")
	if err := os.WriteFile(path, []byte("APP_EXISTING=file\nexport\tAPP_SINGLE='quoted value'\nAPP_DOUBLE=\"line\\nnext\"\nAPP_COMMENT=value # comment\nAPP_HASH=\"value # not comment\"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := loadEnvFile(path); err != nil {
		t.Fatal(err)
	}
	if got := os.Getenv("APP_EXISTING"); got != "ambient" {
		t.Fatalf("APP_EXISTING = %q", got)
	}
	if got := os.Getenv("APP_SINGLE"); got != "quoted value" {
		t.Fatalf("APP_SINGLE = %q", got)
	}
	if got := os.Getenv("APP_DOUBLE"); got != "line\nnext" {
		t.Fatalf("APP_DOUBLE = %q", got)
	}
	if got := os.Getenv("APP_COMMENT"); got != "value" {
		t.Fatalf("APP_COMMENT = %q", got)
	}
	if got := os.Getenv("APP_HASH"); got != "value # not comment" {
		t.Fatalf("APP_HASH = %q", got)
	}
}

func TestLoadEnvFileRejectsReservedHelmrNamespace(t *testing.T) {
	path := filepath.Join(t.TempDir(), "deploy.env")
	if err := os.WriteFile(path, []byte("HELMR_ADAPTER_RUNTIME_PATH=/tmp/adapter\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	err := loadEnvFile(path)
	if err == nil || !strings.Contains(err.Error(), "HELMR_ADAPTER_RUNTIME_PATH uses the reserved HELMR_ namespace") {
		t.Fatalf("err = %v", err)
	}
}

func TestLoadEnvFileRejectsUnterminatedQuotedValue(t *testing.T) {
	path := filepath.Join(t.TempDir(), "deploy.env")
	if err := os.WriteFile(path, []byte("APP_VALUE=\"unterminated\\\"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	err := loadEnvFile(path)
	if err == nil || !strings.Contains(err.Error(), "quoted value is not terminated") {
		t.Fatalf("err = %v", err)
	}
}

func TestDeployCommandSkipPromotionDoesNotPromote(t *testing.T) {
	root, _ := deployCommandFixture(t)
	requests := []string{}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests = append(requests, r.Method+" "+r.URL.Path)
		switch {
		case r.Method == http.MethodPost && r.URL.Path == "/api/deployments":
			_ = json.NewEncoder(w).Encode(api.DeploymentResponse{
				ID:            "deployment-1",
				ProjectID:     "project-resolved",
				EnvironmentID: "environment-resolved",
				Status:        "queued",
			})
		case r.Method == http.MethodGet && r.URL.Path == "/api/deployments/deployment-1/events":
			writeDeploymentEventSSE(t, w, r, "deployment.deployed")
		case r.Method == http.MethodGet && r.URL.Path == "/api/deployments/deployment-1":
			_ = json.NewEncoder(w).Encode(api.DeploymentResponse{ID: "deployment-1", Version: "20260101.1", Status: "deployed"})
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
	if got := strings.Join(requests, ","); got != "POST /api/deployments,GET /api/deployments/deployment-1/events,GET /api/deployments/deployment-1" {
		t.Fatalf("requests = %s", got)
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
		case r.Method == http.MethodGet && r.URL.Path == "/api/deployments/deployment-1/events":
			writeDeploymentEventSSE(t, w, r, "deployment.failed")
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
	t.Setenv(helmrAPIURLEnv, server.URL)
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

func TestDeployCommandRequiresResolvedDeploymentScopeWithSession(t *testing.T) {
	state, _ := installTestCLIConfig(t)
	root, _ := deployCommandFixture(t)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost || r.URL.Path != "/api/projects/agents/environments/prod/deployments" {
			t.Fatalf("%s %s", r.Method, r.URL.Path)
		}
		_ = json.NewEncoder(w).Encode(api.DeploymentResponse{ID: "deployment-1", Status: "queued"})
	}))
	defer server.Close()
	t.Setenv(helmrAPIURLEnv, server.URL)
	if err := state.SaveLogin(server.URL, "session-test"); err != nil {
		t.Fatal(err)
	}

	cmd := newRootCommand()
	cmd.SetOut(&bytes.Buffer{})
	cmd.SetErr(&bytes.Buffer{})
	cmd.SetArgs([]string{"deploy", root, "--env", "prod"})
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

func TestResolveDeployAdapterExtractsEmbeddedAdapter(t *testing.T) {
	t.Setenv("HELMR_ADAPTER_PATH", "")
	t.Setenv("HELMR_ADAPTER_REGISTER_PATH", "")
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("XDG_CACHE_HOME", filepath.Join(home, ".cache"))

	adapter, err := resolveDeployAdapter()
	if err != nil {
		t.Fatal(err)
	}
	for _, path := range []string{adapter.MainPath, adapter.RegisterPath} {
		if !isFile(path) {
			t.Fatalf("adapter file was not extracted: %s", path)
		}
	}
}

func TestResolveDeployAdapterRequiresCompleteOverride(t *testing.T) {
	missing := filepath.Join(t.TempDir(), "register.mjs")
	t.Setenv("HELMR_ADAPTER_PATH", "")
	t.Setenv("HELMR_ADAPTER_REGISTER_PATH", missing)

	_, err := resolveDeployAdapter()
	if err == nil || !strings.Contains(err.Error(), "HELMR_ADAPTER_PATH and HELMR_ADAPTER_REGISTER_PATH must be set together") {
		t.Fatalf("err = %v", err)
	}
}

func TestRunDeployAdapterUsesEmbeddedAdapter(t *testing.T) {
	nodePath := requireNodeForEmbeddedAdapter(t)
	root := t.TempDir()
	if err := os.WriteFile(filepath.Join(root, "helmr.config.ts"), []byte(`import { defineConfig } from "@helmr/sdk"
export default defineConfig({ project: "agents", dirs: ["tasks"] })
`), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(root, "tasks"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "package.json"), []byte(`{"private":true,"type":"module","dependencies":{"@helmr/sdk":"latest"}}`), 0o644); err != nil {
		t.Fatal(err)
	}
	linkLocalWorkspacePackage(t, root, "@helmr/sdk", filepath.Join("sdk", "typescript"))
	linkLocalWorkspacePackage(t, root, "@helmr/proto", filepath.Join("proto", "typescript"))
	oldRuntime := deployAdapterRuntimePath
	deployAdapterRuntimePath = nodePath
	t.Cleanup(func() {
		deployAdapterRuntimePath = oldRuntime
	})
	t.Setenv("HELMR_ADAPTER_PATH", "")
	t.Setenv("HELMR_ADAPTER_REGISTER_PATH", "")
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("XDG_CACHE_HOME", filepath.Join(home, ".cache"))

	cmd := newRootCommand()
	cmd.SetContext(context.Background())
	stdout, err := runDeployAdapter(cmd, "inspect-config", root)
	if err != nil {
		t.Fatal(err)
	}
	var config deployConfig
	if err := json.Unmarshal(stdout, &config); err != nil {
		t.Fatal(err)
	}
	if config.Project != "agents" || len(config.Dirs) != 1 || config.Dirs[0] != "tasks" {
		t.Fatalf("config = %+v", config)
	}
}

func TestRunDeployAdapterReportsMissingRuntime(t *testing.T) {
	oldRuntime := deployAdapterRuntimePath
	deployAdapterRuntimePath = filepath.Join(t.TempDir(), "missing-node")
	t.Cleanup(func() {
		deployAdapterRuntimePath = oldRuntime
	})

	cmd := newRootCommand()
	cmd.SetContext(context.Background())
	_, err := runDeployAdapter(cmd, "inspect-config", t.TempDir())
	if err == nil || !strings.Contains(err.Error(), "install node >=22.18") {
		t.Fatalf("err = %v", err)
	}
}

func TestRunDeployAdapterReportsOldRuntime(t *testing.T) {
	runtime := filepath.Join(t.TempDir(), "node")
	if err := os.WriteFile(runtime, []byte(`#!/bin/sh
if [ "$1" = "-e" ]; then
  echo "node >=22.18 is required for helmr deploy; found 20.0.0" >&2
  exit 1
fi
exit 0
`), 0o755); err != nil {
		t.Fatal(err)
	}
	oldRuntime := deployAdapterRuntimePath
	deployAdapterRuntimePath = runtime
	t.Cleanup(func() {
		deployAdapterRuntimePath = oldRuntime
	})

	cmd := newRootCommand()
	cmd.SetContext(context.Background())
	_, err := runDeployAdapter(cmd, "inspect-config", t.TempDir())
	if err == nil || !strings.Contains(err.Error(), "node >=22.18 is required for helmr deploy; found 20.0.0") {
		t.Fatalf("err = %v", err)
	}
}
