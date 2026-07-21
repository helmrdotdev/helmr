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
		return nil, fmt.Errorf("open Workspace authority renewal stream: %w", err)
	}
	defer stream.Close()
	if err := wire.WriteStreamFrameHeader(stream, wire.StreamHeader{
		Type:             wire.StreamTypeWorkspaceAuthorityRenew,
		RunID:            fence.GetRunId(),
		WorkspaceID:      fence.GetWorkspaceId(),
		WorkspaceMountID: fence.GetWorkspaceMountId(),
	}, 0); err != nil {
		return nil, fmt.Errorf("write Workspace authority renewal header: %w", err)
	}
	if err := frameio.WriteProtoFrame(stream, request); err != nil {
		return nil, fmt.Errorf("write Workspace authority renewal request: %w", err)
	}
	var response workspacev0.RenewWorkspaceAuthorityResponse
	if err := readWorkspaceControlResponse(ctx, stream, &response); err != nil {
		return nil, fmt.Errorf("read Workspace authority renewal response: %w", err)
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
