package artifact_test

import (
	"bytes"
	"encoding/json"
	"reflect"
	"testing"

	"github.com/helmrdotdev/helmr/internal/artifact"
	"github.com/helmrdotdev/helmr/internal/artifact/artifacttest"
)

func TestPreparationSpecBindsMountedProgramAndSelectedComputer(t *testing.T) {
	output := artifact.ProgramOutput{Artifact: artifact.ProgramDescriptor{Digest: artifacttest.Digest("program"), SizeBytes: 4096, MediaType: artifact.ProgramArtifactMediaType}, Metadata: artifacttest.ProgramMetadata(t)}
	for i := range output.Metadata.Definitions {
		if output.Metadata.Definitions[i].Template != nil {
			output.Metadata.Definitions[i].Template.Prepare = true
		}
	}
	specs, err := artifact.BuildComputerPreparationSpecs(output)
	if err != nil || len(specs) != 1 || specs[0].TemplateDefinitionID != "repo" {
		t.Fatalf("specs %v: %v", specs, err)
	}
	spec := specs[0]
	digest, err := artifact.ComputerPreparationSpecDigest(spec)
	if err != nil {
		t.Fatal(err)
	}
	for name, change := range map[string]func(*artifact.ComputerPreparationSpec){
		"Program bytes": func(s *artifact.ComputerPreparationSpec) {
			s.Program.Digest = artifacttest.Digest("changed metadata or payload")
		},
		"Program length":     func(s *artifact.ComputerPreparationSpec) { s.Program.SizeBytes++ },
		"Computer selection": func(s *artifact.ComputerPreparationSpec) { s.TemplateDefinitionID = "other" },
	} {
		t.Run(name, func(t *testing.T) {
			changed := spec
			change(&changed)
			actual, err := artifact.ComputerPreparationSpecDigest(changed)
			if err != nil || actual == digest {
				t.Fatalf("identity %q: %v", actual, err)
			}
		})
	}
	raw, err := artifact.CanonicalComputerPreparationSpec(spec)
	if err != nil {
		t.Fatal(err)
	}
	var indented bytes.Buffer
	if err := json.Indent(&indented, raw, "", "  "); err != nil {
		t.Fatal(err)
	}
	parsed, err := artifact.ParseComputerPreparationSpec(indented.Bytes())
	if err != nil || !reflect.DeepEqual(parsed, spec) {
		t.Fatalf("round trip %v: %v", parsed, err)
	}
	got, err := artifact.ComputerPreparationSpecDigest(parsed)
	if err != nil || got != digest {
		t.Fatalf("round trip identity %q: %v", got, err)
	}
	for _, invalid := range [][]byte{
		bytes.Replace(raw, []byte(`"templateDefinitionId":"repo"`), []byte(`"templateDefinitionId":"repo","templateDefinitionId":"other"`), 1),
		append([]byte(`{"extra":true,`), raw[1:]...),
		bytes.Replace(raw, []byte(`,"templateDefinitionId":"repo"`), nil, 1),
	} {
		if _, err := artifact.ParseComputerPreparationSpec(invalid); err == nil {
			t.Fatalf("accepted %s", invalid)
		}
	}
}

func TestSeedOnlyPreparationSpecHasNoProgram(t *testing.T) {
	output := artifact.ProgramOutput{Artifact: artifact.ProgramDescriptor{Digest: artifacttest.Digest("session"), SizeBytes: 4096, MediaType: artifact.ProgramArtifactMediaType}, Metadata: artifacttest.ProgramMetadata(t)}
	specs, err := artifact.BuildComputerPreparationSpecs(output)
	if err != nil || len(specs) != 1 || specs[0].Program != (artifact.ProgramDescriptor{}) || specs[0].Template == nil {
		t.Fatalf("seed-only spec %+v: %v", specs, err)
	}
	raw, err := artifact.CanonicalComputerPreparationSpec(specs[0])
	if err != nil {
		t.Fatal(err)
	}
	if bytes.Contains(raw, []byte(`"program"`)) {
		t.Fatal("seed-only spec retained Program")
	}
	parsed, err := artifact.ParseComputerPreparationSpec(raw)
	if err != nil {
		t.Fatal(err)
	}
	again, err := artifact.CanonicalComputerPreparationSpec(parsed)
	if err != nil || !bytes.Equal(raw, again) {
		t.Fatalf("roundtrip %s: %v", again, err)
	}
	output.Artifact.Digest = artifacttest.Digest("Agent-only edit")
	changed, err := artifact.BuildComputerPreparationSpecs(output)
	if err != nil {
		t.Fatal(err)
	}
	after, err := artifact.CanonicalComputerPreparationSpec(changed[0])
	if err != nil || !bytes.Equal(raw, after) {
		t.Fatal("seed-only identity changed with Session bytes")
	}
}

func TestSeedOnlyPreparationRuntimeInvalidatesIdentity(t *testing.T) {
	output := artifact.ProgramOutput{Artifact: artifact.ProgramDescriptor{Digest: artifacttest.Digest("session"), SizeBytes: 4096, MediaType: artifact.ProgramArtifactMediaType}, Metadata: artifacttest.ProgramMetadata(t)}
	specs, err := artifact.BuildComputerPreparationSpecs(output)
	if err != nil {
		t.Fatal(err)
	}
	before, err := artifact.ComputerPreparationSpecDigest(specs[0])
	if err != nil {
		t.Fatal(err)
	}
	output.Metadata.RuntimeDigest = artifacttest.Digest("new-runtime")
	changed, err := artifact.BuildComputerPreparationSpecs(output)
	if err != nil {
		t.Fatal(err)
	}
	after, err := artifact.ComputerPreparationSpecDigest(changed[0])
	if err != nil || before == after {
		t.Fatalf("Runtime did not invalidate seed identity: %v", err)
	}
	changed[0].Runtime = nil
	if _, err := artifact.CanonicalComputerPreparationSpec(changed[0]); err == nil {
		t.Fatal("missing qualifying Runtime accepted")
	}
}
