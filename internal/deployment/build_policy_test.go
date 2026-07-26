package deployment

import (
	"errors"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"

	"github.com/helmrdotdev/helmr/internal/jsoncanon"
)

func TestBuildPolicyLookups(t *testing.T) {
	runtime := testRuntimeDescriptor()
	toolchain, toolchainDigest := testToolchainForRuntime(t, runtime)
	document := buildPolicyForRuntime(runtime, toolchain, toolchainDigest)
	policy := parseBuildPolicyDocument(t, document)

	current, err := policy.Current("us-east-1")
	if err != nil {
		t.Fatal(err)
	}
	if current.Runtime != runtime ||
		current.StandardToolchainDigest != toolchainDigest ||
		current.BuildContractVersion != ProgramBuildContractVersion {
		t.Fatalf("Current(us-east-1) = %#v", current)
	}
	resolved, err := policy.ResolveRuntime(runtime.Digest)
	if err != nil {
		t.Fatal(err)
	}
	if resolved != runtime {
		t.Fatalf("ResolveRuntime(runtime) = %#v, want %#v", resolved, runtime)
	}
	resolvedToolchain, err := policy.ResolveToolchain(toolchainDigest)
	if err != nil {
		t.Fatal(err)
	}
	if resolvedToolchain != toolchain {
		t.Fatalf("ResolveToolchain(current) = %#v", resolvedToolchain)
	}
	if _, err := policy.Current("US-EAST-1"); !errors.Is(err, ErrBuildRegionNotConfigured) {
		t.Fatalf("Current(unconfigured) error = %v", err)
	}
	if _, err := policy.ResolveRuntime("sha256:" + strings.Repeat("c", 64)); !errors.Is(err, ErrRuntimeNotRegistered) {
		t.Fatalf("ResolveRuntime(unregistered) error = %v", err)
	}
	if _, err := policy.ResolveToolchain("sha256:" + strings.Repeat("c", 64)); !errors.Is(err, ErrStandardToolchainNotRegistered) {
		t.Fatalf("ResolveToolchain(unregistered) error = %v", err)
	}
}

func TestBuildPolicyValidatesProgramTarget(t *testing.T) {
	runtime := testRuntimeDescriptor()
	toolchain, toolchainDigest := testToolchainForRuntime(t, runtime)
	policy := parseBuildPolicyDocument(
		t,
		buildPolicyForRuntime(runtime, toolchain, toolchainDigest),
	)
	target, err := policy.Current("us-east-1")
	if err != nil {
		t.Fatal(err)
	}
	output := testProgramOutput(t)
	output.Index.RuntimeDigest = runtime.Digest
	output.Index.Architecture = runtime.Architecture
	output.Index.StandardToolchainDigest = toolchainDigest
	if err := ValidateProgramTarget(target, output); err != nil {
		t.Fatal(err)
	}

	wrongRuntime := output
	wrongRuntime.Index = cloneProgramIndex(output.Index)
	wrongRuntime.Index.RuntimeDigest = "sha256:" + strings.Repeat("b", 64)
	if err := ValidateProgramTarget(target, wrongRuntime); err == nil {
		t.Fatal("ValidateProgramTarget accepted another runtime")
	}
	wrongManager := output
	wrongManager.Index = cloneProgramIndex(output.Index)
	wrongManager.Index.Manager.Version = "1.3.11"
	if err := ValidateProgramTarget(target, wrongManager); err != nil {
		t.Fatal(err)
	}
	wrongToolchain := output
	wrongToolchain.Index = cloneProgramIndex(output.Index)
	wrongToolchain.Index.StandardToolchainDigest = "sha256:" + strings.Repeat("c", 64)
	if err := ValidateProgramTarget(target, wrongToolchain); err == nil {
		t.Fatal("ValidateProgramTarget accepted another standard toolchain")
	}
}

func TestLoadBuildPolicyRequiresExactAuthenticatedRegistries(t *testing.T) {
	runtime := testRuntimeDescriptor()
	toolchain, toolchainDigest := testToolchainForRuntime(t, runtime)
	document := buildPolicyForRuntime(runtime, toolchain, toolchainDigest)
	raw := canonicalBuildPolicyForTest(t, document)
	path := filepath.Join(t.TempDir(), "build-policy.json")
	if err := os.WriteFile(path, raw, 0o600); err != nil {
		t.Fatal(err)
	}
	runtimeCatalog := authenticatedRuntimeCatalogForTest(t, document.Runtimes)
	toolchainCatalog := authenticatedToolchainCatalogForTest(t, document.Toolchains)
	policy, err := LoadBuildPolicy(path, runtimeCatalog, toolchainCatalog)
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
	if _, err := LoadBuildPolicy(path, unverifiedCatalog, toolchainCatalog); err == nil {
		t.Fatal("LoadBuildPolicy accepted an unauthenticated runtime catalog")
	}
	toolchainCatalog.authenticated = false
	if _, err := LoadBuildPolicy(path, runtimeCatalog, toolchainCatalog); err == nil {
		t.Fatal("LoadBuildPolicy accepted an unauthenticated standard-toolchain catalog")
	}
}

func TestBuildPolicyRejectsRegistryAndCompatibilityDrift(t *testing.T) {
	runtime := testRuntimeDescriptor()
	toolchain, toolchainDigest := testToolchainForRuntime(t, runtime)
	document := buildPolicyForRuntime(runtime, toolchain, toolchainDigest)
	policy := parseBuildPolicyDocument(t, document)
	runtimeCatalog := authenticatedRuntimeCatalogForTest(t, document.Runtimes)
	toolchainCatalog := authenticatedToolchainCatalogForTest(t, document.Toolchains)

	policy.toolchainsBytes = []byte("drift")
	if err := validateBuildPolicyCatalogs(policy, runtimeCatalog, toolchainCatalog); err == nil {
		t.Fatal("build policy accepted standard-toolchain catalog drift")
	}
	policy.toolchainsBytes = toolchainCatalog.toolchainsBytes
	toolchain.ManagedRuntimeDigest = "sha256:" + strings.Repeat("e", 64)
	policy.toolchains[toolchainDigest] = toolchain
	if err := validateBuildPolicyCatalogs(policy, runtimeCatalog, toolchainCatalog); err == nil {
		t.Fatal("build policy accepted an incompatible toolchain")
	}
}

func TestValidateBuildPolicyUpgrade(t *testing.T) {
	first := testRuntimeDescriptor()
	firstToolchain, firstToolchainDigest := testToolchainForRuntime(t, first)
	previous := parseBuildPolicyDocument(
		t,
		buildPolicyForRuntime(first, firstToolchain, firstToolchainDigest),
	)

	second := first
	second.Digest = "sha256:" + strings.Repeat("b", 64)
	secondToolchain, secondToolchainDigest := testToolchainForRuntime(t, second)
	nextDocument := buildPolicyForRuntime(second, secondToolchain, secondToolchainDigest)
	nextDocument.Runtimes = []RuntimeDescriptor{first, second}
	nextDocument.Toolchains = []Toolchain{firstToolchain, secondToolchain}
	sort.Slice(nextDocument.Toolchains, func(left, right int) bool {
		leftDigest, err := StandardToolchainDigest(nextDocument.Toolchains[left])
		if err != nil {
			t.Fatal(err)
		}
		rightDigest, err := StandardToolchainDigest(nextDocument.Toolchains[right])
		if err != nil {
			t.Fatal(err)
		}
		return leftDigest < rightDigest
	})
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
	mutated = *next
	mutated.toolchains = map[string]Toolchain{}
	if err := ValidateBuildPolicyUpgrade(previous, &mutated); err == nil {
		t.Fatal("ValidateBuildPolicyUpgrade accepted standard-toolchain removal")
	}
	if err := ValidateBuildPolicyUpgrade(nil, next); err == nil {
		t.Fatal("ValidateBuildPolicyUpgrade accepted a nil snapshot")
	}
}

func TestBuildPolicyRejectsInvalidDocuments(t *testing.T) {
	runtime := testRuntimeDescriptor()
	toolchain, toolchainDigest := testToolchainForRuntime(t, runtime)
	validTarget := buildPolicyTarget{
		BuildContractVersion:    ProgramBuildContractVersion,
		RuntimeDigest:           runtime.Digest,
		StandardToolchainDigest: toolchainDigest,
	}
	valid := buildPolicyDocument{
		Current:       map[string]buildPolicyTarget{"us-east-1": validTarget},
		FormatVersion: BuildPolicyFormatVersion,
		Runtimes:      []RuntimeDescriptor{runtime},
		Toolchains:    []Toolchain{toolchain},
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
		"empty toolchains": func(value *buildPolicyDocument) {
			value.Toolchains = nil
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
			target.StandardToolchainDigest = "sha256:" + strings.Repeat("d", 64)
			value.Current = map[string]buildPolicyTarget{"us-east-1": target}
		},
		"invalid materializer": func(value *buildPolicyDocument) {
			target := validTarget
			target.BuildContractVersion = "helmr.dependencies.v1"
			value.Current = map[string]buildPolicyTarget{"us-east-1": target}
		},
		"incompatible toolchain": func(value *buildPolicyDocument) {
			incompatible := toolchain
			incompatible.ManagedRuntimeDigest = "sha256:" + strings.Repeat("e", 64)
			digest, err := StandardToolchainDigest(incompatible)
			if err != nil {
				t.Fatal(err)
			}
			value.Toolchains = []Toolchain{incompatible}
			target := validTarget
			target.StandardToolchainDigest = digest
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
	toolchain, toolchainDigest := testToolchainForRuntime(t, runtime)
	document := buildPolicyForRuntime(runtime, toolchain, toolchainDigest)
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
	toolchain Toolchain,
	toolchainDigest string,
) buildPolicyDocument {
	return buildPolicyDocument{
		Current: map[string]buildPolicyTarget{
			"us-east-1": {
				BuildContractVersion:    ProgramBuildContractVersion,
				RuntimeDigest:           runtime.Digest,
				StandardToolchainDigest: toolchainDigest,
			},
		},
		FormatVersion: BuildPolicyFormatVersion,
		Runtimes:      []RuntimeDescriptor{runtime},
		Toolchains:    []Toolchain{toolchain},
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

func testToolchainForRuntime(
	t *testing.T,
	runtime RuntimeDescriptor,
) (Toolchain, string) {
	t.Helper()
	toolchain := testToolchain(t)
	toolchain.Architecture = runtime.Architecture
	toolchain.ManagedRuntimeDigest = runtime.Digest
	toolchainDigest, err := StandardToolchainDigest(toolchain)
	if err != nil {
		t.Fatal(err)
	}
	return toolchain, toolchainDigest
}

func authenticatedToolchainCatalogForTest(
	t *testing.T,
	toolchains []Toolchain,
) *ToolchainCatalog {
	t.Helper()
	raw, err := CanonicalToolchainCatalog(toolchains)
	if err != nil {
		t.Fatal(err)
	}
	catalog, err := ParseToolchainCatalog(raw)
	if err != nil {
		t.Fatal(err)
	}
	catalog.authenticated = true
	return catalog
}
