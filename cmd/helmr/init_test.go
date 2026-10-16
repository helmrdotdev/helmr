package main

import (
	"bytes"
	"encoding/json"
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
	agentSource, err := os.ReadFile(filepath.Join(root, "agents", "hello.ts"))
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
	var generated struct {
		DevEngines struct {
			Runtime struct{ Name, Version, OnFail string }
		}
	}
	if err := json.Unmarshal(pkg, &generated); err != nil {
		t.Fatal(err)
	}
	engine := generated.DevEngines.Runtime
	if engine.Name != "node" || engine.Version != version.Node() || engine.OnFail != "error" {
		t.Fatalf("generated Node requirement = %+v", engine)
	}
	if string(pkg) != starterPackageJSON() {
		t.Fatalf("package = %q", pkg)
	}
	if string(agentSource) != starterHelloAgent {
		t.Fatalf("agentSource = %q", agentSource)
	}
	for _, expected := range []string{`helloComputer = template({`, `hello = agent({`, `template: helloComputer`, `memory: "1GiB"`} {
		if !strings.Contains(string(agentSource), expected) {
			t.Fatalf("starter Agent is missing current SDK syntax %q", expected)
		}
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
		"created agents/hello.ts",
		"created tsconfig.json",
	} {
		if !strings.Contains(out.String(), message) {
			t.Fatalf("output = %q", out.String())
		}
	}
	if !strings.Contains(string(config), `dirs: ["agents"]`) {
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

func runInit(t *testing.T, args ...string) (string, error) {
	t.Helper()
	var out bytes.Buffer
	cmd := newRootCommand()
	cmd.SetOut(&out)
	cmd.SetErr(&bytes.Buffer{})
	cmd.SetArgs(append([]string{"init"}, args...))
	err := cmd.Execute()
	return out.String(), err
}

func TestInitTemplateWritesTheNamedAgentProject(t *testing.T) {
	root := t.TempDir()
	out, err := runInit(t, "--dir", root, "--template", "claude-code", "--name", "noether")
	if err != nil {
		t.Fatal(err)
	}
	for _, line := range []string{"created agents/noether.ts", "created agents/settings.ts", "created package.json", "Agent: noether (named after Emmy Noether)", "ANTHROPIC_API_KEY"} {
		if !strings.Contains(out, line) {
			t.Fatalf("output = %q", out)
		}
	}
	settings, err := os.ReadFile(filepath.Join(root, "agents", "settings.ts"))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(settings), `export const agentName = "noether"`) || !strings.Contains(string(settings), "Emmy Noether") {
		t.Fatalf("settings = %s", settings)
	}
	if _, err := os.Stat(filepath.Join(root, "agents", "assistant.ts")); !os.IsNotExist(err) {
		t.Fatalf("template Agent file was not renamed: %v", err)
	}
	pkg, err := os.ReadFile(filepath.Join(root, "package.json"))
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(pkg), "file:") || !strings.Contains(string(pkg), `"@anthropic-ai/claude-agent-sdk"`) {
		t.Fatalf("package.json = %s", pkg)
	}
	if _, err := runInit(t, "--dir", root, "--template", "claude-code", "--name", "noether"); err == nil || !strings.Contains(err.Error(), "already exists") {
		t.Fatalf("second init err = %v", err)
	}
}

func TestInitTemplatePicksANameWhenNoneIsGiven(t *testing.T) {
	out, err := runInit(t, "--dir", t.TempDir(), "--template", "claude-code")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out, "Agent: ") || !strings.Contains(out, "(named after ") {
		t.Fatalf("output = %q", out)
	}
}

func TestInitRejectsInvalidTemplateInput(t *testing.T) {
	for args, want := range map[string]string{
		"--template nope":                          `unknown template "nope"`,
		"--template claude-code --provider openai": "does not support provider",
		"--template claude-code --name Bad_Name":   "invalid agent name",
		"--provider anthropic":                     "need --template",
		"--name noether":                           "need --template",
	} {
		_, err := runInit(t, append([]string{"--dir", t.TempDir()}, strings.Fields(args)...)...)
		if err == nil || !strings.Contains(err.Error(), want) {
			t.Fatalf("%s: err = %v, want %q", args, err, want)
		}
	}
}
