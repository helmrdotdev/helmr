package executor

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/helmrdotdev/helmr/internal/frameio"
	computerv0 "github.com/helmrdotdev/helmr/internal/proto/computer/v0"
	programv0 "github.com/helmrdotdev/helmr/internal/proto/program/v0"
	"github.com/helmrdotdev/helmr/internal/vm"
	"github.com/helmrdotdev/helmr/internal/wire"
	"google.golang.org/protobuf/proto"
)

var errComputerControlTransport = errors.New("computer control transport")

func renewComputerAuthorityOnSession(ctx context.Context, session vm.Machine, request *computerv0.RenewComputerAuthorityRequest) (*computerv0.ComputerAuthorityFence, error) {
	if session == nil {
		return nil, errors.New("computer mount session is required")
	}
	if request == nil || request.GetPrevious() == nil || request.GetPrevious().GetFence() == nil {
		return nil, errors.New("previous computer authority is required")
	}
	fence := request.GetPrevious().GetFence()
	stream, err := session.OpenStream(ctx)
	if err != nil {
		return nil, fmt.Errorf("%w: open computer authority renewal stream: %w", errComputerControlTransport, err)
	}
	defer stream.Close()
	if err := wire.WriteStreamFrameHeader(stream, wire.StreamHeader{
		Type:               wire.StreamTypeComputerAuthorityRenew,
		RunID:              fence.GetRunId(),
		ComputerID:         fence.GetComputerId(),
		ComputerInstanceID: fence.GetComputerInstanceId(),
	}, 0); err != nil {
		return nil, fmt.Errorf("%w: write computer authority renewal header: %w", errComputerControlTransport, err)
	}
	if err := frameio.WriteProtoFrame(stream, request); err != nil {
		return nil, fmt.Errorf("%w: write computer authority renewal request: %w", errComputerControlTransport, err)
	}
	var response computerv0.RenewComputerAuthorityResponse
	if err := readComputerControlResponse(ctx, stream, &response); err != nil {
		return nil, fmt.Errorf("%w: read computer authority renewal response: %w", errComputerControlTransport, err)
	}
	if strings.TrimSpace(response.GetError()) != "" {
		return nil, fmt.Errorf("computer authority renewal failed: %s", response.GetError())
	}
	if response.GetFence() == nil {
		return nil, errors.New("computer authority renewal response fence is required")
	}
	expected := proto.Clone(fence).(*computerv0.ComputerAuthorityFence)
	expected.ExpiresAtUnixNano = request.GetNewExpiresAtUnixNano()
	if !proto.Equal(response.GetFence(), expected) {
		return nil, errors.New("computer authority renewal response does not match the requested authority")
	}
	return response.GetFence(), nil
}

func grantProgramResumeOnSession(
	ctx context.Context,
	session vm.Machine,
	request *computerv0.GrantProgramResumeRequest,
) (*programv0.ResumeAttach, error) {
	if session == nil || request == nil || request.GetAuthority() == nil || request.GetAuthority().GetFence() == nil {
		return nil, errors.New("program resume grant and computer authority are required")
	}
	fence := request.GetAuthority().GetFence()
	stream, err := session.OpenStream(ctx)
	if err != nil {
		return nil, fmt.Errorf("open program resume grant stream: %w", err)
	}
	defer stream.Close()
	if err := wire.WriteStreamFrameHeader(stream, wire.StreamHeader{
		Type: wire.StreamTypeProgramResumeGrant, RunID: fence.GetRunId(),
		ComputerID: fence.GetComputerId(), ComputerInstanceID: fence.GetComputerInstanceId(),
	}, 0); err != nil {
		return nil, err
	}
	if err := frameio.WriteProtoFrame(stream, request); err != nil {
		return nil, err
	}
	var response computerv0.GrantProgramResumeResponse
	if err := readComputerControlResponse(ctx, stream, &response); err != nil {
		return nil, err
	}
	attach := response.GetAttach()
	if !proto.Equal(response.GetFence(), fence) || attach == nil ||
		attach.GetRunId() != fence.GetRunId() || attach.GetAttemptNumber() != fence.GetAttemptNumber() || attach.GetRunLeaseId() != fence.GetRunLeaseId() ||
		attach.GetRunWaitId() != request.GetRunWaitId() || attach.GetCheckpointId() != request.GetCheckpointId() || attach.GetResumeRequestVersion() <= 0 ||
		attach.GetResumeAttachId() == "" || attach.GetCorrelationId() == "" {
		return nil, errors.New("program resume grant response did not match exact authority")
	}
	return attach, nil
}
