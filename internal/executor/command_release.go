package executor

import (
	"context"
	"errors"

	computerv0 "github.com/helmrdotdev/helmr/internal/proto/computer/v0"
	"github.com/helmrdotdev/helmr/internal/vm"
	"github.com/helmrdotdev/helmr/internal/wire"
	"github.com/helmrdotdev/helmr/internal/workerapi"
)

func (m ComputerMaterializer) releaseComputerCommand(ctx context.Context, session vm.Machine, mount workerapi.ComputerInstanceAssignment, release workerapi.ComputerCommandRelease, client workerapi.ComputerMaterializerControlPlaneClient) error {
	r := release.Completion
	if release.ComputerID != mount.ComputerID || r.ComputerInstanceID != mount.ComputerInstanceID || r.WriterGeneration != mount.WriterGeneration || r.OrgID != mount.OrgID || release.RequestFingerprint == "" {
		return computerBasicExecProtocol(errors.New("Command release does not match the Instance"))
	}
	if err := (guestControl{machine: session}).releaseCommand(ctx, &computerv0.ComputerCommandReleaseRequest{Authority: &computerv0.ComputerCommandAuthority{OperationId: r.CommandID, ComputerId: release.ComputerID, ComputerInstanceId: r.ComputerInstanceID, WriterGeneration: r.WriterGeneration, ChannelToken: m.channelToken(mount), RequestFingerprint: release.RequestFingerprint}}); err != nil {
		return err
	}
	return client.ReconcileComputerCommand(ctx, r)
}

// releaseCommand asks the guest to release one completed Command. Cancellation
// closes the stream at any step and returns only after that close has finished.
func (g guestControl) releaseCommand(ctx context.Context, request *computerv0.ComputerCommandReleaseRequest) error {
	var response computerv0.ComputerCommandReleaseResponse
	if err := g.exchange(ctx, guestControlExchange{
		header:        wire.StreamHeader{Type: wire.StreamTypeComputerCommandRelease, OperationID: request.GetAuthority().GetOperationId()},
		request:       request,
		response:      &response,
		closeOnCancel: guestControlCloseOnCancelAwait,
	}); err != nil {
		return err
	}
	if response.Error != "" || !response.Released {
		return computerBasicExecProtocol(errors.New(response.Error))
	}
	return nil
}
