package artifact

import (
	"fmt"
	"sort"

	"github.com/helmrdotdev/helmr/internal/definition"
)

func BuildProgramIndex(
	plan definition.BuildPlan,
	locator DeclarationLocator,
	computerImages map[string]definition.ComputerImage,
	configResultDigest string,
	runtimeDigest string,
) (ProgramIndex, error) {
	if err := definition.ValidateBuildPlan(plan); err != nil {
		return ProgramIndex{}, err
	}
	if err := ValidateDeclarationLocator(locator); err != nil {
		return ProgramIndex{}, err
	}
	locators := make(map[string]LocatedDeclaration, len(locator.Declarations))
	for _, located := range locator.Declarations {
		locators[string(located.Kind)+"\x00"+located.DeclaredID] = located
	}
	declarations := make([]ProgramIndexDeclaration, 0, len(plan.Definitions))
	for _, input := range plan.Definitions {
		declaration := ProgramIndexDeclaration{
			Kind:       input.Kind,
			DeclaredID: input.DeclaredID,
		}
		switch input.Kind {
		case definition.KindTask:
			located, exists := locators[string(DeclarationKindTask)+"\x00"+input.DeclaredID]
			if !exists {
				return ProgramIndex{}, fmt.Errorf(
					"task %q has no generated locator",
					input.DeclaredID,
				)
			}
			declaration.Task = input.Task
			declaration.Locator = &ProgramLocator{
				ExportName: located.ExportName,
				ModulePath: located.ModulePath,
				Slot:       located.Slot,
			}
		case definition.KindActor:
			located, exists := locators[string(DeclarationKindActor)+"\x00"+input.DeclaredID]
			if !exists {
				return ProgramIndex{}, fmt.Errorf(
					"actor %q has no generated locator",
					input.DeclaredID,
				)
			}
			declaration.Actor = input.Actor
			declaration.Locator = &ProgramLocator{
				ExportName: located.ExportName,
				ModulePath: located.ModulePath,
				Slot:       located.Slot,
			}
		case definition.KindSandbox:
			image, exists := computerImages[input.DeclaredID]
			if !exists || input.Sandbox == nil {
				return ProgramIndex{}, fmt.Errorf(
					"sandbox %q has no image result",
					input.DeclaredID,
				)
			}
			declaration.Sandbox = &definition.SandboxManifest{
				Image: definition.SandboxImageManifest{
					Profile: image.Profile, Config: image.Config,
					ArtifactDigest: image.Digest,
					MediaType:      image.MediaType,
				},
				Resources: input.Sandbox.Resources,
			}
			if _, err := definition.CompileComputerSpec(*declaration.Sandbox, image); err != nil {
				return ProgramIndex{}, fmt.Errorf("sandbox %q: %w", input.DeclaredID, err)
			}
		default:
			return ProgramIndex{}, fmt.Errorf(
				"definition kind %q is unsupported",
				input.Kind,
			)
		}
		declarations = append(declarations, cloneProgramIndexDeclaration(declaration))
	}
	sort.Slice(declarations, func(left, right int) bool {
		return CompareProgramIndexDeclarations(declarations[left], declarations[right]) < 0
	})
	index := ProgramIndex{
		Architecture:       definition.ArchitectureX8664,
		ConfigResultDigest: configResultDigest,
		Declarations:       declarations,
		Queues:             cloneQueueInputs(plan.Queues),
		RuntimeContract:    definition.RuntimeContract,
		RuntimeDigest:      runtimeDigest,
	}
	if err := ValidateProgramIndex(index); err != nil {
		return ProgramIndex{}, err
	}
	return cloneProgramIndex(index), nil
}
