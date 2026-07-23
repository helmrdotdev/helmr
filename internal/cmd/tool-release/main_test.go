package main

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/helmrdotdev/helmr/internal/deployment"
	"github.com/helmrdotdev/helmr/internal/jsoncanon"
)

func TestCandidateProducesOnlyCanonicalToolchainAndClosure(t *testing.T) {
	root := t.TempDir()
	closurePath := writeObject(
		t,
		root,
		"toolchain",
		[]byte(strings.Repeat("c", 4096)),
	)
	closure := describeForTest(t, closurePath, deployment.ToolchainMediaType)
	toolchain := toolchainForTest(closure)
	raw, err := json.MarshalIndent(toolchain, "", "  ")
	if err != nil {
		t.Fatal(err)
	}
	input := filepath.Join(root, "toolchain.raw.json")
	if err := os.WriteFile(input, raw, 0o444); err != nil {
		t.Fatal(err)
	}
	output := filepath.Join(root, "release")
	if err := run([]string{
		"candidate",
		"--input", input,
		"--closure", closurePath,
		"--output", output,
	}); err != nil {
		t.Fatal(err)
	}
	canonical, err := os.ReadFile(filepath.Join(output, toolchainFile))
	if err != nil {
		t.Fatal(err)
	}
	expectedJSON, err := json.Marshal(toolchain)
	if err != nil {
		t.Fatal(err)
	}
	expected, err := jsoncanon.Transform(expectedJSON)
	if err != nil {
		t.Fatal(err)
	}
	if string(canonical) != string(expected) {
		t.Fatal("candidate toolchain is not exact canonical JSON")
	}
	object := filepath.Join(
		output,
		"objects",
		"sha256",
		strings.TrimPrefix(closure.Digest, "sha256:"),
	)
	if info, err := os.Stat(object); err != nil ||
		info.Size() != closure.SizeBytes ||
		info.Mode().Perm() != 0o444 {
		t.Fatalf("standard-toolchain closure is not exact: %v", err)
	}
	entries, err := os.ReadDir(output)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 2 ||
		entries[0].Name() != "objects" ||
		entries[1].Name() != toolchainFile {
		t.Fatalf("candidate contains unexpected release members: %v", entries)
	}
}

func TestCandidateRejectsClosureDrift(t *testing.T) {
	root := t.TempDir()
	closurePath := writeObject(t, root, "toolchain", []byte("original"))
	toolchain := toolchainForTest(
		describeForTest(t, closurePath, deployment.ToolchainMediaType),
	)
	raw, err := json.Marshal(toolchain)
	if err != nil {
		t.Fatal(err)
	}
	input := filepath.Join(root, "toolchain.json")
	if err := os.WriteFile(input, raw, 0o444); err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(closurePath); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(closurePath, []byte("changed"), 0o444); err != nil {
		t.Fatal(err)
	}
	if err := run([]string{
		"candidate",
		"--input", input,
		"--closure", closurePath,
		"--output", filepath.Join(root, "release"),
	}); err == nil {
		t.Fatal("candidate accepted standard-toolchain closure drift")
	}
}

func TestCopyExclusiveBindsDescriptorToCopiedBytes(t *testing.T) {
	root := t.TempDir()
	source := writeObject(t, root, "source", []byte("source"))
	descriptor := describeForTest(t, source, deployment.ToolchainMediaType)
	descriptor.Digest = digestForTest([]byte("target"))
	destination := filepath.Join(root, strings.Repeat("a", 64))
	if err := copyExclusive(destination, source, descriptor); err == nil {
		t.Fatal("copyExclusive accepted bytes that do not match the descriptor")
	}
}

func toolchainForTest(
	closure deployment.ArtifactDescriptor,
) deployment.Toolchain {
	return deployment.Toolchain{
		Architecture:         deployment.ArchitectureX8664,
		FormatVersion:        deployment.ToolchainFormatVersion,
		ManagedRuntimeDigest: digestForTest([]byte("runtime")),
		ToolchainClosure:     closure,
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
) deployment.ArtifactDescriptor {
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
