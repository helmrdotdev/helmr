//go:build linux

package builder

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/helmrdotdev/helmr/internal/artifact"
	"github.com/helmrdotdev/helmr/internal/artifact/verify"
	"github.com/helmrdotdev/helmr/internal/bundle"
	"github.com/helmrdotdev/helmr/internal/definition"
)

// This fixture is produced by PrepareDeploymentPrograms using the real compiler.
// Each variant contains two callback Templates in one module, a seed-only Template,
// and an Agent in another module. Only the named source changes between variants.
func TestIndependentPreparationPrograms(t *testing.T) {
	root := os.Getenv("HELMR_PREPARATION_PROGRAM_FIXTURE")
	if root == "" {
		t.Skip("HELMR_PREPARATION_PROGRAM_FIXTURE is not set")
	}
	read := func(path string) []byte {
		t.Helper()
		raw, err := os.ReadFile(path)
		if err != nil {
			t.Fatal(err)
		}
		return raw
	}
	compiler, err := artifact.ParseCompilerInputs(read("/nix/helmr/compiler.descriptor.json"))
	if err != nil {
		t.Fatal(err)
	}
	runtime, err := artifact.ParseRuntimeDescriptor(read("/opt/helmr/release/runtime.descriptor.json"))
	if err != nil {
		t.Fatal(err)
	}
	metadata, err := artifact.ParseRuntimeMetadata(read("/opt/helmr/runtime/helmr/runtime.json"))
	if err != nil {
		t.Fatal(err)
	}
	var sessions []string
	outputs := map[string][]ProgramResult{}
	for _, variant := range []string{"baseline", "agent-edit", "dependency-edit", "external-edit", "nested", "nested-agent-edit"} {
		work := t.TempDir()
		ids := []string{"first", "second", "seed"}
		if strings.HasPrefix(variant, "nested") {
			ids = []string{"first"}
		}
		seeds := make([]bundle.ComputerSeed, 0, len(ids))
		for _, id := range ids {
			seeds = append(seeds, bundle.ComputerSeed{DeclaredID: id, Artifact: bundle.ComputerSeedArtifact{Profile: definition.ComputerSeedProfile, Architecture: definition.ArchitectureX8664, Digest: "sha256:" + strings.Repeat("d", 64), SizeBytes: 4096, MediaType: definition.ComputerSeedMediaType}})
		}
		input := PreparedProgramInput{PreparedDirectory: filepath.Join(root, variant, "prepared"), WorkDirectory: work, ProgramObjectPath: filepath.Join(work, "session.squashfs"), SquashFSEncoder: "/opt/helmr/bin/mksquashfs", Compiler: compiler, Runtime: runtime, RuntimeMetadata: metadata, ComputerSeeds: seeds}
		session, err := BuildPreparedProgram(t.Context(), input)
		if err != nil {
			t.Fatal(err)
		}
		sessions = append(sessions, session.Program.Artifact.Digest)
		prepared, err := BuildPreparationPrograms(t.Context(), input)
		if err != nil {
			t.Fatal(err)
		}
		want := 2
		if strings.HasPrefix(variant, "nested") {
			want = 1
		}
		if len(prepared) != want {
			t.Fatalf("%s: got %d callback artifacts", variant, len(prepared))
		}
		for i, result := range prepared {
			defs := result.Program.Metadata.Definitions
			if len(defs) != 1 || defs[0].DeclaredID != []string{"first", "second"}[i] || !defs[0].Template.Prepare {
				t.Fatalf("unexpected preparation definitions: %+v", defs)
			}
			file, err := os.Open(result.ObjectPath)
			if err != nil {
				t.Fatal(err)
			}
			err = verify.ProgramOutputFile(t.Context(), file, result.Program)
			file.Close()
			if err != nil {
				t.Fatal(err)
			}
		}
		outputs[variant] = prepared
	}
	if sessions[0] == sessions[1] {
		t.Fatal("Agent edit failed to change Session artifact")
	}
	for i := range outputs["baseline"] {
		a := outputs["baseline"][i].Program.Artifact.Digest
		if a != outputs["agent-edit"][i].Program.Artifact.Digest {
			t.Fatalf("Agent edit invalidated independent Template %d", i)
		}
	}
	if outputs["baseline"][0].Program.Artifact.Digest == outputs["dependency-edit"][0].Program.Artifact.Digest {
		t.Fatal("readable dependency change reused preparation")
	}
	for i := range outputs["baseline"] {
		if outputs["baseline"][i].Program.Artifact.Digest == outputs["external-edit"][i].Program.Artifact.Digest {
			t.Fatalf("readable external bytes did not invalidate Template %d", i)
		}
	}
	if outputs["nested"][0].Program.Artifact.Digest == outputs["nested-agent-edit"][0].Program.Artifact.Digest {
		t.Fatal("enclosing module source-map change did not invalidate nested Template")
	}

}
