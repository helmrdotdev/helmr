//go:build linux

package builder

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/helmrdotdev/helmr/internal/buildcontext"
	"github.com/helmrdotdev/helmr/internal/deployment"
)

// Run in the canonical builder environment with a disposable project containing
// package-lock.json, an offline npm cache and .helmrignore excluding node_modules.
// Required source files: 日本語.txt, d×80/e×80/deep-file.txt and s×255.
// The environment opt-in supplies real compiler/runtime artifacts; no mock
// compiler, encoder, dependency installation or artifact verifier is used.
func TestSourceCaptureThroughInstalledProgram(t *testing.T) {
	source := os.Getenv("HELMR_BUILD_CAPTURE_FIXTURE")
	if source == "" {
		t.Skip("HELMR_BUILD_CAPTURE_FIXTURE is not set")
	}
	captured, err := buildcontext.Capture(t.Context(), source, t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer captured.Close()
	command := exec.CommandContext(t.Context(), "/usr/local/bin/npm", "ci", "--offline", "--cache", ".npm-cache", "--ignore-scripts", "--no-audit", "--no-fund")
	command.Dir = captured.Path
	if output, err := command.CombinedOutput(); err != nil {
		t.Fatalf("npm ci: %v\n%s", err, output)
	}
	// Install a real local npm package whose shipped files and bin link need
	// extended names. Source policy never runs over this installed namespace.
	dependency := t.TempDir()
	checks := map[string]string{}
	for _, size := range []int{101, 124, 255} {
		name := "node_modules/capture-fixture/" + strings.Repeat("x", size)
		checks[name] = fmt.Sprintf("dependency %d", size)
	}
	checks["node_modules/capture-fixture/日本語.txt"] = "dependency unicode"
	for name, body := range checks {
		if err := os.WriteFile(filepath.Join(dependency, filepath.Base(name)), []byte(body), 0644); err != nil {
			t.Fatal(err)
		}
	}
	manifest, err := json.Marshal(map[string]any{"name": "capture-fixture", "version": "1.0.0", "bin": map[string]string{"capture-fixture": strings.Repeat("x", 255)}})
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dependency, "package.json"), manifest, 0644); err != nil {
		t.Fatal(err)
	}
	install := exec.CommandContext(t.Context(), "/usr/local/bin/npm", "install", "--offline", "--cache", ".npm-cache", "--ignore-scripts", "--no-audit", "--no-fund", "--no-save", "--package-lock=false", "--install-links", dependency)
	installRoot := t.TempDir()
	if err := os.WriteFile(filepath.Join(installRoot, "package.json"), []byte(`{"private":true}`), 0644); err != nil {
		t.Fatal(err)
	}
	install.Dir = installRoot
	if output, err := install.CombinedOutput(); err != nil {
		t.Fatalf("npm install long-name dependency: %v\n%s", err, output)
	}
	// Move npm's actual installed output into the project being finalized.
	if err := os.Rename(filepath.Join(installRoot, "node_modules/capture-fixture"), filepath.Join(captured.Path, "node_modules/capture-fixture")); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(captured.Path, "node_modules/.bin"), 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.Rename(filepath.Join(installRoot, "node_modules/.bin/capture-fixture"), filepath.Join(captured.Path, "node_modules/.bin/capture-fixture")); err != nil {
		t.Fatal(err)
	}
	for name, want := range checks {
		got, err := os.ReadFile(filepath.Join(captured.Path, name))
		if err != nil || string(got) != want {
			t.Fatalf("installed read %q: %v", name, err)
		}
	}
	target := "../capture-fixture/" + strings.Repeat("x", 255)
	if got, err := os.Readlink(filepath.Join(captured.Path, "node_modules/.bin/capture-fixture")); err != nil || got != target {
		t.Fatalf("npm bin link = %q, %v", got, err)
	}
	// These source fixtures are required, not optional discoveries. Compare
	// original bytes now and require those same bytes from the mounted Program.
	for _, name := range []string{
		"日本語.txt",
		filepath.Join(strings.Repeat("d", 80), strings.Repeat("e", 80), "deep-file.txt"),
		strings.Repeat("s", 255),
	} {
		want, err := os.ReadFile(filepath.Join(source, name))
		if err != nil {
			t.Fatalf("required source fixture %q: %v", name, err)
		}
		got, err := os.ReadFile(filepath.Join(captured.Path, name))
		if err != nil || !bytes.Equal(got, want) {
			t.Fatalf("captured source fixture %q differs: %v", name, err)
		}
		checks[name] = string(want)
		t.Logf("required source fixture captured: %q (%d bytes)", name, len(want))
	}
	if err := filepath.WalkDir(captured.Path, func(name string, d os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if !d.Type().IsRegular() {
			return nil
		}
		relative, err := filepath.Rel(captured.Path, name)
		if err != nil {
			return err
		}
		if len(d.Name()) == 124 && strings.HasPrefix(relative, ".npm-cache/") {
			body, err := os.ReadFile(name)
			if err != nil {
				return err
			}
			checks[relative] = string(body)
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	cacheEntries := 0
	for name := range checks {
		if strings.HasPrefix(name, ".npm-cache/") {
			cacheEntries++
		}
	}
	if cacheEntries == 0 {
		t.Fatal("fixture has no real 124-byte npm cache leaves")
	}
	read := func(name string) []byte {
		body, err := os.ReadFile(name)
		if err != nil {
			t.Fatal(err)
		}
		return body
	}
	compiler, err := deployment.ParseCompilerInputs(read("/nix/helmr/compiler.descriptor.json"))
	if err != nil {
		t.Fatal(err)
	}
	runtimeDescriptor, err := deployment.ParseRuntimeDescriptor(read("/opt/helmr/release/runtime.descriptor.json"))
	if err != nil {
		t.Fatal(err)
	}
	metadata, err := deployment.ParseRuntimeMetadata(read("/opt/helmr/runtime/helmr/runtime.json"))
	if err != nil {
		t.Fatal(err)
	}
	work := t.TempDir()
	prepared := filepath.Join(work, "prepared")
	configPath := filepath.Join(t.TempDir(), "config.json")
	if err := os.WriteFile(configPath, []byte(`{"dirs":["tasks"],"ignorePatterns":[]}`), 0o444); err != nil {
		t.Fatal(err)
	}
	input := ProgramInput{ProjectDirectory: captured.Path, WorkDirectory: work,
		NodePath:   "/opt/helmr/runtime/bin/node",
		ConfigPath: configPath, ProgramCompiler: "/nix/helmr/program-compiler.mjs", SquashFSEncoder: "/opt/helmr/bin/mksquashfs",
		Compiler: compiler, Runtime: runtimeDescriptor, RuntimeMetadata: metadata}
	if _, err := PrepareProgram(t.Context(), input, prepared); err != nil {
		t.Fatal(err)
	}
	// Freeze two identical installed inputs before assembly adds compiler
	// output. This tests Program determinism, not reproducibility of npm installs:
	// npm cache logs can change between installations. cp -a is test-only setup
	// preserving the real installed package files, modes and symlinks.
	second := t.TempDir()
	copyTree := exec.CommandContext(t.Context(), "cp", "-a", captured.Path+"/.", second)
	if output, err := copyTree.CombinedOutput(); err != nil {
		t.Fatalf("copy controlled installed tree: %v\n%s", err, output)
	}
	firstDigest, err := deployment.ProgramInputTreeDigest(t.Context(), captured.Path)
	if err != nil {
		t.Fatal(err)
	}
	secondDigest, err := deployment.ProgramInputTreeDigest(t.Context(), second)
	if err != nil {
		t.Fatal(err)
	}
	if firstDigest != secondDigest {
		t.Fatalf("controlled input digests differ: %s != %s", firstDigest, secondDigest)
	}
	results := make([]ProgramResult, 0, 2)
	for _, directory := range []string{captured.Path, second} {
		assemblyWork := t.TempDir()
		result, err := BuildPreparedProgram(t.Context(), PreparedProgramInput{PreparedDirectory: prepared, ProgramDirectory: directory, WorkDirectory: assemblyWork,
			ProgramObjectPath: filepath.Join(assemblyWork, "program.squashfs"), SquashFSEncoder: input.SquashFSEncoder, Compiler: compiler, Runtime: runtimeDescriptor, RuntimeMetadata: metadata,
			WorkspaceImages: []deployment.BundleWorkspaceImage{}})
		if err != nil {
			t.Fatal(err)
		}
		file, err := os.Open(result.ObjectPath)
		if err != nil {
			t.Fatal(err)
		}
		if err := errors.Join(deployment.VerifyProgramOutputFile(t.Context(), file, result.Program), file.Close()); err != nil {
			t.Fatal(err)
		}
		results = append(results, result)
	}
	firstDescriptor, err := json.Marshal(results[0].Program)
	if err != nil {
		t.Fatal(err)
	}
	secondDescriptor, err := json.Marshal(results[1].Program)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(firstDescriptor, secondDescriptor) {
		t.Fatal("final Program descriptors differ for identical installed input")
	}
	firstObject, secondObject := read(results[0].ObjectPath), read(results[1].ObjectPath)
	if !bytes.Equal(firstObject, secondObject) {
		t.Fatal("final Program object bytes differ for identical installed input")
	}
	result := results[0]
	t.Logf("controlled installed input %s: two verified Program objects %s and %s; descriptors and all %d bytes equal; digest %s", firstDigest, results[0].ObjectPath, results[1].ObjectPath, len(firstObject), result.Program.Artifact.Digest)
	// A mount command is optional so the same integration also runs in an
	// unprivileged builder. Set it in a privileged disposable Linux fixture to
	// prove the kernel's final /opt/helmr/program path and link behavior.
	mountCommand := os.Getenv("HELMR_BUILD_CAPTURE_MOUNT")
	if mountCommand == "" {
		t.Logf("verified Program %s; kernel mount/read not selected", result.Program.Artifact.Digest)
		return
	}
	mountpoint := "/opt/helmr/program"
	if err := os.Mkdir(mountpoint, 0755); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.Remove(mountpoint) })
	mount := exec.CommandContext(t.Context(), mountCommand, "-t", "squashfs", "-o", "loop,ro", result.ObjectPath, mountpoint)
	if output, err := mount.CombinedOutput(); err != nil {
		t.Fatalf("mount Program: %v\n%s", err, output)
	}
	defer func() {
		command := exec.Command(filepath.Join(filepath.Dir(mountCommand), "umount"), mountpoint)
		if output, err := command.CombinedOutput(); err != nil {
			t.Errorf("unmount: %v %s", err, output)
		}
	}()
	for name, want := range checks {
		got, err := os.ReadFile(filepath.Join(mountpoint, name))
		if err != nil || string(got) != want {
			t.Fatalf("mounted read %q: %v", name, err)
		}
	}
	got, err := os.ReadFile(filepath.Join(mountpoint, "node_modules/.bin/capture-fixture"))
	if err != nil || string(got) != "dependency 255" {
		t.Fatalf("mounted long symlink: %v", err)
	}
	t.Logf("verified and mounted Program %s; read %d paths including %d real npm cache leaves", result.Program.Artifact.Digest, len(checks), cacheEntries)
}
