package deployment

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"reflect"
	"strings"
	"testing"

	"github.com/helmrdotdev/helmr/internal/jsoncanon"
)

func TestToolsetDocumentsAndRegistryRoundTrip(t *testing.T) {
	manager, toolchain, components, toolset := testToolset(t)

	managerRaw, err := CanonicalManagerRegistration(manager)
	if err != nil {
		t.Fatal(err)
	}
	managerDigest, err := ManagerRegistrationDigest(manager)
	if err != nil {
		t.Fatal(err)
	}
	plainManager := sha256.Sum256(managerRaw)
	if managerDigest == "sha256:"+hex.EncodeToString(plainManager[:]) {
		t.Fatal("manager registration digest is not domain separated")
	}

	toolchainRaw, err := CanonicalToolchain(toolchain)
	if err != nil {
		t.Fatal(err)
	}
	toolchainDigest, err := StandardToolchainDigest(toolchain)
	if err != nil {
		t.Fatal(err)
	}
	plainToolchain := sha256.Sum256(toolchainRaw)
	if toolchainDigest == "sha256:"+hex.EncodeToString(plainToolchain[:]) {
		t.Fatal("standard toolchain digest is not domain separated")
	}
	componentDigest, err := ComponentManifestDigest(components)
	if err != nil {
		t.Fatal(err)
	}
	toolsetDigest, err := DependencyToolsDigest(toolset)
	if err != nil {
		t.Fatal(err)
	}
	const wantManagerDigest = "sha256:d8bb3fb2261a2be3b8f00f701a5952be4b452efa184b44b5d5216fee24ab9deb"
	const wantToolchainDigest = "sha256:9a275667c944b6fe99525bec560de40f7914f72d5d2613f79d891e4dd84e87fd"
	const wantComponentDigest = "sha256:4bcd5bdb5ca58047d6f3f4a186b3753fb983cdf68f50fc06a3f036ae91fec2a4"
	const wantToolsetDigest = "sha256:57e2fafd748e503c15aabcf475695f823e70d49f52431cc0dd2727e774076ae8"
	if managerDigest != wantManagerDigest ||
		toolchainDigest != wantToolchainDigest ||
		componentDigest != wantComponentDigest ||
		toolsetDigest != wantToolsetDigest {
		t.Fatalf(
			"tool digests = %q %q %q %q",
			managerDigest,
			toolchainDigest,
			componentDigest,
			toolsetDigest,
		)
	}

	componentsRaw, err := CanonicalToolComponents(components)
	if err != nil {
		t.Fatal(err)
	}
	parsedComponents, err := ParseToolComponents(componentsRaw)
	if err != nil {
		t.Fatal(err)
	}
	if err := ValidateToolComponents(toolset, parsedComponents); err != nil {
		t.Fatal(err)
	}

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
	if !reflect.DeepEqual(registry.managers, []ManagerRegistration{manager}) ||
		!reflect.DeepEqual(registry.toolchains, []Toolchain{toolchain}) ||
		!reflect.DeepEqual(registry.toolsets, []Toolset{toolset}) {
		t.Fatalf("parsed registry = %#v", registry)
	}
}

func TestToolsetRegistryRejectsInvalidComposition(t *testing.T) {
	manager, toolchain, components, toolset := testToolset(t)
	tests := map[string]func(*ManagerRegistration, *Toolchain, *ToolComponents, *Toolset){
		"manager architecture": func(manager *ManagerRegistration, _ *Toolchain, _ *ToolComponents, _ *Toolset) {
			manager.Architecture = ArchitectureX8664
		},
		"runtime": func(_ *ManagerRegistration, toolchain *Toolchain, _ *ToolComponents, _ *Toolset) {
			toolchain.ManagedRuntimeDigest = toolDigestForTest("other runtime")
		},
		"manager digest": func(_ *ManagerRegistration, _ *Toolchain, _ *ToolComponents, toolset *Toolset) {
			toolset.ManagerRegistrationDigest = toolDigestForTest("other manager")
		},
		"toolchain digest": func(_ *ManagerRegistration, _ *Toolchain, _ *ToolComponents, toolset *Toolset) {
			toolset.StandardToolchainDigest = toolDigestForTest("other toolchain")
		},
		"toolset environment": func(_ *ManagerRegistration, _ *Toolchain, _ *ToolComponents, toolset *Toolset) {
			toolset.Environment[0].Value = "/other"
		},
		"component digest": func(_ *ManagerRegistration, _ *Toolchain, _ *ToolComponents, toolset *Toolset) {
			toolset.ComponentManifestDigest = toolDigestForTest("other components")
		},
	}
	for name, mutate := range tests {
		t.Run(name, func(t *testing.T) {
			manager := manager
			toolchain := toolchain
			components := components
			toolset := toolset
			toolset.Environment = append([]ToolEnvironment(nil), toolset.Environment...)
			mutate(&manager, &toolchain, &components, &toolset)
			if _, err := CanonicalToolRegistry(
				[]ManagerRegistration{manager},
				[]Toolchain{toolchain},
				[]Toolset{toolset},
			); err == nil && name != "toolset environment" && name != "component digest" {
				t.Fatal("CanonicalToolRegistry returned nil error")
			}
			if (name == "toolset environment" || name == "component digest") &&
				ValidateToolComponents(toolset, components) == nil {
				t.Fatal("ValidateToolComponents returned nil error")
			}
		})
	}
}

func TestToolsetDocumentsRejectOpenAndNonCanonicalShapes(t *testing.T) {
	_, _, components, toolset := testToolset(t)
	raw, err := CanonicalToolComponents(components)
	if err != nil {
		t.Fatal(err)
	}
	var value map[string]any
	if err := json.Unmarshal(raw, &value); err != nil {
		t.Fatal(err)
	}
	value["unknown"] = true
	openRaw, err := jsoncanon.Transform(mustJSON(t, value))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := ParseToolComponents(openRaw); err == nil {
		t.Fatal("ParseToolComponents accepted an unknown member")
	}
	if _, err := ParseToolComponents(append([]byte(" "), raw...)); err == nil {
		t.Fatal("ParseToolComponents accepted non-canonical JSON")
	}

	components.SystemAliases[0].Path = "/bin/bash"
	if _, err := CanonicalToolComponents(components); err == nil {
		t.Fatal("CanonicalToolComponents accepted an unregistered system alias")
	}
	toolset.Environment[1].Value = "/workspace/bin"
	if _, err := CanonicalToolset(toolset); err == nil {
		t.Fatal("CanonicalToolset accepted a Workspace PATH")
	}
}

func TestToolsetDocumentsRejectNULAtExecBoundary(t *testing.T) {
	manager, _, components, toolset := testToolset(t)
	manager.Resolution.Argv[0] += "\x00"
	if _, err := CanonicalManagerRegistration(manager); err == nil {
		t.Fatal("CanonicalManagerRegistration accepted NUL in argv")
	}
	toolset.Environment[0].Value += "\x00"
	if _, err := CanonicalToolset(toolset); err == nil {
		t.Fatal("CanonicalToolset accepted NUL in environment")
	}
	components.Launchers[0].Target += "\x00"
	if _, err := CanonicalToolComponents(components); err == nil {
		t.Fatal("CanonicalToolComponents accepted NUL in a target")
	}
	components.Launchers[0].Target = "/nix/store/bbbbbbbbbbbbbbbb-bun/bin/bun"
	components.Launchers[0].Path += "\x00"
	if _, err := CanonicalToolComponents(components); err == nil {
		t.Fatal("CanonicalToolComponents accepted NUL in a launcher path")
	}
}

func TestToolComponentsAllowSharedSystemAliasTarget(t *testing.T) {
	_, _, components, _ := testToolset(t)
	components.SystemAliases[1].Target = components.SystemAliases[0].Target
	if _, err := CanonicalToolComponents(components); err != nil {
		t.Fatal(err)
	}
}

func TestToolsetRegistryEnforcesKeyOrderAndUniqueSelection(t *testing.T) {
	manager, toolchain, _, toolset := testToolset(t)
	second := manager
	second.PackageManager.Version = "1.3.11"
	second.VersionProbe.StdoutBase64 = "MS4zLjExCg=="
	secondDigest, err := ManagerRegistrationDigest(second)
	if err != nil {
		t.Fatal(err)
	}
	secondToolset := toolset
	secondToolset.PackageManager = second.PackageManager
	secondToolset.ManagerRegistrationDigest = secondDigest

	if _, err := CanonicalToolRegistry(
		[]ManagerRegistration{second, manager},
		[]Toolchain{toolchain},
		[]Toolset{toolset, secondToolset},
	); err == nil {
		t.Fatal("CanonicalToolRegistry accepted managers out of order")
	}
	if _, err := CanonicalToolRegistry(
		[]ManagerRegistration{manager, second},
		[]Toolchain{toolchain},
		[]Toolset{secondToolset, toolset},
	); err == nil {
		t.Fatal("CanonicalToolRegistry accepted toolsets out of order")
	}
}

func TestToolsetRegistryAdmitsToolchainsThatDifferOnlyByDigest(t *testing.T) {
	manager, firstToolchain, _, firstToolset := testToolset(t)
	secondToolchain := firstToolchain
	secondToolchain.ToolchainClosure.Digest = toolDigestForTest("second toolchain closure")

	firstDigest, err := StandardToolchainDigest(firstToolchain)
	if err != nil {
		t.Fatal(err)
	}
	secondDigest, err := StandardToolchainDigest(secondToolchain)
	if err != nil {
		t.Fatal(err)
	}
	secondToolset := firstToolset
	secondToolset.StandardToolchainDigest = secondDigest
	secondToolset.ComponentManifestDigest = toolDigestForTest("second components")
	secondToolset.Artifact.Digest = toolDigestForTest("second dependency tools artifact")

	toolchains := []Toolchain{firstToolchain, secondToolchain}
	toolchainDigests := []string{firstDigest, secondDigest}
	if compareToolchains(toolchains[0], toolchains[1], toolchainDigests[0], toolchainDigests[1]) > 0 {
		toolchains[0], toolchains[1] = toolchains[1], toolchains[0]
		toolchainDigests[0], toolchainDigests[1] = toolchainDigests[1], toolchainDigests[0]
	}
	toolsets := []Toolset{firstToolset, secondToolset}
	if compareToolsets(toolsets[0], toolsets[1]) > 0 {
		toolsets[0], toolsets[1] = toolsets[1], toolsets[0]
	}

	raw, err := CanonicalToolRegistry(
		[]ManagerRegistration{manager},
		toolchains,
		toolsets,
	)
	if err != nil {
		t.Fatal(err)
	}
	registry, err := ParseToolRegistry(raw)
	if err != nil {
		t.Fatal(err)
	}
	if len(registry.toolchains) != 2 || len(registry.toolsets) != 2 {
		t.Fatalf(
			"registry members = %d toolchains, %d toolsets",
			len(registry.toolchains),
			len(registry.toolsets),
		)
	}
	if _, err := registry.Resolve(ToolKey{
		Architecture:            firstToolset.Architecture,
		ManagedRuntimeDigest:    firstToolset.ManagedRuntimeDigest,
		MaterializerVersion:     firstToolset.MaterializerVersion,
		PackageManager:          firstToolset.PackageManager,
		StandardToolchainDigest: firstToolset.StandardToolchainDigest,
	}); err == nil {
		t.Fatal("Resolve accepted an unauthenticated registry")
	}
	registry.authenticated = true
	for _, expected := range []Toolset{firstToolset, secondToolset} {
		resolved, err := registry.Resolve(ToolKey{
			Architecture:            expected.Architecture,
			ManagedRuntimeDigest:    expected.ManagedRuntimeDigest,
			MaterializerVersion:     expected.MaterializerVersion,
			PackageManager:          expected.PackageManager,
			StandardToolchainDigest: expected.StandardToolchainDigest,
		})
		if err != nil {
			t.Fatal(err)
		}
		if resolved.StandardToolchainDigest != expected.StandardToolchainDigest {
			t.Fatalf(
				"resolved toolchain = %q, want %q",
				resolved.StandardToolchainDigest,
				expected.StandardToolchainDigest,
			)
		}
	}

	if _, err := CanonicalToolRegistry(
		[]ManagerRegistration{manager},
		[]Toolchain{toolchains[1], toolchains[0]},
		toolsets,
	); err == nil {
		t.Fatal("CanonicalToolRegistry accepted toolchains out of digest order")
	}
	if _, err := CanonicalToolRegistry(
		[]ManagerRegistration{manager},
		toolchains,
		[]Toolset{toolsets[1], toolsets[0]},
	); err == nil {
		t.Fatal("CanonicalToolRegistry accepted toolsets out of toolchain order")
	}
}

func TestToolsetDocumentsRejectOversizedPhysicalObject(t *testing.T) {
	manager, _, _, _ := testToolset(t)
	manager.ManagerClosure.SizeBytes = maxToolArtifactBytes + 1
	if _, err := CanonicalManagerRegistration(manager); err == nil {
		t.Fatal("CanonicalManagerRegistration accepted an oversized object")
	}
}

func TestToolsetRegistryEnforcesNestedDocumentBounds(t *testing.T) {
	manager, toolchain, components, toolset := testToolset(t)
	argument := strings.Repeat("x", maxToolArgBytes)
	oversized := []string{manager.Executable, argument, argument, argument}
	manager.Resolution.Argv = append([]string(nil), oversized...)
	manager.Lifecycle.Argv = append([]string(nil), oversized...)
	manager.VersionProbe.Argv = append([]string(nil), oversized...)
	if _, err := CanonicalManagerRegistration(manager); err == nil {
		t.Fatal("CanonicalManagerRegistration accepted an oversized document")
	}
	components.Manager = manager
	if _, err := CanonicalToolComponents(components); err == nil {
		t.Fatal("CanonicalToolComponents accepted an oversized manager registration")
	}
	if _, err := CanonicalToolRegistry(
		[]ManagerRegistration{manager},
		[]Toolchain{toolchain},
		[]Toolset{toolset},
	); err == nil {
		t.Fatal("CanonicalToolRegistry accepted an oversized manager registration")
	}
}

func testToolset(t *testing.T) (
	ManagerRegistration,
	Toolchain,
	ToolComponents,
	Toolset,
) {
	t.Helper()
	manager := ManagerRegistration{
		Architecture:    ArchitectureAArch64,
		Executable:      "/opt/helmr/dependency-tools/bin/bun",
		FormatVersion:   ToolsetFormatVersion,
		Lifecycle:       ToolCommand{Argv: []string{"/opt/helmr/dependency-tools/bin/bun"}},
		LockfileAdapter: "bun-lock-v0",
		ManagerClosure: ManagerArtifact{
			Digest:    toolDigestForTest("manager closure"),
			MediaType: ManagerComponentMediaType,
			SizeBytes: 1024,
		},
		OfflineStore: ToolOfflineStore{
			ReadOnlyMountPath: "/opt/helmr/offline-store",
			WorkPath:          "/work/offline-store",
		},
		PackageManager: PackageManager{Name: PackageManagerBun, Version: "1.3.10"},
		Proxy:          ToolProxy{RegistryOrigin: "http://127.0.0.1:4873"},
		Resolution:     ToolCommand{Argv: []string{"/opt/helmr/dependency-tools/bin/bun"}},
		VersionProbe: ToolVersionProbe{
			Argv:         []string{"/opt/helmr/dependency-tools/bin/bun", "--version"},
			StdoutBase64: "MS4zLjEwCg==",
		},
	}
	toolchain := Toolchain{
		Architecture:         ArchitectureAArch64,
		FormatVersion:        ToolsetFormatVersion,
		ManagedRuntimeDigest: toolDigestForTest("managed runtime"),
		ToolchainClosure: ManagerArtifact{
			Digest:    toolDigestForTest("toolchain closure"),
			MediaType: ToolchainMediaType,
			SizeBytes: 2048,
		},
	}
	environment := []ToolEnvironment{
		{Name: "HOME", Value: "/work/home"},
		{
			Name:  "PATH",
			Value: "/opt/helmr/dependency-tools/bin:/nix/store/aaaaaaaaaaaaaaaa-toolchain/bin",
		},
	}
	components := ToolComponents{
		Architecture:         ArchitectureAArch64,
		Environment:          append([]ToolEnvironment(nil), environment...),
		FormatVersion:        ToolsetFormatVersion,
		Launchers:            []ToolLink{{Path: "bin/bun", Target: "/nix/store/bbbbbbbbbbbbbbbb-bun/bin/bun"}},
		ManagedRuntimeDigest: toolchain.ManagedRuntimeDigest,
		Manager:              manager,
		MaterializerVersion:  DependencyMaterializerVersion,
		PackageManager:       manager.PackageManager,
		SystemAliases: []ToolLink{
			{Path: "/bin/sh", Target: "/nix/store/cccccccccccccccc-shell/bin/sh"},
			{Path: "/usr/bin/env", Target: "/nix/store/dddddddddddddddd-coreutils/bin/env"},
		},
		Toolchain: toolchain,
	}
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
	toolset := Toolset{
		Architecture: ArchitectureAArch64,
		Artifact: ManagerArtifact{
			Digest:    toolDigestForTest("dependency tools artifact"),
			MediaType: ManagerDependencyToolsMediaType,
			SizeBytes: 4096,
		},
		ComponentManifestDigest:   componentDigest,
		Environment:               append([]ToolEnvironment(nil), environment...),
		FormatVersion:             ToolsetFormatVersion,
		ManagedRuntimeDigest:      toolchain.ManagedRuntimeDigest,
		ManagerRegistrationDigest: managerDigest,
		MaterializerVersion:       DependencyMaterializerVersion,
		PackageManager:            manager.PackageManager,
		StandardToolchainDigest:   toolchainDigest,
	}
	return manager, toolchain, components, toolset
}

func toolDigestForTest(value string) string {
	digest := sha256.Sum256([]byte(value))
	return "sha256:" + hex.EncodeToString(digest[:])
}

func TestToolRegistryRejectsUnknownAndDuplicateMembers(t *testing.T) {
	manager, toolchain, _, toolset := testToolset(t)
	raw, err := CanonicalToolRegistry(
		[]ManagerRegistration{manager},
		[]Toolchain{toolchain},
		[]Toolset{toolset},
	)
	if err != nil {
		t.Fatal(err)
	}
	unknown := bytes.Replace(raw, []byte(`{"formatVersion":0`), []byte(`{"extra":true,"formatVersion":0`), 1)
	if _, err := ParseToolRegistry(unknown); err == nil {
		t.Fatal("ParseToolRegistry accepted an unknown member")
	}
	duplicate := strings.Replace(string(raw), `"formatVersion":0`, `"formatVersion":0,"formatVersion":0`, 1)
	if _, err := ParseToolRegistry([]byte(duplicate)); err == nil {
		t.Fatal("ParseToolRegistry accepted a duplicate member")
	}
}
