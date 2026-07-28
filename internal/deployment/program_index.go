package deployment

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
)

func (declaration ProgramIndexDeclaration) MarshalJSON() ([]byte, error) {
	if declaration.manifestCount() != 1 {
		return nil, errors.New("Program index declaration must contain exactly one manifest")
	}
	switch declaration.Kind {
	case DefinitionKindTask:
		if declaration.Task == nil || declaration.Locator == nil {
			return nil, errors.New("Task Program index declaration requires manifest and locator")
		}
		return json.Marshal(struct {
			DeclaredID string          `json:"declaredId"`
			Kind       DefinitionKind  `json:"kind"`
			Locator    *ProgramLocator `json:"locator"`
			Manifest   *TaskManifest   `json:"manifest"`
		}{declaration.DeclaredID, declaration.Kind, declaration.Locator, declaration.Task})
	case DefinitionKindActor:
		if declaration.Actor == nil || declaration.Locator == nil {
			return nil, errors.New("Actor Program index declaration requires manifest and locator")
		}
		return json.Marshal(struct {
			DeclaredID string          `json:"declaredId"`
			Kind       DefinitionKind  `json:"kind"`
			Locator    *ProgramLocator `json:"locator"`
			Manifest   *ActorManifest  `json:"manifest"`
		}{declaration.DeclaredID, declaration.Kind, declaration.Locator, declaration.Actor})
	case DefinitionKindWorkspace:
		if declaration.Workspace == nil || declaration.Locator != nil {
			return nil, errors.New("Workspace Program index declaration requires manifest and forbids locator")
		}
		return json.Marshal(struct {
			DeclaredID string             `json:"declaredId"`
			Kind       DefinitionKind     `json:"kind"`
			Manifest   *WorkspaceManifest `json:"manifest"`
		}{declaration.DeclaredID, declaration.Kind, declaration.Workspace})
	default:
		return nil, fmt.Errorf("Program index declaration kind %q is unsupported", declaration.Kind)
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
	case DefinitionKindWorkspace:
		var wire struct {
			DeclaredID string             `json:"declaredId"`
			Kind       DefinitionKind     `json:"kind"`
			Manifest   *WorkspaceManifest `json:"manifest"`
		}
		if err := decodeClosedDefinition(raw, &wire); err != nil {
			return err
		}
		declaration.DeclaredID = wire.DeclaredID
		declaration.Workspace = wire.Manifest
	default:
		return fmt.Errorf("Program index declaration kind %q is unsupported", header.Kind)
	}
	return nil
}

func (declaration ProgramIndexDeclaration) manifestCount() int {
	count := 0
	for _, present := range []bool{
		declaration.Task != nil,
		declaration.Actor != nil,
		declaration.Workspace != nil,
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
	if !declaredIDPattern.MatchString(declaration.DeclaredID) {
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
			return errors.New("Task requires manifest and locator")
		}
		if declaration.Task.Payload.Kind != SchemaKindNone &&
			declaration.Task.Payload.Kind != SchemaKindStandard {
			return fmt.Errorf("Task payload kind %q is unsupported", declaration.Task.Payload.Kind)
		}
		if err := validateRunManifest(declaration.Task.Run, queues); err != nil {
			return fmt.Errorf("Task run: %w", err)
		}
		if declaration.Task.Schedule != nil {
			if declaration.Task.Payload.Kind != SchemaKindStandard {
				return errors.New("scheduled Task payload kind must be standard_schema")
			}
			if err := validateScheduleManifest(*declaration.Task.Schedule); err != nil {
				return fmt.Errorf("Task schedule: %w", err)
			}
		}
		return validateProgramLocator(*declaration.Locator)
	case DefinitionKindActor:
		if declaration.Actor == nil || declaration.Locator == nil {
			return errors.New("Actor requires manifest and locator")
		}
		if err := validateRunManifest(declaration.Actor.Run, queues); err != nil {
			return fmt.Errorf("Actor run: %w", err)
		}
		if declaration.Actor.IdleTimeoutMs < 1 ||
			declaration.Actor.IdleTimeoutMs > maxActorIdleMs {
			return fmt.Errorf("Actor idleTimeoutMs must be in [1,%d]", maxActorIdleMs)
		}
		return validateProgramLocator(*declaration.Locator)
	case DefinitionKindWorkspace:
		if declaration.Workspace == nil || declaration.Locator != nil {
			return errors.New("Workspace requires manifest and forbids locator")
		}
		if !sha256DigestPattern.MatchString(
			declaration.Workspace.Image.ArtifactDigest,
		) {
			return errors.New("Workspace image artifactDigest is not a lowercase SHA-256 digest")
		}
		if declaration.Workspace.Image.MediaType != WorkspaceImageArtifactMediaType {
			return fmt.Errorf(
				"Workspace image mediaType = %q, want %q",
				declaration.Workspace.Image.MediaType,
				WorkspaceImageArtifactMediaType,
			)
		}
		if err := validateResourcesManifest(declaration.Workspace.Resources); err != nil {
			return fmt.Errorf("Workspace resources: %w", err)
		}
		if err := validateNetworkManifest(declaration.Workspace.Network); err != nil {
			return fmt.Errorf("Workspace network: %w", err)
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
	if declaration.Workspace != nil {
		value := *declaration.Workspace
		value.Network.DenyCIDRs = append([]string(nil), value.Network.DenyCIDRs...)
		declaration.Workspace = &value
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
					"Task %q has no generated locator",
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
					"Actor %q has no generated locator",
					definition.DeclaredID,
				)
			}
			declaration.Actor = definition.Actor
			declaration.Locator = &ProgramLocator{
				ExportName: located.ExportName,
				ModulePath: located.ModulePath,
				Slot:       located.Slot,
			}
		case DefinitionKindWorkspace:
			image, exists := workspaceImages[definition.DeclaredID]
			if !exists || definition.Workspace == nil {
				return ProgramIndex{}, fmt.Errorf(
					"Workspace %q has no image result",
					definition.DeclaredID,
				)
			}
			declaration.Workspace = &WorkspaceManifest{
				Image: WorkspaceArtifactManifest{
					ArtifactDigest: image.Digest,
					MediaType:      image.MediaType,
				},
				Resources: definition.Workspace.Resources,
				Network:   definition.Workspace.Network,
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
		return errors.New("Program index does not match build plan and Workspace images")
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
