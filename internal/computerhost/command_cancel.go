package computerhost

import (
	"context"
	"errors"

	computerv0 "github.com/helmrdotdev/helmr/internal/proto/computer/v0"
	"github.com/helmrdotdev/helmr/internal/vm"
	"github.com/helmrdotdev/helmr/internal/wire"
	"github.com/helmrdotdev/helmr/internal/workerapi"
)

func (m commandService) cancelComputerCommand(ctx context.Context, machine vm.Machine, mount commandAuthority, request workerapi.ComputerCommandCancellation) error {
	r := request
	if request.ComputerID != mount.ComputerID || r.ComputerInstanceID != mount.ComputerInstanceID || r.WriterGeneration != mount.WriterGeneration || request.RequestFingerprint == "" {
		return computerBasicExecProtocol(errors.New("command cancellation does not match the Instance"))
	}
	return guestControl{machine: machine}.cancelCommand(ctx, &computerv0.ComputerCommandCancelRequest{Authority: &computerv0.ComputerCommandAuthority{OperationId: r.CommandID, ComputerId: request.ComputerID, ComputerInstanceId: r.ComputerInstanceID, WriterGeneration: r.WriterGeneration, ChannelCredential: mount.GuestChannelCredential, OperationExpiresAtUnixNano: request.ExpiresAt.UnixNano(), RequestFingerprint: request.RequestFingerprint}})
}

// cancelCommand asks the guest to cancel one Command. Cancellation closes the
// stream at any step and returns only after that close has finished.
func (g guestControl) cancelCommand(ctx context.Context, request *computerv0.ComputerCommandCancelRequest) error {
	var response computerv0.ComputerCommandCancelResponse
	if _, err := g.exchange(ctx, guestControlExchange{
		header:   wire.StreamHeader{Type: wire.StreamTypeComputerCommandCancel, OperationID: request.GetAuthority().GetOperationId()},
		request:  request,
		response: &response,
	}); err != nil {
		return err
	}
	if response.Error != "" || !response.Accepted {
		return computerBasicExecProtocol(errors.New(response.Error))
	}
	return nil
}
