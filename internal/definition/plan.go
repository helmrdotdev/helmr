package definition

import (
	"bytes"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"

	"github.com/helmrdotdev/helmr/internal/jsoncanon"
)

const (
	BuildPlanFormatVersion      = 0
	DeploymentPlanFormatVersion = 0

	manifestDigestDomain       = "helmr.deployment-definition-manifest.v0\x00"
	maxJSONSafeInteger   int64 = 9007199254740991

	KindAgent    = Kind("agent")
	KindComputer = Kind("computer")

	maxBuildPlanBytes   = 16 << 20
	MaxBuildDefinitions = 10000
	maxBuildImageSteps  = 10000
)

type Kind string

type BuildPlan struct {
	FormatVersion int     `json:"formatVersion"`
	Definitions   []Input `json:"definitions"`
}

type Input struct {
	Kind       Kind   `json:"-"`
	DeclaredID string `json:"-"`
	Agent      *AgentManifest
	Computer   *ComputerInputManifest
}

type ResourcesManifest struct {
	MilliCPU  int64  `json:"milliCpu"`
	MemoryMiB int64  `json:"memoryMiB"`
	DiskMiB   *int64 `json:"diskMiB,omitempty"`
}

func ParseBuildPlan(raw []byte) (BuildPlan, error) {
	if len(raw) == 0 || len(raw) > maxBuildPlanBytes {
		return BuildPlan{}, fmt.Errorf("build plan size is outside [1,%d]", maxBuildPlanBytes)
	}
	canonical, err := jsoncanon.Transform(raw)
	if err != nil {
		return BuildPlan{}, fmt.Errorf("canonicalize build plan: %w", err)
	}
	if !bytes.Equal(raw, canonical) {
		return BuildPlan{}, errors.New("build plan is not RFC 8785 canonical JSON")
	}

	var plan BuildPlan
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&plan); err != nil {
		return BuildPlan{}, fmt.Errorf("decode build plan: %w", err)
	}
	if err := jsoncanon.RequireEOF(decoder, "build plan"); err != nil {
		return BuildPlan{}, err
	}
	if err := ValidateBuildPlan(plan); err != nil {
		return BuildPlan{}, err
	}
	complete, err := CanonicalBuildPlan(plan)
	if err != nil {
		return BuildPlan{}, err
	}
	if !bytes.Equal(raw, complete) {
		return BuildPlan{}, errors.New("build plan does not match the complete canonical v0 shape")
	}
	return plan, nil
}

func CanonicalBuildPlan(plan BuildPlan) ([]byte, error) {
	if err := ValidateBuildPlan(plan); err != nil {
		return nil, err
	}
	raw, err := json.Marshal(plan)
	if err != nil {
		return nil, fmt.Errorf("encode build plan: %w", err)
	}
	canonical, err := jsoncanon.Transform(raw)
	if err != nil {
		return nil, fmt.Errorf("canonicalize build plan: %w", err)
	}
	if len(canonical) > maxBuildPlanBytes {
		return nil, fmt.Errorf("build plan size is outside [1,%d]", maxBuildPlanBytes)
	}
	return canonical, nil
}

func ValidateBuildPlan(plan BuildPlan) error {
	if plan.FormatVersion != BuildPlanFormatVersion {
		return errors.New("unsupported build plan formatVersion")
	}
	if len(plan.Definitions) == 0 || len(plan.Definitions) > MaxBuildDefinitions {
		return errors.New("build plan must contain 1 to 10000 definitions")
	}
	computers := make(map[string]bool)
	imageSteps := 0
	for i, input := range plan.Definitions {
		if err := validateDefinitionInput(input); err != nil {
			return fmt.Errorf("build plan definition %d: %w", i, err)
		}
		if i > 0 && compareDefinitionInputs(plan.Definitions[i-1], input) >= 0 {
			return errors.New("build plan definitions are not in canonical order")
		}
		if input.Computer != nil {
			computers[input.DeclaredID] = true
			imageSteps += ImageBuildStepCount(input.Computer.ImageBuild)
		}
	}
	if imageSteps > maxBuildImageSteps {
		return errors.New("build plan exceeds 10000 image steps")
	}
	for _, input := range plan.Definitions {
		if input.Agent != nil && !computers[input.Agent.ComputerDefinitionID] {
			return errors.New("agent references an undeclared Computer")
		}
	}
	return nil
}

func (input Input) MarshalJSON() ([]byte, error) {
	var manifest any
	switch input.Kind {
	case KindAgent:
		if input.Agent == nil || input.Computer != nil {
			return nil, errors.New("agent input requires exactly one Agent manifest")
		}
		manifest = input.Agent
	case KindComputer:
		if input.Computer == nil || input.Agent != nil {
			return nil, errors.New("computer input requires exactly one Computer manifest")
		}
		manifest = input.Computer
	default:
		return nil, fmt.Errorf("definition kind %q is unsupported", input.Kind)
	}
	return json.Marshal(struct {
		Kind       Kind   `json:"kind"`
		DeclaredID string `json:"declaredId"`
		Manifest   any    `json:"manifest"`
	}{input.Kind, input.DeclaredID, manifest})
}
func (input *Input) UnmarshalJSON(raw []byte) error {
	var wire struct {
		Kind       Kind            `json:"kind"`
		DeclaredID string          `json:"declaredId"`
		Manifest   json.RawMessage `json:"manifest"`
	}
	if err := decodeClosedDefinition(raw, &wire); err != nil {
		return err
	}
	*input = Input{Kind: wire.Kind, DeclaredID: wire.DeclaredID}
	switch wire.Kind {
	case KindAgent:
		return decodeClosedDefinition(wire.Manifest, &input.Agent)
	case KindComputer:
		return decodeClosedDefinition(wire.Manifest, &input.Computer)
	default:
		return fmt.Errorf("definition kind %q is unsupported", wire.Kind)
	}
}

func decodeClosedDefinition(raw []byte, value any) error {
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(value); err != nil {
		return err
	}
	return jsoncanon.RequireEOF(decoder, "definition input")
}

func validateDefinitionInput(input Input) error {
	if !ValidDeclaredID(input.DeclaredID) {
		return errors.New("declaredId is outside the exact ASCII ID domain")
	}
	switch input.Kind {
	case KindAgent:
		if input.Agent == nil || input.Computer != nil {
			return errors.New("agent requires exactly one manifest")
		}
		return ValidateAgentManifest(*input.Agent)
	case KindComputer:
		if input.Computer == nil || input.Agent != nil {
			return errors.New("computer requires exactly one manifest")
		}
		if err := ValidateImageBuild(input.Computer.ImageBuild, string(ArchitectureX8664)); err != nil {
			return err
		}
		return validateComputerSettings(input.Computer.Resources, input.Computer.Refresh, input.Computer.Secrets, input.Computer.BuildSecrets)
	default:
		return fmt.Errorf("definition kind %q is unsupported", input.Kind)
	}
}

func ValidateResourcesManifest(resources ResourcesManifest) error {
	if resources.DiskMiB != nil && !positiveSafeInteger(*resources.DiskMiB) {
		return errors.New("diskMiB must be a positive JavaScript-safe integer")
	}
	if !positiveSafeInteger(resources.MilliCPU) {
		return errors.New("milliCpu must be a positive JavaScript-safe integer")
	}
	if !positiveSafeInteger(resources.MemoryMiB) {
		return errors.New("memoryMiB must be a positive JavaScript-safe integer")
	}
	return nil
}

func positiveSafeInteger(value int64) bool {
	return value > 0 && value <= maxJSONSafeInteger
}

func compareDefinitionInputs(left, right Input) int {
	leftKind := definitionKindOrder(left.Kind)
	rightKind := definitionKindOrder(right.Kind)
	if leftKind < rightKind {
		return -1
	}
	if leftKind > rightKind {
		return 1
	}
	return bytes.Compare([]byte(left.DeclaredID), []byte(right.DeclaredID))
}

func definitionKindOrder(kind Kind) int {
	switch kind {
	case KindAgent:
		return 0
	case KindComputer:
		return 1
	default:
		return 2
	}
}

func CanonicalManifestAndDigest(raw []byte) ([]byte, [sha256.Size]byte, error) {
	canonical, err := jsoncanon.Transform(raw)
	if err != nil {
		return nil, [sha256.Size]byte{}, fmt.Errorf("canonicalize deployment manifest: %w", err)
	}
	if len(canonical) == 0 || canonical[0] != '{' {
		return nil, [sha256.Size]byte{}, fmt.Errorf("deployment manifest root must be an object")
	}
	return canonical, domainDigest(manifestDigestDomain, canonical), nil
}

func domainDigest(domain string, canonical []byte) [sha256.Size]byte {
	hash := sha256.New()
	hash.Write([]byte(domain))
	hash.Write(canonical)
	var digest [sha256.Size]byte
	copy(digest[:], hash.Sum(nil))
	return digest
}
