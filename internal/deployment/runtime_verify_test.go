package deployment

import (
	"bytes"
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
	index, err := verifyRuntimeTopology(context.Background(), inspected)
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
		"missing entry": func(artifact *memoryArtifact) {
			for index := range artifact.entries {
				if artifact.entries[index].Path != runtimeEntryPath {
					continue
				}
				artifact.entries = append(
					artifact.entries[:index],
					artifact.entries[index+1:]...,
				)
				delete(artifact.files, runtimeEntryPath)
				break
			}
		},
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
				_, err = verifyRuntimeTopology(context.Background(), inspected)
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

func TestDigestRuntimeArtifactRejectsTrailingBytes(t *testing.T) {
	raw := []byte("runtime")
	if _, err := verifyRuntimeArtifactReader(
		context.Background(),
		newBytesReaderAt(raw),
		int64(len(raw)-1),
	); err == nil {
		t.Fatal("VerifyRuntimeArtifact accepted bytes beyond descriptor size")
	}
}

func TestDigestRuntimeArtifactRejectsTruncatedBytes(t *testing.T) {
	raw := []byte("runtime")
	if _, err := verifyRuntimeArtifactReader(
		context.Background(),
		newBytesReaderAt(raw),
		int64(len(raw)+1),
	); err == nil {
		t.Fatal("VerifyRuntimeArtifact accepted bytes shorter than descriptor size")
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
			value.Architecture = ArchitectureAArch64
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
	index, err := CanonicalRuntimeIndex(RuntimeIndex{
		Architecture:      ArchitectureX8664,
		FormatVersion:     RuntimeIndexFormatVersion,
		RuntimeAPIVersion: RuntimeAPIVersion,
	})
	if err != nil {
		t.Fatal(err)
	}
	artifact := newMemoryArtifact()
	artifact.addDirectory("bin")
	artifact.addDirectory("helmr")
	artifact.addDirectory("lib")
	artifact.addFile(runtimeNodePath, []byte("node"), 0755)
	artifact.addFile(runtimeEntryPath, []byte("entry"), 0644)
	artifact.addFile(runtimeIndexPath, index, 0644)
	artifact.addFile(runtimePreloadPath, []byte("preload"), 0644)
	artifact.addFile(runtimeLibcPath, []byte("libc"), 0644)
	artifact.addFile("lib/locale-archive", []byte("locale"), 0644)
	return testRuntimeDescriptor(), artifact
}

type bytesReaderAt struct {
	raw []byte
}

func newBytesReaderAt(raw []byte) *bytesReaderAt {
	return &bytesReaderAt{raw: append([]byte(nil), raw...)}
}

func (reader *bytesReaderAt) ReadAt(destination []byte, offset int64) (int, error) {
	return bytes.NewReader(reader.raw).ReadAt(destination, offset)
}
