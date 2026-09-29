package computerhost

import (
	"context"
	"errors"
	"fmt"

	computerv0 "github.com/helmrdotdev/helmr/internal/proto/computer/v0"
	"github.com/helmrdotdev/helmr/internal/wire"
	"google.golang.org/protobuf/proto"
)

func (g guestControl) verifyRestore(
	ctx context.Context,
	request *computerv0.VerifyComputerRestoreRequest,
) error {
	if g.machine == nil || request == nil || request.GetIdentity() == nil {
		return errors.New("restored computer verification is required")
	}
	var response computerv0.VerifyComputerRestoreResponse
	step, err := g.exchange(ctx, guestControlExchange{
		header: wire.StreamHeader{
			Type: wire.StreamTypeComputerRestoreVerify, ComputerID: request.GetIdentity().GetComputerId(),
			CheckpointID: request.GetIdentity().GetCheckpointId(),
		},
		request:      request,
		response:     &response,
		cancellation: guestControlCancelReadOnly,
	})
	if err != nil {
		if step == guestControlOpen {
			return fmt.Errorf("open restored computer verification stream: %w", err)
		}
		return err
	}
	if !proto.Equal(response.GetIdentity(), request.GetIdentity()) {
		return errors.New("restored computer verification response changed its identity")
	}

	return nil
}
