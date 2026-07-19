//go:build linux

package deployment

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

type installedToolCorpusFixture struct {
	root     string
	registry *ToolRegistry
	toolset  Toolset
	owner    toolCorpusOwner
}

func newInstalledToolCorpusFixture(t *testing.T) installedToolCorpusFixture {
	t.Helper()
	manager, toolchain, _, toolset := testToolset(t)
	contents := map[string][]byte{
		"manager":   []byte(strings.Repeat("m", 1024)),
		"toolchain": []byte(strings.Repeat("t", 2048)),
		"toolset":   []byte(strings.Repeat("s", 4096)),
	}
	manager.ManagerClosure = toolArtifactForBytes(
		contents["manager"],
		ManagerComponentMediaType,
	)
	toolchain.ToolchainClosure = toolArtifactForBytes(
		contents["toolchain"],
		ToolchainMediaType,
	)
	toolset.Artifact = toolArtifactForBytes(
		contents["toolset"],
		ManagerDependencyToolsMediaType,
	)
	var err error
	toolset.ManagerRegistrationDigest, err = ManagerRegistrationDigest(manager)
	if err != nil {
		t.Fatal(err)
	}
	toolset.StandardToolchainDigest, err = StandardToolchainDigest(toolchain)
	if err != nil {
		t.Fatal(err)
	}
	raw, err := CanonicalToolRegistry(
		[]ManagerRegistration{manager},
		[]Toolchain{toolchain},
		[]Toolset{toolset},
	)
	if err != nil {
		t.Fatal(err)
	}
	registry, err := ParseToolRegistry(raw)
	if err != nil {
		t.Fatal(err)
	}
	registry.authenticated = true
	manifest, err := CanonicalToolCorpus(registry, ArchitectureAArch64)
	if err != nil {
		t.Fatal(err)
	}

	root := t.TempDir()
	objects := filepath.Join(root, "objects")
	digests := filepath.Join(objects, "sha256")
	for _, directory := range []string{root, objects, digests} {
		if err := os.MkdirAll(directory, 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.Chmod(directory, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	writeToolCorpusFixtureFile(t, filepath.Join(root, "corpus.json"), manifest)
	for name, content := range contents {
		var artifact ManagerArtifact
		switch name {
		case "manager":
			artifact = manager.ManagerClosure
		case "toolchain":
			artifact = toolchain.ToolchainClosure
		case "toolset":
			artifact = toolset.Artifact
		}
		writeToolCorpusFixtureFile(
			t,
			filepath.Join(digests, strings.TrimPrefix(artifact.Digest, "sha256:")),
			content,
		)
	}
	return installedToolCorpusFixture{
		root:     root,
		registry: registry,
		toolset:  toolset,
		owner: toolCorpusOwner{
			uid: uint32(os.Geteuid()),
			gid: uint32(os.Getegid()),
		},
	}
}

func toolArtifactForBytes(raw []byte, mediaType string) ManagerArtifact {
	digest := sha256.Sum256(raw)
	return ManagerArtifact{
		Digest:    "sha256:" + hex.EncodeToString(digest[:]),
		MediaType: mediaType,
		SizeBytes: int64(len(raw)),
	}
}

func writeToolCorpusFixtureFile(t *testing.T, path string, raw []byte) {
	t.Helper()
	if err := os.WriteFile(path, raw, 0o444); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(path, 0o444); err != nil {
		t.Fatal(err)
	}
}

func TestLoadToolCorpusVerifiesAndReopensToolset(t *testing.T) {
	fixture := newInstalledToolCorpusFixture(t)
	corpus, err := loadToolCorpus(
		context.Background(),
		fixture.root,
		fixture.registry,
		ArchitectureAArch64,
		fixture.owner,
	)
	if err != nil {
		t.Fatal(err)
	}
	defer corpus.Close()
	file, err := corpus.OpenToolset(context.Background(), fixture.toolset)
	if err != nil {
		t.Fatal(err)
	}
	defer file.Close()
	if file.Descriptor() != toolObject(fixture.toolset.Artifact) {
		t.Fatalf("descriptor = %#v", file.Descriptor())
	}
	if file.File() == nil {
		t.Fatal("toolset file is nil")
	}
}

func TestLoadToolCorpusRejectsExtraEntry(t *testing.T) {
	fixture := newInstalledToolCorpusFixture(t)
	writeToolCorpusFixtureFile(
		t,
		filepath.Join(fixture.root, "objects", "sha256", strings.Repeat("f", 64)),
		[]byte("extra"),
	)
	if _, err := loadToolCorpus(
		context.Background(),
		fixture.root,
		fixture.registry,
		ArchitectureAArch64,
		fixture.owner,
	); err == nil {
		t.Fatal("loadToolCorpus accepted an extra object")
	}
}

func TestLoadToolCorpusRejectsHardlink(t *testing.T) {
	fixture := newInstalledToolCorpusFixture(t)
	path := filepath.Join(
		fixture.root,
		"objects",
		"sha256",
		strings.TrimPrefix(fixture.toolset.Artifact.Digest, "sha256:"),
	)
	if err := os.Link(path, filepath.Join(t.TempDir(), "alias")); err != nil {
		t.Fatal(err)
	}
	if _, err := loadToolCorpus(
		context.Background(),
		fixture.root,
		fixture.registry,
		ArchitectureAArch64,
		fixture.owner,
	); err == nil {
		t.Fatal("loadToolCorpus accepted a hard-linked object")
	}
}

func TestOpenToolsetRejectsPostReadinessMutation(t *testing.T) {
	fixture := newInstalledToolCorpusFixture(t)
	corpus, err := loadToolCorpus(
		context.Background(),
		fixture.root,
		fixture.registry,
		ArchitectureAArch64,
		fixture.owner,
	)
	if err != nil {
		t.Fatal(err)
	}
	defer corpus.Close()
	path := filepath.Join(
		fixture.root,
		"objects",
		"sha256",
		strings.TrimPrefix(fixture.toolset.Artifact.Digest, "sha256:"),
	)
	if err := os.Chmod(path, 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := corpus.OpenToolset(context.Background(), fixture.toolset); err == nil {
		t.Fatal("OpenToolset accepted a mutated object")
	}
}
