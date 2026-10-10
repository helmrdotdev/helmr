package artifact_test

import (
	"bytes"
	"encoding/json"
	"testing"

	"github.com/helmrdotdev/helmr/internal/artifact"
	"github.com/helmrdotdev/helmr/internal/artifact/artifacttest"
)

func TestPreparationSpecBindsMountedProgramAndSelectedComputer(t *testing.T) {
	output := artifact.ProgramOutput{Artifact: artifact.ProgramDescriptor{Digest: artifacttest.Digest("program"), SizeBytes: 4096, MediaType: artifact.ProgramArtifactMediaType}, Metadata: artifacttest.ProgramMetadata(t)}
	specs, err := artifact.BuildComputerPreparationSpecs(output)
	if err != nil || len(specs) != 1 || specs[0].ComputerDefinitionID != "repo" {
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
		"Computer selection": func(s *artifact.ComputerPreparationSpec) { s.ComputerDefinitionID = "other" },
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
	if err != nil || parsed != spec {
		t.Fatalf("round trip %v: %v", parsed, err)
	}
	got, err := artifact.ComputerPreparationSpecDigest(parsed)
	if err != nil || got != digest {
		t.Fatalf("round trip identity %q: %v", got, err)
	}
	for _, invalid := range [][]byte{
		bytes.Replace(raw, []byte(`"computerDefinitionId":"repo"`), []byte(`"computerDefinitionId":"repo","computerDefinitionId":"other"`), 1),
		append([]byte(`{"extra":true,`), raw[1:]...),
		bytes.Replace(raw, []byte(`"computerDefinitionId":"repo",`), nil, 1),
	} {
		if _, err := artifact.ParseComputerPreparationSpec(invalid); err == nil {
			t.Fatalf("accepted %s", invalid)
		}
	}
}
