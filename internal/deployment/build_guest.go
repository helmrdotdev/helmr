package deployment

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"

	"github.com/helmrdotdev/helmr/internal/archive"
	"github.com/helmrdotdev/helmr/internal/jsoncanon"
)

const (
	BuildGuestFormatVersion = 0

	BuildGuestSucceeded = BuildGuestOutcome("succeeded")
	BuildGuestFailed    = BuildGuestOutcome("failed")

	MaxBuildGuestResultBytes = 80 << 20
)

type BuildGuestOutcome string

type BuildGuestRequest struct {
	FormatVersion   int            `json:"formatVersion"`
	Manager         BuildManager   `json:"manager"`
	Runtime         BuildRuntime   `json:"runtime"`
	Toolchain       BuildToolchain `json:"toolchain"`
	LockfileName    string         `json:"lockfileName"`
	SourceDigest    string         `json:"sourceDigest"`
	SourceSizeBytes int64          `json:"sourceSizeBytes"`
}

type BuildGuestResult struct {
	FormatVersion int                 `json:"formatVersion"`
	Outcome       BuildGuestOutcome   `json:"outcome"`
	TreeDigest    string              `json:"treeDigest,omitempty"`
	TreeSizeBytes int64               `json:"treeSizeBytes,omitempty"`
	Config        *BuildConfig        `json:"config,omitempty"`
	Verification  *VerificationResult `json:"verification,omitempty"`
	Error         *BuildError         `json:"error,omitempty"`
	Logs          *BuildLogs          `json:"logs,omitempty"`
}

type BuildManager struct {
	Artifact       ArtifactDescriptor `json:"artifact"`
	Entrypoint     ManagerEntrypoint  `json:"entrypoint"`
	PackageManager PackageManager     `json:"packageManager"`
}

type BuildRuntime struct {
	Artifact    ArtifactDescriptor `json:"artifact"`
	NodeVersion string             `json:"nodeVersion"`
}

type BuildToolchain struct {
	Artifact      ArtifactDescriptor `json:"artifact"`
	RuntimeDigest string             `json:"runtimeDigest"`
}

func ParseBuildGuestRequest(raw []byte) (BuildGuestRequest, error) {
	var request BuildGuestRequest
	if err := parseBuildGuestDocument(raw, &request, validateBuildGuestRequest); err != nil {
		return BuildGuestRequest{}, err
	}
	return request, nil
}

func ParseBuildGuestResult(raw []byte) (BuildGuestResult, error) {
	var result BuildGuestResult
	if err := parseBuildGuestDocument(raw, &result, validateBuildGuestResult); err != nil {
		return BuildGuestResult{}, err
	}
	return result, nil
}

func CanonicalBuildGuestRequest(request BuildGuestRequest) ([]byte, error) {
	return canonicalBuildGuestDocument(request, validateBuildGuestRequest)
}

func CanonicalBuildGuestResult(result BuildGuestResult) ([]byte, error) {
	return canonicalBuildGuestDocument(result, validateBuildGuestResult)
}

func validateBuildGuestRequest(request BuildGuestRequest) error {
	if request.FormatVersion != BuildGuestFormatVersion {
		return fmt.Errorf(
			"build formatVersion = %d, want %d",
			request.FormatVersion,
			BuildGuestFormatVersion,
		)
	}
	if err := validateBuildManager(request.Manager); err != nil {
		return err
	}
	if err := validateBuildRuntime(request.Runtime); err != nil {
		return err
	}
	if err := validateBuildToolchain(request.Toolchain); err != nil {
		return err
	}
	if request.Toolchain.RuntimeDigest != request.Runtime.Artifact.Digest {
		return errors.New("build toolchain does not match Runtime")
	}
	switch request.Manager.PackageManager.Name {
	case PackageManagerNPM:
		if request.LockfileName != "package-lock.json" &&
			request.LockfileName != "npm-shrinkwrap.json" {
			return errors.New("npm build lockfile name is invalid")
		}
	case PackageManagerPNPM:
		if request.LockfileName != "pnpm-lock.yaml" {
			return errors.New("pnpm build lockfile name is invalid")
		}
	case PackageManagerBun:
		if request.LockfileName != "bun.lock" {
			return errors.New("Bun build lockfile name is invalid")
		}
	}
	if !sha256DigestPattern.MatchString(request.SourceDigest) {
		return errors.New("build source digest is invalid")
	}
	if request.SourceSizeBytes < 1 ||
		request.SourceSizeBytes > archive.MaxSourceArtifactBytes {
		return errors.New("build source size is invalid")
	}
	return nil
}

func validateBuildGuestResult(result BuildGuestResult) error {
	if result.FormatVersion != BuildGuestFormatVersion {
		return fmt.Errorf(
			"build result formatVersion = %d, want %d",
			result.FormatVersion,
			BuildGuestFormatVersion,
		)
	}
	switch result.Outcome {
	case BuildGuestSucceeded:
		if result.Error != nil {
			return errors.New("successful build result forbids error")
		}
		if !sha256DigestPattern.MatchString(result.TreeDigest) ||
			result.TreeSizeBytes < 1 ||
			result.TreeSizeBytes > maxBuildTreeStreamBytes {
			return errors.New("successful build tree descriptor is invalid")
		}
		if result.Verification == nil {
			return errors.New("successful build result requires verification")
		}
		if result.Config == nil {
			return errors.New("successful build result requires config")
		}
		if err := ValidateBuildConfig(*result.Config); err != nil {
			return err
		}
		if err := ValidateVerificationResult(*result.Verification); err != nil {
			return err
		}
		if result.Logs == nil {
			return errors.New("successful build result requires logs")
		}
	case BuildGuestFailed:
		if result.TreeDigest != "" ||
			result.TreeSizeBytes != 0 ||
			result.Config != nil ||
			result.Verification != nil {
			return errors.New("failed build result forbids success data")
		}
		if result.Error == nil {
			return errors.New("failed build result requires error")
		}
		if err := validateBuildFailed(BuildFailed{Error: *result.Error}); err != nil {
			return err
		}
	default:
		return fmt.Errorf("build result outcome %q is unsupported", result.Outcome)
	}
	if result.Logs != nil {
		return validateBuildLogs(*result.Logs)
	}
	return nil
}

func validateBuildManager(manager BuildManager) error {
	if err := validatePackageManagerSyntax(manager.PackageManager); err != nil {
		return err
	}
	expectedKind, expectedPath, _, err := managerDistribution(manager.PackageManager)
	if err != nil {
		return err
	}
	if manager.Entrypoint.Kind != expectedKind ||
		manager.Entrypoint.Path != expectedPath {
		return errors.New("build Manager entrypoint does not match its family")
	}
	return validateInputArtifact(
		manager.Artifact,
		ManagerTreeMediaType,
		maxManagerTreeBytes,
		"build Manager",
	)
}

func validateBuildRuntime(runtime BuildRuntime) error {
	if _, _, _, ok := parseReleaseVersion(runtime.NodeVersion); !ok {
		return errors.New("build Runtime Node version is invalid")
	}
	return validateInputArtifact(
		runtime.Artifact,
		RuntimeArtifactMediaType,
		maxRuntimePhysicalBytes,
		"build Runtime",
	)
}

func validateBuildToolchain(toolchain BuildToolchain) error {
	if !sha256DigestPattern.MatchString(toolchain.RuntimeDigest) {
		return errors.New("build toolchain Runtime digest is invalid")
	}
	return validateInputArtifact(
		toolchain.Artifact,
		ToolchainMediaType,
		maxToolArtifactBytes,
		"build toolchain",
	)
}

func canonicalBuildGuestDocument[T any](
	value T,
	validate func(T) error,
) ([]byte, error) {
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
	if len(canonical) == 0 || len(canonical) > MaxBuildGuestResultBytes {
		return nil, errors.New("build guest document size is invalid")
	}
	return canonical, nil
}

func parseBuildGuestDocument[T any](
	raw []byte,
	value *T,
	validate func(T) error,
) error {
	if len(raw) == 0 || len(raw) > MaxBuildGuestResultBytes {
		return errors.New("build guest document size is invalid")
	}
	canonical, err := jsoncanon.Transform(raw)
	if err != nil {
		return err
	}
	if !bytes.Equal(raw, canonical) {
		return errors.New("build guest document is not canonical JSON")
	}
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(value); err != nil {
		return err
	}
	if err := ensureEOF(decoder, "build guest document"); err != nil {
		return err
	}
	if err := validate(*value); err != nil {
		return err
	}
	complete, err := canonicalBuildGuestDocument(*value, validate)
	if err != nil {
		return err
	}
	if !bytes.Equal(raw, complete) {
		return errors.New("build guest document does not match its complete v0 shape")
	}
	return nil
}
