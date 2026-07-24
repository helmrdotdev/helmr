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
	BuildInstallSucceeded   = BuildInstallOutcome("succeeded")
	BuildInstallFailed      = BuildInstallOutcome("failed")

	maxBuildGuestRequestBytes  = 64 << 10
	maxBuildInstallResultBytes = 24 << 20
	buildGuestCloseTimeout     = 30 * time.Second
	buildNetworkStatusTimeout  = 5 * time.Second
)

type BuildInstallOutcome string

type BuildInstallRequest struct {
	FormatVersion     int               `json:"formatVersion"`
	Manager           Manager           `json:"manager"`
	ManagerDigest     string            `json:"managerDigest"`
	Runtime           RuntimeDescriptor `json:"runtime"`
	StandardToolchain Toolchain         `json:"standardToolchain"`
	SourceDigest      string            `json:"sourceDigest"`
	SourceSizeBytes   int64             `json:"sourceSizeBytes"`
}

type BuildInstallResult struct {
	FormatVersion int                 `json:"formatVersion"`
	Outcome       BuildInstallOutcome `json:"outcome"`
	TreeDigest    string              `json:"treeDigest,omitempty"`
	TreeSizeBytes int64               `json:"treeSizeBytes,omitempty"`
	Error         *BuildError         `json:"error,omitempty"`
	Logs          *BuildLogs          `json:"logs,omitempty"`
}

type BuildVerificationRequest struct {
	FormatVersion     int                 `json:"formatVersion"`
	Runtime           RuntimeDescriptor   `json:"runtime"`
	StandardToolchain Toolchain           `json:"standardToolchain"`
	Tree              BuildTreeDescriptor `json:"tree"`
}

type BuildGuest struct {
	Connector vm.Connector
	WorkDir   string
	Encoder   string
}

type BuildInstall struct {
	Tree *BuildTree
	Logs BuildLogs
}

func (guest BuildGuest) Install(
	ctx context.Context,
	runID string,
	request BuildInstallRequest,
	source io.Reader,
	manager *ArtifactSnapshot,
	runtime *RuntimeArtifactSnapshot,
	toolchain *toolchainSnapshot,
) (_ *BuildInstall, returnErr error) {
	if guest.Connector == nil {
		return nil, errors.New("build guest connector is required")
	}
	if source == nil {
		return nil, errors.New("submitted source is nil")
	}
	if manager == nil || runtime == nil || toolchain == nil {
		return nil, errors.New("build install snapshots are incomplete")
	}
	raw, err := canonicalBuildGuestDocument(request, validateBuildInstallRequest)
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
		return nil, vm.NewGuestError(fmt.Errorf("connect build install guest: %w", err))
	}
	defer func() {
		returnErr = errors.Join(returnErr, closeBuildGuest(session))
	}()
	stream := session.Stream()
	bodySize := uint64(4+len(raw)) + uint64(request.SourceSizeBytes)
	if err := wire.WriteStreamFrameHeader(
		stream,
		wire.StreamHeader{Type: wire.StreamTypeBuildInstall, RunID: runID},
		bodySize,
	); err != nil {
		return nil, vm.NewGuestError(fmt.Errorf("write build install header: %w", err))
	}
	if err := frameio.WriteMessageFrame(stream, raw); err != nil {
		return nil, vm.NewGuestError(fmt.Errorf("write build install request: %w", err))
	}
	written, err := io.CopyN(stream, source, request.SourceSizeBytes)
	if err != nil || written != request.SourceSizeBytes {
		return nil, vm.NewGuestError(fmt.Errorf("write submitted source: %w", err))
	}
	if err := stream.CloseWrite(); err != nil {
		return nil, vm.NewGuestError(fmt.Errorf("half-close build install request: %w", err))
	}
	resultRaw, err := frameio.ReadMessageFrameBounded(stream, maxBuildInstallResultBytes)
	if err != nil {
		return nil, vm.NewGuestError(fmt.Errorf("read build install result: %w", err))
	}
	result, err := ParseBuildInstallResult(resultRaw)
	if err != nil {
		return nil, vm.NewGuestError(err)
	}
	networkFailure, err := readBuildNetworkFailure(session, result.Logs)
	if err != nil {
		return nil, err
	}
	if networkFailure != nil {
		return nil, *networkFailure
	}
	if result.Outcome == BuildInstallFailed {
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
			return nil, vm.NewGuestError(errors.New("build install response contains trailing data"))
		}
		return nil, vm.NewGuestError(fmt.Errorf("read build install response tail: %w", err))
	}
	return &BuildInstall{Tree: tree, Logs: *result.Logs}, nil
}

func readBuildNetworkFailure(
	session vm.Session,
	logs *BuildLogs,
) (*BuildFailure, error) {
	network, ok := session.(vm.BuildNetworkSession)
	if !ok {
		return nil, errors.New(
			"build install session does not expose network accounting",
		)
	}
	ctx, cancel := context.WithTimeout(
		context.Background(),
		buildNetworkStatusTimeout,
	)
	defer cancel()
	status, err := network.BuildNetworkStatus(ctx)
	if err != nil {
		return nil, fmt.Errorf("read build network accounting: %w", err)
	}
	switch {
	case status.LimitPackets != 0:
		return &BuildFailure{
			Reason:  BuildFailureNetworkLimit,
			Message: "build public-egress limit was exceeded",
			Logs:    logs,
		}, nil
	case status.DeniedPackets != 0:
		return &BuildFailure{
			Reason:  BuildFailureNetworkDenied,
			Message: "build attempted a denied network destination or protocol",
			Logs:    logs,
		}, nil
	default:
		return nil, nil
	}
}

func (guest BuildGuest) Verify(
	ctx context.Context,
	runID string,
	request BuildVerificationRequest,
	runtime *RuntimeArtifactSnapshot,
	toolchain *toolchainSnapshot,
	tree *BuildTree,
) (_ VerificationResult, returnErr error) {
	if guest.Connector == nil {
		return VerificationResult{}, errors.New("build guest connector is required")
	}
	if runtime == nil || toolchain == nil || tree == nil {
		return VerificationResult{}, errors.New("build verification snapshots are incomplete")
	}
	raw, err := canonicalBuildGuestDocument(request, validateBuildVerificationRequest)
	if err != nil {
		return VerificationResult{}, err
	}
	session, err := guest.Connector.Connect(ctx, vm.ConnectRequest{
		ID:          runID,
		OwnerKind:   vm.OwnerBuild,
		Resources:   compute.BuildGuestResources(),
		PIDsMax:     compute.BuildGuestPIDsMax,
		Networkless: true,
		ReadOnlyDrives: []vm.ReadOnlyDrive{
			{ID: vm.ManagedRuntimeDrive, Source: runtime},
			{ID: vm.ToolchainDrive, Source: toolchain},
			{ID: vm.BuildTreeDrive, Source: tree},
		},
	})
	if err != nil {
		return VerificationResult{}, vm.NewGuestError(fmt.Errorf("connect build verification guest: %w", err))
	}
	defer func() {
		returnErr = errors.Join(returnErr, closeBuildGuest(session))
	}()
	stream := session.Stream()
	if err := wire.WriteStreamFrameHeader(
		stream,
		wire.StreamHeader{Type: wire.StreamTypeBuildVerify, RunID: runID},
		uint64(len(raw)+4),
	); err != nil {
		return VerificationResult{}, vm.NewGuestError(fmt.Errorf("write build verification header: %w", err))
	}
	if err := frameio.WriteMessageFrame(stream, raw); err != nil {
		return VerificationResult{}, vm.NewGuestError(fmt.Errorf("write build verification request: %w", err))
	}
	if err := stream.CloseWrite(); err != nil {
		return VerificationResult{}, vm.NewGuestError(fmt.Errorf("half-close build verification request: %w", err))
	}
	result, err := ReadVerificationResultFrame(stream)
	if err != nil {
		return VerificationResult{}, vm.NewGuestError(err)
	}
	return result, nil
}

func ParseBuildInstallRequest(raw []byte) (BuildInstallRequest, error) {
	var request BuildInstallRequest
	if err := parseBuildGuestDocument(raw, &request, validateBuildInstallRequest); err != nil {
		return BuildInstallRequest{}, err
	}
	return request, nil
}

func ParseBuildInstallResult(raw []byte) (BuildInstallResult, error) {
	var result BuildInstallResult
	if err := parseBuildGuestDocument(raw, &result, validateBuildInstallResult); err != nil {
		return BuildInstallResult{}, err
	}
	return result, nil
}

func ParseBuildVerificationRequest(raw []byte) (BuildVerificationRequest, error) {
	var request BuildVerificationRequest
	if err := parseBuildGuestDocument(raw, &request, validateBuildVerificationRequest); err != nil {
		return BuildVerificationRequest{}, err
	}
	return request, nil
}

func CanonicalBuildInstallResult(result BuildInstallResult) ([]byte, error) {
	return canonicalBuildGuestDocument(result, validateBuildInstallResult)
}

func validateBuildInstallRequest(request BuildInstallRequest) error {
	if request.FormatVersion != BuildGuestFormatVersion {
		return fmt.Errorf("build install formatVersion = %d, want %d", request.FormatVersion, BuildGuestFormatVersion)
	}
	if err := validateManager(request.Manager); err != nil {
		return err
	}
	if request.ManagerDigest != request.Manager.Tree.Digest {
		return errors.New("build install Manager digest does not match")
	}
	if err := ValidateRuntimeDescriptor(request.Runtime); err != nil {
		return err
	}
	if err := validateToolchain(request.StandardToolchain); err != nil {
		return err
	}
	if request.StandardToolchain.Architecture != request.Runtime.Architecture ||
		request.StandardToolchain.ManagedRuntimeDigest != request.Runtime.Digest {
		return errors.New("build install standard toolchain does not match Runtime")
	}
	if !sha256DigestPattern.MatchString(request.SourceDigest) {
		return errors.New("build install source digest is invalid")
	}
	if request.SourceSizeBytes < 1 ||
		request.SourceSizeBytes > archive.MaxSourceArtifactBytes {
		return errors.New("build install source size is invalid")
	}
	return nil
}

func validateBuildInstallResult(result BuildInstallResult) error {
	if result.FormatVersion != BuildGuestFormatVersion {
		return fmt.Errorf("build install result formatVersion = %d, want %d", result.FormatVersion, BuildGuestFormatVersion)
	}
	switch result.Outcome {
	case BuildInstallSucceeded:
		if result.Error != nil {
			return errors.New("successful build install forbids error")
		}
		if !sha256DigestPattern.MatchString(result.TreeDigest) ||
			result.TreeSizeBytes < 1 ||
			result.TreeSizeBytes > maxBuildTreeStreamBytes {
			return errors.New("successful build install tree descriptor is invalid")
		}
		if result.Logs == nil {
			return errors.New("successful build install requires logs")
		}
	case BuildInstallFailed:
		if result.TreeDigest != "" || result.TreeSizeBytes != 0 {
			return errors.New("failed build install forbids tree descriptor")
		}
		if result.Error == nil {
			return errors.New("failed build install requires error")
		}
		failed := BuildFailed{Error: *result.Error}
		if err := validateBuildFailed(failed); err != nil {
			return err
		}
	default:
		return fmt.Errorf("build install result outcome %q is unsupported", result.Outcome)
	}
	if result.Logs != nil {
		if err := validateBuildLogs(*result.Logs); err != nil {
			return err
		}
	}
	return nil
}

func validateBuildVerificationRequest(request BuildVerificationRequest) error {
	if request.FormatVersion != BuildGuestFormatVersion {
		return fmt.Errorf("build verification formatVersion = %d, want %d", request.FormatVersion, BuildGuestFormatVersion)
	}
	if err := ValidateRuntimeDescriptor(request.Runtime); err != nil {
		return err
	}
	if err := validateToolchain(request.StandardToolchain); err != nil {
		return err
	}
	if request.StandardToolchain.Architecture != request.Runtime.Architecture ||
		request.StandardToolchain.ManagedRuntimeDigest != request.Runtime.Digest {
		return errors.New("build verification standard toolchain does not match Runtime")
	}
	if !sha256DigestPattern.MatchString(request.Tree.Digest) ||
		request.Tree.SizeBytes < 1 ||
		request.Tree.SizeBytes > maxBuildTreePhysicalBytes {
		return errors.New("build verification tree descriptor is invalid")
	}
	return nil
}

func canonicalBuildGuestDocument[T any](value T, validate func(T) error) ([]byte, error) {
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
	if len(canonical) == 0 || len(canonical) > maxBuildGuestRequestBytes {
		return nil, errors.New("build guest document size is invalid")
	}
	return canonical, nil
}

func parseBuildGuestDocument[T any](
	raw []byte,
	value *T,
	validate func(T) error,
) error {
	if len(raw) == 0 || len(raw) > maxBuildGuestRequestBytes {
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
	ctx, cancel := context.WithTimeout(context.Background(), buildGuestCloseTimeout)
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
