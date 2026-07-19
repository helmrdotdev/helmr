package main

import (
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/helmrdotdev/helmr/internal/deployment"
)

func TestToolCandidateCommandsProduceClosedRegistry(t *testing.T) {
	root := t.TempDir()
	managerPath := writeObject(t, root, "manager", []byte(strings.Repeat("m", 4096)))
	toolchainPath := writeObject(t, root, "toolchain", []byte(strings.Repeat("c", 4096)))
	toolsetPath := writeObject(t, root, "toolset", []byte(strings.Repeat("t", 4096)))
	manager := describeForTest(t, managerPath, deployment.ManagerComponentMediaType)
	toolchain := describeForTest(t, toolchainPath, deployment.ToolchainMediaType)
	components := toolComponentsForTest(manager, toolchain)
	raw, err := json.MarshalIndent(components, "", "  ")
	if err != nil {
		t.Fatal(err)
	}
	rawPath := filepath.Join(root, "components.raw.json")
	if err := os.WriteFile(rawPath, raw, 0o444); err != nil {
		t.Fatal(err)
	}
	componentsPath := filepath.Join(root, "components.json")
	if err := run([]string{
		"components",
		"--input", rawPath,
		"--output", componentsPath,
	}); err != nil {
		t.Fatal(err)
	}
	canonical, err := os.ReadFile(componentsPath)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := deployment.ParseToolComponents(canonical); err != nil {
		t.Fatal(err)
	}

	output := filepath.Join(root, "release")
	if err := run([]string{
		"registry",
		"--components", componentsPath,
		"--manager", managerPath,
		"--toolchain", toolchainPath,
		"--toolset", toolsetPath,
		"--output", output,
	}); err != nil {
		t.Fatal(err)
	}
	registryBytes, err := os.ReadFile(
		filepath.Join(output, toolRegistryFile),
	)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := deployment.ParseToolRegistry(registryBytes); err != nil {
		t.Fatal(err)
	}
	toolset := describeForTest(
		t,
		toolsetPath,
		deployment.ManagerDependencyToolsMediaType,
	)
	for _, descriptor := range []deployment.ManagerArtifact{
		manager,
		toolchain,
		toolset,
	} {
		path := filepath.Join(
			output,
			"objects",
			"sha256",
			strings.TrimPrefix(descriptor.Digest, "sha256:"),
		)
		if info, err := os.Stat(path); err != nil || info.Size() != descriptor.SizeBytes {
			t.Fatalf("dependency tool object %q is not exact: %v", descriptor.Digest, err)
		}
	}
}

func TestToolCandidateRegistryRejectsComponentObjectDrift(t *testing.T) {
	root := t.TempDir()
	managerPath := writeObject(t, root, "manager", []byte(strings.Repeat("m", 4096)))
	toolchainPath := writeObject(t, root, "toolchain", []byte(strings.Repeat("c", 4096)))
	toolsetPath := writeObject(t, root, "toolset", []byte(strings.Repeat("t", 4096)))
	components := toolComponentsForTest(
		describeForTest(t, managerPath, deployment.ManagerComponentMediaType),
		describeForTest(t, toolchainPath, deployment.ToolchainMediaType),
	)
	canonical, err := deployment.CanonicalToolComponents(components)
	if err != nil {
		t.Fatal(err)
	}
	componentsPath := filepath.Join(root, "components.json")
	if err := os.WriteFile(componentsPath, canonical, 0o444); err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(toolchainPath); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(toolchainPath, []byte("changed"), 0o444); err != nil {
		t.Fatal(err)
	}
	if err := run([]string{
		"registry",
		"--components", componentsPath,
		"--manager", managerPath,
		"--toolchain", toolchainPath,
		"--toolset", toolsetPath,
		"--output", filepath.Join(root, "release"),
	}); err == nil {
		t.Fatal("registry accepted standard toolchain drift")
	}
}

func TestCopyExclusiveBindsDescriptorToCopiedBytes(t *testing.T) {
	root := t.TempDir()
	source := writeObject(t, root, "source", []byte("source"))
	descriptor := describeForTest(
		t,
		source,
		deployment.ManagerComponentMediaType,
	)
	descriptor.Digest = digestForTest([]byte("target"))
	destination := filepath.Join(root, strings.Repeat("a", 64))
	if err := copyExclusive(destination, source, descriptor); err == nil {
		t.Fatal("copyExclusive accepted bytes that do not match the descriptor")
	}
}

func toolComponentsForTest(
	managerArtifact,
	toolchainArtifact deployment.ManagerArtifact,
) deployment.ToolComponents {
	runtimeDigest := digestForTest([]byte("runtime"))
	manager := deployment.ManagerRegistration{
		Architecture:    deployment.ArchitectureX8664,
		Executable:      "/opt/helmr/dependency-tools/bin/bun",
		FormatVersion:   deployment.ToolsetFormatVersion,
		Lifecycle:       deployment.ToolCommand{Argv: []string{"/opt/helmr/dependency-tools/bin/bun"}},
		LockfileAdapter: "bun-lock-v0",
		ManagerClosure:  managerArtifact,
		OfflineStore: deployment.ToolOfflineStore{
			ReadOnlyMountPath: "/opt/helmr/offline-store",
			WorkPath:          "/work/offline-store",
		},
		PackageManager: deployment.PackageManager{
			Name:    deployment.PackageManagerBun,
			Version: "1.3.10",
		},
		Proxy:      deployment.ToolProxy{RegistryOrigin: "http://127.0.0.1:4873"},
		Resolution: deployment.ToolCommand{Argv: []string{"/opt/helmr/dependency-tools/bin/bun"}},
		VersionProbe: deployment.ToolVersionProbe{
			Argv:         []string{"/opt/helmr/dependency-tools/bin/bun", "--version"},
			StdoutBase64: base64.StdEncoding.EncodeToString([]byte("1.3.10\n")),
		},
	}
	toolchain := deployment.Toolchain{
		Architecture:         deployment.ArchitectureX8664,
		FormatVersion:        deployment.ToolsetFormatVersion,
		ManagedRuntimeDigest: runtimeDigest,
		ToolchainClosure:     toolchainArtifact,
	}
	environment := []deployment.ToolEnvironment{
		{Name: "HOME", Value: "/work/home"},
		{Name: "PATH", Value: "/opt/helmr/dependency-tools/bin:/nix/store/aaaaaaaa-toolchain/bin"},
	}
	return deployment.ToolComponents{
		Architecture:         deployment.ArchitectureX8664,
		Environment:          environment,
		FormatVersion:        deployment.ToolsetFormatVersion,
		Launchers:            []deployment.ToolLink{{Path: "bin/bun", Target: "/nix/store/bbbbbbbb-bun/bin/bun"}},
		ManagedRuntimeDigest: runtimeDigest,
		Manager:              manager,
		MaterializerVersion:  deployment.DependencyMaterializerVersion,
		PackageManager:       manager.PackageManager,
		SystemAliases: []deployment.ToolLink{
			{Path: "/bin/sh", Target: "/nix/store/cccccccc-bash/bin/sh"},
			{Path: "/usr/bin/env", Target: "/nix/store/dddddddd-coreutils/bin/env"},
		},
		Toolchain: toolchain,
	}
}

func writeObject(t *testing.T, root, name string, raw []byte) string {
	t.Helper()
	path := filepath.Join(root, name+".squashfs")
	if err := os.WriteFile(path, raw, 0o444); err != nil {
		t.Fatal(err)
	}
	return path
}

func describeForTest(
	t *testing.T,
	path,
	mediaType string,
) deployment.ManagerArtifact {
	t.Helper()
	descriptor, err := describe(path, mediaType)
	if err != nil {
		t.Fatal(err)
	}
	return descriptor
}

func digestForTest(raw []byte) string {
	digest := sha256.Sum256(raw)
	return "sha256:" + hex.EncodeToString(digest[:])
}
