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

type installedToolchainCorpusFixture struct {
	root      string
	catalog   *ToolchainCatalog
	toolchain Toolchain
	owner     toolCorpusOwner
}

func newInstalledToolchainCorpusFixture(
	t *testing.T,
) installedToolchainCorpusFixture {
	t.Helper()
	toolchain := testToolchain(t)
	content := []byte(strings.Repeat("t", 2048))
	toolchain.ToolchainClosure = toolArtifactForBytes(
		content,
		ToolchainMediaType,
	)
	catalog := authenticatedToolchainCatalogForTest(
		t,
		[]Toolchain{toolchain},
	)
	root := t.TempDir()
	digests := filepath.Join(root, "objects", "sha256")
	if err := os.MkdirAll(digests, 0o755); err != nil {
		t.Fatal(err)
	}
	for _, directory := range []string{
		root,
		filepath.Join(root, "objects"),
		digests,
	} {
		if err := os.Chmod(directory, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	for _, name := range []string{
		"catalog.json",
		"catalog.sigstore.json",
		"trusted-root.json",
	} {
		writeToolCorpusFixtureFile(
			t,
			filepath.Join(root, name),
			[]byte(name),
		)
	}
	writeToolCorpusFixtureFile(
		t,
		filepath.Join(
			digests,
			strings.TrimPrefix(
				toolchain.ToolchainClosure.Digest,
				"sha256:",
			),
		),
		content,
	)
	return installedToolchainCorpusFixture{
		root:      root,
		catalog:   catalog,
		toolchain: toolchain,
		owner: toolCorpusOwner{
			uid: uint32(os.Geteuid()),
			gid: uint32(os.Getegid()),
		},
	}
}

func toolArtifactForBytes(raw []byte, mediaType string) ArtifactDescriptor {
	digest := sha256.Sum256(raw)
	return ArtifactDescriptor{
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

func TestLoadToolchainCorpusVerifiesAndReopensClosure(t *testing.T) {
	fixture := newInstalledToolchainCorpusFixture(t)
	corpus, err := loadToolchainCorpus(
		context.Background(),
		fixture.root,
		fixture.catalog,
		ArchitectureX8664,
		fixture.owner,
	)
	if err != nil {
		t.Fatal(err)
	}
	defer corpus.Close()
	file, err := corpus.OpenToolchain(
		context.Background(),
		fixture.toolchain,
	)
	if err != nil {
		t.Fatal(err)
	}
	defer file.Close()
	if file.Descriptor() != toolObject(
		fixture.toolchain.ToolchainClosure,
	) {
		t.Fatalf("descriptor = %#v", file.Descriptor())
	}
	if file.File() == nil {
		t.Fatal("standard-toolchain file is nil")
	}
}

func TestLoadToolchainCorpusRejectsExtraAndWrongArchitecture(t *testing.T) {
	fixture := newInstalledToolchainCorpusFixture(t)
	writeToolCorpusFixtureFile(
		t,
		filepath.Join(
			fixture.root,
			"objects",
			"sha256",
			strings.Repeat("f", 64),
		),
		[]byte("extra"),
	)
	if _, err := loadToolchainCorpus(
		context.Background(),
		fixture.root,
		fixture.catalog,
		ArchitectureX8664,
		fixture.owner,
	); err == nil {
		t.Fatal("loadToolchainCorpus accepted an extra object")
	}
	if _, err := loadToolchainCorpus(
		context.Background(),
		fixture.root,
		fixture.catalog,
		RuntimeArchitecture("aarch64"),
		fixture.owner,
	); err == nil {
		t.Fatal("loadToolchainCorpus accepted an absent architecture")
	}
}

func TestLoadToolchainCorpusRejectsHardlink(t *testing.T) {
	fixture := newInstalledToolchainCorpusFixture(t)
	path := filepath.Join(
		fixture.root,
		"objects",
		"sha256",
		strings.TrimPrefix(
			fixture.toolchain.ToolchainClosure.Digest,
			"sha256:",
		),
	)
	if err := os.Link(path, filepath.Join(t.TempDir(), "alias")); err != nil {
		t.Fatal(err)
	}
	if _, err := loadToolchainCorpus(
		context.Background(),
		fixture.root,
		fixture.catalog,
		ArchitectureX8664,
		fixture.owner,
	); err == nil {
		t.Fatal("loadToolchainCorpus accepted a hard-linked object")
	}
}

func TestOpenToolchainRejectsPostReadinessMutation(t *testing.T) {
	fixture := newInstalledToolchainCorpusFixture(t)
	corpus, err := loadToolchainCorpus(
		context.Background(),
		fixture.root,
		fixture.catalog,
		ArchitectureX8664,
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
		strings.TrimPrefix(
			fixture.toolchain.ToolchainClosure.Digest,
			"sha256:",
		),
	)
	if err := os.Chmod(path, 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := corpus.OpenToolchain(
		context.Background(),
		fixture.toolchain,
	); err == nil {
		t.Fatal("OpenToolchain accepted a mutated object")
	}
}
