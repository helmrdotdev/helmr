package deployment

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/helmrdotdev/helmr/internal/jsoncanon"
)

func TestBuildPolicyLookups(t *testing.T) {
	runtime := testRuntimeDescriptor()
	registry, toolchainDigest := authenticatedToolRegistryForRuntime(t, runtime)
	document := buildPolicyForRuntime(runtime, toolchainDigest)
	policy := parseBuildPolicyDocument(t, document)
	policy.registry = registry

	current, err := policy.Current("us-east-1")
	if err != nil {
		t.Fatal(err)
	}
	if current.Runtime != runtime ||
		current.StandardToolchainDigest != toolchainDigest ||
		current.MaterializerVersion != DependencyMaterializerVersion {
		t.Fatalf("Current(us-east-1) = %#v", current)
	}
	resolved, err := policy.ResolveRuntime(runtime.Digest)
	if err != nil {
		t.Fatal(err)
	}
	if resolved != runtime {
		t.Fatalf("ResolveRuntime(runtime) = %#v, want %#v", resolved, runtime)
	}
	toolset, err := policy.ResolveToolset(current, PackageManager{
		Name:    PackageManagerBun,
		Version: "1.3.10",
	})
	if err != nil {
		t.Fatal(err)
	}
	if toolset.StandardToolchainDigest != current.StandardToolchainDigest ||
		toolset.ManagedRuntimeDigest != current.Runtime.Digest {
		t.Fatalf("ResolveToolset(current) = %#v", toolset)
	}
	if _, err := policy.Current("US-EAST-1"); !errors.Is(err, ErrBuildRegionNotConfigured) {
		t.Fatalf("Current(unconfigured) error = %v", err)
	}
	if _, err := policy.ResolveRuntime("sha256:" + strings.Repeat("c", 64)); !errors.Is(err, ErrRuntimeNotRegistered) {
		t.Fatalf("ResolveRuntime(unregistered) error = %v", err)
	}
}

func TestBuildPolicyValidatesProgramTarget(t *testing.T) {
	runtime := testRuntimeDescriptor()
	registry, toolchainDigest := authenticatedToolRegistryForRuntime(t, runtime)
	policy := parseBuildPolicyDocument(
		t,
		buildPolicyForRuntime(runtime, toolchainDigest),
	)
	policy.registry = registry
	target, err := policy.Current("us-east-1")
	if err != nil {
		t.Fatal(err)
	}
	receipt := testProgramReceipt(t)
	receipt.Index.RuntimeDigest = runtime.Digest
	receipt.Index.Architecture = runtime.Architecture
	receipt.DependencyIndex.RuntimeDigest = runtime.Digest
	receipt.DependencyIndex.Architecture = runtime.Architecture
	receipt.DependencyIndex.PackageManager = registry.toolsets[0].PackageManager
	receipt.DependencyIndex.MaterializerVersion = target.MaterializerVersion
	receipt.DependencyIndex.DependencyToolsDigest, err = DependencyToolsDigest(
		registry.toolsets[0],
	)
	if err != nil {
		t.Fatal(err)
	}
	if err := ValidateProgramTarget(target, receipt); err != nil {
		t.Fatal(err)
	}

	wrongRuntime := cloneProgramReceipt(receipt)
	wrongRuntime.Index.RuntimeDigest = "sha256:" + strings.Repeat("b", 64)
	wrongRuntime.DependencyIndex.RuntimeDigest = wrongRuntime.Index.RuntimeDigest
	if err := ValidateProgramTarget(target, wrongRuntime); err == nil {
		t.Fatal("ValidateProgramTarget accepted another runtime")
	}
	wrongManager := cloneProgramReceipt(receipt)
	wrongManager.DependencyIndex.PackageManager.Version = "1.3.11"
	if err := ValidateProgramTarget(target, wrongManager); err != nil {
		t.Fatal(err)
	}
	if _, err := policy.ResolveToolset(
		target,
		wrongManager.DependencyIndex.PackageManager,
	); err == nil {
		t.Fatal("ResolveToolset accepted an unregistered package manager")
	}
	wrongToolset := cloneProgramReceipt(receipt)
	wrongToolset.DependencyIndex.DependencyToolsDigest = "sha256:" + strings.Repeat("c", 64)
	if err := ValidateProgramTarget(target, wrongToolset); err != nil {
		t.Fatal(err)
	}
}

func TestLoadBuildPolicyRequiresExactAuthenticatedRegistries(t *testing.T) {
	runtime := testRuntimeDescriptor()
	registry, toolchainDigest := authenticatedToolRegistryForRuntime(t, runtime)
	document := buildPolicyForRuntime(runtime, toolchainDigest)
	document.ToolRegistryDigest = registry.digest
	raw := canonicalBuildPolicyForTest(t, document)
	path := filepath.Join(t.TempDir(), "build-policy.json")
	if err := os.WriteFile(path, raw, 0o600); err != nil {
		t.Fatal(err)
	}
	catalog := authenticatedRuntimeCatalogForTest(t, document.Runtimes)
	policy, err := LoadBuildPolicy(path, catalog, registry)
	if err != nil {
		t.Fatal(err)
	}
	current, err := policy.Current("us-east-1")
	if err != nil {
		t.Fatal(err)
	}
	if current.Runtime != runtime {
		t.Fatalf("loaded runtime = %#v, want %#v", current.Runtime, runtime)
	}

	unverifiedCatalogRaw, err := CanonicalRuntimeCatalog(document.Runtimes)
	if err != nil {
		t.Fatal(err)
	}
	unverifiedCatalog, err := ParseRuntimeCatalog(unverifiedCatalogRaw)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := LoadBuildPolicy(path, unverifiedCatalog, registry); err == nil {
		t.Fatal("LoadBuildPolicy accepted an unauthenticated runtime catalog")
	}
	registry.authenticated = false
	if _, err := LoadBuildPolicy(path, catalog, registry); err == nil {
		t.Fatal("LoadBuildPolicy accepted an unauthenticated tool registry")
	}
}

func TestBuildPolicyRejectsRegistryAndCompatibilityDrift(t *testing.T) {
	runtime := testRuntimeDescriptor()
	registry, toolchainDigest := authenticatedToolRegistryForRuntime(t, runtime)
	document := buildPolicyForRuntime(runtime, toolchainDigest)
	document.ToolRegistryDigest = registry.digest
	policy := parseBuildPolicyDocument(t, document)
	catalog := authenticatedRuntimeCatalogForTest(t, document.Runtimes)

	policy.toolRegistryDigest = "sha256:" + strings.Repeat("f", 64)
	if err := validateBuildPolicyRegistries(policy, catalog, registry); err == nil {
		t.Fatal("build policy accepted registry digest drift")
	}
	policy.toolRegistryDigest = registry.digest
	registry.toolchains[0].ManagedRuntimeDigest = "sha256:" + strings.Repeat("e", 64)
	if err := validateBuildPolicyRegistries(policy, catalog, registry); err == nil {
		t.Fatal("build policy accepted an incompatible toolchain")
	}
}

func TestValidateBuildPolicyUpgrade(t *testing.T) {
	first := testRuntimeDescriptor()
	_, firstToolchain := authenticatedToolRegistryForRuntime(t, first)
	previous := parseBuildPolicyDocument(t, buildPolicyForRuntime(first, firstToolchain))

	second := first
	second.Architecture = ArchitectureAArch64
	second.Digest = "sha256:" + strings.Repeat("b", 64)
	nextDocument := buildPolicyForRuntime(second, "sha256:"+strings.Repeat("d", 64))
	nextDocument.Runtimes = []RuntimeDescriptor{first, second}
	next := parseBuildPolicyDocument(t, nextDocument)
	if err := ValidateBuildPolicyUpgrade(previous, next); err != nil {
		t.Fatal(err)
	}

	if err := ValidateBuildPolicyUpgrade(next, previous); err == nil {
		t.Fatal("ValidateBuildPolicyUpgrade accepted runtime removal")
	}
	mutated := *previous
	mutated.runtimes = map[string]RuntimeDescriptor{}
	changed := first
	changed.SizeBytes++
	mutated.runtimes[first.Digest] = changed
	if err := ValidateBuildPolicyUpgrade(previous, &mutated); err == nil {
		t.Fatal("ValidateBuildPolicyUpgrade accepted runtime mutation")
	}
	if err := ValidateBuildPolicyUpgrade(nil, next); err == nil {
		t.Fatal("ValidateBuildPolicyUpgrade accepted a nil snapshot")
	}
}

func TestBuildPolicyRejectsInvalidDocuments(t *testing.T) {
	runtime := testRuntimeDescriptor()
	validTarget := buildPolicyTarget{
		MaterializerVersion:     DependencyMaterializerVersion,
		RuntimeDigest:           runtime.Digest,
		StandardToolchainDigest: "sha256:" + strings.Repeat("d", 64),
	}
	valid := buildPolicyDocument{
		Current:            map[string]buildPolicyTarget{"us-east-1": validTarget},
		FormatVersion:      BuildPolicyFormatVersion,
		Runtimes:           []RuntimeDescriptor{runtime},
		ToolRegistryDigest: "sha256:" + strings.Repeat("e", 64),
	}
	tests := map[string]func(*buildPolicyDocument){
		"format version": func(value *buildPolicyDocument) {
			value.FormatVersion++
		},
		"null current": func(value *buildPolicyDocument) {
			value.Current = nil
		},
		"empty runtimes": func(value *buildPolicyDocument) {
			value.Runtimes = nil
		},
		"invalid registry digest": func(value *buildPolicyDocument) {
			value.ToolRegistryDigest = "latest"
		},
		"invalid region": func(value *buildPolicyDocument) {
			value.Current = map[string]buildPolicyTarget{" region": validTarget}
		},
		"dangling runtime": func(value *buildPolicyDocument) {
			target := validTarget
			target.RuntimeDigest = "sha256:" + strings.Repeat("f", 64)
			value.Current = map[string]buildPolicyTarget{"us-east-1": target}
		},
		"invalid toolchain": func(value *buildPolicyDocument) {
			target := validTarget
			target.StandardToolchainDigest = "latest"
			value.Current = map[string]buildPolicyTarget{"us-east-1": target}
		},
		"invalid materializer": func(value *buildPolicyDocument) {
			target := validTarget
			target.MaterializerVersion = "helmr.dependencies.v1"
			value.Current = map[string]buildPolicyTarget{"us-east-1": target}
		},
	}
	for name, mutate := range tests {
		t.Run(name, func(t *testing.T) {
			document := valid
			document.Current = map[string]buildPolicyTarget{"us-east-1": validTarget}
			mutate(&document)
			if _, err := ParseBuildPolicy(canonicalBuildPolicyForTest(t, document)); err == nil {
				t.Fatal("ParseBuildPolicy returned nil error")
			}
		})
	}
}

func TestBuildPolicyRequiresClosedCanonicalShape(t *testing.T) {
	runtime := testRuntimeDescriptor()
	document := buildPolicyForRuntime(runtime, "sha256:"+strings.Repeat("d", 64))
	canonical := canonicalBuildPolicyForTest(t, document)
	tests := map[string][]byte{
		"noncanonical": append([]byte(" "), canonical...),
		"unknown field": append(
			canonical[:len(canonical)-1],
			[]byte(`,"unknown":true}`)...,
		),
		"duplicate field": []byte(strings.Replace(
			string(canonical),
			`"formatVersion":0`,
			`"formatVersion":0,"formatVersion":0`,
			1,
		)),
	}
	for name, raw := range tests {
		t.Run(name, func(t *testing.T) {
			if _, err := ParseBuildPolicy(raw); err == nil {
				t.Fatal("ParseBuildPolicy returned nil error")
			}
		})
	}
}

func buildPolicyForRuntime(
	runtime RuntimeDescriptor,
	toolchainDigest string,
) buildPolicyDocument {
	return buildPolicyDocument{
		Current: map[string]buildPolicyTarget{
			"us-east-1": {
				MaterializerVersion:     DependencyMaterializerVersion,
				RuntimeDigest:           runtime.Digest,
				StandardToolchainDigest: toolchainDigest,
			},
		},
		FormatVersion:      BuildPolicyFormatVersion,
		Runtimes:           []RuntimeDescriptor{runtime},
		ToolRegistryDigest: "sha256:" + strings.Repeat("e", 64),
	}
}

func parseBuildPolicyDocument(
	t *testing.T,
	document buildPolicyDocument,
) *BuildPolicy {
	t.Helper()
	policy, err := ParseBuildPolicy(canonicalBuildPolicyForTest(t, document))
	if err != nil {
		t.Fatal(err)
	}
	return policy
}

func canonicalBuildPolicyForTest(
	t *testing.T,
	document buildPolicyDocument,
) []byte {
	t.Helper()
	raw, err := canonicalBuildPolicyDocument(document)
	if err != nil {
		t.Fatal(err)
	}
	canonical, err := jsoncanon.Transform(raw)
	if err != nil {
		t.Fatal(err)
	}
	return canonical
}

func authenticatedRuntimeCatalogForTest(
	t *testing.T,
	runtimes []RuntimeDescriptor,
) *RuntimeCatalog {
	t.Helper()
	raw, err := CanonicalRuntimeCatalog(runtimes)
	if err != nil {
		t.Fatal(err)
	}
	catalog, err := ParseRuntimeCatalog(raw)
	if err != nil {
		t.Fatal(err)
	}
	catalog.authenticated = true
	return catalog
}

func authenticatedToolRegistryForRuntime(
	t *testing.T,
	runtime RuntimeDescriptor,
) (*ToolRegistry, string) {
	t.Helper()
	manager, toolchain, components, toolset := testToolset(t)
	manager.Architecture = runtime.Architecture
	toolchain.Architecture = runtime.Architecture
	toolchain.ManagedRuntimeDigest = runtime.Digest
	components.Architecture = runtime.Architecture
	components.ManagedRuntimeDigest = runtime.Digest
	components.Manager = manager
	components.Toolchain = toolchain
	toolset.Architecture = runtime.Architecture
	toolset.ManagedRuntimeDigest = runtime.Digest

	managerDigest, err := ManagerRegistrationDigest(manager)
	if err != nil {
		t.Fatal(err)
	}
	toolchainDigest, err := StandardToolchainDigest(toolchain)
	if err != nil {
		t.Fatal(err)
	}
	componentDigest, err := ComponentManifestDigest(components)
	if err != nil {
		t.Fatal(err)
	}
	toolset.ManagerRegistrationDigest = managerDigest
	toolset.StandardToolchainDigest = toolchainDigest
	toolset.ComponentManifestDigest = componentDigest
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
	return registry, toolchainDigest
}
