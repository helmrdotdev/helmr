package artifacttest

import (
	"strings"
	"testing"

	"github.com/helmrdotdev/helmr/internal/artifact"
	"github.com/helmrdotdev/helmr/internal/definition"
)

// ModulePath identifies either generated Agent entry module.
func ModulePath(digit string) string {
	if digit == "a" {
		return "helmr/app/entry-0.mjs"
	}
	return "helmr/app/entry-1.mjs"
}

func DefinitionIndex() artifact.DefinitionIndex {
	return artifact.DefinitionIndex{APIVersion: "helmr.definition-index.v1", Agents: []artifact.AgentBundleEntry{
		{ID: "build", TemplateDefinitionID: "repo", ModulePath: ModulePath("a"), ExportName: "build"},
		{ID: "chat", TemplateDefinitionID: "repo", ModulePath: ModulePath("b"), ExportName: "chat"},
	}, Templates: []artifact.TemplateBundleEntry{{ID: "repo", ModulePath: ModulePath("a"), ExportName: "build", ThroughAgent: true}}}
}

func ProgramMetadata(t *testing.T) artifact.ProgramMetadata {
	t.Helper()
	plan := BuildPlan()
	index, err := artifact.BuildProgramMetadata(
		plan,
		DefinitionIndex(),
		map[string]definition.ComputerSeed{
			"repo": {
				Profile:      definition.ComputerSeedProfile,
				Digest:       "sha256:" + strings.Repeat("d", 64),
				SizeBytes:    4096,
				MediaType:    definition.ComputerSeedMediaType,
				Architecture: definition.ArchitectureX8664,
			},
		},
		"sha256:"+strings.Repeat("4", 64),
		"sha256:"+strings.Repeat("f", 64),
	)
	if err != nil {
		t.Fatal(err)
	}
	return index
}

// PreparationOutputs supplies independent metadata fixtures, not verified bytes.
func PreparationOutputs(t *testing.T, session artifact.ProgramOutput) []artifact.ProgramOutput {
	t.Helper()
	outputs := make([]artifact.ProgramOutput, 0)
	for _, d := range session.Metadata.Definitions {
		if d.Kind != definition.KindTemplate || !d.Template.Prepare {
			continue
		}
		metadata := session.Metadata.Clone()
		for _, selected := range metadata.Definitions {
			if selected.Kind == definition.KindTemplate && selected.DeclaredID == d.DeclaredID {
				metadata.Definitions = []artifact.ProgramDefinition{selected}
				break
			}
		}
		raw, err := artifact.CanonicalProgramMetadata(metadata)
		if err != nil {
			t.Fatal(err)
		}
		outputs = append(outputs, artifact.ProgramOutput{Artifact: artifact.ProgramDescriptor{Digest: Digest("preparation:" + string(raw)), SizeBytes: 4096, MediaType: artifact.ProgramArtifactMediaType}, Metadata: metadata})
	}
	return outputs
}
