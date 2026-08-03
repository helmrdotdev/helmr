package programbuild

import (
	"context"
	"errors"
	"fmt"
	"io"
	"time"

	"github.com/helmrdotdev/helmr/internal/compute"
	"github.com/helmrdotdev/helmr/internal/deployment"
	"github.com/helmrdotdev/helmr/internal/frameio"
	"github.com/helmrdotdev/helmr/internal/vm"
	"github.com/helmrdotdev/helmr/internal/wire"
)

const buildGuestCloseTimeout = 30 * time.Second

type buildGuest struct {
	connector vm.Connector
	workDir   string
	encoder   string
}

type buildExecution struct {
	tree           *deployment.BuildTree
	treeDescriptor deployment.BuildTreeDescriptor
	config         deployment.BuildConfig
	verification   deployment.VerificationResult
	logs           deployment.BuildLogs
}

func (guest buildGuest) execute(
	ctx context.Context,
	binding vm.WorkloadBinding,
	request deployment.BuildGuestRequest,
	source io.Reader,
	manager *deployment.ArtifactSnapshot,
	runtime *deployment.ArtifactSnapshot,
	toolchain *deployment.ArtifactSnapshot,
) (_ *buildExecution, returnErr error) {
	if guest.connector == nil {
		return nil, errors.New("build guest connector is required")
	}
	if source == nil {
		return nil, errors.New("submitted source is nil")
	}
	if manager == nil || runtime == nil || toolchain == nil {
		return nil, errors.New("build snapshots are incomplete")
	}
	raw, err := deployment.CanonicalBuildGuestRequest(request)
	if err != nil {
		return nil, err
	}
	session, err := guest.connector.Connect(ctx, vm.ConnectRequest{
		ID:        binding.OwnerID,
		OwnerKind: vm.OwnerBuild,
		Binding:   binding,
		Resources: compute.BuildGuestResources(),
		PIDsMax:   compute.BuildGuestPIDsMax,
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
	network, ok := session.(vm.BuildNetworkSession)
	if !ok {
		return nil, errors.New("build session does not expose network status")
	}
	stream := session.Stream()
	bodySize := uint64(4+len(raw)) + uint64(request.SourceSizeBytes)
	if err := wire.WriteStreamFrameHeader(
		stream,
		wire.StreamHeader{Type: wire.StreamTypeBuild, RunID: binding.OwnerID},
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
	resultRaw, err := frameio.ReadMessageFrameBounded(
		stream,
		deployment.MaxBuildGuestResultBytes,
	)
	if err != nil {
		return nil, vm.NewGuestError(fmt.Errorf("read build result: %w", err))
	}
	result, err := deployment.ParseBuildGuestResult(resultRaw)
	if err != nil {
		return nil, vm.NewGuestError(err)
	}
	var tree *deployment.BuildTree
	if result.Outcome == deployment.BuildGuestSucceeded {
		tree, err = deployment.IngestBuildTreeArchive(
			ctx,
			guest.workDir,
			guest.encoder,
			result.TreeDigest,
			result.TreeSizeBytes,
			stream,
		)
		if err != nil {
			return nil, err
		}
	}
	var trailing [1]byte
	if _, err := io.ReadFull(stream, trailing[:]); !errors.Is(err, io.EOF) {
		if tree != nil {
			_ = tree.Close()
		}
		if err == nil {
			return nil, vm.NewGuestError(errors.New("build response contains trailing data"))
		}
		return nil, vm.NewGuestError(fmt.Errorf("read build response tail: %w", err))
	}
	status, err := network.BuildNetworkStatus(ctx)
	if err != nil {
		if tree != nil {
			_ = tree.Close()
		}
		return nil, fmt.Errorf("read build network status: %w", err)
	}
	if failure := buildNetworkFailure(status, result.Logs); failure != nil {
		if tree != nil {
			_ = tree.Close()
		}
		return nil, *failure
	}
	if result.Outcome == deployment.BuildGuestFailed {
		return nil, buildFailure{
			reason:  result.Error.ReasonCode,
			message: result.Error.Message,
			logs:    result.Logs,
		}
	}
	treeDescriptor, err := tree.Descriptor()
	if err != nil {
		_ = tree.Close()
		return nil, err
	}
	return &buildExecution{
		tree:           tree,
		treeDescriptor: treeDescriptor,
		config:         *result.Config,
		verification:   *result.Verification,
		logs:           *result.Logs,
	}, nil
}

func buildNetworkFailure(
	status vm.BuildNetworkStatus,
	logs *deployment.BuildLogs,
) *buildFailure {
	if status.LimitPackets == 0 {
		return nil
	}
	return &buildFailure{
		reason:  deployment.BuildFailureNetworkLimit,
		message: "build public-egress limit was exceeded",
		logs:    logs,
	}
}

func closeBuildGuest(session vm.Session) error {
	ctx, cancel := context.WithTimeout(context.Background(), buildGuestCloseTimeout)
	defer cancel()
	if err := session.Close(ctx); err != nil {
		return vm.NewGuestError(fmt.Errorf("close build guest: %w", err))
	}
	return nil
}

type buildFailure struct {
	reason  deployment.BuildFailureReason
	message string
	logs    *deployment.BuildLogs
}

func (failure buildFailure) Error() string {
	return failure.message
}
