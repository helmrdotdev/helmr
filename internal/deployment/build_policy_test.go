package deployment

import (
	"bytes"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

func TestBuildPolicyAdmitsDomainsWithoutReleaseCatalog(t *testing.T) {
	raw := testBuildPolicy(t)
	policy, err := ParseBuildPolicy(raw)
	if err != nil {
		t.Fatal(err)
	}
	if _, flags, err := policy.Node("24.11.1"); err != nil ||
		!slices.Equal(flags, []string{NodeNoExperimentalStripTypes, "--enable-source-maps"}) {
		t.Fatalf("Node 24.11.1 = %q, %v", flags, err)
	}
	if _, flags, err := policy.Node("24.12.0"); err != nil ||
		!slices.Equal(flags, []string{NodeNoStripTypes, "--enable-source-maps"}) {
		t.Fatalf("Node 24.12.0 = %q, %v", flags, err)
	}
	if _, _, err := policy.Node("23.1.0"); err == nil {
		t.Fatal("Node 23.1.0 was admitted")
	}
	for _, manager := range []PackageManager{
		{Name: PackageManagerNPM, Version: "11.99.0"},
		{Name: PackageManagerPNPM, Version: "11.1.1"},
		{Name: PackageManagerBun, Version: "1.4.0"},
	} {
		if _, err := policy.Manager(manager); err != nil {
			t.Fatalf("Manager(%+v): %v", manager, err)
		}
	}
	for _, manager := range []PackageManager{
		{Name: PackageManagerNPM, Version: "12.0.0"},
		{Name: PackageManagerPNPM, Version: "latest"},
		{Name: PackageManagerBun, Version: "1.3.9"},
	} {
		if _, err := policy.Manager(manager); err == nil {
			t.Fatalf("Manager(%+v) was admitted", manager)
		}
	}
	digest, err := policy.Digest()
	if err != nil || !strings.HasPrefix(digest, "sha256:") {
		t.Fatalf("Digest() = %q, %v", digest, err)
	}
}

func TestBuildPolicyRequiresCompleteCanonicalShape(t *testing.T) {
	raw := testBuildPolicy(t)
	if _, err := ParseBuildPolicy(append([]byte(" "), raw...)); err == nil {
		t.Fatal("noncanonical build policy was admitted")
	}
	if _, err := ParseBuildPolicy(bytes.Replace(
		raw,
		[]byte(`"fixtureSet":"helmr.platform.fixtures.v0"`),
		[]byte(`"fixtureSet":"other"`),
		1,
	)); err == nil {
		t.Fatal("unknown fixture set was admitted")
	}
	if _, err := ParseBuildPolicy(bytes.Replace(
		raw,
		[]byte(`"formatVersion":0`),
		[]byte(`"formatVersion":0,"releases":[]`),
		1,
	)); err == nil {
		t.Fatal("release catalog member was admitted")
	}
}

func TestLoadBuildPolicyReadsCanonicalPolicy(t *testing.T) {
	path := filepath.Join(t.TempDir(), "build-policy.json")
	if err := os.WriteFile(path, testBuildPolicy(t), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := LoadBuildPolicy(path); err != nil {
		t.Fatal(err)
	}
}

func TestComposeBuildPolicyProducesClosedPolicy(t *testing.T) {
	raw, err := ComposeBuildPolicy(
		RuntimeInputs{
			Harness: ArtifactDescriptor{
				Digest: testDigest("harness"), MediaType: PlatformTreeInputMediaType, SizeBytes: 4096,
			},
		},
		ToolchainInputs{
			Base: ArtifactDescriptor{
				Digest: testDigest("toolchain"), MediaType: PlatformTreeInputMediaType, SizeBytes: 8192,
			},
			Compiler: testCompilerInputs(),
		},
		[]byte("node release keyring"),
		[]string{
			"FFEEDDCCBBAA99887766554433221100FFEEDDCC",
			"00112233445566778899AABBCCDDEEFF00112233",
		},
	)
	if err != nil {
		t.Fatal(err)
	}
	policy, err := ParseBuildPolicy(raw)
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := policy.Node("22.18.0"); err != nil {
		t.Fatal(err)
	}
	if _, err := policy.Manager(PackageManager{Name: PackageManagerPNPM, Version: "11.1.0"}); err != nil {
		t.Fatal(err)
	}
}

func testBuildPolicy(t *testing.T) []byte {
	t.Helper()
	raw, err := canonicalBuildPolicy(buildPolicyDocument{
		Architecture:            ArchitectureX8664,
		Denies:                  BuildPolicyDenies{Digests: []string{}, Selectors: []string{}},
		DescriptorSchemaVersion: PlatformDescriptorSchemaV0,
		FixtureSet:              PlatformFixtureSet,
		FormatVersion:           BuildPolicyFormatVersion,
		Managers: []ManagerPolicy{
			{
				AdapterVersion: ManagerAdapterVersion, AllowedOrigin: BunReleaseOrigin,
				AllowedRedirectHosts: []string{"api.github.com", "github.com", "objects.githubusercontent.com"},
				Domain:               VersionDomain{Major: 1, Minimum: "1.3.10"},
				MetadataOrigin:       BunMetadataOrigin, Name: PackageManagerBun,
			},
			{
				AdapterVersion: ManagerAdapterVersion, AllowedOrigin: NPMReleaseOrigin,
				AllowedRedirectHosts: []string{"registry.npmjs.org"},
				Domain:               VersionDomain{Major: 11, Minimum: "11.4.2"},
				MetadataOrigin:       NPMReleaseOrigin, Name: PackageManagerNPM,
			},
			{
				AdapterVersion: ManagerAdapterVersion, AllowedOrigin: PNPMReleaseOrigin,
				AllowedRedirectHosts: []string{"registry.npmjs.org"},
				Domain:               VersionDomain{Major: 11, Minimum: "11.1.0"},
				MetadataOrigin:       PNPMReleaseOrigin, Name: PackageManagerPNPM,
			},
		},
		Node: NodePolicy{
			AdapterVersion: NodeRuntimeAdapterVersion, AllowedOrigin: NodeReleaseOrigin,
			AllowedRedirectHosts:   []string{"nodejs.org"},
			Domains:                []VersionDomain{{Major: 22, Minimum: "22.18.0"}, {Major: 24, Minimum: "24.3.0"}},
			ReleaseKeyFingerprints: []string{"00112233445566778899AABBCCDDEEFF00112233"},
			ReleaseKeyring:         "AQ==",
		},
		Runtime: RuntimeInputs{
			Harness: ArtifactDescriptor{
				Digest:    "sha256:" + strings.Repeat("2", 64),
				MediaType: PlatformTreeInputMediaType,
				SizeBytes: 4096,
			},
		},
		Toolchain: ToolchainInputs{
			Base: ArtifactDescriptor{
				Digest:    "sha256:" + strings.Repeat("3", 64),
				MediaType: PlatformTreeInputMediaType,
				SizeBytes: 4096,
			},
			Compiler: testCompilerInputs(),
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	return raw
}

func testCompilerInputs() CompilerInputs {
	return CompilerInputs{
		APIVersion: "helmr.compiler.v0",
		ConfigEvaluator: CompilerEntrypoint{
			APIVersion: ConfigEvaluatorAPIVersion,
			Digest:     testDigest("config evaluator"),
			Entrypoint: "/nix/helmr/config-evaluator.mjs",
		},
		Esbuild: EsbuildInputs{
			APIPackageDigest: testDigest("esbuild api"),
			BinaryDigest:     testDigest("esbuild binary"),
			BinaryPath:       "/nix/helmr/esbuild",
			PackagePath:      "/nix/node_modules/esbuild",
			Version:          "0.28.1",
		},
		OptionsContractDigest: testDigest("compiler options contract"),
		Output: CompilerOutputContract{
			Aggregate:    "analysis-only",
			FinalModules: "independent",
			SharedChunks: false,
			SourceMaps:   "external",
		},
		ProgramCompiler: CompilerEntrypoint{
			APIVersion: "helmr.compiler.v0",
			Digest:     testDigest("program compiler"),
			Entrypoint: "/nix/helmr/program-compiler.mjs",
		},
		Source: CompilerSourceContract{
			DeclarationExtensions: []string{".cjs", ".cts", ".js", ".jsx", ".mjs", ".mts", ".ts", ".tsx"},
			PackageDependencies:   "external",
			Semantics:             "pinned-esbuild",
			WorkspaceDependencies: "bundled",
		},
	}
}
