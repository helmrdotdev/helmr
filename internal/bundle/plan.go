package bundle

import (
	"bytes"
	"errors"
	"fmt"

	"github.com/helmrdotdev/helmr/internal/artifact"
	"github.com/helmrdotdev/helmr/internal/definition"
)

// Plan is the final scheduler and execution projection committed by
// a deployment bundle. It contains no producer build instructions.
type Plan struct {
	FormatVersion int                          `json:"formatVersion"`
	Definitions   []artifact.ProgramDefinition `json:"definitions"`
}

// PlanFromProgramMetadata derives the final scheduler projection from
// the verified Program metadata. Producer build instructions and provenance are
// intentionally absent from both sides of this boundary.
func PlanFromProgramMetadata(index artifact.ProgramMetadata) (Plan, error) {
	if err := artifact.ValidateProgramMetadata(index); err != nil {
		return Plan{}, fmt.Errorf("deployment plan program metadata: %w", err)
	}
	cloned := index.Clone()
	plan := Plan{
		FormatVersion: definition.DeploymentPlanFormatVersion,
		Definitions:   cloned.Definitions,
	}
	if err := validatePlan(plan); err != nil {
		return Plan{}, err
	}
	return plan, nil
}

func validatePlan(plan Plan) error {
	if plan.FormatVersion != definition.DeploymentPlanFormatVersion {
		return fmt.Errorf(
			"deployment plan formatVersion = %d, want %d",
			plan.FormatVersion,
			definition.DeploymentPlanFormatVersion,
		)
	}
	if len(plan.Definitions) == 0 {
		return errors.New("deployment plan definitions must be a non-empty array")
	}
	if len(plan.Definitions) > definition.MaxBuildDefinitions {
		return fmt.Errorf("deployment plan contains more than %d definitions", definition.MaxBuildDefinitions)
	}
	for position, definition := range plan.Definitions {
		if err := artifact.ValidateProgramDefinition(definition); err != nil {
			return fmt.Errorf("deployment plan definition %d: %w", position, err)
		}
		if position > 0 && artifact.CompareProgramDefinitions(
			plan.Definitions[position-1],
			definition,
		) >= 0 {
			return fmt.Errorf(
				"deployment plan definitions are not in canonical order at position %d",
				position,
			)
		}
	}
	computers := make(map[string]struct{})
	for _, d := range plan.Definitions {
		if d.Computer != nil {
			computers[d.DeclaredID] = struct{}{}
		}
	}
	for _, d := range plan.Definitions {
		if d.Agent != nil {
			if _, exists := computers[d.Agent.ComputerDefinitionID]; !exists {
				return errors.New("deployment Agent references an absent Computer")
			}
		}
	}

	return nil
}

func validateProgramMetadataDeployment(index artifact.ProgramMetadata, plan Plan) error {
	if err := validatePlan(plan); err != nil {
		return err
	}
	if len(index.Definitions) != len(plan.Definitions) {
		return errors.New("program metadata does not match deployment plan")
	}
	for position := range index.Definitions {
		left, err := index.Definitions[position].MarshalJSON()
		if err != nil {
			return err
		}
		right, err := plan.Definitions[position].MarshalJSON()
		if err != nil {
			return err
		}
		if !bytes.Equal(left, right) {
			return errors.New("program metadata does not match deployment plan")
		}
	}
	return nil
}

func deploymentPlanComputers(plan Plan) []artifact.ProgramDefinition {
	computers := make([]artifact.ProgramDefinition, 0)
	for _, declaration := range plan.Definitions {
		if declaration.Kind == definition.KindComputer {
			computers = append(computers, declaration.Clone())
		}
	}
	return computers
}
