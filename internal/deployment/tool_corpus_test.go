package deployment

import (
	"bytes"
	"encoding/json"
	"strings"
	"testing"

	"github.com/helmrdotdev/helmr/internal/jsoncanon"
)

func TestToolCorpusIsExactArchitectureClosure(t *testing.T) {
	manager, toolchain, _, toolset := testToolset(t)
	registryRaw, err := CanonicalToolRegistry(
		[]ManagerRegistration{manager},
		[]Toolchain{toolchain},
		[]Toolset{toolset},
	)
	if err != nil {
		t.Fatal(err)
	}
	registry, err := ParseToolRegistry(registryRaw)
	if err != nil {
		t.Fatal(err)
	}
	raw, err := CanonicalToolCorpus(registry, ArchitectureAArch64)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := ParseToolCorpus(raw, registry, ArchitectureAArch64); err == nil {
		t.Fatal("ParseToolCorpus accepted an unauthenticated registry")
	}
	registry.authenticated = true
	corpus, err := ParseToolCorpus(raw, registry, ArchitectureAArch64)
	if err != nil {
		t.Fatal(err)
	}
	if corpus.architecture != ArchitectureAArch64 ||
		len(corpus.objects) != 3 ||
		len(corpus.raw) == 0 ||
		!validToolDigest(corpus.digest) {
		t.Fatalf("parsed corpus = %#v", corpus)
	}

	var document toolCorpusDocument
	if err := json.Unmarshal(raw, &document); err != nil {
		t.Fatal(err)
	}
	if document.Registry.Digest != registry.digest ||
		document.Registry.SizeBytes != int64(len(registryRaw)) ||
		document.ObjectCount != 3 ||
		document.TotalSizeBytes != 1024+2048+4096 {
		t.Fatalf("corpus manifest = %#v", document)
	}
}

func TestToolCorpusRejectsManifestDrift(t *testing.T) {
	manager, toolchain, _, toolset := testToolset(t)
	registryRaw, err := CanonicalToolRegistry(
		[]ManagerRegistration{manager},
		[]Toolchain{toolchain},
		[]Toolset{toolset},
	)
	if err != nil {
		t.Fatal(err)
	}
	registry, err := ParseToolRegistry(registryRaw)
	if err != nil {
		t.Fatal(err)
	}
	registry.authenticated = true
	raw, err := CanonicalToolCorpus(registry, ArchitectureAArch64)
	if err != nil {
		t.Fatal(err)
	}

	var document map[string]any
	if err := json.Unmarshal(raw, &document); err != nil {
		t.Fatal(err)
	}
	objects := document["objects"].([]any)
	first := objects[0].(map[string]any)
	first["sizeBytes"] = first["sizeBytes"].(float64) + 1
	mutated, err := jsoncanon.Transform(mustJSON(t, document))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := ParseToolCorpus(mutated, registry, ArchitectureAArch64); err == nil {
		t.Fatal("ParseToolCorpus accepted object drift")
	}

	open := bytes.Replace(raw, []byte(`{"architecture"`), []byte(`{"extra":true,"architecture"`), 1)
	if _, err := ParseToolCorpus(open, registry, ArchitectureAArch64); err == nil {
		t.Fatal("ParseToolCorpus accepted an unknown member")
	}
	if _, err := ParseToolCorpus(append([]byte(" "), raw...), registry, ArchitectureAArch64); err == nil {
		t.Fatal("ParseToolCorpus accepted non-canonical JSON")
	}
}

func TestToolCorpusRejectsCrossArchitectureRequest(t *testing.T) {
	manager, toolchain, _, toolset := testToolset(t)
	registryRaw, err := CanonicalToolRegistry(
		[]ManagerRegistration{manager},
		[]Toolchain{toolchain},
		[]Toolset{toolset},
	)
	if err != nil {
		t.Fatal(err)
	}
	registry, err := ParseToolRegistry(registryRaw)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := CanonicalToolCorpus(registry, ArchitectureX8664); err == nil {
		t.Fatal("CanonicalToolCorpus accepted an architecture without a closure")
	}
}

func TestToolCorpusFiltersArchitectureAndDeduplicatesObjects(t *testing.T) {
	manager, toolchain, _, toolset := testToolset(t)

	secondManager := manager
	secondManager.PackageManager.Version = "1.3.11"
	secondManager.VersionProbe.StdoutBase64 = "MS4zLjExCg=="
	secondManagerDigest, err := ManagerRegistrationDigest(secondManager)
	if err != nil {
		t.Fatal(err)
	}
	secondToolset := toolset
	secondToolset.PackageManager = secondManager.PackageManager
	secondToolset.ManagerRegistrationDigest = secondManagerDigest
	secondToolset.Artifact.Digest = toolDigestForTest("second aarch64 toolset")

	foreignManager := manager
	foreignManager.Architecture = ArchitectureX8664
	foreignManager.ManagerClosure.Digest = toolDigestForTest("x86 manager closure")
	foreignManagerDigest, err := ManagerRegistrationDigest(foreignManager)
	if err != nil {
		t.Fatal(err)
	}
	foreignToolchain := toolchain
	foreignToolchain.Architecture = ArchitectureX8664
	foreignToolchain.ToolchainClosure.Digest = toolDigestForTest("x86 toolchain closure")
	foreignToolchainDigest, err := StandardToolchainDigest(foreignToolchain)
	if err != nil {
		t.Fatal(err)
	}
	foreignToolset := toolset
	foreignToolset.Architecture = ArchitectureX8664
	foreignToolset.ManagerRegistrationDigest = foreignManagerDigest
	foreignToolset.StandardToolchainDigest = foreignToolchainDigest
	foreignToolset.Artifact.Digest = toolDigestForTest("x86 toolset")

	registryRaw, err := CanonicalToolRegistry(
		[]ManagerRegistration{manager, foreignManager, secondManager},
		[]Toolchain{toolchain, foreignToolchain},
		[]Toolset{toolset, foreignToolset, secondToolset},
	)
	if err != nil {
		t.Fatal(err)
	}
	registry, err := ParseToolRegistry(registryRaw)
	if err != nil {
		t.Fatal(err)
	}
	raw, err := CanonicalToolCorpus(registry, ArchitectureAArch64)
	if err != nil {
		t.Fatal(err)
	}
	var document toolCorpusDocument
	if err := json.Unmarshal(raw, &document); err != nil {
		t.Fatal(err)
	}
	if document.ObjectCount != 4 {
		t.Fatalf("objectCount = %d, want 4", document.ObjectCount)
	}
	for _, object := range document.Objects {
		for _, foreignDigest := range []string{
			foreignManager.ManagerClosure.Digest,
			foreignToolchain.ToolchainClosure.Digest,
			foreignToolset.Artifact.Digest,
		} {
			if object.Digest == foreignDigest {
				t.Fatalf("aarch64 corpus contains foreign object %q", foreignDigest)
			}
		}
	}
}

func TestToolRegistryRejectsDivergentSharedObjectDigest(t *testing.T) {
	manager, toolchain, _, toolset := testToolset(t)
	secondManager := manager
	secondManager.PackageManager.Version = "1.3.11"
	secondManager.VersionProbe.StdoutBase64 = "MS4zLjExCg=="
	secondManager.ManagerClosure.SizeBytes++
	secondManagerDigest, err := ManagerRegistrationDigest(secondManager)
	if err != nil {
		t.Fatal(err)
	}
	secondToolset := toolset
	secondToolset.PackageManager = secondManager.PackageManager
	secondToolset.ManagerRegistrationDigest = secondManagerDigest
	secondToolset.Artifact.Digest = toolDigestForTest("second dependency tools")

	if _, err := CanonicalToolRegistry(
		[]ManagerRegistration{manager, secondManager},
		[]Toolchain{toolchain},
		[]Toolset{toolset, secondToolset},
	); err == nil {
		t.Fatal("CanonicalToolRegistry accepted one digest with divergent sizes")
	}

	toolchain.ToolchainClosure.Digest = manager.ManagerClosure.Digest
	toolchain.ToolchainClosure.SizeBytes = manager.ManagerClosure.SizeBytes
	toolchainDigest, err := StandardToolchainDigest(toolchain)
	if err != nil {
		t.Fatal(err)
	}
	toolset.StandardToolchainDigest = toolchainDigest
	if _, err := CanonicalToolRegistry(
		[]ManagerRegistration{manager},
		[]Toolchain{toolchain},
		[]Toolset{toolset},
	); err == nil {
		t.Fatal("CanonicalToolRegistry accepted one digest with divergent media types")
	}
}

func TestToolRegistryRejectsCorpusByteOverflow(t *testing.T) {
	manager, toolchain, _, toolset := testToolset(t)
	manager.ManagerClosure.SizeBytes = maxToolArtifactBytes
	toolchain.ToolchainClosure.SizeBytes = maxToolArtifactBytes
	toolset.Artifact.SizeBytes = maxToolArtifactBytes
	managerDigest, err := ManagerRegistrationDigest(manager)
	if err != nil {
		t.Fatal(err)
	}
	toolchainDigest, err := StandardToolchainDigest(toolchain)
	if err != nil {
		t.Fatal(err)
	}
	toolset.ManagerRegistrationDigest = managerDigest
	toolset.StandardToolchainDigest = toolchainDigest

	managers := []ManagerRegistration{manager}
	toolchains := []Toolchain{toolchain}
	toolsets := []Toolset{toolset}
	versions := []string{"1.3.11", "1.3.12"}
	for index := 0; index < 2; index++ {
		nextManager := manager
		nextManager.PackageManager.Version = versions[index]
		nextManager.VersionProbe.StdoutBase64 = "MS4zLjExCg=="
		nextManager.ManagerClosure.Digest = toolDigestForTest("manager closure " + strings.Repeat("x", index+1))
		managerDigest, err := ManagerRegistrationDigest(nextManager)
		if err != nil {
			t.Fatal(err)
		}
		nextToolset := toolset
		nextToolset.PackageManager = nextManager.PackageManager
		nextToolset.ManagerRegistrationDigest = managerDigest
		nextToolset.Artifact.Digest = toolDigestForTest("toolset artifact " + strings.Repeat("x", index+1))
		managers = append(managers, nextManager)
		toolsets = append(toolsets, nextToolset)
	}
	if _, err := CanonicalToolRegistry(managers, toolchains, toolsets); err == nil {
		t.Fatal("CanonicalToolRegistry accepted more than 16 GiB of unique objects")
	}
}
