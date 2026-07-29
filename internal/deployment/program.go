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
	ProgramIndexFormatVersion  = 0
	programVerificationVersion = 0

	RuntimeAPIVersion                     = "helmr.runtime.v0"
	ConfigEvaluatorAPIVersion             = "helmr.config-evaluator.v0"
	ProgramBuildContractVersion           = "helmr.program-build.v0"
	ProgramArtifactMediaType              = "application/vnd.helmr.deployment-program.v0+squashfs"
	manifestDigestDomain                  = "helmr.deployment-definition-manifest.v0\x00"
	maxJSONSafeInteger              int64 = 9007199254740991
	maxProgramFileSizeBytes         int64 = 16777216
	maxProgramVerificationSizeBytes       = 17891328
	ArchitectureX8664                     = RuntimeArchitecture("x86_64")
	DeclarationKindTask                   = DeclarationKind("task")
	DeclarationKindActor                  = DeclarationKind("actor")
	DeclarationSlotHandler                = DeclarationSlot("handler")
	DeclarationSlotPayloadSchema          = DeclarationSlot("payloadSchema")
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

type ProgramLocator struct {
	ExportName string          `json:"exportName"`
	ModulePath string          `json:"modulePath"`
	Slot       DeclarationSlot `json:"slot"`
}

type ProgramIndexDeclaration struct {
	Kind       DefinitionKind `json:"-"`
	DeclaredID string         `json:"-"`
	Task       *TaskManifest
	Actor      *ActorManifest
	Workspace  *WorkspaceManifest
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

type ProgramManager struct {
	Digest  string             `json:"digest"`
	Name    PackageManagerName `json:"name"`
	Version string             `json:"version"`
}

type ProgramSubmittedSource struct {
	LockfileDigest string `json:"lockfileDigest"`
	LockfileName   string `json:"lockfileName"`
	SourceDigest   string `json:"sourceDigest"`
}

type BuildProvenance struct {
	Architecture         RuntimeArchitecture    `json:"architecture"`
	BuildContractVersion string                 `json:"buildContractVersion"`
	Config               ProgramConfig          `json:"config"`
	Manager              ProgramManager         `json:"manager"`
	RuntimeDigest        string                 `json:"runtimeDigest"`
	ToolchainDigest      string                 `json:"toolchainDigest"`
	Submitted            ProgramSubmittedSource `json:"submitted"`
}

type ProgramConfig struct {
	EvaluatorAPIVersion string `json:"evaluatorApiVersion"`
	SourceDigest        string `json:"sourceDigest"`
	ResultDigest        string `json:"resultDigest"`
}

type ProgramIndex struct {
	Architecture       RuntimeArchitecture       `json:"architecture"`
	ConfigResultDigest string                    `json:"configResultDigest"`
	Declarations       []ProgramIndexDeclaration `json:"declarations"`
	FormatVersion      int                       `json:"formatVersion"`
	Queues             []QueueInput              `json:"queues"`
	RuntimeAPIVersion  string                    `json:"runtimeApiVersion"`
}

// ProgramOutput is the build worker's verified Program publication result.
// Artifact IDs are assigned by Control.
type ProgramOutput struct {
	Artifact ProgramDescriptor `json:"artifact"`
	Index    ProgramIndex      `json:"index"`
}

type programVerification struct {
	FormatVersion int          `json:"formatVersion"`
	Index         ProgramIndex `json:"index"`
}

func validateProgramDescriptor(
	descriptor ProgramDescriptor,
	name string,
	mediaType string,
	maxSize int64,
) error {
	if !sha256DigestPattern.MatchString(descriptor.Digest) {
		return fmt.Errorf(
			"%s Artifact digest is not a lowercase SHA-256 digest",
			name,
		)
	}
	if descriptor.SizeBytes < 1 || descriptor.SizeBytes > maxSize {
		return fmt.Errorf(
			"%s Artifact sizeBytes is outside [1,%d]",
			name,
			maxSize,
		)
	}
	if descriptor.MediaType != mediaType {
		return fmt.Errorf(
			"%s Artifact mediaType = %q, want %q",
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
		maxProgramPhysicalBytes,
	); err != nil {
		return err
	}
	if err := ValidateProgramIndex(output.Index); err != nil {
		return fmt.Errorf("program output index: %w", err)
	}
	return nil
}

func parseProgramVerification(raw []byte) (programVerification, error) {
	if len(raw) == 0 || len(raw) > maxProgramVerificationSizeBytes {
		return programVerification{}, fmt.Errorf(
			"program verification size is outside [1,%d]",
			maxProgramVerificationSizeBytes,
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
	verified.Index = cloneProgramIndex(verified.Index)
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
	if len(canonical) > maxProgramVerificationSizeBytes {
		return nil, fmt.Errorf(
			"program verification size is outside [1,%d]",
			maxProgramVerificationSizeBytes,
		)
	}
	return canonical, nil
}

func validateProgramVerification(verified programVerification) error {
	if verified.FormatVersion != programVerificationVersion {
		return fmt.Errorf(
			"program verification formatVersion = %d, want %d",
			verified.FormatVersion,
			programVerificationVersion,
		)
	}
	if err := ValidateProgramIndex(verified.Index); err != nil {
		return fmt.Errorf("program verification index: %w", err)
	}
	return nil
}

func cloneProgramIndex(index ProgramIndex) ProgramIndex {
	index.Declarations = append([]ProgramIndexDeclaration(nil), index.Declarations...)
	for position := range index.Declarations {
		index.Declarations[position] = cloneProgramIndexDeclaration(
			index.Declarations[position],
		)
	}
	index.Queues = append([]QueueInput(nil), index.Queues...)
	for position := range index.Queues {
		if index.Queues[position].ConcurrencyLimit != nil {
			value := *index.Queues[position].ConcurrencyLimit
			index.Queues[position].ConcurrencyLimit = &value
		}
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
		if err := validateQueueInput(queue); err != nil {
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
		if err := validateProgramIndexDeclaration(declaration, queues); err != nil {
			return fmt.Errorf("program index declaration %d: %w", position, err)
		}
		if position > 0 &&
			compareProgramIndexDeclarations(index.Declarations[position-1], declaration) >= 0 {
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
	if err := validateProgramConfig(provenance.Config); err != nil {
		return fmt.Errorf("%s config: %w", prefix, err)
	}
	if !validArchitecture(provenance.Architecture) {
		return fmt.Errorf("%s architecture %q is unsupported", prefix, provenance.Architecture)
	}
	if !sha256DigestPattern.MatchString(provenance.ToolchainDigest) {
		return fmt.Errorf("%s toolchainDigest is not a lowercase SHA-256 digest", prefix)
	}
	if !sha256DigestPattern.MatchString(provenance.Manager.Digest) {
		return fmt.Errorf("%s manager.digest is not a lowercase SHA-256 digest", prefix)
	}
	if err := ValidatePackageManager(PackageManager{
		Name: provenance.Manager.Name, Version: provenance.Manager.Version,
	}); err != nil {
		return fmt.Errorf("%s manager: %w", prefix, err)
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

func validateProgramConfig(config ProgramConfig) error {
	if config.EvaluatorAPIVersion != ConfigEvaluatorAPIVersion ||
		!sha256DigestPattern.MatchString(config.SourceDigest) ||
		!sha256DigestPattern.MatchString(config.ResultDigest) {
		return errors.New("config provenance is invalid")
	}
	return nil
}

func validProgramLockfile(manager PackageManagerName, lockfile string) bool {
	switch manager {
	case PackageManagerNPM:
		return lockfile == "package-lock.json" || lockfile == "npm-shrinkwrap.json"
	case PackageManagerPNPM:
		return lockfile == "pnpm-lock.yaml"
	case PackageManagerBun:
		return lockfile == "bun.lock"
	default:
		return false
	}
}

func validProgramLockfileName(lockfile string) bool {
	return validProgramLockfile(PackageManagerNPM, lockfile) ||
		validProgramLockfile(PackageManagerPNPM, lockfile) ||
		validProgramLockfile(PackageManagerBun, lockfile)
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
	return architecture == ArchitectureX8664
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
