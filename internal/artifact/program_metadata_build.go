package artifact

import (
	"errors"
	"fmt"

	"github.com/helmrdotdev/helmr/internal/cas"
	"github.com/helmrdotdev/helmr/internal/definition"
)

func BuildProgramMetadata(plan definition.BuildPlan, runtimeIndex DefinitionIndex,
	computerSeeds map[string]definition.ComputerSeed, configResultDigest, runtimeDigest string,
) (ProgramMetadata, error) {
	if err := definition.ValidateBuildPlan(plan); err != nil {
		return ProgramMetadata{}, err
	}
	declarations := make([]ProgramDefinition, 0, len(plan.Definitions))
	for _, input := range plan.Definitions {
		d := ProgramDefinition{Kind: input.Kind, DeclaredID: input.DeclaredID}
		switch input.Kind {
		case definition.KindAgent:
			d.Agent = input.Agent
		case definition.KindComputer:
			image, exists := computerSeeds[input.DeclaredID]
			if !exists {
				return ProgramMetadata{}, fmt.Errorf("computer %q has no seed result", input.DeclaredID)
			}
			if image.Architecture != definition.ArchitectureX8664 || image.SizeBytes > definition.MaxComputerSeedBytes {
				return ProgramMetadata{}, errors.New("unsupported Computer seed architecture or size")
			}
			if err := cas.ValidateDescriptor(cas.Descriptor{Digest: image.Digest, SizeBytes: image.SizeBytes, MediaType: image.MediaType}); err != nil {
				return ProgramMetadata{}, err
			}
			d.Computer = &definition.ComputerManifest{
				Seed:      definition.ComputerSeedManifest{Profile: image.Profile, Config: image.Config, ArtifactDigest: image.Digest, MediaType: image.MediaType},
				Resources: input.Computer.Resources, Prepare: input.Computer.Prepare,
				Refresh: input.Computer.Refresh, Secrets: input.Computer.Secrets, BuildSecrets: input.Computer.BuildSecrets,
			}
		default:
			return ProgramMetadata{}, fmt.Errorf("unsupported definition kind %q", input.Kind)
		}
		declarations = append(declarations, cloneProgramDefinition(d))
	}
	index := ProgramMetadata{Architecture: definition.ArchitectureX8664, ConfigResultDigest: configResultDigest,
		Definitions: declarations, RuntimeContract: definition.RuntimeContract, RuntimeDigest: runtimeDigest}
	if err := ValidateProgramMetadata(index); err != nil {
		return ProgramMetadata{}, err
	}
	if err := ValidateProgramDefinitionIndex(index, runtimeIndex); err != nil {
		return ProgramMetadata{}, err
	}
	return index, nil
}

// ValidateProgramDefinitionIndex binds executable locations to the exact metadata set.
func ValidateProgramDefinitionIndex(index ProgramMetadata, runtimeIndex DefinitionIndex) error {
	if err := ValidateDefinitionIndex(runtimeIndex); err != nil {
		return err
	}
	if len(index.Definitions) != len(runtimeIndex.Agents)+len(runtimeIndex.Computers) {
		return errors.New("definition index does not match program definitions")
	}
	agents := make(map[string]string, len(runtimeIndex.Agents))
	computers := make(map[string]struct{}, len(runtimeIndex.Computers))
	for _, a := range runtimeIndex.Agents {
		agents[a.ID] = a.ComputerDefinitionID
	}
	for _, c := range runtimeIndex.Computers {
		computers[c.ID] = struct{}{}
	}
	for _, d := range index.Definitions {
		switch d.Kind {
		case definition.KindAgent:
			if d.Agent == nil || agents[d.DeclaredID] != d.Agent.ComputerDefinitionID {
				return errors.New("agent bundle entry does not match program definition")
			}
		case definition.KindComputer:
			if _, exists := computers[d.DeclaredID]; !exists {
				return errors.New("computer bundle entry does not match program definition")
			}
		default:
			return errors.New("invalid program definition kind")
		}
	}
	return nil
}

// ValidateBuildPlanDefinitionIndex checks the compiler's metadata before seeds exist.
func ValidateBuildPlanDefinitionIndex(plan definition.BuildPlan, runtimeIndex DefinitionIndex) error {
	if err := definition.ValidateBuildPlan(plan); err != nil {
		return err
	}
	declarations := make([]ProgramDefinition, 0, len(plan.Definitions))
	for _, d := range plan.Definitions {
		declarations = append(declarations, ProgramDefinition{Kind: d.Kind, DeclaredID: d.DeclaredID, Agent: d.Agent})
	}
	return ValidateProgramDefinitionIndex(ProgramMetadata{Definitions: declarations}, runtimeIndex)
}
