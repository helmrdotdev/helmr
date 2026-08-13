package deployment

import (
	"bytes"
	"errors"
	"fmt"
)

const DeploymentPlanFormatVersion = 0

// DeploymentPlan is the final scheduler and execution projection committed by
// a deployment bundle. It contains no producer build instructions.
type DeploymentPlan struct {
	FormatVersion int                       `json:"formatVersion"`
	Definitions   []ProgramIndexDeclaration `json:"definitions"`
	Queues        []QueueInput              `json:"queues"`
}

func ValidateDeploymentPlan(plan DeploymentPlan) error {
	if plan.FormatVersion != DeploymentPlanFormatVersion {
		return fmt.Errorf(
			"deployment plan formatVersion = %d, want %d",
			plan.FormatVersion,
			DeploymentPlanFormatVersion,
		)
	}
	if len(plan.Definitions) == 0 {
		return errors.New("deployment plan definitions must be a non-empty array")
	}
	if len(plan.Definitions) > maxBuildDefinitions {
		return fmt.Errorf("deployment plan contains more than %d definitions", maxBuildDefinitions)
	}
	if plan.Queues == nil {
		return errors.New("deployment plan queues must be an array")
	}
	if len(plan.Queues) > maxBuildQueues {
		return fmt.Errorf("deployment plan contains more than %d queues", maxBuildQueues)
	}

	queues := make(map[string]struct{}, len(plan.Queues))
	for position, queue := range plan.Queues {
		if err := validateQueueInput(queue); err != nil {
			return fmt.Errorf("deployment plan queue %d: %w", position, err)
		}
		if position > 0 && bytes.Compare(
			[]byte(plan.Queues[position-1].Name),
			[]byte(queue.Name),
		) >= 0 {
			return fmt.Errorf(
				"deployment plan queues are not in canonical order at position %d",
				position,
			)
		}
		queues[queue.Name] = struct{}{}
	}

	for position, definition := range plan.Definitions {
		if err := validateProgramIndexDeclaration(definition, queues); err != nil {
			return fmt.Errorf("deployment plan definition %d: %w", position, err)
		}
		if position > 0 && compareProgramIndexDeclarations(
			plan.Definitions[position-1],
			definition,
		) >= 0 {
			return fmt.Errorf(
				"deployment plan definitions are not in canonical order at position %d",
				position,
			)
		}
	}
	return nil
}

func validateProgramIndexDeployment(index ProgramIndex, plan DeploymentPlan) error {
	if err := ValidateDeploymentPlan(plan); err != nil {
		return err
	}
	if len(index.Queues) != len(plan.Queues) || len(index.Declarations) != len(plan.Definitions) {
		return errors.New("program index does not match deployment plan")
	}
	for position := range index.Queues {
		left, err := CanonicalQueueConfig(QueueConfig{
			FormatVersion: DeploymentPlanFormatVersion,
			Queues:        []QueueInput{index.Queues[position]},
		})
		if err != nil {
			return err
		}
		right, err := CanonicalQueueConfig(QueueConfig{
			FormatVersion: DeploymentPlanFormatVersion,
			Queues:        []QueueInput{plan.Queues[position]},
		})
		if err != nil {
			return err
		}
		if !bytes.Equal(left, right) {
			return errors.New("program index does not match deployment plan")
		}
	}
	for position := range index.Declarations {
		left, err := index.Declarations[position].MarshalJSON()
		if err != nil {
			return err
		}
		right, err := plan.Definitions[position].MarshalJSON()
		if err != nil {
			return err
		}
		if !bytes.Equal(left, right) {
			return errors.New("program index does not match deployment plan")
		}
	}
	return nil
}

func deploymentPlanSandboxes(plan DeploymentPlan) []ProgramIndexDeclaration {
	sandboxes := make([]ProgramIndexDeclaration, 0)
	for _, definition := range plan.Definitions {
		if definition.Kind == DefinitionKindSandbox {
			sandboxes = append(sandboxes, cloneProgramIndexDeclaration(definition))
		}
	}
	return sandboxes
}
