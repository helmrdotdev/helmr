package verify

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/helmrdotdev/helmr/internal/artifact"
	"github.com/helmrdotdev/helmr/internal/artifact/artifacttest"
	"github.com/helmrdotdev/helmr/internal/definition"
)

func TestRuntimeTopologyAcceptsClosedLayout(t *testing.T) {
	descriptor, memory := newRuntimeTopology(t)
	inspected, err := artifact.Inspect(
		context.Background(),
		memory,
		artifact.RoleRuntime,
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
	if _, err := Runtime(nil, "/sys/fs/cgroup", "lease", nil); err == nil {
		t.Fatal("nil context was accepted")
	}
	if _, err := Runtime(
		context.Background(),
		"/sys/fs/cgroup",
		"lease",
		nil,
	); err == nil {
		t.Fatal("nil snapshot was accepted")
	}
}

func TestRuntimeTopologyRejectsOpenOrDivergentLayout(t *testing.T) {
	removePath := func(filePath string) func(*artifacttest.Memory) {
		return func(artifact *artifacttest.Memory) {
			artifact.Remove(filePath)
		}
	}
	tests := map[string]func(*artifacttest.Memory){
		"extra top level": func(artifact *artifacttest.Memory) {
			artifact.AddDirectory("etc")
		},
		"extra bin": func(artifact *artifacttest.Memory) {
			artifact.AddFile("bin/other", []byte("other"), 0755)
		},
		"extra helmr": func(artifact *artifacttest.Memory) {
			artifact.AddFile("helmr/other", []byte("other"), 0644)
		},
		"extra share": func(artifact *artifacttest.Memory) {
			artifact.AddFile("share/other", []byte("other"), 0644)
		},
		"missing entry":    removePath(runtimeEntryPath),
		"missing metadata": removePath(runtimeMetadataPath),
		"missing license":  removePath(runtimeLicensePath),
		"node mode": func(memory *artifacttest.Memory) {
			memory.Mutate(runtimeNodePath, func(entry *artifact.Entry) {
				entry.Mode = 0644
			})
		},
		"entry mode": func(memory *artifacttest.Memory) {
			memory.Mutate(runtimeEntryPath, func(entry *artifact.Entry) {
				entry.Mode = 0755
			})
		},
		"metadata mode": func(memory *artifacttest.Memory) {
			memory.Mutate(runtimeMetadataPath, func(entry *artifact.Entry) {
				entry.Mode = 0755
			})
		},
		"metadata Node flags": func(memory *artifacttest.Memory) {
			invalid := artifact.RuntimeMetadata{
				ModulePolicyDigest: artifacttest.Digest("preload"),
				Architecture:       definition.ArchitectureX8664, FormatVersion: artifact.RuntimeMetadataFormatVersion,
				NodeVersion:      "24.21.0",
				ProgramNodeFlags: []string{"--no-experimental-strip-types", "--enable-source-maps"},
				RuntimeContract:  definition.RuntimeContract,
			}
			raw, err := json.Marshal(invalid)
			if err != nil {
				t.Fatal(err)
			}
			memory.Files[runtimeMetadataPath] = raw
			memory.Mutate(runtimeMetadataPath, func(entry *artifact.Entry) {
				entry.SizeBytes = int64(len(raw))
			})
		},
		"libc mode": func(memory *artifacttest.Memory) {
			memory.Mutate(runtimeLibcPath, func(entry *artifact.Entry) {
				entry.Mode = 0755
			})
		},
		"license mode": func(memory *artifacttest.Memory) {
			memory.Mutate(runtimeLicensePath, func(entry *artifact.Entry) {
				entry.Mode = 0755
			})
		},
	}
	for name, mutate := range tests {
		t.Run(name, func(t *testing.T) {
			descriptor, memory := newRuntimeTopology(t)
			mutate(memory)
			inspected, err := artifact.Inspect(
				context.Background(),
				memory,
				artifact.RoleRuntime,
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
	logical, err := artifactLogicalLimit(artifact.RoleRuntime)
	if err != nil {
		t.Fatal(err)
	}
	physical, err := artifactPhysicalLimit(artifact.RoleRuntime)
	if err != nil {
		t.Fatal(err)
	}
	if logical != artifact.MaxRuntimeLogicalBytes || physical != artifact.MaxRuntimePhysicalBytes {
		t.Fatalf(
			"runtime bounds = (%d,%d), want (%d,%d)",
			logical,
			physical,
			artifact.MaxRuntimeLogicalBytes,
			artifact.MaxRuntimePhysicalBytes,
		)
	}
}

func TestVerifiedRuntimeResultMatchesDescriptor(t *testing.T) {
	descriptor := artifacttest.RuntimeDescriptor()
	index, err := verifiedRuntimeResult(canonicalVerifierRuntimeIndex(t), descriptor)
	if err != nil {
		t.Fatal(err)
	}
	if index.Architecture != descriptor.Architecture {
		t.Fatalf("architecture = %q", index.Architecture)
	}

	for name, mutate := range map[string]func(*artifact.RuntimeDescriptor){
		"architecture": func(value *artifact.RuntimeDescriptor) {
			value.Architecture = definition.RuntimeArchitecture("aarch64")
		},
		"runtime API": func(value *artifact.RuntimeDescriptor) {
			value.RuntimeContract = "helmr.runtime.unsupported"
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

func newRuntimeTopology(t *testing.T) (artifact.RuntimeDescriptor, *artifacttest.Memory) {
	t.Helper()
	metadata := artifact.RuntimeMetadata{
		ModulePolicyDigest: artifacttest.Digest("preload"),
		Architecture:       definition.ArchitectureX8664,
		FormatVersion:      artifact.RuntimeMetadataFormatVersion,
		NodeVersion:        "24.21.0",
		ProgramNodeFlags:   artifacttest.NodeProgramFlags(),
		RuntimeContract:    definition.RuntimeContract,
	}
	metadataRaw, err := artifact.CanonicalRuntimeMetadata(metadata)
	if err != nil {
		t.Fatal(err)
	}
	memory := artifacttest.NewMemory()
	memory.AddDirectory("bin")
	memory.AddDirectory("helmr")
	memory.AddDirectory("lib")
	memory.AddDirectory("share")
	memory.AddDirectory("share/licenses")
	memory.AddDirectory("share/licenses/node")
	memory.AddDirectory("share/licenses/debian")
	for _, name := range []string{"libc6", "libgcc-s1", "libstdc++6"} {
		memory.AddFile("share/licenses/debian/"+name, []byte("copyright"), 0644)
	}
	memory.AddFile("helmr/module-preload.mjs", []byte("preload"), 0644)
	memory.AddFile(runtimeNodePath, []byte("node"), 0755)
	memory.AddFile(runtimeEntryPath, []byte("entry"), 0644)
	memory.AddFile(runtimeMetadataPath, metadataRaw, 0644)
	memory.AddFile(runtimeLibcPath, []byte("libc"), 0644)
	memory.AddFile(runtimeLicensePath, []byte("license"), 0644)
	memory.AddFile("lib/locale-archive", []byte("locale"), 0644)
	return artifacttest.RuntimeDescriptor(), memory
}
