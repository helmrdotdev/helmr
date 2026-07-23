package deployment

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"sort"
	"strings"
	"testing"
)

func testToolchain(t *testing.T) Toolchain {
	t.Helper()
	return Toolchain{
		Architecture:         ArchitectureAArch64,
		FormatVersion:        ToolchainFormatVersion,
		ManagedRuntimeDigest: toolDigestForTest("managed runtime"),
		ToolchainClosure: ArtifactDescriptor{
			Digest:    toolDigestForTest("toolchain closure"),
			MediaType: ToolchainMediaType,
			SizeBytes: 2048,
		},
	}
}

func toolDigestForTest(value string) string {
	digest := sha256.Sum256([]byte(value))
	return "sha256:" + hex.EncodeToString(digest[:])
}

func TestToolchainCatalogRoundTrip(t *testing.T) {
	runtime := testRuntimeDescriptor()
	toolchain, toolchainDigest := testToolchainForRuntime(t, runtime)
	raw, err := CanonicalToolchainCatalog([]Toolchain{toolchain})
	if err != nil {
		t.Fatal(err)
	}
	catalog, err := ParseToolchainCatalog(raw)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := catalog.Resolve(toolchainDigest); err == nil {
		t.Fatal("unauthenticated standard-toolchain catalog resolved a member")
	}
	catalog.authenticated = true
	catalogDigest, err := catalog.Digest()
	if err != nil {
		t.Fatal(err)
	}
	hash := sha256.Sum256(raw)
	if want := fmt.Sprintf("sha256:%x", hash[:]); catalogDigest != want {
		t.Fatalf("catalog digest = %q, want %q", catalogDigest, want)
	}
	resolved, err := catalog.Resolve(toolchainDigest)
	if err != nil {
		t.Fatal(err)
	}
	if resolved != toolchain {
		t.Fatalf("resolved standard toolchain = %#v, want %#v", resolved, toolchain)
	}
	if _, err := catalog.Resolve("sha256:" + strings.Repeat("f", 64)); err == nil {
		t.Fatal("standard-toolchain catalog resolved an unregistered digest")
	}
}

func TestToolchainSourceDocumentIsClosedAndCanonical(t *testing.T) {
	toolchain, _ := testToolchainForRuntime(t, testRuntimeDescriptor())
	raw, err := CanonicalToolchain(toolchain)
	if err != nil {
		t.Fatal(err)
	}
	parsed, err := ParseToolchain(raw)
	if err != nil {
		t.Fatal(err)
	}
	if parsed != toolchain {
		t.Fatalf("parsed standard toolchain = %#v", parsed)
	}
	if _, err := ParseToolchain(append([]byte(" "), raw...)); err == nil {
		t.Fatal("ParseToolchain accepted noncanonical JSON")
	}
	unknown := append(raw[:len(raw)-1], []byte(`,"unknown":true}`)...)
	if _, err := ParseToolchain(unknown); err == nil {
		t.Fatal("ParseToolchain accepted an unknown field")
	}
}

func TestToolchainCatalogRejectsInvalidDocuments(t *testing.T) {
	first, _ := testToolchainForRuntime(t, testRuntimeDescriptor())
	second := first
	second.ToolchainClosure.Digest = toolDigestForTest("second toolchain closure")
	firstDigest, err := StandardToolchainDigest(first)
	if err != nil {
		t.Fatal(err)
	}
	secondDigest, err := StandardToolchainDigest(second)
	if err != nil {
		t.Fatal(err)
	}
	ordered := []Toolchain{first, second}
	if firstDigest > secondDigest {
		ordered[0], ordered[1] = ordered[1], ordered[0]
	}

	tests := map[string][]Toolchain{
		"empty":     nil,
		"duplicate": {first, first},
		"unordered": {ordered[1], ordered[0]},
	}
	for name, toolchains := range tests {
		t.Run(name, func(t *testing.T) {
			if _, err := CanonicalToolchainCatalog(toolchains); err == nil {
				t.Fatal("CanonicalToolchainCatalog returned nil error")
			}
		})
	}

	raw, err := CanonicalToolchainCatalog([]Toolchain{first})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := ParseToolchainCatalog(append([]byte(" "), raw...)); err == nil {
		t.Fatal("ParseToolchainCatalog accepted noncanonical JSON")
	}
	unknown := append(raw[:len(raw)-1], []byte(`,"unknown":true}`)...)
	if _, err := ParseToolchainCatalog(unknown); err == nil {
		t.Fatal("ParseToolchainCatalog accepted an unknown field")
	}
}

func TestToolchainCatalogDerivesDeduplicatedArchitectureObjects(t *testing.T) {
	first, _ := testToolchainForRuntime(t, testRuntimeDescriptor())
	first.Architecture = ArchitectureAArch64
	foreign := first
	foreign.Architecture = ArchitectureX8664
	foreign.ManagedRuntimeDigest = toolDigestForTest("x86 runtime")
	second := first
	second.ManagedRuntimeDigest = toolDigestForTest("second runtime")
	second.ToolchainClosure.Digest = toolDigestForTest("second closure")
	toolchains := []Toolchain{first, foreign, second}
	sort.Slice(toolchains, func(left, right int) bool {
		leftDigest, _ := StandardToolchainDigest(toolchains[left])
		rightDigest, _ := StandardToolchainDigest(toolchains[right])
		return leftDigest < rightDigest
	})
	catalog := authenticatedToolchainCatalogForTest(t, toolchains)
	all, err := toolchainClosureObjects(catalog, "")
	if err != nil {
		t.Fatal(err)
	}
	if len(all) != 2 {
		t.Fatalf("deduplicated object count = %d, want 2", len(all))
	}
	aarch64, err := toolchainClosureObjects(
		catalog,
		ArchitectureAArch64,
	)
	if err != nil {
		t.Fatal(err)
	}
	if len(aarch64) != 2 {
		t.Fatalf("aarch64 object count = %d, want 2", len(aarch64))
	}

	divergent := foreign
	divergent.ToolchainClosure.SizeBytes++
	toolchains = []Toolchain{first, divergent}
	sort.Slice(toolchains, func(left, right int) bool {
		leftDigest, _ := StandardToolchainDigest(toolchains[left])
		rightDigest, _ := StandardToolchainDigest(toolchains[right])
		return leftDigest < rightDigest
	})
	if _, err := CanonicalToolchainCatalog(toolchains); err == nil {
		t.Fatal("catalog accepted divergent descriptors for one closure digest")
	}
}

func TestToolchainCatalogRejectsPhysicalCorpusOverflow(t *testing.T) {
	toolchains := make([]Toolchain, 5)
	for index := range toolchains {
		toolchain, _ := testToolchainForRuntime(t, testRuntimeDescriptor())
		toolchain.ManagedRuntimeDigest = toolDigestForTest(
			fmt.Sprintf("runtime %d", index),
		)
		toolchain.ToolchainClosure = ArtifactDescriptor{
			Digest: toolDigestForTest(
				fmt.Sprintf("closure %d", index),
			),
			MediaType: ToolchainMediaType,
			SizeBytes: maxToolArtifactBytes,
		}
		toolchains[index] = toolchain
	}
	sort.Slice(toolchains, func(left, right int) bool {
		leftDigest, _ := StandardToolchainDigest(toolchains[left])
		rightDigest, _ := StandardToolchainDigest(toolchains[right])
		return leftDigest < rightDigest
	})
	if _, err := CanonicalToolchainCatalog(toolchains); err == nil {
		t.Fatal("catalog accepted more than 16 GiB of unique closures")
	}
}
