package deployment

import (
	"context"
	"encoding/json"
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
		"missing entry":    removePath(runtimeEntryPath),
		"missing metadata": removePath(runtimeMetadataPath),
		"missing license":  removePath(runtimeLicensePath),
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
		"metadata mode": func(artifact *memoryArtifact) {
			artifact.mutate(runtimeMetadataPath, func(entry *artifactEntry) {
				entry.Mode = 0755
			})
		},
		"metadata Node flags": func(artifact *memoryArtifact) {
			invalid := RuntimeMetadata{
				Architecture: ArchitectureX8664, FormatVersion: RuntimeMetadataFormatVersion,
				NodeVersion:      "24.20.0",
				ProgramNodeFlags: []string{NodeNoExperimentalStripTypes, "--enable-source-maps"},
				RuntimeContract:  RuntimeContract,
			}
			raw, err := json.Marshal(invalid)
			if err != nil {
				t.Fatal(err)
			}
			artifact.files[runtimeMetadataPath] = raw
			artifact.mutate(runtimeMetadataPath, func(entry *artifactEntry) {
				entry.SizeBytes = int64(len(raw))
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
			value.RuntimeContract = "helmr.runtime.v1"
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
	metadata := RuntimeMetadata{
		Architecture:     ArchitectureX8664,
		FormatVersion:    RuntimeMetadataFormatVersion,
		NodeVersion:      "24.20.0",
		ProgramNodeFlags: []string{NodeNoStripTypes, "--enable-source-maps"},
		RuntimeContract:  RuntimeContract,
	}
	metadataRaw, err := CanonicalRuntimeMetadata(metadata)
	if err != nil {
		t.Fatal(err)
	}
	artifact := newMemoryArtifact()
	artifact.addDirectory("bin")
	artifact.addDirectory("helmr")
	artifact.addDirectory("lib")
	artifact.addDirectory("share")
	artifact.addDirectory("share/licenses")
	artifact.addDirectory("share/licenses/node")
	artifact.addFile(runtimeNodePath, []byte("node"), 0755)
	artifact.addFile(runtimeEntryPath, []byte("entry"), 0644)
	artifact.addFile(runtimeMetadataPath, metadataRaw, 0644)
	artifact.addFile(runtimeLibcPath, []byte("libc"), 0644)
	artifact.addFile(runtimeLicensePath, []byte("license"), 0644)
	artifact.addFile("lib/locale-archive", []byte("locale"), 0644)
	return testRuntimeDescriptor(), artifact
}
