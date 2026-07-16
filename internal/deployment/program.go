package deployment

import (
	"bytes"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"io"
	"regexp"
	"slices"

	"github.com/helmrdotdev/helmr/internal/jsoncanon"
)

const (
	ProgramIndexFormatVersion = 0

	ProgramBundleFormatVersion   = "helmr.program-bundle.v0"
	RuntimeAPIVersion            = "helmr.runtime-api.v0"
	CheckpointProtocolVersion    = "helmr.checkpoint.v0"
	manifestDigestDomain         = "helmr.deployment-definition-manifest.v0\x00"
	runtimeContractDigestDomain  = "helmr.program-runtime-abi.v0\x00"
	ArchitectureAArch64          = RuntimeArchitecture("aarch64")
	ArchitectureX8664            = RuntimeArchitecture("x86_64")
	DeclarationKindTask          = DeclarationKind("task")
	DeclarationKindActor         = DeclarationKind("actor")
	DeclarationKindRunStream     = DeclarationKind("run_stream")
	DeclarationSlotHandler       = DeclarationSlot("handler")
	DeclarationSlotPayloadSchema = DeclarationSlot("payloadSchema")
	DeclarationSlotSchema        = DeclarationSlot("schema")
)

var declaredIDPattern = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]{0,127}$`)

type RuntimeArchitecture string
type DeclarationKind string
type DeclarationSlot string

type ProgramRuntimeABI struct {
	BundleFormatVersion       string `json:"bundleFormatVersion"`
	RuntimeAPIVersion         string `json:"runtimeApiVersion"`
	CheckpointProtocolVersion string `json:"checkpointProtocolVersion"`
}

func CurrentProgramRuntimeABI() ProgramRuntimeABI {
	return ProgramRuntimeABI{
		BundleFormatVersion:       ProgramBundleFormatVersion,
		RuntimeAPIVersion:         RuntimeAPIVersion,
		CheckpointProtocolVersion: CheckpointProtocolVersion,
	}
}

type ProgramDeclaration struct {
	Kind       DeclarationKind   `json:"kind"`
	DeclaredID string            `json:"declaredId"`
	Slots      []DeclarationSlot `json:"slots"`
}

type ProgramIndex struct {
	FormatVersion          int                   `json:"formatVersion"`
	RuntimeContract        ProgramRuntimeABI     `json:"runtimeContract"`
	SupportedArchitectures []RuntimeArchitecture `json:"supportedArchitectures"`
	Declarations           []ProgramDeclaration  `json:"declarations"`
}

func ParseProgramIndex(raw []byte) (ProgramIndex, error) {
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
	if err := ensureEOF(decoder); err != nil {
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
	return index, nil
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
	return canonical, nil
}

func ValidateProgramIndex(index ProgramIndex) error {
	if index.FormatVersion != ProgramIndexFormatVersion {
		return fmt.Errorf("program index formatVersion = %d, want %d", index.FormatVersion, ProgramIndexFormatVersion)
	}
	if err := ValidateCurrentProgramRuntimeABI(index.RuntimeContract); err != nil {
		return fmt.Errorf("program index runtimeContract: %w", err)
	}
	if !validArchitectures(index.SupportedArchitectures) {
		return fmt.Errorf("program index supportedArchitectures is not a non-empty canonical architecture set")
	}
	if len(index.Declarations) == 0 {
		return fmt.Errorf("program index declarations must not be empty")
	}
	for position, declaration := range index.Declarations {
		if err := validateDeclaration(declaration); err != nil {
			return fmt.Errorf("program index declaration %d: %w", position, err)
		}
		if position > 0 && compareDeclarations(index.Declarations[position-1], declaration) >= 0 {
			return fmt.Errorf("program index declarations are not in canonical order at position %d", position)
		}
	}
	return nil
}

func ValidateCurrentProgramRuntimeABI(abi ProgramRuntimeABI) error {
	if abi != CurrentProgramRuntimeABI() {
		return fmt.Errorf("program runtime ABI does not match the toolchain-owned v0 tuple")
	}
	return nil
}

func ProgramRuntimeABIDigest(abi ProgramRuntimeABI) ([sha256.Size]byte, error) {
	if err := ValidateCurrentProgramRuntimeABI(abi); err != nil {
		return [sha256.Size]byte{}, err
	}
	raw, err := json.Marshal(abi)
	if err != nil {
		return [sha256.Size]byte{}, fmt.Errorf("encode program runtime ABI: %w", err)
	}
	canonical, err := jsoncanon.Transform(raw)
	if err != nil {
		return [sha256.Size]byte{}, fmt.Errorf("canonicalize program runtime ABI: %w", err)
	}
	return domainDigest(runtimeContractDigestDomain, canonical), nil
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

func validArchitectures(architectures []RuntimeArchitecture) bool {
	return slices.Equal(architectures, []RuntimeArchitecture{ArchitectureAArch64}) ||
		slices.Equal(architectures, []RuntimeArchitecture{ArchitectureX8664}) ||
		slices.Equal(architectures, []RuntimeArchitecture{ArchitectureAArch64, ArchitectureX8664})
}

func validateDeclaration(declaration ProgramDeclaration) error {
	if !declaredIDPattern.MatchString(declaration.DeclaredID) {
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
	case DeclarationKindRunStream:
		if !slices.Equal(declaration.Slots, []DeclarationSlot{DeclarationSlotSchema}) {
			return fmt.Errorf("run_stream slots must be [schema]")
		}
	default:
		return fmt.Errorf("unknown kind %q", declaration.Kind)
	}
	return nil
}

func compareDeclarations(left, right ProgramDeclaration) int {
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
	case DeclarationKindRunStream:
		return 2
	default:
		return 3
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

func ensureEOF(decoder *json.Decoder) error {
	if err := decoder.Decode(&struct{}{}); err != io.EOF {
		if err == nil {
			return fmt.Errorf("program index contains trailing data")
		}
		return fmt.Errorf("decode program index trailing data: %w", err)
	}
	return nil
}
