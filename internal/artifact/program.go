// Package artifact defines Program and Node runtime artifacts: descriptors,
// media types, the Program metadata and manifest, the compiler contract,
// declaration locators, build config, payload digests, runtime metadata, and
// the SquashFS profile and inspected entry tree every such artifact conforms
// to. It holds only pure contract code. Checkpoint, Computer image and disk
// version artifacts are defined by their own owners.
package artifact

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"regexp"

	"github.com/helmrdotdev/helmr/internal/definition"
	"github.com/helmrdotdev/helmr/internal/jsoncanon"
)

const (
	ProgramArtifactMediaType              = "application/vnd.helmr.deployment-program.v0+squashfs"
	maxJSONSafeInteger              int64 = 9007199254740991
	MaxProgramFileSizeBytes         int64 = 16777216
	MaxProgramVerificationSizeBytes       = 17891328
)

var sha256DigestPattern = regexp.MustCompile(`^sha256:[0-9a-f]{64}$`)

type ProgramDefinition struct {
	Kind       definition.Kind `json:"-"`
	DeclaredID string          `json:"-"`
	Agent      *definition.AgentManifest
	Computer   *definition.ComputerManifest
}

type ProgramDescriptor struct {
	Digest    string `json:"digest"`
	SizeBytes int64  `json:"sizeBytes"`
	MediaType string `json:"mediaType"`
}

type ProgramFile struct {
	Digest    string `json:"digest"`
	SizeBytes int64  `json:"sizeBytes"`
}

type ProgramConfig struct {
	EvaluatorContract string `json:"evaluatorContract"`
	SourceDigest      string `json:"sourceDigest"`
	ResultDigest      string `json:"resultDigest"`
}

type ProgramMetadata struct {
	Architecture       definition.RuntimeArchitecture `json:"architecture"`
	ConfigResultDigest string                         `json:"configResultDigest"`
	Definitions        []ProgramDefinition            `json:"definitions"`
	RuntimeContract    string                         `json:"runtimeContract"`
	RuntimeDigest      string                         `json:"runtimeDigest"`
}

// ProgramOutput is the canonical Program artifact and its definition metadata.
type ProgramOutput struct {
	Artifact ProgramDescriptor `json:"artifact"`
	Metadata ProgramMetadata   `json:"metadata"`
}

func ParseProgramOutput(raw []byte) (ProgramOutput, error) {
	if len(raw) == 0 || len(raw) > MaxProgramVerificationSizeBytes {
		return ProgramOutput{}, fmt.Errorf(
			"program output size is outside [1,%d]",
			MaxProgramVerificationSizeBytes,
		)
	}
	canonical, err := jsoncanon.Transform(raw)
	if err != nil {
		return ProgramOutput{}, fmt.Errorf("canonicalize program output: %w", err)
	}
	if !bytes.Equal(raw, canonical) {
		return ProgramOutput{}, errors.New("program output is not RFC 8785 canonical JSON")
	}
	var output ProgramOutput
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&output); err != nil {
		return ProgramOutput{}, fmt.Errorf("decode program output: %w", err)
	}
	if err := jsoncanon.RequireEOF(decoder, "program output"); err != nil {
		return ProgramOutput{}, err
	}
	if err := ValidateProgramOutput(output); err != nil {
		return ProgramOutput{}, err
	}
	complete, err := CanonicalProgramOutput(output)
	if err != nil {
		return ProgramOutput{}, err
	}
	if !bytes.Equal(raw, complete) {
		return ProgramOutput{}, errors.New("program output does not match the complete canonical v0 shape")
	}
	output.Metadata = cloneProgramMetadata(output.Metadata)
	return output, nil
}

func CanonicalProgramOutput(output ProgramOutput) ([]byte, error) {
	if err := ValidateProgramOutput(output); err != nil {
		return nil, err
	}
	raw, err := json.Marshal(output)
	if err != nil {
		return nil, fmt.Errorf("encode program output: %w", err)
	}
	canonical, err := jsoncanon.Transform(raw)
	if err != nil {
		return nil, fmt.Errorf("canonicalize program output: %w", err)
	}
	if len(canonical) == 0 || len(canonical) > MaxProgramVerificationSizeBytes {
		return nil, fmt.Errorf(
			"program output size is outside [1,%d]",
			MaxProgramVerificationSizeBytes,
		)
	}
	return canonical, nil
}

func validateProgramDescriptor(
	descriptor ProgramDescriptor,
	name string,
	mediaType string,
	maxSize int64,
) error {
	if !sha256DigestPattern.MatchString(descriptor.Digest) {
		return fmt.Errorf(
			"%s artifact digest is not a lowercase SHA-256 digest",
			name,
		)
	}
	if descriptor.SizeBytes < 1 || descriptor.SizeBytes > maxSize {
		return fmt.Errorf(
			"%s artifact sizeBytes is outside [1,%d]",
			name,
			maxSize,
		)
	}
	if descriptor.MediaType != mediaType {
		return fmt.Errorf(
			"%s artifact mediaType = %q, want %q",
			name,
			descriptor.MediaType,
			mediaType,
		)
	}
	return nil
}

func ValidateProgramOutput(output ProgramOutput) error {
	if err := validateProgramDescriptor(
		output.Artifact,
		"program",
		ProgramArtifactMediaType,
		MaxProgramPhysicalBytes,
	); err != nil {
		return err
	}
	if err := ValidateProgramMetadata(output.Metadata); err != nil {
		return fmt.Errorf("program output index: %w", err)
	}
	return nil
}

func (index ProgramMetadata) Clone() ProgramMetadata {
	return cloneProgramMetadata(index)
}

func cloneProgramMetadata(index ProgramMetadata) ProgramMetadata {
	declarations := make([]ProgramDefinition, len(index.Definitions))
	copy(declarations, index.Definitions)
	index.Definitions = declarations
	for position := range index.Definitions {
		index.Definitions[position] = cloneProgramDefinition(
			index.Definitions[position],
		)
	}
	return index
}

func ParseProgramMetadata(raw []byte) (ProgramMetadata, error) {
	if len(raw) == 0 || len(raw) > int(MaxProgramFileSizeBytes) {
		return ProgramMetadata{}, fmt.Errorf("program metadata size is outside [1,%d]", MaxProgramFileSizeBytes)
	}
	canonical, err := jsoncanon.Transform(raw)
	if err != nil {
		return ProgramMetadata{}, fmt.Errorf("canonicalize program metadata: %w", err)
	}
	if !bytes.Equal(raw, canonical) {
		return ProgramMetadata{}, fmt.Errorf("program metadata is not RFC 8785 canonical JSON")
	}

	var index ProgramMetadata
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&index); err != nil {
		return ProgramMetadata{}, fmt.Errorf("decode program metadata: %w", err)
	}
	if err := jsoncanon.RequireEOF(decoder, "program metadata"); err != nil {
		return ProgramMetadata{}, err
	}
	if err := ValidateProgramMetadata(index); err != nil {
		return ProgramMetadata{}, err
	}
	complete, err := CanonicalProgramMetadata(index)
	if err != nil {
		return ProgramMetadata{}, err
	}
	if !bytes.Equal(raw, complete) {
		return ProgramMetadata{}, fmt.Errorf("program metadata does not match the complete canonical v0 shape")
	}
	return cloneProgramMetadata(index), nil
}

func CanonicalProgramMetadata(index ProgramMetadata) ([]byte, error) {
	if err := ValidateProgramMetadata(index); err != nil {
		return nil, err
	}
	raw, err := json.Marshal(index)
	if err != nil {
		return nil, fmt.Errorf("encode program metadata: %w", err)
	}
	canonical, err := jsoncanon.Transform(raw)
	if err != nil {
		return nil, fmt.Errorf("canonicalize program metadata: %w", err)
	}
	if len(canonical) > int(MaxProgramFileSizeBytes) {
		return nil, fmt.Errorf("program metadata size is outside [1,%d]", MaxProgramFileSizeBytes)
	}
	return canonical, nil
}

func ValidateProgramMetadata(index ProgramMetadata) error {
	if index.RuntimeContract != definition.RuntimeContract {
		return fmt.Errorf("program metadata runtimeContract = %q, want %q", index.RuntimeContract, definition.RuntimeContract)
	}
	if !sha256DigestPattern.MatchString(index.RuntimeDigest) {
		return errors.New("program metadata runtimeDigest is not a lowercase SHA-256 digest")
	}
	if !validArchitecture(index.Architecture) {
		return fmt.Errorf("program metadata architecture %q is unsupported", index.Architecture)
	}
	if !sha256DigestPattern.MatchString(index.ConfigResultDigest) {
		return errors.New("program metadata configResultDigest is not a lowercase SHA-256 digest")
	}
	if len(index.Definitions) == 0 || len(index.Definitions) > definition.MaxBuildDefinitions {
		return fmt.Errorf("program metadata declarations must contain 1 to %d definitions", definition.MaxBuildDefinitions)
	}
	for position, declaration := range index.Definitions {
		if err := ValidateProgramDefinition(declaration); err != nil {
			return fmt.Errorf("program metadata declaration %d: %w", position, err)
		}
		if position > 0 &&
			CompareProgramDefinitions(index.Definitions[position-1], declaration) >= 0 {
			return fmt.Errorf("program metadata declarations are not in canonical order at position %d", position)
		}
	}
	computers := make(map[string]struct{})
	for _, declaration := range index.Definitions {
		if declaration.Computer != nil {
			computers[declaration.DeclaredID] = struct{}{}
		}
	}
	for _, declaration := range index.Definitions {
		if declaration.Agent != nil {
			if _, found := computers[declaration.Agent.ComputerDefinitionID]; !found {
				return errors.New("agent references an absent Computer")
			}
		}
	}

	return nil
}

func validArchitecture(architecture definition.RuntimeArchitecture) bool {
	return architecture == definition.ArchitectureX8664
}
