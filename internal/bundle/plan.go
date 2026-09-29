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
	FormatVersion int                                `json:"formatVersion"`
	Definitions   []artifact.ProgramIndexDeclaration `json:"definitions"`
	Queues        []definition.QueueInput            `json:"queues"`
}

// PlanFromProgramIndex derives the final scheduler projection from
// the verified Program index. Producer build instructions and provenance are
// intentionally absent from both sides of this boundary.
func PlanFromProgramIndex(index artifact.ProgramIndex) (Plan, error) {
	if err := artifact.ValidateProgramIndex(index); err != nil {
		return Plan{}, fmt.Errorf("deployment plan program index: %w", err)
	}
	cloned := index.Clone()
	plan := Plan{
		FormatVersion: definition.DeploymentPlanFormatVersion,
		Definitions:   cloned.Declarations,
		Queues:        cloned.Queues,
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
	if plan.Queues == nil {
		return errors.New("deployment plan queues must be an array")
	}
	if len(plan.Queues) > definition.MaxBuildQueues {
		return fmt.Errorf("deployment plan contains more than %d queues", definition.MaxBuildQueues)
	}

	queues := make(map[string]struct{}, len(plan.Queues))
	for position, queue := range plan.Queues {
		if err := definition.ValidateQueueInput(queue); err != nil {
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
		if err := artifact.ValidateProgramIndexDeclaration(definition, queues); err != nil {
			return fmt.Errorf("deployment plan definition %d: %w", position, err)
		}
		if position > 0 && artifact.CompareProgramIndexDeclarations(
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

func validateProgramIndexDeployment(index artifact.ProgramIndex, plan Plan) error {
	if err := validatePlan(plan); err != nil {
		return err
	}
	if len(index.Queues) != len(plan.Queues) || len(index.Declarations) != len(plan.Definitions) {
		return errors.New("program index does not match deployment plan")
	}
	for position := range index.Queues {
		left, err := definition.CanonicalQueueConfig(definition.QueueConfig{
			FormatVersion: definition.DeploymentPlanFormatVersion,
			Queues:        []definition.QueueInput{index.Queues[position]},
		})
		if err != nil {
			return err
		}
		right, err := definition.CanonicalQueueConfig(definition.QueueConfig{
			FormatVersion: definition.DeploymentPlanFormatVersion,
			Queues:        []definition.QueueInput{plan.Queues[position]},
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

func deploymentPlanSandboxes(plan Plan) []artifact.ProgramIndexDeclaration {
	sandboxes := make([]artifact.ProgramIndexDeclaration, 0)
	for _, declaration := range plan.Definitions {
		if declaration.Kind == definition.KindSandbox {
			sandboxes = append(sandboxes, declaration.Clone())
		}
	}
	return sandboxes
}
