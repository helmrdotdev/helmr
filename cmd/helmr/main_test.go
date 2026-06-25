package main

import (
	"archive/tar"
	"bytes"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/helmrdotdev/helmr/internal/cli/session"
	"github.com/helmrdotdev/helmr/internal/version"
	"github.com/spf13/cobra"
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

func writeDeploymentEventSSE(t *testing.T, w http.ResponseWriter, r *http.Request, kind string) {
	t.Helper()
	if r.URL.Query().Get("follow") != "1" {
		t.Fatalf("events query = %s", r.URL.RawQuery)
	}
	w.Header().Set("Content-Type", "text/event-stream")
	_, _ = fmt.Fprintf(w, "id: 1\nevent: deployment_event\ndata: {\"id\":\"1\",\"deployment_id\":\"deployment-1\",\"kind\":%q,\"message\":\"Deployment lifecycle changed\"}\n\n", kind)
}

func requireNodeForEmbeddedAdapter(t *testing.T) string {
	t.Helper()
	nodePath, err := exec.LookPath("node")
	if err != nil {
		t.Skip("node is not available")
	}
	cmd := exec.Command(nodePath, "-e", `const [major = 0, minor = 0] = process.versions.node.split(".").map(Number); process.exit(major > 22 || (major === 22 && minor >= 18) ? 0 : 42)`)
	if err := cmd.Run(); err != nil {
		t.Skip("node >=22.18 is not available")
	}
	return nodePath
}

func linkLocalWorkspacePackage(t *testing.T, projectRoot string, name string, packagePath string) {
	t.Helper()
	repoRoot, err := filepath.Abs("../..")
	if err != nil {
		t.Fatal(err)
	}
	target := filepath.Join(repoRoot, packagePath)
	link := filepath.Join(projectRoot, "node_modules", filepath.FromSlash(name))
	if err := os.MkdirAll(filepath.Dir(link), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(target, link); err != nil {
		t.Fatal(err)
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
if [ "$1" = "-e" ]; then
	exit 0
fi
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
	cleanup := func() {
		deployAdapterRuntimePath = oldAdapterRuntime
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

func TestResumeCommandIsNotRegistered(t *testing.T) {
	cmd := newRootCommand()
	cmd.SetOut(&bytes.Buffer{})
	cmd.SetErr(&bytes.Buffer{})
	cmd.SetArgs([]string{"resume", "respond", "wait-1"})
	err := cmd.Execute()
	if err == nil || !strings.Contains(err.Error(), `unknown command "resume"`) {
		t.Fatalf("err = %v", err)
	}
}

func TestReplayCommandIsNotRegistered(t *testing.T) {
	cmd := newRootCommand()
	cmd.SetOut(&bytes.Buffer{})
	cmd.SetErr(&bytes.Buffer{})
	cmd.SetArgs([]string{"replay", "run-1"})
	err := cmd.Execute()
	if err == nil || !strings.Contains(err.Error(), `unknown command "replay"`) {
		t.Fatalf("err = %v", err)
	}
}

func TestGreenfieldCommandSurface(t *testing.T) {
	root := newRootCommand()
	for _, path := range [][]string{
		{"workspace"},
		{"task"},
		{"session"},
		{"run"},
		{"deployment"},
		{"sandbox"},
		{"token"},
		{"session", "stream"},
		{"session", "stream", "input", "send"},
		{"session", "stream", "input", "list"},
		{"session", "stream", "output", "list"},
		{"token", "create"},
		{"token", "get"},
		{"token", "complete"},
		{"token", "cancel"},
	} {
		if commandByPath(root, path...) == nil {
			t.Fatalf("command %q is not registered", strings.Join(path, " "))
		}
	}
	for _, path := range [][]string{
		{"workspaces"},
		{"tasks"},
		{"sessions"},
		{"runs"},
		{"waitpoint"},
		{"ps"},
		{"show"},
		{"logs"},
		{"events"},
		{"wait"},
		{"cancel"},
		{"promote"},
		{"session", "run"},
		{"session", "runs"},
		{"session", "logs"},
		{"session", "events"},
		{"session", "input"},
		{"session", "output"},
		{"session", "output", "follow"},
		{"session", "stream", "output", "follow"},
		{"token", "list"},
		{"token", "claim"},
		{"workspace", "file"},
		{"workspace", "cp"},
		{"workspace", "port"},
		{"workspace", "version"},
		{"workspace", "fork"},
	} {
		if commandByPath(root, path...) != nil {
			t.Fatalf("command %q must not be registered", strings.Join(path, " "))
		}
	}
}

func commandByPath(root *cobra.Command, path ...string) *cobra.Command {
	current := root
	for _, name := range path {
		found := false
		for _, child := range current.Commands() {
			if child.Name() == name {
				current = child
				found = true
				break
			}
		}
		if !found {
			return nil
		}
	}
	return current
}
