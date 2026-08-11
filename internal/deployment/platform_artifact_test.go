package deployment

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"slices"
	"strings"
	"testing"

	"github.com/ProtonMail/go-crypto/openpgp"
	"github.com/ProtonMail/go-crypto/openpgp/clearsign"
)

func TestManagerConformanceNamesAreFamilySpecific(t *testing.T) {
	common := []string{"entrypoint", "reported-version", "required-options"}
	for _, name := range []PackageManagerName{PackageManagerNPM, PackageManagerBun} {
		if actual := managerConformanceNames(name); !slices.Equal(actual, common) {
			t.Fatalf("%s conformance = %v, want %v", name, actual, common)
		}
	}
	pnpm := []string{
		"entrypoint",
		"pnpm-manager-replacement-denied",
		"pnpm-runtime-replacement-denied",
		"reported-version",
		"required-options",
	}
	if actual := managerConformanceNames(PackageManagerPNPM); !slices.Equal(actual, pnpm) {
		t.Fatalf("pnpm conformance = %v, want %v", actual, pnpm)
	}
	conformance := PlatformConformance{
		Results: []PlatformConformanceResult{
			{Name: "required-options", Outcome: "passed"},
			{Name: "pnpm-runtime-replacement-denied", Outcome: "passed"},
			{Name: "reported-version", Outcome: "passed"},
			{Name: "entrypoint", Outcome: "passed"},
			{Name: "pnpm-manager-replacement-denied", Outcome: "passed"},
		},
	}
	if err := normalizeConformance(
		&conformance,
		PlatformConformanceSet,
		nil,
	); err != nil {
		t.Fatal(err)
	}
	for index, result := range conformance.Results {
		if result.Name != pnpm[index] {
			t.Fatalf("normalized pnpm conformance = %v, want %v", conformance.Results, pnpm)
		}
	}
}

func TestRuntimeConformanceNamesIncludeExecutedChecks(t *testing.T) {
	want := []string{
		"network-denied",
		"node-architecture",
		"node-disable-types",
		"node-module-abi",
		"node-reported-version",
		"node-source-maps",
		"runtime-entrypoint",
		"runtime-loader-environment",
	}
	if actual := runtimeConformanceNames(); !slices.Equal(actual, want) {
		t.Fatalf("runtime conformance = %v, want %v", actual, want)
	}
}

func TestVerifyRetainedNodeSourceUsesPinnedReleaseKey(t *testing.T) {
	source := []byte("node distribution")
	sourceDigest := sha256.Sum256(source)
	filename := "node-v24.16.0-linux-x64.tar.xz"
	checksums := []byte(hex.EncodeToString(sourceDigest[:]) + "  " + filename + "\n")

	entity, err := openpgp.NewEntity("Helmr Test", "", "test@helmr.dev", nil)
	if err != nil {
		t.Fatal(err)
	}
	var keyring bytes.Buffer
	if err := entity.Serialize(&keyring); err != nil {
		t.Fatal(err)
	}
	var signed bytes.Buffer
	writer, err := clearsign.Encode(&signed, entity.PrivateKey, nil)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := writer.Write(checksums); err != nil {
		t.Fatal(err)
	}
	if err := writer.Close(); err != nil {
		t.Fatal(err)
	}

	memory := newMemoryArtifact()
	memory.addDirectory("helmr")
	memory.addDirectory("helmr/upstream")
	memory.addFile("helmr/upstream/SHASUMS256.txt", checksums, 0644)
	memory.addFile("helmr/upstream/SHASUMS256.txt.asc", signed.Bytes(), 0644)
	artifact, err := inspectArtifact(
		context.Background(),
		memory,
		runtimeArtifact,
		maxRuntimeLogicalBytes,
		squashFSPhysicalAlign,
	)
	if err != nil {
		t.Fatal(err)
	}
	fingerprint := strings.ToUpper(hex.EncodeToString(entity.PrimaryKey.Fingerprint))
	integrity := PlatformIntegrity{
		Identity: fingerprint,
		Evidence: []PlatformEvidenceFile{
			{Path: "helmr/upstream/SHASUMS256.txt"},
			{Path: "helmr/upstream/SHASUMS256.txt.asc"},
			{Path: "helmr/upstream/runtime-inputs.json"},
			{Path: "helmr/upstream/source"},
		},
		Source: PlatformSource{
			Digest:    "sha256:" + hex.EncodeToString(sourceDigest[:]),
			Origin:    NodeReleaseOrigin + "v24.16.0/" + filename,
			SizeBytes: int64(len(source)),
		},
	}
	expectation := PlatformArtifactExpectation{
		IntegrityIdentities: []string{fingerprint},
		NodeReleaseKeyring:  base64.StdEncoding.EncodeToString(keyring.Bytes()),
		SourceOrigin:        integrity.Source.Origin,
	}
	if err := verifyRetainedNodeSource(
		context.Background(),
		artifact,
		bytes.NewReader(source),
		integrity,
		expectation,
	); err != nil {
		t.Fatal(err)
	}
	plainPath := "helmr/upstream/SHASUMS256.txt"
	memory.files[plainPath][0] ^= 1
	err = verifyRetainedNodeSource(
		context.Background(),
		artifact,
		bytes.NewReader(source),
		integrity,
		expectation,
	)
	memory.files[plainPath][0] ^= 1
	if err == nil || err.Error() != "retained Node.js checksum document does not match its signed cleartext" {
		t.Fatalf("mismatched retained checksum error = %v", err)
	}
	if err := verifyRetainedNodeSource(
		context.Background(),
		artifact,
		bytes.NewReader([]byte("tampered source")),
		integrity,
		expectation,
	); err == nil {
		t.Fatal("tampered Node distribution was accepted")
	}
}

func TestValidateRetainedSourceEvidenceBindsSourceDescriptor(t *testing.T) {
	integrity := PlatformIntegrity{
		Evidence: []PlatformEvidenceFile{{
			Path:      "helmr/upstream/source",
			Digest:    "sha256:source",
			SizeBytes: 42,
		}},
		Source: PlatformSource{
			Digest:    "sha256:source",
			SizeBytes: 42,
		},
	}
	if err := validateRetainedSourceEvidence(integrity); err != nil {
		t.Fatal(err)
	}
	integrity.Source.Digest = "sha256:tampered"
	if err := validateRetainedSourceEvidence(integrity); err == nil {
		t.Fatal("mismatched retained source digest was accepted")
	}
	integrity.Source.Digest = "sha256:source"
	integrity.Source.SizeBytes++
	if err := validateRetainedSourceEvidence(integrity); err == nil {
		t.Fatal("mismatched retained source size was accepted")
	}
}

func TestValidatePlatformIntegrityUsesExactBunRedirectHosts(t *testing.T) {
	expectation := PlatformArtifactExpectation{
		AllowedRedirectHosts: []string{
			"api.github.com",
			"github.com",
			"objects.githubusercontent.com",
			"release-assets.githubusercontent.com",
		},
		IntegrityIdentities: []string{"github-releases"},
		IntegrityKind:       "github-sha256",
		SourceOrigin:        "https://github.com/oven-sh/bun/releases/download/bun-v1.3.10/bun-linux-x64-baseline.zip",
	}
	integrity := PlatformIntegrity{
		Identity:      "github-releases",
		IntegrityKind: "github-sha256",
		Redirects: []string{
			"https://release-assets.githubusercontent.com/github-production-release-asset/fixture?sp=r&sig=fixture",
		},
		Source: PlatformSource{Origin: expectation.SourceOrigin},
	}
	if err := validatePlatformIntegrity(&inspectedArtifact{}, integrity, expectation); err != nil {
		t.Fatal(err)
	}
	integrity.Redirects[0] = "https://media.githubusercontent.com/oven-sh/bun/fixture.zip?sig=fixture"
	if err := validatePlatformIntegrity(&inspectedArtifact{}, integrity, expectation); err == nil ||
		err.Error() != "platform redirect escaped its policy hosts" {
		t.Fatalf("sibling redirect error = %v", err)
	}
}

func TestVerifyToolchainCompilerBindsExecutableInputs(t *testing.T) {
	memory := newMemoryArtifact()
	for _, path := range []string{
		"helmr",
		"include",
		"include/node",
		"node_modules",
		"node_modules/@esbuild",
		"node_modules/@esbuild/linux-x64",
		"node_modules/@esbuild/linux-x64/bin",
		"node_modules/esbuild",
		"node_modules/esbuild/lib",
	} {
		memory.addDirectory(path)
	}
	config := []byte("config")
	program := []byte("program")
	binary := []byte("binary")
	memory.addFile("helmr/config-evaluator.mjs", config, 0644)
	memory.addFile("helmr/program-compiler.mjs", program, 0644)
	memory.addLink(
		"helmr/esbuild",
		"../node_modules/@esbuild/linux-x64/bin/esbuild",
	)
	memory.addFile(
		"node_modules/@esbuild/linux-x64/bin/esbuild",
		binary,
		0755,
	)
	memory.addFile("node_modules/esbuild/lib/main.js", []byte("api"), 0644)
	memory.addFile("node_modules/esbuild/package.json", []byte("{}"), 0644)
	memory.addFile("include/node/node.h", []byte("header"), 0644)
	artifact, err := inspectArtifact(
		context.Background(),
		memory,
		toolchainArtifact,
		maxToolArtifactBytes,
		squashFSPhysicalAlign,
	)
	if err != nil {
		t.Fatal(err)
	}
	compiler := testCompilerInputs()
	compiler.ConfigEvaluator.Digest = digestBytes(config)
	compiler.ProgramCompiler.Digest = digestBytes(program)
	compiler.Esbuild.BinaryDigest = digestBytes(binary)
	compiler.Esbuild.APIPackageDigest, err = compilerPackageDigest(
		context.Background(),
		artifact,
		"node_modules/esbuild",
	)
	if err != nil {
		t.Fatal(err)
	}
	headersDigest, err := artifactDirectoryDigest(
		context.Background(),
		artifact,
		"include/node",
	)
	if err != nil {
		t.Fatal(err)
	}
	descriptor := ToolchainArtifactDescriptor{
		Compiler:          compiler,
		NodeHeadersDigest: headersDigest,
	}
	descriptorRaw, err := CanonicalPlatformDocument(descriptor)
	if err != nil {
		t.Fatal(err)
	}
	memory.addFile(PlatformDescriptorPath, descriptorRaw, 0644)
	artifact, err = inspectArtifact(
		context.Background(),
		memory,
		toolchainArtifact,
		maxToolArtifactBytes,
		squashFSPhysicalAlign,
	)
	if err != nil {
		t.Fatal(err)
	}
	if err := verifyToolchainCompiler(
		context.Background(),
		artifact,
		compiler,
	); err != nil {
		t.Fatal(err)
	}
	memory.files["helmr/program-compiler.mjs"] = []byte("tampered")
	if err := verifyToolchainCompiler(
		context.Background(),
		artifact,
		compiler,
	); err == nil {
		t.Fatal("tampered Program Compiler was accepted")
	}
}
