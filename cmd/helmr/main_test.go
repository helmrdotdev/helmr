package main

import (
	"archive/tar"
	"bytes"
	"fmt"
	"io"
	"net/http"
	"os"
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
	_, _ = fmt.Fprintf(w, "id: 1\nevent: deployment_event\ndata: {\"id\":\"1\",\"deployment_id\":\"019c10d5-a6f7-7af1-8f5f-bb97bcc0dc35\",\"kind\":%q,\"message\":\"Deployment lifecycle changed\"}\n\n", kind)
}

func deployCommandFixture(t *testing.T) (string, func()) {
	t.Helper()
	root := t.TempDir()
	if err := os.WriteFile(filepath.Join(root, "helmr.config.ts"), []byte(`export default { dirs: ["./tasks"] }`), 0o644); err != nil {
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
	if err := os.WriteFile(filepath.Join(root, ".helmrignore"), []byte("node_modules/\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(root, "tasks"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "tasks", "deploy.ts"), []byte(`export const deploy = task("deploy", async () => {})`), 0o644); err != nil {
		t.Fatal(err)
	}
	oldTemp := deployArchiveTempDir
	deployArchiveTempDir = t.TempDir()
	cleanup := func() {
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

func TestGreenfieldCommandSurface(t *testing.T) {
	root := newRootCommand()
	for _, path := range [][]string{
		{"workspace"},
		{"task"},
		{"actor"},
		{"run"},
		{"schedule"},
		{"deployment"},
		{"token"},
		{"task", "start"},
		{"actor", "start"},
		{"actor", "get"},
		{"actor", "input", "send"},
		{"actor", "output", "read"},
		{"actor", "close"},
		{"schedule", "list"},
		{"schedule", "get"},
		{"token", "get"},
		{"token", "complete"},
		{"token", "cancel"},
	} {
		if commandByPath(root, path...) == nil {
			t.Fatalf("command %q is not registered", strings.Join(path, " "))
		}
	}
	if commandByPath(root, "run", "start") != nil {
		t.Fatal("removed command \"run start\" is still registered")
	}
	for _, path := range [][]string{
		{"schedule", "create"},
		{"schedule", "update"},
		{"schedule", "delete"},
		{"schedule", "activate"},
		{"schedule", "deactivate"},
		{"actor", "list"},
		{"actor", "update"},
		{"actor", "cancel"},
	} {
		if commandByPath(root, path...) != nil {
			t.Fatalf("unsupported command %q is registered", strings.Join(path, " "))
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
