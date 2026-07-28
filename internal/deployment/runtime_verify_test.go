package deployment

import (
	"context"
	"testing"
)

func TestRuntimeTopologyAcceptsClosedLayout(t *testing.T) {
	descriptor, artifact := newRuntimeTopology(t)
	inspected, err := inspectArtifact(
		context.Background(),
		artifact,
		runtimeArtifact,
		maxRuntimeLogicalBytes,
		descriptor.SizeBytes,
	)
	if err != nil {
		t.Fatal(err)
	}
	index, err := verifyRuntimeTopology(
		context.Background(),
		inspected,
		runtimeArtifactObject(descriptor),
	)
	if err != nil {
		t.Fatal(err)
	}
	if index.Architecture != descriptor.Architecture {
		t.Fatalf("runtime architecture = %q, want %q", index.Architecture, descriptor.Architecture)
	}
}

func TestVerifyRuntimeArtifactRejectsNilContextAndSnapshot(t *testing.T) {
	//lint:ignore SA1012 nil is the contract violation under test
	if _, err := VerifyRuntimeArtifact(nil, "/sys/fs/cgroup", "lease", nil); err == nil {
		t.Fatal("nil context was accepted")
	}
	if _, err := VerifyRuntimeArtifact(
		context.Background(),
		"/sys/fs/cgroup",
		"lease",
		nil,
	); err == nil {
		t.Fatal("nil snapshot was accepted")
	}
}

func TestRuntimeTopologyRejectsOpenOrDivergentLayout(t *testing.T) {
	removePath := func(filePath string) func(*memoryArtifact) {
		return func(artifact *memoryArtifact) {
			for index := range artifact.entries {
				if artifact.entries[index].Path != filePath {
					continue
				}
				artifact.entries = append(
					artifact.entries[:index],
					artifact.entries[index+1:]...,
				)
				delete(artifact.files, filePath)
				break
			}
		}
	}
	tests := map[string]func(*memoryArtifact){
		"extra top level": func(artifact *memoryArtifact) {
			artifact.addDirectory("etc")
		},
		"extra bin": func(artifact *memoryArtifact) {
			artifact.addFile("bin/other", []byte("other"), 0755)
		},
		"extra helmr": func(artifact *memoryArtifact) {
			artifact.addFile("helmr/other", []byte("other"), 0644)
		},
		"extra share": func(artifact *memoryArtifact) {
			artifact.addFile("share/other", []byte("other"), 0644)
		},
		"missing entry":   removePath(runtimeEntryPath),
		"missing license": removePath(runtimeLicensePath),
		"node mode": func(artifact *memoryArtifact) {
			artifact.mutate(runtimeNodePath, func(entry *artifactEntry) {
				entry.Mode = 0644
			})
		},
		"entry mode": func(artifact *memoryArtifact) {
			artifact.mutate(runtimeEntryPath, func(entry *artifactEntry) {
				entry.Mode = 0755
			})
		},
		"libc mode": func(artifact *memoryArtifact) {
			artifact.mutate(runtimeLibcPath, func(entry *artifactEntry) {
				entry.Mode = 0755
			})
		},
		"license mode": func(artifact *memoryArtifact) {
			artifact.mutate(runtimeLicensePath, func(entry *artifactEntry) {
				entry.Mode = 0755
			})
		},
	}
	for name, mutate := range tests {
		t.Run(name, func(t *testing.T) {
			descriptor, artifact := newRuntimeTopology(t)
			mutate(artifact)
			inspected, err := inspectArtifact(
				context.Background(),
				artifact,
				runtimeArtifact,
				maxRuntimeLogicalBytes,
				descriptor.SizeBytes,
			)
			if err == nil {
				_, err = verifyRuntimeTopology(
					context.Background(),
					inspected,
					runtimeArtifactObject(descriptor),
				)
			}
			if err == nil {
				t.Fatal("runtime topology was accepted")
			}
		})
	}
}

func TestRuntimeArtifactRoleUsesRuntimeBounds(t *testing.T) {
	logical, err := artifactLogicalLimit(runtimeArtifact)
	if err != nil {
		t.Fatal(err)
	}
	physical, err := artifactPhysicalLimit(runtimeArtifact)
	if err != nil {
		t.Fatal(err)
	}
	if logical != maxRuntimeLogicalBytes || physical != maxRuntimePhysicalBytes {
		t.Fatalf(
			"runtime bounds = (%d,%d), want (%d,%d)",
			logical,
			physical,
			maxRuntimeLogicalBytes,
			maxRuntimePhysicalBytes,
		)
	}
}

func TestVerifiedRuntimeResultMatchesDescriptor(t *testing.T) {
	descriptor := testRuntimeDescriptor()
	index, err := verifiedRuntimeResult(canonicalVerifierRuntimeIndex(t), descriptor)
	if err != nil {
		t.Fatal(err)
	}
	if index.Architecture != descriptor.Architecture {
		t.Fatalf("architecture = %q", index.Architecture)
	}

	for name, mutate := range map[string]func(*RuntimeDescriptor){
		"architecture": func(value *RuntimeDescriptor) {
			value.Architecture = RuntimeArchitecture("aarch64")
		},
		"runtime API": func(value *RuntimeDescriptor) {
			value.RuntimeAPIVersion = "helmr.runtime.v1"
		},
	} {
		t.Run(name, func(t *testing.T) {
			divergent := descriptor
			mutate(&divergent)
			if _, err := verifiedRuntimeResult(
				canonicalVerifierRuntimeIndex(t),
				divergent,
			); err == nil {
				t.Fatal("divergent descriptor was accepted")
			}
		})
	}
	if _, err := verifiedRuntimeResult(canonicalVerifierProgramIndex(t), descriptor); err == nil {
		t.Fatal("Program payload was accepted as a Runtime result")
	}
}

func newRuntimeTopology(t *testing.T) (RuntimeDescriptor, *memoryArtifact) {
	t.Helper()
	sourceRaw := []byte("upstream")
	source := PlatformSource{
		Digest:    digestDocument(sourceRaw),
		Origin:    "https://nodejs.org/dist/v24.16.0/node-v24.16.0-linux-x64.tar.xz",
		SizeBytes: int64(len(sourceRaw)),
	}
	evidence := []PlatformEvidenceFile{{
		Digest:    digestDocument(sourceRaw),
		Path:      "helmr/upstream/source",
		SizeBytes: int64(len(sourceRaw)),
	}}
	integrity := PlatformIntegrity{
		Evidence:      evidence,
		FormatVersion: PlatformArtifactDocumentFormatVersion,
		Identity:      "00112233445566778899AABBCCDDEEFF00112233",
		IntegrityKind: "openpgp-sha256",
		Redirects:     []string{},
		Source:        source,
	}
	integrityRaw, err := CanonicalPlatformDocument(integrity)
	if err != nil {
		t.Fatal(err)
	}
	results := make([]PlatformConformanceResult, len(runtimeConformanceNames()))
	for index, name := range runtimeConformanceNames() {
		results[index] = PlatformConformanceResult{Name: name, Outcome: "passed"}
	}
	conformance := PlatformConformance{
		FixtureSet:    PlatformFixtureSet,
		FormatVersion: PlatformArtifactDocumentFormatVersion,
		Inputs:        evidence,
		Results:       results,
	}
	conformanceRaw, err := CanonicalPlatformDocument(conformance)
	if err != nil {
		t.Fatal(err)
	}
	runtimeDescriptor := RuntimeArtifactDescriptor{
		AdapterVersion:          NodeRuntimeAdapterVersion,
		Architecture:            ArchitectureX8664,
		ConformanceDigest:       digestDocument(conformanceRaw),
		DescriptorSchemaVersion: PlatformDescriptorSchemaV0,
		Entrypoint:              "/opt/helmr/runtime/helmr/entry.mjs",
		IntegrityDigest:         digestDocument(integrityRaw),
		Kind:                    "runtime",
		MediaType:               RuntimeArtifactMediaType,
		NodeModuleABI:           "137",
		NodeVersion:             "24.16.0",
		ProgramNodeFlags:        []string{NodeNoStripTypes, "--enable-source-maps"},
		RuntimeAPIVersion:       RuntimeAPIVersion,
		RuntimeHarnessDigest:    testDigest("harness"),
		Source:                  source,
	}
	runtimeDescriptorRaw, err := CanonicalPlatformDocument(runtimeDescriptor)
	if err != nil {
		t.Fatal(err)
	}
	artifact := newMemoryArtifact()
	artifact.addDirectory("bin")
	artifact.addDirectory("helmr")
	artifact.addDirectory("helmr/upstream")
	artifact.addDirectory("lib")
	artifact.addDirectory("share")
	artifact.addDirectory("share/licenses")
	artifact.addDirectory("share/licenses/node")
	artifact.addFile(runtimeNodePath, []byte("node"), 0755)
	artifact.addFile(runtimeEntryPath, []byte("entry"), 0644)
	artifact.addFile(PlatformDescriptorPath, runtimeDescriptorRaw, 0644)
	artifact.addFile(PlatformIntegrityPath, integrityRaw, 0644)
	artifact.addFile(PlatformConformancePath, conformanceRaw, 0644)
	artifact.addFile("helmr/upstream/source", sourceRaw, 0644)
	artifact.addFile(runtimeLibcPath, []byte("libc"), 0644)
	artifact.addFile(runtimeLicensePath, []byte("license"), 0644)
	artifact.addFile("lib/locale-archive", []byte("locale"), 0644)
	return testRuntimeDescriptor(), artifact
}

func runtimeArtifactObject(descriptor RuntimeDescriptor) ArtifactDescriptor {
	return ArtifactDescriptor{
		Digest:    descriptor.Digest,
		MediaType: descriptor.MediaType,
		SizeBytes: descriptor.SizeBytes,
	}
}
