//go:build linux

package builder

import (
	"bytes"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/helmrdotdev/helmr/internal/artifact"
	"github.com/helmrdotdev/helmr/internal/artifact/verify"
	"github.com/helmrdotdev/helmr/internal/bundle"
)

// The opt-in fixture is the actual preparation graph's exported result, with no
// Computer Image declarations. All compiler, Runtime and encoder inputs are the
// release artifacts installed in the canonical builder environment.
func TestPreparedProgramFinalization(t *testing.T) {
	prepared := os.Getenv("HELMR_PREPARED_PROGRAM_FIXTURE")
	if prepared == "" {
		t.Skip("HELMR_PREPARED_PROGRAM_FIXTURE is not set")
	}
	read := func(name string) []byte {
		raw, err := os.ReadFile(name)
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
	before, err := programPayloadDigest(t.Context(), filepath.Join(prepared, "payload"))
	if err != nil {
		t.Fatal(err)
	}
	var previousDescriptor, previousObject []byte
	for i := 0; i < 2; i++ {
		work := t.TempDir()
		result, err := BuildPreparedProgram(t.Context(), PreparedProgramInput{
			PreparedDirectory: prepared, WorkDirectory: work, ProgramObjectPath: filepath.Join(work, "program.squashfs"),
			SquashFSEncoder: "/opt/helmr/bin/mksquashfs", Compiler: compiler, Runtime: runtime, RuntimeMetadata: metadata,
			ComputerImages: []bundle.ComputerImage{},
		})
		if err != nil {
			t.Fatal(err)
		}
		file, err := os.Open(result.ObjectPath)
		if err != nil {
			t.Fatal(err)
		}
		if err := errors.Join(verify.ProgramOutputFile(t.Context(), file, result.Program), file.Close()); err != nil {
			t.Fatal(err)
		}
		descriptor, err := json.Marshal(result.Program)
		if err != nil {
			t.Fatal(err)
		}
		object := read(result.ObjectPath)
		if i != 0 && (!bytes.Equal(previousDescriptor, descriptor) || !bytes.Equal(previousObject, object)) {
			t.Fatal("same prepared payload produced different Program bytes")
		}
		previousDescriptor, previousObject = descriptor, object
	}
	after, err := programPayloadDigest(t.Context(), filepath.Join(prepared, "payload"))
	if err != nil {
		t.Fatal(err)
	}
	if after != before {
		t.Fatal("finalization modified the prepared payload")
	}
}
