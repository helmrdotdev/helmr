package deployment

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"sort"

	"github.com/helmrdotdev/helmr/internal/sourceid"
)

func (declaration ProgramIndexDeclaration) MarshalJSON() ([]byte, error) {
	if declaration.manifestCount() != 1 {
		return nil, errors.New("program index declaration must contain exactly one manifest")
	}
	switch declaration.Kind {
	case DefinitionKindTask:
		if declaration.Task == nil || declaration.Locator == nil {
			return nil, errors.New("task program index declaration requires manifest and locator")
		}
		return json.Marshal(struct {
			DeclaredID string          `json:"declaredId"`
			Kind       DefinitionKind  `json:"kind"`
			Locator    *ProgramLocator `json:"locator"`
			Manifest   *TaskManifest   `json:"manifest"`
		}{declaration.DeclaredID, declaration.Kind, declaration.Locator, declaration.Task})
	case DefinitionKindActor:
		if declaration.Actor == nil || declaration.Locator == nil {
			return nil, errors.New("actor program index declaration requires manifest and locator")
		}
		return json.Marshal(struct {
			DeclaredID string          `json:"declaredId"`
			Kind       DefinitionKind  `json:"kind"`
			Locator    *ProgramLocator `json:"locator"`
			Manifest   *ActorManifest  `json:"manifest"`
		}{declaration.DeclaredID, declaration.Kind, declaration.Locator, declaration.Actor})
	case DefinitionKindSandbox:
		if declaration.Sandbox == nil || declaration.Locator != nil {
			return nil, errors.New("sandbox program index declaration requires manifest and forbids locator")
		}
		return json.Marshal(struct {
			DeclaredID string           `json:"declaredId"`
			Kind       DefinitionKind   `json:"kind"`
			Manifest   *SandboxManifest `json:"manifest"`
		}{declaration.DeclaredID, declaration.Kind, declaration.Sandbox})
	default:
		return nil, fmt.Errorf("program index declaration kind %q is unsupported", declaration.Kind)
	}
}

func (declaration *ProgramIndexDeclaration) UnmarshalJSON(raw []byte) error {
	var header struct {
		Kind DefinitionKind `json:"kind"`
	}
	if err := json.Unmarshal(raw, &header); err != nil {
		return err
	}
	*declaration = ProgramIndexDeclaration{Kind: header.Kind}
	switch header.Kind {
	case DefinitionKindTask:
		var wire struct {
			DeclaredID string          `json:"declaredId"`
			Kind       DefinitionKind  `json:"kind"`
			Locator    *ProgramLocator `json:"locator"`
			Manifest   *TaskManifest   `json:"manifest"`
		}
		if err := decodeClosedDefinition(raw, &wire); err != nil {
			return err
		}
		declaration.DeclaredID = wire.DeclaredID
		declaration.Locator = wire.Locator
		declaration.Task = wire.Manifest
	case DefinitionKindActor:
		var wire struct {
			DeclaredID string          `json:"declaredId"`
			Kind       DefinitionKind  `json:"kind"`
			Locator    *ProgramLocator `json:"locator"`
			Manifest   *ActorManifest  `json:"manifest"`
		}
		if err := decodeClosedDefinition(raw, &wire); err != nil {
			return err
		}
		declaration.DeclaredID = wire.DeclaredID
		declaration.Locator = wire.Locator
		declaration.Actor = wire.Manifest
	case DefinitionKindSandbox:
		var wire struct {
			DeclaredID string           `json:"declaredId"`
			Kind       DefinitionKind   `json:"kind"`
			Manifest   *SandboxManifest `json:"manifest"`
		}
		if err := decodeClosedDefinition(raw, &wire); err != nil {
			return err
		}
		declaration.DeclaredID = wire.DeclaredID
		declaration.Sandbox = wire.Manifest
	default:
		return fmt.Errorf("program index declaration kind %q is unsupported", header.Kind)
	}
	return nil
}

func (declaration ProgramIndexDeclaration) manifestCount() int {
	count := 0
	for _, present := range []bool{
		declaration.Task != nil,
		declaration.Actor != nil,
		declaration.Sandbox != nil,
	} {
		if present {
			count++
		}
	}
	return count
}

func validateProgramIndexDeclaration(
	declaration ProgramIndexDeclaration,
	queues map[string]struct{},
) error {
	if !sourceid.Valid(declaration.DeclaredID) {
		return fmt.Errorf(
			"declaredId %q is outside the exact ASCII ID domain",
			declaration.DeclaredID,
		)
	}
	if declaration.manifestCount() != 1 {
		return errors.New("must contain exactly one manifest")
	}
	switch declaration.Kind {
	case DefinitionKindTask:
		if declaration.Task == nil || declaration.Locator == nil {
			return errors.New("task requires manifest and locator")
		}
		if declaration.Task.Payload.Kind != SchemaKindNone &&
			declaration.Task.Payload.Kind != SchemaKindStandard {
			return fmt.Errorf("task payload kind %q is unsupported", declaration.Task.Payload.Kind)
		}
		if err := validateRunManifest(declaration.Task.Run, queues); err != nil {
			return fmt.Errorf("task run: %w", err)
		}
		if declaration.Task.Schedule != nil {
			if declaration.Task.Payload.Kind != SchemaKindStandard {
				return errors.New("scheduled task payload kind must be standard_schema")
			}
			if err := validateScheduleManifest(*declaration.Task.Schedule); err != nil {
				return fmt.Errorf("task schedule: %w", err)
			}
		}
		return validateProgramLocator(*declaration.Locator)
	case DefinitionKindActor:
		if declaration.Actor == nil || declaration.Locator == nil {
			return errors.New("actor requires manifest and locator")
		}
		if err := validateRunManifest(declaration.Actor.Run, queues); err != nil {
			return fmt.Errorf("actor run: %w", err)
		}
		if declaration.Actor.IdleTimeoutMs < 1 ||
			declaration.Actor.IdleTimeoutMs > maxActorIdleMs {
			return fmt.Errorf("actor idleTimeoutMs must be in [1,%d]", maxActorIdleMs)
		}
		return validateProgramLocator(*declaration.Locator)
	case DefinitionKindSandbox:
		if declaration.Sandbox == nil || declaration.Locator != nil {
			return errors.New("sandbox requires manifest and forbids locator")
		}
		if !sha256DigestPattern.MatchString(
			declaration.Sandbox.Image.ArtifactDigest,
		) {
			return errors.New("workspace image artifactDigest is not a lowercase SHA-256 digest")
		}
		if declaration.Sandbox.Image.MediaType != WorkspaceImageArtifactMediaType {
			return fmt.Errorf(
				"sandbox image mediaType = %q, want %q",
				declaration.Sandbox.Image.MediaType,
				WorkspaceImageArtifactMediaType,
			)
		}
		if err := validateResourcesManifest(declaration.Sandbox.Resources); err != nil {
			return fmt.Errorf("sandbox resources: %w", err)
		}
		return nil
	default:
		return fmt.Errorf("kind %q is unsupported", declaration.Kind)
	}
}

func validateProgramLocator(locator ProgramLocator) error {
	return validateLocatedDeclaration(LocatedDeclaration{
		DeclaredID: "locator",
		ExportName: locator.ExportName,
		Kind:       DeclarationKindTask,
		ModulePath: locator.ModulePath,
		Slot:       locator.Slot,
	})
}

func compareProgramIndexDeclarations(
	left ProgramIndexDeclaration,
	right ProgramIndexDeclaration,
) int {
	if compared := bytes.Compare([]byte(left.Kind), []byte(right.Kind)); compared != 0 {
		return compared
	}
	if compared := bytes.Compare([]byte(left.DeclaredID), []byte(right.DeclaredID)); compared != 0 {
		return compared
	}
	leftModule, leftExport := "", ""
	if left.Locator != nil {
		leftModule, leftExport = left.Locator.ModulePath, left.Locator.ExportName
	}
	rightModule, rightExport := "", ""
	if right.Locator != nil {
		rightModule, rightExport = right.Locator.ModulePath, right.Locator.ExportName
	}
	if compared := bytes.Compare([]byte(leftModule), []byte(rightModule)); compared != 0 {
		return compared
	}
	return bytes.Compare([]byte(leftExport), []byte(rightExport))
}

func cloneProgramIndexDeclaration(
	declaration ProgramIndexDeclaration,
) ProgramIndexDeclaration {
	if declaration.Task != nil {
		value := *declaration.Task
		declaration.Task = &value
	}
	if declaration.Actor != nil {
		value := *declaration.Actor
		declaration.Actor = &value
	}
	if declaration.Sandbox != nil {
		value := *declaration.Sandbox
		declaration.Sandbox = &value
	}
	if declaration.Locator != nil {
		value := *declaration.Locator
		declaration.Locator = &value
	}
	return declaration
}

func buildProgramIndex(
	plan BuildPlan,
	locator DeclarationLocator,
	images []WorkspaceImage,
	configResultDigest string,
) (ProgramIndex, error) {
	if err := ValidateBuildPlan(plan); err != nil {
		return ProgramIndex{}, err
	}
	if err := ValidateDeclarationLocator(locator); err != nil {
		return ProgramIndex{}, err
	}
	locators := make(map[string]LocatedDeclaration, len(locator.Declarations))
	for _, located := range locator.Declarations {
		locators[string(located.Kind)+"\x00"+located.DeclaredID] = located
	}
	workspaceImages := make(map[string]WorkspaceImageArtifact, len(images))
	for _, image := range images {
		workspaceImages[image.DeclaredID] = image.Artifact
	}
	declarations := make([]ProgramIndexDeclaration, 0, len(plan.Definitions))
	for _, definition := range plan.Definitions {
		declaration := ProgramIndexDeclaration{
			Kind:       definition.Kind,
			DeclaredID: definition.DeclaredID,
		}
		switch definition.Kind {
		case DefinitionKindTask:
			located, exists := locators[string(DeclarationKindTask)+"\x00"+definition.DeclaredID]
			if !exists {
				return ProgramIndex{}, fmt.Errorf(
					"task %q has no generated locator",
					definition.DeclaredID,
				)
			}
			declaration.Task = definition.Task
			declaration.Locator = &ProgramLocator{
				ExportName: located.ExportName,
				ModulePath: located.ModulePath,
				Slot:       located.Slot,
			}
		case DefinitionKindActor:
			located, exists := locators[string(DeclarationKindActor)+"\x00"+definition.DeclaredID]
			if !exists {
				return ProgramIndex{}, fmt.Errorf(
					"actor %q has no generated locator",
					definition.DeclaredID,
				)
			}
			declaration.Actor = definition.Actor
			declaration.Locator = &ProgramLocator{
				ExportName: located.ExportName,
				ModulePath: located.ModulePath,
				Slot:       located.Slot,
			}
		case DefinitionKindSandbox:
			image, exists := workspaceImages[definition.DeclaredID]
			if !exists || definition.Sandbox == nil {
				return ProgramIndex{}, fmt.Errorf(
					"sandbox %q has no image result",
					definition.DeclaredID,
				)
			}
			declaration.Sandbox = &SandboxManifest{
				Image: SandboxImageManifest{
					ArtifactDigest: image.Digest,
					MediaType:      image.MediaType,
				},
				Resources: definition.Sandbox.Resources,
			}
		default:
			return ProgramIndex{}, fmt.Errorf(
				"definition kind %q is unsupported",
				definition.Kind,
			)
		}
		declarations = append(declarations, cloneProgramIndexDeclaration(declaration))
	}
	sort.Slice(declarations, func(left, right int) bool {
		return compareProgramIndexDeclarations(declarations[left], declarations[right]) < 0
	})
	index := ProgramIndex{
		Architecture:       ArchitectureX8664,
		ConfigResultDigest: configResultDigest,
		Declarations:       declarations,
		FormatVersion:      ProgramIndexFormatVersion,
		Queues:             cloneQueueInputs(plan.Queues),
		RuntimeAPIVersion:  RuntimeAPIVersion,
	}
	if err := ValidateProgramIndex(index); err != nil {
		return ProgramIndex{}, err
	}
	return cloneProgramIndex(index), nil
}

func validateProgramIndexBuild(
	index ProgramIndex,
	plan BuildPlan,
	images []WorkspaceImage,
	configResultDigest string,
) error {
	locator := DeclarationLocator{
		FormatVersion: DeclarationLocatorFormatVersion,
		Declarations:  make([]LocatedDeclaration, 0),
	}
	for _, declaration := range index.Declarations {
		if declaration.Locator == nil {
			continue
		}
		locator.Declarations = append(locator.Declarations, LocatedDeclaration{
			DeclaredID: declaration.DeclaredID,
			ExportName: declaration.Locator.ExportName,
			Kind:       DeclarationKind(declaration.Kind),
			ModulePath: declaration.Locator.ModulePath,
			Slot:       declaration.Locator.Slot,
		})
	}
	sort.Slice(locator.Declarations, func(left, right int) bool {
		return compareDeclarations(
			locatedDeclarationProjection(locator.Declarations[left]),
			locatedDeclarationProjection(locator.Declarations[right]),
		) < 0
	})
	expected, err := buildProgramIndex(plan, locator, images, configResultDigest)
	if err != nil {
		return err
	}
	actualRaw, err := CanonicalProgramIndex(index)
	if err != nil {
		return err
	}
	expectedRaw, err := CanonicalProgramIndex(expected)
	if err != nil {
		return err
	}
	if !bytes.Equal(actualRaw, expectedRaw) {
		return errors.New("program index does not match build plan and workspace images")
	}
	return nil
}

func programIndexExecutionDeclarations(index ProgramIndex) []ProgramDeclaration {
	declarations := make([]ProgramDeclaration, 0)
	for _, declaration := range index.Declarations {
		switch declaration.Kind {
		case DefinitionKindTask:
			slots := []DeclarationSlot{DeclarationSlotHandler}
			if declaration.Task.Payload.Kind == SchemaKindStandard {
				slots = append(slots, DeclarationSlotPayloadSchema)
			}
			declarations = append(declarations, ProgramDeclaration{
				Kind:       DeclarationKindTask,
				DeclaredID: declaration.DeclaredID,
				Slots:      slots,
			})
		case DefinitionKindActor:
			declarations = append(declarations, ProgramDeclaration{
				Kind:       DeclarationKindActor,
				DeclaredID: declaration.DeclaredID,
				Slots:      []DeclarationSlot{DeclarationSlotHandler},
			})
		}
	}
	sort.Slice(declarations, func(left, right int) bool {
		return compareDeclarations(declarations[left], declarations[right]) < 0
	})
	return declarations
}

func cloneQueueInputs(source []QueueInput) []QueueInput {
	cloned := append([]QueueInput(nil), source...)
	for index := range cloned {
		if cloned[index].ConcurrencyLimit != nil {
			value := *cloned[index].ConcurrencyLimit
			cloned[index].ConcurrencyLimit = &value
		}
	}
	return cloned
}
