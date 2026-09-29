package artifact

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"sort"

	"github.com/helmrdotdev/helmr/internal/definition"
	"github.com/helmrdotdev/helmr/internal/secretbinding"
)

func (declaration ProgramIndexDeclaration) MarshalJSON() ([]byte, error) {
	if declaration.manifestCount() != 1 {
		return nil, errors.New("program index declaration must contain exactly one manifest")
	}
	switch declaration.Kind {
	case definition.KindTask:
		if declaration.Task == nil || declaration.Locator == nil {
			return nil, errors.New("task program index declaration requires manifest and locator")
		}
		return json.Marshal(struct {
			DeclaredID string                   `json:"declaredId"`
			Kind       definition.Kind          `json:"kind"`
			Locator    *ProgramLocator          `json:"locator"`
			Manifest   *definition.TaskManifest `json:"manifest"`
		}{declaration.DeclaredID, declaration.Kind, declaration.Locator, declaration.Task})
	case definition.KindActor:
		if declaration.Actor == nil || declaration.Locator == nil {
			return nil, errors.New("actor program index declaration requires manifest and locator")
		}
		return json.Marshal(struct {
			DeclaredID string                    `json:"declaredId"`
			Kind       definition.Kind           `json:"kind"`
			Locator    *ProgramLocator           `json:"locator"`
			Manifest   *definition.ActorManifest `json:"manifest"`
		}{declaration.DeclaredID, declaration.Kind, declaration.Locator, declaration.Actor})
	case definition.KindSandbox:
		if declaration.Sandbox == nil || declaration.Locator != nil {
			return nil, errors.New("sandbox program index declaration requires manifest and forbids locator")
		}
		return json.Marshal(struct {
			DeclaredID string                      `json:"declaredId"`
			Kind       definition.Kind             `json:"kind"`
			Manifest   *definition.SandboxManifest `json:"manifest"`
		}{declaration.DeclaredID, declaration.Kind, declaration.Sandbox})
	default:
		return nil, fmt.Errorf("program index declaration kind %q is unsupported", declaration.Kind)
	}
}

func (declaration *ProgramIndexDeclaration) UnmarshalJSON(raw []byte) error {
	var header struct {
		Kind definition.Kind `json:"kind"`
	}
	if err := json.Unmarshal(raw, &header); err != nil {
		return err
	}
	*declaration = ProgramIndexDeclaration{Kind: header.Kind}
	switch header.Kind {
	case definition.KindTask:
		var wire struct {
			DeclaredID string                   `json:"declaredId"`
			Kind       definition.Kind          `json:"kind"`
			Locator    *ProgramLocator          `json:"locator"`
			Manifest   *definition.TaskManifest `json:"manifest"`
		}
		if err := decodeClosedDefinition(raw, &wire); err != nil {
			return err
		}
		declaration.DeclaredID = wire.DeclaredID
		declaration.Locator = wire.Locator
		declaration.Task = wire.Manifest
	case definition.KindActor:
		var wire struct {
			DeclaredID string                    `json:"declaredId"`
			Kind       definition.Kind           `json:"kind"`
			Locator    *ProgramLocator           `json:"locator"`
			Manifest   *definition.ActorManifest `json:"manifest"`
		}
		if err := decodeClosedDefinition(raw, &wire); err != nil {
			return err
		}
		declaration.DeclaredID = wire.DeclaredID
		declaration.Locator = wire.Locator
		declaration.Actor = wire.Manifest
	case definition.KindSandbox:
		var wire struct {
			DeclaredID string                      `json:"declaredId"`
			Kind       definition.Kind             `json:"kind"`
			Manifest   *definition.SandboxManifest `json:"manifest"`
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

func ValidateProgramIndexDeclaration(
	declaration ProgramIndexDeclaration,
	queues map[string]struct{},
) error {
	if !definition.ValidDeclaredID(declaration.DeclaredID) {
		return fmt.Errorf(
			"declaredId %q is outside the exact ASCII ID domain",
			declaration.DeclaredID,
		)
	}
	if declaration.manifestCount() != 1 {
		return errors.New("must contain exactly one manifest")
	}
	switch declaration.Kind {
	case definition.KindTask:
		if declaration.Task == nil || declaration.Locator == nil {
			return errors.New("task requires manifest and locator")
		}
		if declaration.Task.Payload.Kind != definition.SchemaKindNone &&
			declaration.Task.Payload.Kind != definition.SchemaKindStandard {
			return fmt.Errorf("task payload kind %q is unsupported", declaration.Task.Payload.Kind)
		}
		if err := definition.ValidateRunManifest(declaration.Task.Run, queues); err != nil {
			return fmt.Errorf("task run: %w", err)
		}
		if declaration.Task.Schedule != nil {
			if declaration.Task.Payload.Kind != definition.SchemaKindStandard {
				return errors.New("scheduled task payload kind must be standard_schema")
			}
			if err := definition.ValidateScheduleManifest(*declaration.Task.Schedule); err != nil {
				return fmt.Errorf("task schedule: %w", err)
			}
		}
		return validateProgramLocator(*declaration.Locator)
	case definition.KindActor:
		if declaration.Actor == nil || declaration.Locator == nil {
			return errors.New("actor requires manifest and locator")
		}
		if err := definition.ValidateRunManifest(declaration.Actor.Run, queues); err != nil {
			return fmt.Errorf("actor run: %w", err)
		}
		if declaration.Actor.IdleTimeoutMs < 1 ||
			declaration.Actor.IdleTimeoutMs > definition.MaxActorIdleMs {
			return fmt.Errorf("actor idleTimeoutMs must be in [1,%d]", definition.MaxActorIdleMs)
		}
		return validateProgramLocator(*declaration.Locator)
	case definition.KindSandbox:
		if declaration.Sandbox == nil || declaration.Locator != nil {
			return errors.New("sandbox requires manifest and forbids locator")
		}
		if !sha256DigestPattern.MatchString(
			declaration.Sandbox.Image.ArtifactDigest,
		) {
			return errors.New("computer image artifactDigest is not a lowercase SHA-256 digest")
		}
		if declaration.Sandbox.Image.Profile != definition.ComputerSeedProfile {
			return fmt.Errorf("sandbox disk profile %q is unsupported", declaration.Sandbox.Image.Profile)
		}
		if declaration.Sandbox.Image.MediaType != definition.ComputerSeedMediaType {
			return fmt.Errorf(
				"sandbox image mediaType = %q, want %q",
				declaration.Sandbox.Image.MediaType,
				definition.ComputerSeedMediaType,
			)
		}
		if err := definition.ValidateResourcesManifest(declaration.Sandbox.Resources); err != nil {
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

func CompareProgramIndexDeclarations(
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

// Clone returns a deep copy of the declaration.
func (declaration ProgramIndexDeclaration) Clone() ProgramIndexDeclaration {
	return cloneProgramIndexDeclaration(declaration)
}

func cloneProgramIndexDeclaration(
	declaration ProgramIndexDeclaration,
) ProgramIndexDeclaration {
	if declaration.Task != nil {
		value := *declaration.Task
		value.Run = cloneRunManifest(value.Run)
		if value.Schedule != nil {
			schedule := *value.Schedule
			schedule.Computer.Secrets = make(
				[]secretbinding.Binding,
				len(value.Schedule.Computer.Secrets),
			)
			for index, binding := range value.Schedule.Computer.Secrets {
				schedule.Computer.Secrets[index] = binding
				if binding.Env != nil {
					env := *binding.Env
					env.AllowedOrigins = append([]string(nil), env.AllowedOrigins...)
					schedule.Computer.Secrets[index].Env = &env
				}
				if binding.File != nil {
					file := *binding.File
					schedule.Computer.Secrets[index].File = &file
				}
			}
			value.Schedule = &schedule
		}
		declaration.Task = &value
	}
	if declaration.Actor != nil {
		value := *declaration.Actor
		value.Run = cloneRunManifest(value.Run)
		declaration.Actor = &value
	}
	if declaration.Sandbox != nil {
		value := *declaration.Sandbox
		value.Image.Config.Env = slices.Clone(value.Image.Config.Env)
		value.Image.Config.Entrypoint = slices.Clone(value.Image.Config.Entrypoint)
		value.Image.Config.Cmd = slices.Clone(value.Image.Config.Cmd)
		declaration.Sandbox = &value
	}
	if declaration.Locator != nil {
		value := *declaration.Locator
		declaration.Locator = &value
	}
	return declaration
}

func cloneRunManifest(run definition.RunManifest) definition.RunManifest {
	if run.TTLMs != nil {
		value := *run.TTLMs
		run.TTLMs = &value
	}
	if run.Retry.MaxAttempts != nil {
		value := *run.Retry.MaxAttempts
		run.Retry.MaxAttempts = &value
	}
	if run.Retry.Backoff != nil {
		value := *run.Retry.Backoff
		run.Retry.Backoff = &value
	}
	return run
}

func ProgramIndexExecutionDeclarations(index ProgramIndex) []ProgramDeclaration {
	declarations := make([]ProgramDeclaration, 0)
	for _, declaration := range index.Declarations {
		switch declaration.Kind {
		case definition.KindTask:
			slots := []DeclarationSlot{DeclarationSlotHandler}
			if declaration.Task.Payload.Kind == definition.SchemaKindStandard {
				slots = append(slots, DeclarationSlotPayloadSchema)
			}
			declarations = append(declarations, ProgramDeclaration{
				Kind:       DeclarationKindTask,
				DeclaredID: declaration.DeclaredID,
				Slots:      slots,
			})
		case definition.KindActor:
			declarations = append(declarations, ProgramDeclaration{
				Kind:       DeclarationKindActor,
				DeclaredID: declaration.DeclaredID,
				Slots:      []DeclarationSlot{DeclarationSlotHandler},
			})
		}
	}
	sort.Slice(declarations, func(left, right int) bool {
		return CompareDeclarations(declarations[left], declarations[right]) < 0
	})
	return declarations
}

func cloneQueueInputs(source []definition.QueueInput) []definition.QueueInput {
	cloned := make([]definition.QueueInput, len(source))
	copy(cloned, source)
	for index := range cloned {
		if cloned[index].ConcurrencyLimit != nil {
			value := *cloned[index].ConcurrencyLimit
			cloned[index].ConcurrencyLimit = &value
		}
	}
	return cloned
}

func decodeClosedDefinition(raw []byte, value any) error {
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(value); err != nil {
		return err
	}
	return ensureEOF(decoder, "definition input")
}
