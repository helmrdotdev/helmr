package executor

import (
	"context"
	"errors"
	"fmt"
	"github.com/helmrdotdev/helmr/internal/frameio"
	computerv0 "github.com/helmrdotdev/helmr/internal/proto/computer/v0"
	"github.com/helmrdotdev/helmr/internal/vm"
	"github.com/helmrdotdev/helmr/internal/wire"
	"google.golang.org/protobuf/proto"
)

func verifyRestoredComputerOnSession(
	ctx context.Context,
	session vm.Machine,
	request *computerv0.VerifyComputerRestoreRequest,
) error {
	if session == nil || request == nil || request.GetIdentity() == nil {
		return errors.New("restored computer verification is required")
	}
	stream, err := session.OpenStream(ctx)
	if err != nil {
		return fmt.Errorf("open restored computer verification stream: %w", err)
	}
	defer stream.Close()
	if err := wire.WriteStreamFrameHeader(stream, wire.StreamHeader{
		Type: wire.StreamTypeComputerRestoreVerify, ComputerID: request.GetIdentity().GetComputerId(),
		CheckpointID: request.GetIdentity().GetCheckpointId(),
	}, 0); err != nil {
		return err
	}
	if err := frameio.WriteProtoFrame(stream, request); err != nil {
		return err
	}
	var response computerv0.VerifyComputerRestoreResponse
	if err := readComputerControlResponse(ctx, stream, &response); err != nil {
		return err
	}
	if !proto.Equal(response.GetIdentity(), request.GetIdentity()) {
		return errors.New("restored computer verification response changed its identity")
	}

	return nil
}
