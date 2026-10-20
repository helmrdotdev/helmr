package bundle

import (
	"errors"
	"fmt"
	"reflect"

	"github.com/helmrdotdev/helmr/internal/artifact"
	"github.com/helmrdotdev/helmr/internal/definition"
)

// Preparation returns the independently built Program for a selected Template.
func (m Manifest) Preparation(template string) (artifact.ProgramOutput, bool) {
	for _, output := range m.Preparations {
		if len(output.Metadata.Definitions) == 1 && output.Metadata.Definitions[0].DeclaredID == template {
			return output, true
		}
	}
	return artifact.ProgramOutput{}, false
}

// ProgramObject resolves an artifact whose metadata was checked with the bundle.
func (m Manifest) ProgramObject(digest string) (artifact.ProgramOutput, bool) {
	if m.Program.Artifact.Digest == digest {
		return m.Program, true
	}
	for _, output := range m.Preparations {
		if output.Artifact.Digest == digest {
			return output, true
		}
	}
	return artifact.ProgramOutput{}, false
}

func validatePreparations(m Manifest) error {
	templates := make([]artifact.ProgramDefinition, 0)
	for _, d := range m.Program.Metadata.Definitions {
		if d.Kind == definition.KindTemplate && d.Template.Prepare {
			templates = append(templates, d)
		}
	}
	if m.Preparations == nil || len(m.Preparations) != len(templates) {
		return errors.New("deployment preparations do not match Templates")
	}
	programs := map[string]artifact.ProgramOutput{m.Program.Artifact.Digest: m.Program}
	for i, output := range m.Preparations {
		if err := artifact.ValidateProgramOutput(output); err != nil {
			return fmt.Errorf("preparation Program: %w", err)
		}
		metadata := output.Metadata
		if len(metadata.Definitions) != 1 || !reflect.DeepEqual(metadata.Definitions[0], templates[i]) {
			return errors.New("preparation Template differs from Session Program")
		}
		if metadata.RuntimeDigest != m.Program.Metadata.RuntimeDigest || metadata.RuntimeContract != m.Program.Metadata.RuntimeContract || metadata.Architecture != m.Program.Metadata.Architecture {
			return errors.New("preparation Runtime differs from Session Program")
		}
		if previous, ok := programs[output.Artifact.Digest]; ok && !reflect.DeepEqual(previous, output) {
			return errors.New("program artifact has conflicting metadata")
		}
		programs[output.Artifact.Digest] = output
	}
	return nil
}
