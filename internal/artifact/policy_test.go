package artifact_test

import (
	"encoding/json"
	"github.com/helmrdotdev/helmr/internal/artifact"
	"github.com/helmrdotdev/helmr/internal/artifact/artifacttest"
	"github.com/helmrdotdev/helmr/internal/networkpolicy"
	"testing"
)

func TestPreparationIdentityBindsResolvedPolicyInBothBranches(t *testing.T) {
	for _, callback := range []bool{false, true} {
		t.Run(map[bool]string{false: "seed", true: "callback"}[callback], func(t *testing.T) {
			output := artifact.ProgramOutput{Artifact: artifact.ProgramDescriptor{Digest: artifacttest.Digest("unchanged-program"), SizeBytes: 4096, MediaType: artifact.ProgramArtifactMediaType}, Metadata: artifacttest.ProgramMetadata(t)}
			m := output.Metadata.Definitions[2].Template
			m.Prepare = callback
			specs, err := artifact.BuildComputerPreparationSpecs(output)
			if err != nil {
				t.Fatal(err)
			}
			original, err := artifact.ComputerPreparationSpecDigest(specs[0])
			if err != nil {
				t.Fatal(err)
			}
			m.Network = json.RawMessage(`{"allowDomains":["api.example.com"]}`)
			changed, err := artifact.BuildComputerPreparationSpecs(output)
			if err != nil {
				t.Fatal(err)
			}
			digest, err := artifact.ComputerPreparationSpecDigest(changed[0])
			if err != nil || digest == original {
				t.Fatal("network did not invalidate preparation", err)
			}
			want := networkpolicy.Blocked
			if callback {
				want = networkpolicy.Allowlist
			}
			if changed[0].PrepareNetworkPolicy.Kind != want {
				t.Fatal("wrong resolved phase policy")
			}
			if !callback {
				m.Network[0] = '!'
				if _, err := artifact.CanonicalComputerPreparationSpec(changed[0]); err != nil {
					t.Fatal("preparation retained mutable Template reference", err)
				}
			}
		})
	}
}
func TestProgramCloneCopiesPolicyMetadata(t *testing.T) {
	metadata := artifacttest.ProgramMetadata(t)
	metadata.Definitions[0].Agent.RuntimeMcp = json.RawMessage(`{"allow":[]}`)
	metadata.Definitions[2].Template.PrepareNetwork = json.RawMessage(`{"blockAll":true}`)
	cloned := metadata.Clone()
	metadata.Definitions[0].Agent.RuntimeMcp[0] = '!'
	metadata.Definitions[2].Template.Network[0] = '!'
	metadata.Definitions[2].Template.PrepareNetwork[0] = '!'
	if cloned.Definitions[0].Agent.RuntimeMcp[0] != '{' || cloned.Definitions[2].Template.Network[0] != '{' || cloned.Definitions[2].Template.PrepareNetwork[0] != '{' {
		t.Fatal("policy metadata aliases source")
	}
}
