package deployment

import (
	"encoding/hex"
	"encoding/json"
	"fmt"
	"strings"
	"unicode"
	"unicode/utf8"

	"github.com/google/uuid"
	"github.com/helmrdotdev/helmr/internal/api"
	"github.com/helmrdotdev/helmr/internal/jsoncanon"
	"github.com/helmrdotdev/helmr/internal/region"
)

const (
	ReuseFormatVersion    = 0
	reuseKeyDomain        = "helmr.deployment-build-reuse.v0\x00"
	reuseWorkerProtocolV0 = "helmr.worker.v0"
)

type ReuseDescriptor struct {
	FormatVersion           int                 `json:"formatVersion"`
	OrgID                   string              `json:"orgId"`
	ProjectID               string              `json:"projectId"`
	EnvironmentID           string              `json:"environmentId"`
	BuildRegionID           string              `json:"buildRegionId"`
	ContentHash             string              `json:"contentHash"`
	APIVersion              string              `json:"apiVersion"`
	SDKVersion              string              `json:"sdkVersion"`
	CLIVersion              string              `json:"cliVersion"`
	BundleFormatVersion     int32               `json:"bundleFormatVersion"`
	WorkerProtocolVersion   string              `json:"workerProtocolVersion"`
	Architecture            RuntimeArchitecture `json:"architecture"`
	BuildContractVersion    string              `json:"buildContractVersion"`
	RuntimeDigest           string              `json:"runtimeDigest"`
	StandardToolchainDigest string              `json:"standardToolchainDigest"`
}

func CanonicalReuseDescriptor(descriptor ReuseDescriptor) ([]byte, error) {
	if err := ValidateReuseDescriptor(descriptor); err != nil {
		return nil, err
	}
	raw, err := json.Marshal(descriptor)
	if err != nil {
		return nil, fmt.Errorf("encode deployment reuse descriptor: %w", err)
	}
	canonical, err := jsoncanon.Transform(raw)
	if err != nil {
		return nil, fmt.Errorf("canonicalize deployment reuse descriptor: %w", err)
	}
	return canonical, nil
}

func ReuseKey(descriptor ReuseDescriptor) (string, error) {
	canonical, err := CanonicalReuseDescriptor(descriptor)
	if err != nil {
		return "", err
	}
	digest := domainDigest(reuseKeyDomain, canonical)
	return "sha256:" + hex.EncodeToString(digest[:]), nil
}

func ValidateReuseDescriptor(descriptor ReuseDescriptor) error {
	if descriptor.FormatVersion != ReuseFormatVersion {
		return fmt.Errorf(
			"deployment reuse descriptor formatVersion = %d, want %d",
			descriptor.FormatVersion,
			ReuseFormatVersion,
		)
	}
	for name, value := range map[string]string{
		"orgId":         descriptor.OrgID,
		"projectId":     descriptor.ProjectID,
		"environmentId": descriptor.EnvironmentID,
	} {
		parsed, err := uuid.Parse(value)
		if err != nil || parsed == uuid.Nil || parsed.String() != value {
			return fmt.Errorf("deployment reuse descriptor %s is not a canonical UUID", name)
		}
	}
	if err := region.ValidateID(descriptor.BuildRegionID); err != nil {
		return fmt.Errorf("deployment reuse descriptor buildRegionId: %w", err)
	}
	if !sha256DigestPattern.MatchString(descriptor.ContentHash) {
		return fmt.Errorf("deployment reuse descriptor contentHash is not a lowercase SHA-256 digest")
	}
	if err := validateReuseText("apiVersion", descriptor.APIVersion); err != nil {
		return err
	}
	if len(descriptor.APIVersion) > api.MaxClientVersionBytes {
		return fmt.Errorf("deployment reuse descriptor apiVersion exceeds %d bytes", api.MaxClientVersionBytes)
	}
	if err := api.ValidateClientVersion(descriptor.SDKVersion); err != nil {
		return fmt.Errorf("deployment reuse descriptor sdkVersion: %w", err)
	}
	if err := api.ValidateClientVersion(descriptor.CLIVersion); err != nil {
		return fmt.Errorf("deployment reuse descriptor cliVersion: %w", err)
	}
	if descriptor.BundleFormatVersion < 1 {
		return fmt.Errorf("deployment reuse descriptor bundleFormatVersion must be positive")
	}
	if descriptor.WorkerProtocolVersion != reuseWorkerProtocolV0 {
		return fmt.Errorf(
			"deployment reuse descriptor workerProtocolVersion = %q, want %q",
			descriptor.WorkerProtocolVersion,
			reuseWorkerProtocolV0,
		)
	}
	if !validArchitecture(descriptor.Architecture) {
		return fmt.Errorf(
			"deployment reuse descriptor architecture %q is unsupported",
			descriptor.Architecture,
		)
	}
	if !sha256DigestPattern.MatchString(descriptor.RuntimeDigest) {
		return fmt.Errorf("deployment reuse descriptor runtimeDigest is not a lowercase SHA-256 digest")
	}
	if !sha256DigestPattern.MatchString(descriptor.StandardToolchainDigest) {
		return fmt.Errorf("deployment reuse descriptor standardToolchainDigest is not a lowercase SHA-256 digest")
	}
	if descriptor.BuildContractVersion != ProgramBuildContractVersion {
		return fmt.Errorf(
			"deployment reuse descriptor buildContractVersion = %q, want %q",
			descriptor.BuildContractVersion,
			ProgramBuildContractVersion,
		)
	}
	return nil
}

func validateReuseText(name, value string) error {
	if !utf8.ValidString(value) ||
		strings.TrimSpace(value) != value ||
		value == "" {
		return fmt.Errorf("deployment reuse descriptor %s is not normalized", name)
	}
	for _, character := range value {
		if unicode.IsControl(character) {
			return fmt.Errorf("deployment reuse descriptor %s contains a control character", name)
		}
	}
	return nil
}
