package computerhost

import (
	"context"
	"errors"

	"github.com/helmrdotdev/helmr/internal/frameio"
	computerv0 "github.com/helmrdotdev/helmr/internal/proto/computer/v0"
	"github.com/helmrdotdev/helmr/internal/vm"
	"github.com/helmrdotdev/helmr/internal/wire"
	"github.com/helmrdotdev/helmr/internal/workerapi"
)

// releaseComputerCommand owns its guest stream beyond the guest response: the
// control plane reconciles the release while that stream is still open, and
// cancellation closes the stream at any point and is awaited before returning.
func (m Server) releaseComputerCommand(ctx context.Context, session vm.Machine, mount workerapi.ComputerInstanceAssignment, release workerapi.ComputerCommandRelease, client workerapi.ComputerServerControlPlaneClient) error {
	r := release.Completion
	if release.ComputerID != mount.ComputerID || r.ComputerInstanceID != mount.ComputerInstanceID || r.WriterGeneration != mount.WriterGeneration || r.OrgID != mount.OrgID || release.RequestFingerprint == "" {
		return computerBasicExecProtocol(errors.New("Command release does not match the Instance"))
	}
	conn, err := session.OpenStream(ctx)
	if err != nil {
		return err
	}
	defer conn.Close()
	closed := make(chan struct{})
	stopClose := context.AfterFunc(ctx, func() { defer close(closed); _ = conn.Close() })
	defer func() {
		if !stopClose() {
			<-closed
		}
	}()
	if _, err := writeGuestControlRequest(conn, wire.StreamHeader{Type: wire.StreamTypeComputerCommandRelease, OperationID: r.CommandID}, &computerv0.ComputerCommandReleaseRequest{Authority: &computerv0.ComputerCommandAuthority{OperationId: r.CommandID, ComputerId: release.ComputerID, ComputerInstanceId: r.ComputerInstanceID, WriterGeneration: r.WriterGeneration, ChannelToken: m.channelToken(mount), RequestFingerprint: release.RequestFingerprint}}); err != nil {
		return err
	}
	var response computerv0.ComputerCommandReleaseResponse
	if err := frameio.ReadProtoFrame(conn, &response); err != nil {
		return err
	}
	if response.Error != "" || !response.Released {
		return computerBasicExecProtocol(errors.New(response.Error))
	}
	return client.ReconcileComputerCommand(ctx, r)
}
