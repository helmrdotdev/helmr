package deployment

import (
	"bytes"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"regexp"
	"slices"

	"github.com/helmrdotdev/helmr/internal/jsoncanon"
)

const (
	ProgramIndexFormatVersion   = 0
	ProgramReceiptFormatVersion = 0

	RuntimeAPIVersion                        = "helmr.runtime.v0"
	ProgramCodeArtifactMediaType             = "application/vnd.helmr.deployment-program-code.v0+squashfs"
	ProgramDependencyArtifactMediaType       = "application/vnd.helmr.deployment-program-dependencies.v0+squashfs"
	manifestDigestDomain                     = "helmr.deployment-definition-manifest.v0\x00"
	maxJSONSafeInteger                 int64 = 9007199254740991
	maxProgramFileSizeBytes            int64 = 16777216
	maxProgramReceiptSizeBytes               = 17825792
	ArchitectureAArch64                      = RuntimeArchitecture("aarch64")
	ArchitectureX8664                        = RuntimeArchitecture("x86_64")
	DeclarationKindTask                      = DeclarationKind("task")
	DeclarationKindActor                     = DeclarationKind("actor")
	DeclarationKindRunStream                 = DeclarationKind("run_stream")
	DeclarationSlotHandler                   = DeclarationSlot("handler")
	DeclarationSlotPayloadSchema             = DeclarationSlot("payloadSchema")
	DeclarationSlotSchema                    = DeclarationSlot("schema")
)

var declaredIDPattern = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]{0,127}$`)
var sha256DigestPattern = regexp.MustCompile(`^sha256:[0-9a-f]{64}$`)

type RuntimeArchitecture string
type DeclarationKind string
type DeclarationSlot string

type ProgramDeclaration struct {
	Kind       DeclarationKind   `json:"kind"`
	DeclaredID string            `json:"declaredId"`
	Slots      []DeclarationSlot `json:"slots"`
}

type ProgramDescriptor struct {
	Digest    string `json:"digest"`
	SizeBytes int64  `json:"sizeBytes"`
	MediaType string `json:"mediaType"`
}

type ProgramDependencies = ProgramDescriptor

type ProgramFile struct {
	Digest    string `json:"digest"`
	SizeBytes int64  `json:"sizeBytes"`
}

type ProgramIndex struct {
	FormatVersion     int                  `json:"formatVersion"`
	RuntimeAPIVersion string               `json:"runtimeApiVersion"`
	RuntimeDigest     string               `json:"runtimeDigest"`
	Architecture      RuntimeArchitecture  `json:"architecture"`
	Dependencies      ProgramDependencies  `json:"dependencies"`
	PackageGraph      ProgramFile          `json:"packageGraph"`
	Modules           ProgramFile          `json:"modules"`
	Declarations      []ProgramDeclaration `json:"declarations"`
}

type ProgramReceipt struct {
	FormatVersion   int               `json:"formatVersion"`
	Code            ProgramDescriptor `json:"code"`
	Dependencies    ProgramDescriptor `json:"dependencies"`
	DependencyIndex DependencyIndex   `json:"dependencyIndex"`
	Index           ProgramIndex      `json:"index"`
}

type programVerification struct {
	FormatVersion   int             `json:"formatVersion"`
	DependencyIndex DependencyIndex `json:"dependencyIndex"`
	Index           ProgramIndex    `json:"index"`
}

func ParseProgramReceipt(raw []byte) (ProgramReceipt, error) {
	if len(raw) == 0 || len(raw) > maxProgramReceiptSizeBytes {
		return ProgramReceipt{}, fmt.Errorf(
			"program receipt size is outside [1,%d]",
			maxProgramReceiptSizeBytes,
		)
	}
	canonical, err := jsoncanon.Transform(raw)
	if err != nil {
		return ProgramReceipt{}, fmt.Errorf("canonicalize program receipt: %w", err)
	}
	if !bytes.Equal(raw, canonical) {
		return ProgramReceipt{}, fmt.Errorf("program receipt is not RFC 8785 canonical JSON")
	}

	var receipt ProgramReceipt
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&receipt); err != nil {
		return ProgramReceipt{}, fmt.Errorf("decode program receipt: %w", err)
	}
	if err := ensureEOF(decoder, "program receipt"); err != nil {
		return ProgramReceipt{}, err
	}
	if err := ValidateProgramReceipt(receipt); err != nil {
		return ProgramReceipt{}, err
	}
	complete, err := CanonicalProgramReceipt(receipt)
	if err != nil {
		return ProgramReceipt{}, err
	}
	if !bytes.Equal(raw, complete) {
		return ProgramReceipt{}, fmt.Errorf(
			"program receipt does not match the complete canonical v0 shape",
		)
	}
	return cloneProgramReceipt(receipt), nil
}

func CanonicalProgramReceipt(receipt ProgramReceipt) ([]byte, error) {
	if err := ValidateProgramReceipt(receipt); err != nil {
		return nil, err
	}
	raw, err := json.Marshal(receipt)
	if err != nil {
		return nil, fmt.Errorf("encode program receipt: %w", err)
	}
	canonical, err := jsoncanon.Transform(raw)
	if err != nil {
		return nil, fmt.Errorf("canonicalize program receipt: %w", err)
	}
	if len(canonical) > maxProgramReceiptSizeBytes {
		return nil, fmt.Errorf(
			"program receipt size is outside [1,%d]",
			maxProgramReceiptSizeBytes,
		)
	}
	return canonical, nil
}

func ValidateProgramReceipt(receipt ProgramReceipt) error {
	if receipt.FormatVersion != ProgramReceiptFormatVersion {
		return fmt.Errorf(
			"program receipt formatVersion = %d, want %d",
			receipt.FormatVersion,
			ProgramReceiptFormatVersion,
		)
	}
	if err := validateProgramDescriptor(
		receipt.Code,
		"code",
		ProgramCodeArtifactMediaType,
		maxCodePhysicalBytes,
	); err != nil {
		return err
	}
	if err := validateProgramDescriptor(
		receipt.Dependencies,
		"dependencies",
		ProgramDependencyArtifactMediaType,
		maxDependencyPhysicalBytes,
	); err != nil {
		return err
	}
	if err := ValidateProgramIndex(receipt.Index); err != nil {
		return fmt.Errorf("program receipt index: %w", err)
	}
	if err := ValidateDependencyIndex(receipt.DependencyIndex); err != nil {
		return fmt.Errorf("program receipt dependencyIndex: %w", err)
	}
	if receipt.Index.Dependencies != receipt.Dependencies {
		return fmt.Errorf(
			"program receipt index dependency descriptor does not match dependencies",
		)
	}
	return validateProgramIndexes(receipt.Index, receipt.DependencyIndex)
}

func validateProgramDescriptor(
	descriptor ProgramDescriptor,
	name string,
	mediaType string,
	maxSize int64,
) error {
	if !sha256DigestPattern.MatchString(descriptor.Digest) {
		return fmt.Errorf(
			"program receipt %s.digest is not a lowercase SHA-256 digest",
			name,
		)
	}
	if descriptor.SizeBytes < 1 || descriptor.SizeBytes > maxSize {
		return fmt.Errorf(
			"program receipt %s.sizeBytes is outside [1,%d]",
			name,
			maxSize,
		)
	}
	if descriptor.MediaType != mediaType {
		return fmt.Errorf(
			"program receipt %s.mediaType = %q, want %q",
			name,
			descriptor.MediaType,
			mediaType,
		)
	}
	return nil
}

func cloneProgramReceipt(receipt ProgramReceipt) ProgramReceipt {
	receipt.Index = cloneReceiptProgramIndex(receipt.Index)
	return receipt
}

func parseProgramVerification(raw []byte) (programVerification, error) {
	if len(raw) == 0 || len(raw) > maxProgramReceiptSizeBytes {
		return programVerification{}, fmt.Errorf(
			"program verification size is outside [1,%d]",
			maxProgramReceiptSizeBytes,
		)
	}
	canonical, err := jsoncanon.Transform(raw)
	if err != nil {
		return programVerification{}, fmt.Errorf("canonicalize program verification: %w", err)
	}
	if !bytes.Equal(raw, canonical) {
		return programVerification{}, errors.New("program verification is not RFC 8785 canonical JSON")
	}
	var verified programVerification
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&verified); err != nil {
		return programVerification{}, fmt.Errorf("decode program verification: %w", err)
	}
	if err := ensureEOF(decoder, "program verification"); err != nil {
		return programVerification{}, err
	}
	if err := validateProgramVerification(verified); err != nil {
		return programVerification{}, err
	}
	complete, err := canonicalProgramVerification(verified)
	if err != nil {
		return programVerification{}, err
	}
	if !bytes.Equal(raw, complete) {
		return programVerification{}, errors.New(
			"program verification does not match the complete canonical v0 shape",
		)
	}
	verified.Index = cloneReceiptProgramIndex(verified.Index)
	return verified, nil
}

func canonicalProgramVerification(verified programVerification) ([]byte, error) {
	if err := validateProgramVerification(verified); err != nil {
		return nil, err
	}
	raw, err := json.Marshal(verified)
	if err != nil {
		return nil, fmt.Errorf("encode program verification: %w", err)
	}
	canonical, err := jsoncanon.Transform(raw)
	if err != nil {
		return nil, fmt.Errorf("canonicalize program verification: %w", err)
	}
	if len(canonical) > maxProgramReceiptSizeBytes {
		return nil, fmt.Errorf(
			"program verification size is outside [1,%d]",
			maxProgramReceiptSizeBytes,
		)
	}
	return canonical, nil
}

func validateProgramVerification(verified programVerification) error {
	if verified.FormatVersion != ProgramReceiptFormatVersion {
		return fmt.Errorf(
			"program verification formatVersion = %d, want %d",
			verified.FormatVersion,
			ProgramReceiptFormatVersion,
		)
	}
	if err := ValidateProgramIndex(verified.Index); err != nil {
		return fmt.Errorf("program verification index: %w", err)
	}
	if err := ValidateDependencyIndex(verified.DependencyIndex); err != nil {
		return fmt.Errorf("program verification dependencyIndex: %w", err)
	}
	return validateProgramIndexes(verified.Index, verified.DependencyIndex)
}

func validateProgramIndexes(index ProgramIndex, dependencies DependencyIndex) error {
	if index.RuntimeDigest != dependencies.RuntimeDigest ||
		index.Architecture != dependencies.Architecture {
		return errors.New("program and dependency indexes disagree on runtime or architecture")
	}
	if index.PackageGraph.Digest != dependencies.PackageGraphDigest ||
		index.PackageGraph.SizeBytes != dependencies.PackageGraphSizeBytes {
		return errors.New("program and dependency indexes disagree on package graph identity")
	}
	return nil
}

func cloneReceiptProgramIndex(index ProgramIndex) ProgramIndex {
	index.Declarations = append([]ProgramDeclaration(nil), index.Declarations...)
	for position := range index.Declarations {
		index.Declarations[position].Slots = append(
			[]DeclarationSlot(nil),
			index.Declarations[position].Slots...,
		)
	}
	return index
}

func ParseProgramIndex(raw []byte) (ProgramIndex, error) {
	if len(raw) == 0 || len(raw) > int(maxProgramFileSizeBytes) {
		return ProgramIndex{}, fmt.Errorf("program index size is outside [1,%d]", maxProgramFileSizeBytes)
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
	if len(canonical) > int(maxProgramFileSizeBytes) {
		return nil, fmt.Errorf("program index size is outside [1,%d]", maxProgramFileSizeBytes)
	}
	return canonical, nil
}

func ValidateProgramIndex(index ProgramIndex) error {
	if index.FormatVersion != ProgramIndexFormatVersion {
		return fmt.Errorf("program index formatVersion = %d, want %d", index.FormatVersion, ProgramIndexFormatVersion)
	}
	if index.RuntimeAPIVersion != RuntimeAPIVersion {
		return fmt.Errorf("program index runtimeApiVersion = %q, want %q", index.RuntimeAPIVersion, RuntimeAPIVersion)
	}
	if !sha256DigestPattern.MatchString(index.RuntimeDigest) {
		return fmt.Errorf("program index runtimeDigest is not a lowercase SHA-256 digest")
	}
	if !validArchitecture(index.Architecture) {
		return fmt.Errorf("program index architecture %q is unsupported", index.Architecture)
	}
	if !sha256DigestPattern.MatchString(index.Dependencies.Digest) {
		return fmt.Errorf("program index dependencies.digest is not a lowercase SHA-256 digest")
	}
	if index.Dependencies.SizeBytes < 1 || index.Dependencies.SizeBytes > maxJSONSafeInteger {
		return fmt.Errorf("program index dependencies.sizeBytes is not a positive JavaScript-safe integer")
	}
	if index.Dependencies.MediaType != ProgramDependencyArtifactMediaType {
		return fmt.Errorf("program index dependencies.mediaType = %q, want %q", index.Dependencies.MediaType, ProgramDependencyArtifactMediaType)
	}
	if err := validateProgramFile(index.PackageGraph, "packageGraph"); err != nil {
		return err
	}
	if err := validateProgramFile(index.Modules, "modules"); err != nil {
		return err
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

func validateProgramFile(file ProgramFile, name string) error {
	if !sha256DigestPattern.MatchString(file.Digest) {
		return fmt.Errorf("program index %s.digest is not a lowercase SHA-256 digest", name)
	}
	if file.SizeBytes < 1 || file.SizeBytes > maxProgramFileSizeBytes {
		return fmt.Errorf("program index %s.sizeBytes is outside [1,%d]", name, maxProgramFileSizeBytes)
	}
	return nil
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

func validArchitecture(architecture RuntimeArchitecture) bool {
	return architecture == ArchitectureAArch64 || architecture == ArchitectureX8664
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

func ensureEOF(decoder *json.Decoder, label string) error {
	if err := decoder.Decode(&struct{}{}); err != io.EOF {
		if err == nil {
			return fmt.Errorf("%s contains trailing data", label)
		}
		return fmt.Errorf("decode %s trailing data: %w", label, err)
	}
	return nil
}
