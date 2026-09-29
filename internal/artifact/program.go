// Package artifact defines Program and Node runtime artifacts: descriptors,
// media types, the Program index and manifest, the compiler contract,
// declaration locators, build config, payload digests, runtime metadata, and
// the SquashFS profile and inspected entry tree every such artifact conforms
// to. It holds only pure contract code. Checkpoint, Computer image and disk
// version artifacts are defined by their own owners.
package artifact

import (
	"bytes"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"regexp"
	"slices"
	"strings"

	"github.com/helmrdotdev/helmr/internal/definition"
	"github.com/helmrdotdev/helmr/internal/jsoncanon"
)

const (
	ProgramArtifactMediaType              = "application/vnd.helmr.deployment-program.v0+squashfs"
	maxJSONSafeInteger              int64 = 9007199254740991
	MaxProgramFileSizeBytes         int64 = 16777216
	MaxProgramVerificationSizeBytes       = 17891328
	DeclarationKindTask                   = DeclarationKind("task")
	DeclarationKindActor                  = DeclarationKind("actor")
	DeclarationSlotHandler                = DeclarationSlot("handler")
	DeclarationSlotPayloadSchema          = DeclarationSlot("payloadSchema")
)

var sha256DigestPattern = regexp.MustCompile(`^sha256:[0-9a-f]{64}$`)

type DeclarationKind string
type DeclarationSlot string

type ProgramDeclaration struct {
	Kind       DeclarationKind   `json:"kind"`
	DeclaredID string            `json:"declaredId"`
	Slots      []DeclarationSlot `json:"slots"`
}

type ProgramLocator struct {
	ExportName string          `json:"exportName"`
	ModulePath string          `json:"modulePath"`
	Slot       DeclarationSlot `json:"slot"`
}

type ProgramIndexDeclaration struct {
	Kind       definition.Kind `json:"-"`
	DeclaredID string          `json:"-"`
	Task       *definition.TaskManifest
	Actor      *definition.ActorManifest
	Sandbox    *definition.SandboxManifest
	Locator    *ProgramLocator
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

type ProgramIndex struct {
	Architecture       definition.RuntimeArchitecture `json:"architecture"`
	ConfigResultDigest string                         `json:"configResultDigest"`
	Declarations       []ProgramIndexDeclaration      `json:"declarations"`
	Queues             []definition.QueueInput        `json:"queues"`
	RuntimeContract    string                         `json:"runtimeContract"`
	RuntimeDigest      string                         `json:"runtimeDigest"`
}

// ProgramOutput is the canonical Program artifact and its execution index.
type ProgramOutput struct {
	Artifact ProgramDescriptor `json:"artifact"`
	Index    ProgramIndex      `json:"index"`
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
	if err := ensureEOF(decoder, "program output"); err != nil {
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
	output.Index = cloneProgramIndex(output.Index)
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
	if err := ValidateProgramIndex(output.Index); err != nil {
		return fmt.Errorf("program output index: %w", err)
	}
	return nil
}

func (index ProgramIndex) Clone() ProgramIndex {
	return cloneProgramIndex(index)
}

func cloneProgramIndex(index ProgramIndex) ProgramIndex {
	declarations := make([]ProgramIndexDeclaration, len(index.Declarations))
	copy(declarations, index.Declarations)
	index.Declarations = declarations
	for position := range index.Declarations {
		index.Declarations[position] = cloneProgramIndexDeclaration(
			index.Declarations[position],
		)
	}
	queues := make([]definition.QueueInput, len(index.Queues))
	copy(queues, index.Queues)
	index.Queues = queues
	for position := range index.Queues {
		if index.Queues[position].ConcurrencyLimit != nil {
			value := *index.Queues[position].ConcurrencyLimit
			index.Queues[position].ConcurrencyLimit = &value
		}
	}
	return index
}

func ParseProgramIndex(raw []byte) (ProgramIndex, error) {
	if len(raw) == 0 || len(raw) > int(MaxProgramFileSizeBytes) {
		return ProgramIndex{}, fmt.Errorf("program index size is outside [1,%d]", MaxProgramFileSizeBytes)
	}
	canonical, err := jsoncanon.Transform(raw)
	if err != nil {
		return ProgramIndex{}, fmt.Errorf("canonicalize program index: %w", err)
	}
	if !bytes.Equal(raw, canonical) {
		return ProgramIndex{}, fmt.Errorf("program index is not RFC 8785 canonical JSON")
	}

	var index ProgramIndex
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&index); err != nil {
		return ProgramIndex{}, fmt.Errorf("decode program index: %w", err)
	}
	if err := ensureEOF(decoder, "program index"); err != nil {
		return ProgramIndex{}, err
	}
	if err := ValidateProgramIndex(index); err != nil {
		return ProgramIndex{}, err
	}
	complete, err := CanonicalProgramIndex(index)
	if err != nil {
		return ProgramIndex{}, err
	}
	if !bytes.Equal(raw, complete) {
		return ProgramIndex{}, fmt.Errorf("program index does not match the complete canonical v0 shape")
	}
	return cloneProgramIndex(index), nil
}

func CanonicalProgramIndex(index ProgramIndex) ([]byte, error) {
	if err := ValidateProgramIndex(index); err != nil {
		return nil, err
	}
	raw, err := json.Marshal(index)
	if err != nil {
		return nil, fmt.Errorf("encode program index: %w", err)
	}
	canonical, err := jsoncanon.Transform(raw)
	if err != nil {
		return nil, fmt.Errorf("canonicalize program index: %w", err)
	}
	if len(canonical) > int(MaxProgramFileSizeBytes) {
		return nil, fmt.Errorf("program index size is outside [1,%d]", MaxProgramFileSizeBytes)
	}
	return canonical, nil
}

func ValidateProgramIndex(index ProgramIndex) error {
	if index.RuntimeContract != definition.RuntimeContract {
		return fmt.Errorf("program index runtimeContract = %q, want %q", index.RuntimeContract, definition.RuntimeContract)
	}
	if !sha256DigestPattern.MatchString(index.RuntimeDigest) {
		return errors.New("program index runtimeDigest is not a lowercase SHA-256 digest")
	}
	if !validArchitecture(index.Architecture) {
		return fmt.Errorf("program index architecture %q is unsupported", index.Architecture)
	}
	if !sha256DigestPattern.MatchString(index.ConfigResultDigest) {
		return errors.New("program index configResultDigest is not a lowercase SHA-256 digest")
	}
	if index.Queues == nil {
		return errors.New("program index queues must be an array")
	}
	queues := make(map[string]struct{}, len(index.Queues))
	for position, queue := range index.Queues {
		if err := definition.ValidateQueueInput(queue); err != nil {
			return fmt.Errorf("program index queue %d: %w", position, err)
		}
		if position > 0 && bytes.Compare(
			[]byte(index.Queues[position-1].Name),
			[]byte(queue.Name),
		) >= 0 {
			return fmt.Errorf(
				"program index queues are not in canonical order at position %d",
				position,
			)
		}
		queues[queue.Name] = struct{}{}
	}
	if len(index.Declarations) == 0 {
		return fmt.Errorf("program index declarations must not be empty")
	}
	for position, declaration := range index.Declarations {
		if err := ValidateProgramIndexDeclaration(declaration, queues); err != nil {
			return fmt.Errorf("program index declaration %d: %w", position, err)
		}
		if position > 0 &&
			CompareProgramIndexDeclarations(index.Declarations[position-1], declaration) >= 0 {
			return fmt.Errorf("program index declarations are not in canonical order at position %d", position)
		}
	}
	return nil
}

func BuildPlanProgramDeclarations(plan definition.BuildPlan) []ProgramDeclaration {
	declarations := make([]ProgramDeclaration, 0)
	for _, input := range plan.Definitions {
		switch input.Kind {
		case definition.KindTask:
			slots := []DeclarationSlot{DeclarationSlotHandler}
			if input.Task.Payload.Kind == definition.SchemaKindStandard {
				slots = append(slots, DeclarationSlotPayloadSchema)
			}
			declarations = append(declarations, ProgramDeclaration{
				Kind: DeclarationKindTask, DeclaredID: input.DeclaredID, Slots: slots,
			})
		case definition.KindActor:
			declarations = append(declarations, ProgramDeclaration{
				Kind: DeclarationKindActor, DeclaredID: input.DeclaredID,
				Slots: []DeclarationSlot{DeclarationSlotHandler},
			})
		}
	}
	return declarations
}

func validArchitecture(architecture definition.RuntimeArchitecture) bool {
	return architecture == definition.ArchitectureX8664
}

func hasNodeModulesComponent(value string) bool {
	for item := range strings.SplitSeq(value, "/") {
		if item == "node_modules" {
			return true
		}
	}
	return false
}

func ValidateDeclaration(declaration ProgramDeclaration) error {
	if !definition.ValidDeclaredID(declaration.DeclaredID) {
		return fmt.Errorf("declaredId %q is outside the exact ASCII ID domain", declaration.DeclaredID)
	}
	switch declaration.Kind {
	case DeclarationKindTask:
		if !slices.Equal(declaration.Slots, []DeclarationSlot{DeclarationSlotHandler}) &&
			!slices.Equal(declaration.Slots, []DeclarationSlot{DeclarationSlotHandler, DeclarationSlotPayloadSchema}) {
			return fmt.Errorf("task slots must be [handler] or [handler,payloadSchema]")
		}
	case DeclarationKindActor:
		if !slices.Equal(declaration.Slots, []DeclarationSlot{DeclarationSlotHandler}) {
			return fmt.Errorf("actor slots must be [handler]")
		}
	default:
		return fmt.Errorf("unknown kind %q", declaration.Kind)
	}
	return nil
}

func CompareDeclarations(left, right ProgramDeclaration) int {
	leftKind := declarationKindOrder(left.Kind)
	rightKind := declarationKindOrder(right.Kind)
	if leftKind < rightKind {
		return -1
	}
	if leftKind > rightKind {
		return 1
	}
	return bytes.Compare([]byte(left.DeclaredID), []byte(right.DeclaredID))
}

func declarationKindOrder(kind DeclarationKind) int {
	switch kind {
	case DeclarationKindTask:
		return 0
	case DeclarationKindActor:
		return 1
	default:
		return 2
	}
}

func domainDigest(domain string, canonical []byte) [sha256.Size]byte {
	hash := sha256.New()
	hash.Write([]byte(domain))
	hash.Write(canonical)
	var digest [sha256.Size]byte
	copy(digest[:], hash.Sum(nil))
	return digest
}

func ensureEOF(decoder *json.Decoder, label string) error {
	if err := decoder.Decode(&struct{}{}); err != io.EOF {
		if err == nil {
			return fmt.Errorf("%s contains trailing data", label)
		}
		return fmt.Errorf("decode %s trailing data: %w", label, err)
	}
	return nil
}
