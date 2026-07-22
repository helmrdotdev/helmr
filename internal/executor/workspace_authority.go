package executor

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/helmrdotdev/helmr/internal/frameio"
	workspacev0 "github.com/helmrdotdev/helmr/internal/proto/workspace/v0"
	"github.com/helmrdotdev/helmr/internal/vm"
	"github.com/helmrdotdev/helmr/internal/wire"
	"google.golang.org/protobuf/proto"
)

var errWorkspaceControlTransport = errors.New("Workspace control transport")

func renewWorkspaceAuthorityOnSession(ctx context.Context, session vm.Session, request *workspacev0.RenewWorkspaceAuthorityRequest) (*workspacev0.WorkspaceAuthorityFence, error) {
	if session == nil {
		return nil, errors.New("Workspace mount session is required")
	}
	if request == nil || request.GetPrevious() == nil || request.GetPrevious().GetFence() == nil {
		return nil, errors.New("previous Workspace authority is required")
	}
	fence := request.GetPrevious().GetFence()
	stream, err := session.OpenStream(ctx)
	if err != nil {
		return nil, fmt.Errorf("%w: open Workspace authority renewal stream: %w", errWorkspaceControlTransport, err)
	}
	defer stream.Close()
	if err := wire.WriteStreamFrameHeader(stream, wire.StreamHeader{
		Type:             wire.StreamTypeWorkspaceAuthorityRenew,
		RunID:            fence.GetRunId(),
		WorkspaceID:      fence.GetWorkspaceId(),
		WorkspaceMountID: fence.GetWorkspaceMountId(),
	}, 0); err != nil {
		return nil, fmt.Errorf("%w: write Workspace authority renewal header: %w", errWorkspaceControlTransport, err)
	}
	if err := frameio.WriteProtoFrame(stream, request); err != nil {
		return nil, fmt.Errorf("%w: write Workspace authority renewal request: %w", errWorkspaceControlTransport, err)
	}
	var response workspacev0.RenewWorkspaceAuthorityResponse
	if err := readWorkspaceControlResponse(ctx, stream, &response); err != nil {
		return nil, fmt.Errorf("%w: read Workspace authority renewal response: %w", errWorkspaceControlTransport, err)
	}
	if strings.TrimSpace(response.GetError()) != "" {
		return nil, fmt.Errorf("Workspace authority renewal failed: %s", response.GetError())
	}
	if response.GetFence() == nil {
		return nil, errors.New("Workspace authority renewal response fence is required")
	}
	expected := proto.Clone(fence).(*workspacev0.WorkspaceAuthorityFence)
	expected.ExpiresAtUnixNano = request.GetNewExpiresAtUnixNano()
	if !proto.Equal(response.GetFence(), expected) {
		return nil, errors.New("Workspace authority renewal response does not match the requested authority")
	}
	return response.GetFence(), nil
}

func grantProgramResumeOnSession(
	ctx context.Context,
	session vm.Session,
	request *workspacev0.GrantProgramResumeRequest,
) error {
	if session == nil || request == nil || request.GetAuthority() == nil || request.GetAuthority().GetFence() == nil {
		return errors.New("Program resume grant and Workspace authority are required")
	}
	fence := request.GetAuthority().GetFence()
	stream, err := session.OpenStream(ctx)
	if err != nil {
		return fmt.Errorf("open Program resume grant stream: %w", err)
	}
	defer stream.Close()
	if err := wire.WriteStreamFrameHeader(stream, wire.StreamHeader{
		Type: wire.StreamTypeProgramResumeGrant, RunID: fence.GetRunId(),
		WorkspaceID: fence.GetWorkspaceId(), WorkspaceMountID: fence.GetWorkspaceMountId(),
	}, 0); err != nil {
		return err
	}
	if err := frameio.WriteProtoFrame(stream, request); err != nil {
		return err
	}
	var response workspacev0.GrantProgramResumeResponse
	if err := readWorkspaceControlResponse(ctx, stream, &response); err != nil {
		return err
	}
	if !proto.Equal(response.GetFence(), fence) ||
		response.GetRunWaitId() != request.GetRunWaitId() ||
		response.GetCheckpointId() != request.GetCheckpointId() ||
		response.GetResumeAttachId() != request.GetResumeAttachId() ||
		response.GetResumeRequestVersion() != request.GetResumeRequestVersion() ||
		response.GetCorrelationId() != request.GetCorrelationId() {
		return errors.New("Program resume grant response did not match exact authority")
	}
	return nil
}

func verifyRestoredProgramOnSession(
	ctx context.Context,
	session vm.Session,
	request *workspacev0.VerifyProgramRestoreRequest,
) error {
	if session == nil || request == nil {
		return errors.New("restored Program verification is required")
	}
	stream, err := session.OpenStream(ctx)
	if err != nil {
		return fmt.Errorf("open restored Program verification stream: %w", err)
	}
	defer stream.Close()
	if err := wire.WriteStreamFrameHeader(stream, wire.StreamHeader{
		Type: wire.StreamTypeProgramRestoreVerify, RunID: request.GetRunId(),
		RunWaitID: request.GetRunWaitId(), CheckpointID: request.GetCheckpointId(),
	}, 0); err != nil {
		return err
	}
	if err := frameio.WriteProtoFrame(stream, request); err != nil {
		return err
	}
	var response workspacev0.VerifyProgramRestoreResponse
	if err := readWorkspaceControlResponse(ctx, stream, &response); err != nil {
		return err
	}
	if response.GetRunId() != request.GetRunId() || response.GetAttemptNumber() != request.GetAttemptNumber() ||
		response.GetRunWaitId() != request.GetRunWaitId() || response.GetCheckpointId() != request.GetCheckpointId() ||
		response.GetCorrelationId() != request.GetCorrelationId() {
		return errors.New("restored Program verification response changed its identity")
	}
	return nil
}

func beginWorkspaceFinalizationOnSession(
	ctx context.Context,
	session vm.Session,
	request *workspacev0.BeginWorkspaceFinalizationRequest,
) (*workspacev0.BeginWorkspaceFinalizationResponse, error) {
	if session == nil {
		return nil, errors.New("Workspace mount session is required")
	}
	if request == nil || request.GetPrevious() == nil || request.GetPrevious().GetFence() == nil {
		return nil, errors.New("previous Workspace authority is required")
	}
	fence := request.GetPrevious().GetFence()
	stream, err := session.OpenStream(ctx)
	if err != nil {
		return nil, fmt.Errorf("open Workspace finalization stream: %w", err)
	}
	defer stream.Close()
	if err := wire.WriteStreamFrameHeader(stream, wire.StreamHeader{
		Type:             wire.StreamTypeWorkspaceFinalizationBegin,
		RunID:            fence.GetRunId(),
		WorkspaceID:      fence.GetWorkspaceId(),
		WorkspaceMountID: fence.GetWorkspaceMountId(),
		OperationID:      request.GetOperationId(),
	}, 0); err != nil {
		return nil, fmt.Errorf("write Workspace finalization header: %w", err)
	}
	if err := frameio.WriteProtoFrame(stream, request); err != nil {
		return nil, fmt.Errorf("write Workspace finalization request: %w", err)
	}
	var response workspacev0.BeginWorkspaceFinalizationResponse
	if err := readWorkspaceControlResponse(ctx, stream, &response); err != nil {
		return nil, fmt.Errorf("read Workspace finalization response: %w", err)
	}
	if strings.TrimSpace(response.GetError()) != "" {
		return nil, fmt.Errorf("Workspace finalization failed: %s", response.GetError())
	}
	if response.GetFence() == nil {
		return nil, errors.New("Workspace finalization response fence is required")
	}
	expected := proto.Clone(fence).(*workspacev0.WorkspaceAuthorityFence)
	expected.ExpiresAtUnixNano = request.GetFinalizationExpiresAtUnixNano()
	if !proto.Equal(response.GetFence(), expected) ||
		response.GetOperationId() != request.GetOperationId() ||
		response.GetKind() != request.GetKind() {
		return nil, errors.New("Workspace finalization response does not match the requested authority")
	}
	return &response, nil
}
