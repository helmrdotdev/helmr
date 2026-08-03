package imagebuild

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"path"
	"slices"
	"strings"
	"unicode/utf8"

	"github.com/helmrdotdev/helmr/internal/ids"
	"github.com/helmrdotdev/helmr/internal/jsoncanon"
	"github.com/helmrdotdev/helmr/internal/sha256sum"
)

const (
	ExecutionABI = "helmr.image-build.v0"
	LLBABI       = "helmr.image-llb.v0"
	CacheABI     = "helmr.image-cache.v0"

	RequestDocumentMaxBytes    = 16 << 20
	CredentialEnvelopeMaxBytes = 1 << 20
	MaxSourceArchiveBytes      = int64(11 << 30)
	MaxSourceArchiveEntries    = 100000
	MaxOCIArchiveBytes         = int64(16 << 30)
	MaxRegistryPasswordBytes   = 64 << 10
	MaxGuestRequestBodyBytes   = uint64(RequestDocumentMaxBytes+CredentialEnvelopeMaxBytes+8) + uint64(MaxSourceArchiveBytes)
)

type CacheMode string

const (
	CachePrefer CacheMode = "prefer"
	CacheBypass CacheMode = "bypass"
)

type GuestRequest struct {
	ExecutionABI string `json:"executionAbi"`
	LLBABI       string `json:"llbAbi"`
	CacheABI     string `json:"cacheAbi"`

	OperationID          string `json:"operationId"`
	AttemptID            string `json:"attemptId"`
	BuildLeaseID         string `json:"buildLeaseId"`
	BuildLeaseGeneration int64  `json:"buildLeaseGeneration"`
	WorkerEpoch          int64  `json:"workerEpoch"`
	RuntimeIdentityID    string `json:"runtimeIdentityId"`

	Architecture           string            `json:"architecture"`
	Plan                   Build             `json:"plan"`
	PlanDigest             string            `json:"planDigest"`
	SubmittedSourceDigest  string            `json:"submittedSourceDigest"`
	BuildTreeDigest        string            `json:"buildTreeDigest"`
	AdmittedPaths          []SourcePath      `json:"admittedPaths"`
	AdmittedPathSetDigest  string            `json:"admittedPathSetDigest"`
	SourceArchiveDigest    string            `json:"sourceArchiveDigest"`
	SourceArchiveSizeBytes int64             `json:"sourceArchiveSizeBytes"`
	SourceArchiveEntries   int               `json:"sourceArchiveEntries"`
	ResolutionSetDigest    string            `json:"resolutionSetDigest"`
	RegistryBindings       []RegistryBinding `json:"registryBindings"`
	RequestedCacheMode     CacheMode         `json:"requestedCacheMode"`
	CacheBinding           *CacheBinding     `json:"cacheBinding"`
}

// SourceAdmission is the exact non-secret Worker measurement submitted to
// Control Plane before registry Secret resolution and physical-attempt allocation.
type SourceAdmission struct {
	Architecture           string
	Plan                   Build
	PlanDigest             string
	SubmittedSourceDigest  string
	BuildTreeDigest        string
	AdmittedPaths          []SourcePath
	AdmittedPathSetDigest  string
	SourceArchiveDigest    string
	SourceArchiveSizeBytes int64
	SourceArchiveEntries   int
}

type RegistryBinding struct {
	Authority            string `json:"authority"`
	Username             string `json:"username"`
	ResolutionID         string `json:"resolutionId"`
	SecretID             string `json:"secretId"`
	SecretVersionID      string `json:"secretVersionId"`
	RevocationGeneration int64  `json:"revocationGeneration"`
}

type CacheBinding struct {
	Authority string `json:"authority"`
	Username  string `json:"username"`
	Ref       string `json:"ref"`
}

type SourcePath struct {
	Path string         `json:"path"`
	Kind SourcePathKind `json:"kind"`
}

type SourcePathKind string

const (
	SourcePathFile      SourcePathKind = "file"
	SourcePathDirectory SourcePathKind = "directory"
	SourcePathSymlink   SourcePathKind = "symlink"
)

type CredentialEnvelope struct {
	OperationID         string                    `json:"operationId"`
	AttemptID           string                    `json:"attemptId"`
	ResolutionSetDigest string                    `json:"resolutionSetDigest"`
	RegistryCredentials []RegistryCredentialValue `json:"registryCredentials"`
}

type RegistryCredentialValue struct {
	Authority string `json:"authority"`
	Username  string `json:"username"`
	Password  []byte `json:"password"`
}

type GuestOutcome string

const (
	GuestSucceeded GuestOutcome = "succeeded"
	GuestFailed    GuestOutcome = "failed"
)

type GuestFailureReason string

const (
	GuestFailureImage        GuestFailureReason = "image_failed"
	GuestFailureOutputQuota  GuestFailureReason = "output_quota_exceeded"
	GuestFailureNetworkQuota GuestFailureReason = "network_quota_exceeded"
)

type GuestResult struct {
	ExecutionABI  string             `json:"executionAbi"`
	Outcome       GuestOutcome       `json:"outcome"`
	FailureReason GuestFailureReason `json:"failureReason,omitempty"`
	OCIDigest     string             `json:"ociDigest,omitempty"`
	OCISizeBytes  int64              `json:"ociSizeBytes,omitempty"`
	Error         string             `json:"error,omitempty"`
}

func CanonicalGuestRequest(request GuestRequest) ([]byte, error) {
	return canonicalDocument(request, ValidateGuestRequest, RequestDocumentMaxBytes)
}

func ParseGuestRequest(raw []byte) (GuestRequest, error) {
	var request GuestRequest
	if err := parseDocument(raw, &request, ValidateGuestRequest, RequestDocumentMaxBytes); err != nil {
		return GuestRequest{}, err
	}
	return request, nil
}

func CanonicalCredentialEnvelope(envelope CredentialEnvelope) ([]byte, error) {
	return canonicalDocument(envelope, ValidateCredentialEnvelope, CredentialEnvelopeMaxBytes)
}

func ParseCredentialEnvelope(raw []byte) (CredentialEnvelope, error) {
	var envelope CredentialEnvelope
	if err := parseCredentialEnvelope(raw, &envelope); err != nil {
		return CredentialEnvelope{}, err
	}
	return envelope, nil
}

func parseCredentialEnvelope(raw []byte, envelope *CredentialEnvelope) error {
	if err := parseDocument(raw, envelope, ValidateCredentialEnvelope, CredentialEnvelopeMaxBytes); err != nil {
		for index := range envelope.RegistryCredentials {
			clear(envelope.RegistryCredentials[index].Password)
		}
		return err
	}
	return nil
}

func CanonicalGuestResult(result GuestResult) ([]byte, error) {
	return canonicalDocument(result, validateGuestWireResult, RequestDocumentMaxBytes)
}

func ParseGuestResult(raw []byte) (GuestResult, error) {
	var result GuestResult
	if err := parseDocument(raw, &result, validateGuestWireResult, RequestDocumentMaxBytes); err != nil {
		return GuestResult{}, err
	}
	return result, nil
}

func ValidateGuestRequest(request GuestRequest) error {
	if request.ExecutionABI != ExecutionABI || request.LLBABI != LLBABI || request.CacheABI != CacheABI {
		return errors.New("image-build ABI does not match the guest")
	}
	for _, identity := range []struct {
		label string
		value string
	}{
		{label: "operation ID", value: request.OperationID},
		{label: "attempt ID", value: request.AttemptID},
		{label: "build lease ID", value: request.BuildLeaseID},
	} {
		if err := ids.Validate(identity.value); err != nil {
			return fmt.Errorf("image-build %s is invalid", identity.label)
		}
	}
	if !sha256sum.ValidDigest(request.RuntimeIdentityID) {
		return errors.New("image-build runtime identity ID is invalid")
	}
	if request.BuildLeaseGeneration < 1 || request.WorkerEpoch < 1 {
		return errors.New("image-build lease generation or worker epoch is invalid")
	}
	if err := ValidateSourceAdmission(SourceAdmission{
		Architecture: request.Architecture, Plan: request.Plan, PlanDigest: request.PlanDigest,
		SubmittedSourceDigest: request.SubmittedSourceDigest, BuildTreeDigest: request.BuildTreeDigest,
		AdmittedPaths: request.AdmittedPaths, AdmittedPathSetDigest: request.AdmittedPathSetDigest,
		SourceArchiveDigest:    request.SourceArchiveDigest,
		SourceArchiveSizeBytes: request.SourceArchiveSizeBytes,
		SourceArchiveEntries:   request.SourceArchiveEntries,
	}); err != nil {
		return err
	}
	if request.RequestedCacheMode != CachePrefer && request.RequestedCacheMode != CacheBypass {
		return errors.New("image-build requested cache mode is invalid")
	}
	if err := validateRegistryBindings(request.RegistryBindings); err != nil {
		return err
	}
	if err := matchPlanRegistryBindings(request.Plan, request.Architecture, request.RegistryBindings); err != nil {
		return err
	}
	if ResolutionSetDigest(request.RegistryBindings) != request.ResolutionSetDigest {
		return errors.New("image-build resolution-set digest does not match its bindings")
	}
	if err := validateCacheBinding(request.RequestedCacheMode, request.CacheBinding, request.RegistryBindings); err != nil {
		return err
	}
	return nil
}

func ValidateSourceAdmission(admission SourceAdmission) error {
	if err := Validate(admission.Plan, admission.Architecture); err != nil {
		return err
	}
	planDigest, err := Digest(admission.Plan, admission.Architecture)
	if err != nil {
		return err
	}
	if planDigest != admission.PlanDigest {
		return errors.New("image-build plan digest does not match the canonical plan")
	}
	for _, descriptor := range []struct {
		label  string
		digest string
	}{
		{label: "submitted source", digest: admission.SubmittedSourceDigest},
		{label: "build tree", digest: admission.BuildTreeDigest},
		{label: "admitted path set", digest: admission.AdmittedPathSetDigest},
		{label: "source archive", digest: admission.SourceArchiveDigest},
	} {
		if !sha256sum.ValidDigest(descriptor.digest) {
			return fmt.Errorf("image-build %s digest is invalid", descriptor.label)
		}
	}
	if admission.AdmittedPaths == nil || !slices.IsSortedFunc(admission.AdmittedPaths, func(left, right SourcePath) int {
		return strings.Compare(left.Path, right.Path)
	}) {
		return errors.New("image-build admitted paths must be a sorted array")
	}
	for index, sourcePath := range admission.AdmittedPaths {
		if !validSourcePath(sourcePath) {
			return fmt.Errorf("image-build admitted path %d is invalid", index)
		}
		if index > 0 && admission.AdmittedPaths[index-1].Path == sourcePath.Path {
			return errors.New("image-build admitted paths must be unique")
		}
	}
	if err := validateAdmittedPaths(admission.Plan, admission.AdmittedPaths); err != nil {
		return err
	}
	if PathSetDigest(admission.AdmittedPaths) != admission.AdmittedPathSetDigest {
		return errors.New("image-build admitted path-set digest does not match its paths")
	}
	if admission.SourceArchiveSizeBytes < 1 || admission.SourceArchiveSizeBytes > MaxSourceArchiveBytes ||
		admission.SourceArchiveEntries < 0 || admission.SourceArchiveEntries > MaxSourceArchiveEntries ||
		admission.SourceArchiveEntries != len(admission.AdmittedPaths) {
		return errors.New("image-build source archive descriptor is invalid")
	}
	return nil
}

func ValidateCredentialEnvelope(envelope CredentialEnvelope) error {
	if err := ids.Validate(envelope.OperationID); err != nil {
		return errors.New("image-build credential operation ID is invalid")
	}
	if err := ids.Validate(envelope.AttemptID); err != nil {
		return errors.New("image-build credential attempt ID is invalid")
	}
	if !sha256sum.ValidDigest(envelope.ResolutionSetDigest) {
		return errors.New("image-build credential resolution-set digest is invalid")
	}
	if envelope.RegistryCredentials == nil || len(envelope.RegistryCredentials) > maxRegistryAuthorities+1 {
		return errors.New("image-build registry credentials must be a bounded array")
	}
	for index, credential := range envelope.RegistryCredentials {
		authority, err := CanonicalRegistryAuthority(credential.Authority)
		if err != nil || authority != credential.Authority {
			return fmt.Errorf("image-build registry credential %d authority is invalid", index)
		}
		if !validImageString(credential.Username, maxRegistryUsernameBytes) || strings.TrimSpace(credential.Username) != credential.Username {
			return fmt.Errorf("image-build registry credential %d username is invalid", index)
		}
		if len(credential.Password) < 1 || len(credential.Password) > MaxRegistryPasswordBytes {
			return fmt.Errorf("image-build registry credential %d password size is invalid", index)
		}
		if index > 0 && envelope.RegistryCredentials[index-1].Authority >= credential.Authority {
			return errors.New("image-build registry credentials must be unique and sorted by authority")
		}
	}
	return nil
}

func MatchCredentialEnvelope(request GuestRequest, envelope CredentialEnvelope) error {
	if request.OperationID != envelope.OperationID || request.AttemptID != envelope.AttemptID ||
		request.ResolutionSetDigest != envelope.ResolutionSetDigest {
		return errors.New("image-build credential envelope does not match the request")
	}
	expected := credentialBindings(request)
	if len(expected) != len(envelope.RegistryCredentials) {
		return errors.New("image-build credential envelope is incomplete")
	}
	for index, binding := range expected {
		credential := envelope.RegistryCredentials[index]
		if binding.Authority != credential.Authority || binding.Username != credential.Username {
			return errors.New("image-build credential envelope authority set does not match the request")
		}
	}
	return nil
}

type credentialBinding struct {
	Authority string
	Username  string
}

func credentialBindings(request GuestRequest) []credentialBinding {
	bindings := make([]credentialBinding, 0, len(request.RegistryBindings)+1)
	for _, binding := range request.RegistryBindings {
		bindings = append(bindings, credentialBinding{
			Authority: binding.Authority,
			Username:  binding.Username,
		})
	}
	if request.CacheBinding != nil {
		bindings = append(bindings, credentialBinding{
			Authority: request.CacheBinding.Authority,
			Username:  request.CacheBinding.Username,
		})
	}
	slices.SortFunc(bindings, func(left, right credentialBinding) int {
		return strings.Compare(left.Authority, right.Authority)
	})
	return bindings
}

func ValidateGuestResult(result GuestResult) error {
	if result.ExecutionABI != ExecutionABI {
		return errors.New("image-build result ABI does not match")
	}
	switch result.Outcome {
	case GuestSucceeded:
		if result.FailureReason != "" || result.Error != "" || !sha256sum.ValidDigest(result.OCIDigest) || result.OCISizeBytes < 1 || result.OCISizeBytes > MaxOCIArchiveBytes {
			return errors.New("successful image-build result is incomplete")
		}
	case GuestFailed:
		if result.FailureReason != GuestFailureImage &&
			result.FailureReason != GuestFailureOutputQuota &&
			result.FailureReason != GuestFailureNetworkQuota ||
			result.OCIDigest != "" || result.OCISizeBytes != 0 || result.Error == "" || len(result.Error) > 4096 || !utf8.ValidString(result.Error) {
			return errors.New("failed image-build result is incomplete")
		}
	default:
		return errors.New("image-build result outcome is invalid")
	}
	return nil
}

func validateGuestWireResult(result GuestResult) error {
	if err := ValidateGuestResult(result); err != nil {
		return err
	}
	if result.FailureReason == GuestFailureNetworkQuota {
		return errors.New("image-build network quota is host-authoritative")
	}
	return nil
}

func validateRegistryBindings(bindings []RegistryBinding) error {
	if bindings == nil || len(bindings) > maxRegistryAuthorities {
		return errors.New("image-build registry bindings must be a bounded array")
	}
	for index, binding := range bindings {
		authority, err := CanonicalRegistryAuthority(binding.Authority)
		if err != nil || authority != binding.Authority {
			return fmt.Errorf("image-build registry binding %d authority is invalid", index)
		}
		if !validImageString(binding.Username, maxRegistryUsernameBytes) || strings.TrimSpace(binding.Username) != binding.Username {
			return fmt.Errorf("image-build registry binding %d username is invalid", index)
		}
		if err := ids.Validate(binding.ResolutionID); err != nil {
			return fmt.Errorf("image-build registry binding %d resolution ID is invalid", index)
		}
		if err := ids.Validate(binding.SecretID); err != nil {
			return fmt.Errorf("image-build registry binding %d secret ID is invalid", index)
		}
		if err := ids.Validate(binding.SecretVersionID); err != nil {
			return fmt.Errorf("image-build registry binding %d secret version ID is invalid", index)
		}
		if binding.RevocationGeneration < 0 {
			return fmt.Errorf("image-build registry binding %d revocation generation is invalid", index)
		}
		if index > 0 && bindings[index-1].Authority >= binding.Authority {
			return errors.New("image-build registry bindings must be unique and sorted by authority")
		}
	}
	return nil
}

func matchPlanRegistryBindings(
	plan Build,
	architecture string,
	bindings []RegistryBinding,
) error {
	expected, err := RegistryCredentials(plan, architecture)
	if err != nil {
		return err
	}
	if len(expected) != len(bindings) {
		return errors.New("image-build registry bindings do not match the plan")
	}
	for index := range expected {
		if expected[index].Authority != bindings[index].Authority ||
			expected[index].Username != bindings[index].Username {
			return errors.New("image-build registry bindings do not match the plan")
		}
	}
	return nil
}

func validateCacheBinding(
	mode CacheMode,
	binding *CacheBinding,
	registryBindings []RegistryBinding,
) error {
	if binding == nil {
		return nil
	}
	if mode != CachePrefer {
		return errors.New("image-build bypass request contains a cache binding")
	}
	authority, err := CanonicalRegistryAuthority(binding.Authority)
	if err != nil || authority != binding.Authority {
		return errors.New("image-build cache authority is invalid")
	}
	if binding.Username == "" || !validImageString(binding.Username, maxRegistryUsernameBytes) ||
		strings.TrimSpace(binding.Username) != binding.Username {
		return errors.New("image-build cache username is invalid")
	}
	if err := ValidateCacheReference(binding.Ref); err != nil {
		return err
	}
	refAuthority, err := RegistryAuthority(binding.Ref)
	if err != nil || refAuthority != binding.Authority {
		return errors.New("image-build cache ref does not match its authority")
	}
	for _, userBinding := range registryBindings {
		if userBinding.Authority == binding.Authority {
			return errors.New("image-build cache authority collides with a user registry binding")
		}
	}
	return nil
}

func validSourcePath(sourcePath SourcePath) bool {
	if sourcePath.Path == "" || !utf8.ValidString(sourcePath.Path) ||
		strings.TrimSpace(sourcePath.Path) != sourcePath.Path ||
		path.IsAbs(sourcePath.Path) || path.Clean(sourcePath.Path) != sourcePath.Path ||
		sourcePath.Path == "." || sourcePath.Path == ".." ||
		strings.HasPrefix(sourcePath.Path, "../") {
		return false
	}
	switch sourcePath.Kind {
	case SourcePathFile, SourcePathDirectory, SourcePathSymlink:
		return true
	default:
		return false
	}
}

func validateAdmittedPaths(plan Build, admitted []SourcePath) error {
	files := make(map[string]struct{})
	directories := make(map[string]struct{})
	for _, image := range plan.Images {
		for _, step := range image.Steps {
			switch {
			case step.CopySourceFile != nil:
				files[step.CopySourceFile.Path] = struct{}{}
			case step.CopySourceDir != nil:
				directories[step.CopySourceDir.Path] = struct{}{}
			}
		}
	}
	seen := make(map[string]SourcePathKind, len(admitted))
	for _, entry := range admitted {
		if entry.Path == "helmr" || strings.HasPrefix(entry.Path, "helmr/") {
			return errors.New("image-build admitted paths contain platform compiler output")
		}
		seen[entry.Path] = entry.Kind
		allowed := false
		if _, ok := files[entry.Path]; ok {
			allowed = entry.Kind == SourcePathFile
		}
		for root := range directories {
			if root == "." || entry.Path == root || strings.HasPrefix(entry.Path, root+"/") {
				allowed = true
				break
			}
		}
		if !allowed && entry.Kind == SourcePathDirectory {
			for root := range files {
				if strings.HasPrefix(root, entry.Path+"/") {
					allowed = true
					break
				}
			}
			if !allowed {
				for root := range directories {
					if root != "." && strings.HasPrefix(root, entry.Path+"/") {
						allowed = true
						break
					}
				}
			}
		}
		if !allowed {
			return fmt.Errorf("image-build admitted path %q is outside the plan source roots", entry.Path)
		}
	}
	for root := range files {
		if seen[root] != SourcePathFile {
			return fmt.Errorf("image-build admitted file root %q is missing", root)
		}
	}
	for root := range directories {
		if root != "." && seen[root] != SourcePathDirectory {
			return fmt.Errorf("image-build admitted directory root %q is missing", root)
		}
	}
	return nil
}

func canonicalDocument[T any](value T, validate func(T) error, maxBytes int) ([]byte, error) {
	if err := validate(value); err != nil {
		return nil, err
	}
	raw, err := json.Marshal(value)
	if err != nil {
		return nil, err
	}
	canonical, err := jsoncanon.Transform(raw)
	if err != nil {
		return nil, err
	}
	if len(canonical) == 0 || len(canonical) > maxBytes {
		return nil, errors.New("image-build protocol document size is invalid")
	}
	return canonical, nil
}

func parseDocument[T any](raw []byte, value *T, validate func(T) error, maxBytes int) error {
	if len(raw) == 0 || len(raw) > maxBytes {
		return errors.New("image-build protocol document size is invalid")
	}
	canonical, err := jsoncanon.Transform(raw)
	if err != nil {
		return err
	}
	if !bytes.Equal(raw, canonical) {
		return errors.New("image-build protocol document is not canonical JSON")
	}
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(value); err != nil {
		return err
	}
	if err := decoder.Decode(new(any)); !errors.Is(err, io.EOF) {
		if err == nil {
			return errors.New("image-build protocol document contains trailing JSON")
		}
		return err
	}
	if err := validate(*value); err != nil {
		return err
	}
	complete, err := canonicalDocument(*value, validate, maxBytes)
	if err != nil {
		return err
	}
	if !bytes.Equal(raw, complete) {
		return errors.New("image-build protocol document does not match its complete v0 shape")
	}
	return nil
}

func PathSetDigest(paths []SourcePath) string {
	raw, err := json.Marshal(paths)
	if err != nil {
		panic(err)
	}
	return sha256sum.DigestBytes(raw)
}

func ResolutionSetDigest(bindings []RegistryBinding) string {
	raw, err := json.Marshal(bindings)
	if err != nil {
		panic(err)
	}
	return sha256sum.DigestBytes(raw)
}
