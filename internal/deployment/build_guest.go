package deployment

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"time"

	"github.com/helmrdotdev/helmr/internal/archive"
	"github.com/helmrdotdev/helmr/internal/compute"
	"github.com/helmrdotdev/helmr/internal/frameio"
	"github.com/helmrdotdev/helmr/internal/jsoncanon"
	"github.com/helmrdotdev/helmr/internal/vm"
	"github.com/helmrdotdev/helmr/internal/wire"
)

const (
	BuildGuestFormatVersion = 0

	BuildGuestSucceeded = BuildGuestOutcome("succeeded")
	BuildGuestFailed    = BuildGuestOutcome("failed")

	maxBuildGuestResultBytes  = 80 << 20
	buildGuestCloseTimeout    = 30 * time.Second
	buildNetworkCutoffTimeout = 10 * time.Second
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

type BuildFetchResult struct {
	FormatVersion int               `json:"formatVersion"`
	Outcome       BuildGuestOutcome `json:"outcome"`
	Error         *BuildError       `json:"error,omitempty"`
	Logs          *BuildLogs        `json:"logs,omitempty"`
}

type BuildContinue struct {
	FormatVersion int `json:"formatVersion"`
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

type BuildGuest struct {
	Connector vm.Connector
	WorkDir   string
	Encoder   string
}

type BuildExecution struct {
	Tree         *BuildTree
	Config       BuildConfig
	Verification VerificationResult
	Logs         BuildLogs
}

func (guest BuildGuest) Execute(
	ctx context.Context,
	runID string,
	request BuildGuestRequest,
	source io.Reader,
	manager *ArtifactSnapshot,
	runtime *ArtifactSnapshot,
	toolchain *ArtifactSnapshot,
) (_ *BuildExecution, returnErr error) {
	if guest.Connector == nil {
		return nil, errors.New("build guest connector is required")
	}
	if source == nil {
		return nil, errors.New("submitted source is nil")
	}
	if manager == nil || runtime == nil || toolchain == nil {
		return nil, errors.New("build snapshots are incomplete")
	}
	raw, err := canonicalBuildGuestDocument(request, validateBuildGuestRequest)
	if err != nil {
		return nil, err
	}
	session, err := guest.Connector.Connect(ctx, vm.ConnectRequest{
		ID:        runID,
		OwnerKind: vm.OwnerBuild,
		Resources: compute.BuildGuestResources(),
		PIDsMax:   compute.BuildGuestPIDsMax,
		Network:   compute.DefaultNetworkPolicy(),
		ReadOnlyDrives: []vm.ReadOnlyDrive{
			{ID: vm.ManagerDrive, Source: manager},
			{ID: vm.ManagedRuntimeDrive, Source: runtime},
			{ID: vm.ToolchainDrive, Source: toolchain},
		},
	})
	if err != nil {
		return nil, vm.NewGuestError(fmt.Errorf("connect staged build guest: %w", err))
	}
	defer func() {
		returnErr = errors.Join(returnErr, closeBuildGuest(session))
	}()
	transition, ok := session.(vm.BuildNetworkTransitionSession)
	if !ok {
		return nil, errors.New(
			"staged build session does not expose network authority transition",
		)
	}
	stream := session.Stream()
	bodySize := uint64(4+len(raw)) + uint64(request.SourceSizeBytes)
	if err := wire.WriteStreamFrameHeader(
		stream,
		wire.StreamHeader{Type: wire.StreamTypeBuild, RunID: runID},
		bodySize,
	); err != nil {
		return nil, vm.NewGuestError(fmt.Errorf("write build header: %w", err))
	}
	if err := frameio.WriteMessageFrame(stream, raw); err != nil {
		return nil, vm.NewGuestError(fmt.Errorf("write build request: %w", err))
	}
	written, err := io.CopyN(stream, source, request.SourceSizeBytes)
	if err != nil || written != request.SourceSizeBytes {
		return nil, vm.NewGuestError(fmt.Errorf("write submitted source: %w", err))
	}
	fetchRaw, err := frameio.ReadMessageFrameBounded(
		stream,
		maxBuildGuestResultBytes,
	)
	if err != nil {
		return nil, vm.NewGuestError(fmt.Errorf("read Fetch result: %w", err))
	}
	fetch, err := ParseBuildFetchResult(fetchRaw)
	if err != nil {
		return nil, vm.NewGuestError(err)
	}
	if fetch.Outcome == BuildGuestFailed {
		return nil, BuildFailure{
			Reason:  fetch.Error.ReasonCode,
			Message: fetch.Error.Message,
			Logs:    fetch.Logs,
		}
	}

	cutoffCtx, cancel := context.WithTimeout(ctx, buildNetworkCutoffTimeout)
	cutoff, err := transition.CutoffBuildNetwork(cutoffCtx)
	cancel()
	if err != nil {
		return nil, &buildAuthorityTransitionError{cause: err}
	}
	if failure := buildNetworkFailure(cutoff.Before, fetch.Logs); failure != nil {
		return nil, *failure
	}
	continued, err := canonicalBuildGuestDocument(
		BuildContinue{FormatVersion: BuildGuestFormatVersion},
		validateBuildContinue,
	)
	if err != nil {
		return nil, err
	}
	if err := frameio.WriteMessageFrame(stream, continued); err != nil {
		return nil, vm.NewGuestError(fmt.Errorf("continue networkless build: %w", err))
	}
	if err := stream.CloseWrite(); err != nil {
		return nil, vm.NewGuestError(fmt.Errorf("half-close staged build request: %w", err))
	}
	resultRaw, err := frameio.ReadMessageFrameBounded(
		stream,
		maxBuildGuestResultBytes,
	)
	if err != nil {
		return nil, vm.NewGuestError(fmt.Errorf("read staged build result: %w", err))
	}
	result, err := ParseBuildGuestResult(resultRaw)
	if err != nil {
		return nil, vm.NewGuestError(err)
	}
	if result.Outcome == BuildGuestFailed {
		return nil, BuildFailure{
			Reason:  result.Error.ReasonCode,
			Message: result.Error.Message,
			Logs:    result.Logs,
		}
	}
	tree, err := IngestBuildTreeArchive(
		ctx,
		guest.WorkDir,
		guest.Encoder,
		result.TreeDigest,
		result.TreeSizeBytes,
		stream,
	)
	if err != nil {
		return nil, err
	}
	var trailing [1]byte
	if _, err := io.ReadFull(stream, trailing[:]); !errors.Is(err, io.EOF) {
		_ = tree.Close()
		if err == nil {
			return nil, vm.NewGuestError(
				errors.New("staged build response contains trailing data"),
			)
		}
		return nil, vm.NewGuestError(
			fmt.Errorf("read staged build response tail: %w", err),
		)
	}
	return &BuildExecution{
		Tree:         tree,
		Config:       cloneBuildConfig(*result.Config),
		Verification: *result.Verification,
		Logs:         *result.Logs,
	}, nil
}

func buildNetworkFailure(
	status vm.BuildNetworkStatus,
	logs *BuildLogs,
) *BuildFailure {
	switch {
	case status.LimitPackets != 0:
		return &BuildFailure{
			Reason:  BuildFailureNetworkLimit,
			Message: "dependency Fetch public-egress limit was exceeded",
			Logs:    logs,
		}
	case status.DeniedPackets != 0:
		return &BuildFailure{
			Reason:  BuildFailureNetworkDenied,
			Message: "dependency Fetch attempted a denied destination or protocol",
			Logs:    logs,
		}
	default:
		return nil
	}
}

type buildAuthorityTransitionError struct {
	cause error
}

func (err *buildAuthorityTransitionError) Error() string {
	return fmt.Sprintf("remove build network authority: %v", err.cause)
}

func (err *buildAuthorityTransitionError) Unwrap() error {
	return err.cause
}

func (*buildAuthorityTransitionError) FatalWorker() bool {
	return true
}

func ParseBuildGuestRequest(raw []byte) (BuildGuestRequest, error) {
	var request BuildGuestRequest
	if err := parseBuildGuestDocument(raw, &request, validateBuildGuestRequest); err != nil {
		return BuildGuestRequest{}, err
	}
	return request, nil
}

func ParseBuildFetchResult(raw []byte) (BuildFetchResult, error) {
	var result BuildFetchResult
	if err := parseBuildGuestDocument(raw, &result, validateBuildFetchResult); err != nil {
		return BuildFetchResult{}, err
	}
	return result, nil
}

func ParseBuildContinue(raw []byte) (BuildContinue, error) {
	var continued BuildContinue
	if err := parseBuildGuestDocument(raw, &continued, validateBuildContinue); err != nil {
		return BuildContinue{}, err
	}
	return continued, nil
}

func ParseBuildGuestResult(raw []byte) (BuildGuestResult, error) {
	var result BuildGuestResult
	if err := parseBuildGuestDocument(raw, &result, validateBuildGuestResult); err != nil {
		return BuildGuestResult{}, err
	}
	return result, nil
}

func CanonicalBuildFetchResult(result BuildFetchResult) ([]byte, error) {
	return canonicalBuildGuestDocument(result, validateBuildFetchResult)
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

func validateBuildFetchResult(result BuildFetchResult) error {
	if result.FormatVersion != BuildGuestFormatVersion {
		return fmt.Errorf(
			"Fetch result formatVersion = %d, want %d",
			result.FormatVersion,
			BuildGuestFormatVersion,
		)
	}
	switch result.Outcome {
	case BuildGuestSucceeded:
		if result.Error != nil {
			return errors.New("successful Fetch result forbids error")
		}
		if result.Logs == nil {
			return errors.New("successful Fetch result requires logs")
		}
	case BuildGuestFailed:
		if result.Error == nil {
			return errors.New("failed Fetch result requires error")
		}
		if err := validateBuildFailed(BuildFailed{Error: *result.Error}); err != nil {
			return err
		}
	default:
		return fmt.Errorf("Fetch result outcome %q is unsupported", result.Outcome)
	}
	if result.Logs != nil {
		return validateBuildLogs(*result.Logs)
	}
	return nil
}

func validateBuildContinue(continued BuildContinue) error {
	if continued.FormatVersion != BuildGuestFormatVersion {
		return fmt.Errorf(
			"build continuation formatVersion = %d, want %d",
			continued.FormatVersion,
			BuildGuestFormatVersion,
		)
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
	if len(canonical) == 0 || len(canonical) > maxBuildGuestResultBytes {
		return nil, errors.New("build guest document size is invalid")
	}
	return canonical, nil
}

func parseBuildGuestDocument[T any](
	raw []byte,
	value *T,
	validate func(T) error,
) error {
	if len(raw) == 0 || len(raw) > maxBuildGuestResultBytes {
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

func closeBuildGuest(session vm.Session) error {
	ctx, cancel := context.WithTimeout(
		context.Background(),
		buildGuestCloseTimeout,
	)
	defer cancel()
	if err := session.Close(ctx); err != nil {
		return vm.NewGuestError(fmt.Errorf("close build guest: %w", err))
	}
	return nil
}

type BuildFailure struct {
	Reason  BuildFailureReason
	Message string
	Logs    *BuildLogs
}

func (failure BuildFailure) Error() string {
	return failure.Message
}
