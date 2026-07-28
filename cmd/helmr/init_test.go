package main

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/helmrdotdev/helmr/internal/version"
)

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
	ignore, err := os.ReadFile(filepath.Join(root, ".helmrignore"))
	if err != nil {
		t.Fatal(err)
	}
	tsconfig, err := os.ReadFile(filepath.Join(root, "tsconfig.json"))
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
	if string(ignore) != starterHelmrIgnore {
		t.Fatalf("ignore = %q", ignore)
	}
	if string(tsconfig) != starterTSConfig {
		t.Fatalf("tsconfig = %q", tsconfig)
	}
	for _, message := range []string{
		"created .helmrignore",
		"created helmr.config.ts",
		"created package.json",
		"created tasks/hello.ts",
		"created tsconfig.json",
	} {
		if !strings.Contains(out.String(), message) {
			t.Fatalf("output = %q", out.String())
		}
	}
	if !strings.Contains(string(config), `dirs: ["tasks"]`) {
		t.Fatalf("output = %q", out.String())
	}
}

func TestStarterSDKVersionUsesLatestForNonReleaseBuilds(t *testing.T) {
	originalVersion := version.Version
	t.Cleanup(func() {
		version.Version = originalVersion
	})

	tests := map[string]string{
		"dev":                    "latest",
		"0.0.0-dev+abc123":       "latest",
		"0.0.0-dev+abc123-dirty": "latest",
		"abc123":                 "latest",
		"v1.2.3":                 "1.2.3",
		"v1.2.3-rc.1":            "1.2.3-rc.1",
	}
	for input, want := range tests {
		version.Version = input
		if got := starterSDKVersion(); got != want {
			t.Fatalf("starterSDKVersion(%q) = %q, want %q", input, got, want)
		}
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

func TestInitCommandRejectsExistingPackageJSONWithoutForce(t *testing.T) {
	root := t.TempDir()
	if err := os.WriteFile(filepath.Join(root, "package.json"), []byte(`{"type":"module","dependencies":{"left-pad":"1.3.0"}}`), 0o644); err != nil {
		t.Fatal(err)
	}
	cmd := newRootCommand()
	cmd.SetOut(&bytes.Buffer{})
	cmd.SetErr(&bytes.Buffer{})
	cmd.SetArgs([]string{"init", "--dir", root})
	err := cmd.Execute()
	if err == nil || !strings.Contains(err.Error(), "package.json already exists; pass --force to overwrite") {
		t.Fatalf("err = %v", err)
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
	if string(packageContents) != starterPackageJSON() {
		t.Fatalf("package = %q", packageContents)
	}
}
