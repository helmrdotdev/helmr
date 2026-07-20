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
	"strings"

	"github.com/helmrdotdev/helmr/internal/jsoncanon"
)

const (
	ProgramIndexFormatVersion   = 0
	ProgramReceiptFormatVersion = 0

	RuntimeAPIVersion                        = "helmr.runtime.v0"
	ProgramBuildContractVersion              = "helmr.program-build.v0"
	ProgramCodeArtifactMediaType             = "application/vnd.helmr.deployment-program-code.v0+squashfs"
	ProgramDependencyArtifactMediaType       = "application/vnd.helmr.deployment-program-dependencies.v0+squashfs"
	manifestDigestDomain                     = "helmr.deployment-definition-manifest.v0\x00"
	maxJSONSafeInteger                 int64 = 9007199254740991
	maxProgramFileSizeBytes            int64 = 16777216
	maxProgramReceiptSizeBytes               = 17891328
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

type ProgramManager struct {
	CapsuleDigest string             `json:"capsuleDigest"`
	Name          PackageManagerName `json:"name"`
	Version       string             `json:"version"`
}

type ProgramSubmittedSource struct {
	LockfileDigest string `json:"lockfileDigest"`
	LockfileName   string `json:"lockfileName"`
	SourceDigest   string `json:"sourceDigest"`
}

type BuildProvenance struct {
	Architecture            RuntimeArchitecture    `json:"architecture"`
	BuildContractVersion    string                 `json:"buildContractVersion"`
	Manager                 ProgramManager         `json:"manager"`
	RuntimeDigest           string                 `json:"runtimeDigest"`
	StandardToolchainDigest string                 `json:"standardToolchainDigest"`
	Submitted               ProgramSubmittedSource `json:"submitted"`
}

type ProgramIndex struct {
	Architecture            RuntimeArchitecture    `json:"architecture"`
	BuildContractVersion    string                 `json:"buildContractVersion"`
	Declarations            []ProgramDeclaration   `json:"declarations"`
	DependenciesDigest      string                 `json:"dependenciesDigest"`
	FormatVersion           int                    `json:"formatVersion"`
	Manager                 ProgramManager         `json:"manager"`
	RuntimeAPIVersion       string                 `json:"runtimeApiVersion"`
	RuntimeDigest           string                 `json:"runtimeDigest"`
	StandardToolchainDigest string                 `json:"standardToolchainDigest"`
	Submitted               ProgramSubmittedSource `json:"submitted"`
}

type ProgramReceipt struct {
	FormatVersion int               `json:"formatVersion"`
	Code          ProgramDescriptor `json:"code"`
	Dependencies  ProgramDescriptor `json:"dependencies"`
	Index         ProgramIndex      `json:"index"`
}

type programVerification struct {
	FormatVersion int          `json:"formatVersion"`
	Index         ProgramIndex `json:"index"`
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
	if receipt.Index.DependenciesDigest != receipt.Dependencies.Digest {
		return fmt.Errorf(
			"program receipt index dependenciesDigest does not match dependencies",
		)
	}
	return nil
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
	if err := validateBuildProvenance("program index", BuildProvenance{
		Architecture:            index.Architecture,
		BuildContractVersion:    index.BuildContractVersion,
		Manager:                 index.Manager,
		RuntimeDigest:           index.RuntimeDigest,
		StandardToolchainDigest: index.StandardToolchainDigest,
		Submitted:               index.Submitted,
	}); err != nil {
		return err
	}
	if !sha256DigestPattern.MatchString(index.DependenciesDigest) {
		return fmt.Errorf("program index dependenciesDigest is not a lowercase SHA-256 digest")
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

func validateBuildProvenance(prefix string, provenance BuildProvenance) error {
	if provenance.BuildContractVersion != ProgramBuildContractVersion {
		return fmt.Errorf(
			"%s buildContractVersion = %q, want %q",
			prefix,
			provenance.BuildContractVersion,
			ProgramBuildContractVersion,
		)
	}
	if !sha256DigestPattern.MatchString(provenance.RuntimeDigest) {
		return fmt.Errorf("%s runtimeDigest is not a lowercase SHA-256 digest", prefix)
	}
	if !validArchitecture(provenance.Architecture) {
		return fmt.Errorf("%s architecture %q is unsupported", prefix, provenance.Architecture)
	}
	if !sha256DigestPattern.MatchString(provenance.StandardToolchainDigest) {
		return fmt.Errorf("%s standardToolchainDigest is not a lowercase SHA-256 digest", prefix)
	}
	if !sha256DigestPattern.MatchString(provenance.Manager.CapsuleDigest) {
		return fmt.Errorf("%s manager.capsuleDigest is not a lowercase SHA-256 digest", prefix)
	}
	if provenance.Manager.Name != PackageManagerNPM && provenance.Manager.Name != PackageManagerBun {
		return fmt.Errorf("%s manager.name %q is unsupported", prefix, provenance.Manager.Name)
	}
	if len(provenance.Manager.Version) == 0 ||
		len(provenance.Manager.Version) > maxPackageManagerVersionBytes ||
		!packageManagerVersionPattern.MatchString(provenance.Manager.Version) {
		return fmt.Errorf(
			"%s manager.version %q is not an admitted SemVer",
			prefix,
			provenance.Manager.Version,
		)
	}
	if !validProgramLockfile(provenance.Manager.Name, provenance.Submitted.LockfileName) {
		return fmt.Errorf(
			"%s submitted.lockfileName = %q is unsupported for %s",
			prefix,
			provenance.Submitted.LockfileName,
			provenance.Manager.Name,
		)
	}
	if !sha256DigestPattern.MatchString(provenance.Submitted.LockfileDigest) {
		return fmt.Errorf("%s submitted.lockfileDigest is not a lowercase SHA-256 digest", prefix)
	}
	if !sha256DigestPattern.MatchString(provenance.Submitted.SourceDigest) {
		return fmt.Errorf("%s submitted.sourceDigest is not a lowercase SHA-256 digest", prefix)
	}
	return nil
}

func validProgramLockfile(manager PackageManagerName, lockfile string) bool {
	switch manager {
	case PackageManagerNPM:
		return lockfile == "package-lock.json"
	case PackageManagerBun:
		return lockfile == "bun.lock" || lockfile == "bun.lockb"
	default:
		return false
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

func validArchitecture(architecture RuntimeArchitecture) bool {
	return architecture == ArchitectureAArch64 || architecture == ArchitectureX8664
}

func hasNodeModulesComponent(value string) bool {
	for _, item := range strings.Split(value, "/") {
		if item == "node_modules" {
			return true
		}
	}
	return false
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
