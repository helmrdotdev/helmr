package executor

import (
	"context"
	"errors"

	"github.com/helmrdotdev/helmr/internal/frameio"
	computerv0 "github.com/helmrdotdev/helmr/internal/proto/computer/v0"
	"github.com/helmrdotdev/helmr/internal/vm"
	"github.com/helmrdotdev/helmr/internal/wire"
	"github.com/helmrdotdev/helmr/internal/workerapi"
)

func (m ComputerMaterializer) releaseComputerCommand(ctx context.Context, session vm.Session, mount workerapi.ComputerInstanceAssignment, release workerapi.ComputerCommandRelease, client workerapi.ComputerMaterializerControlPlaneClient) error {
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
	if err := wire.WriteStreamFrameHeader(conn, wire.StreamHeader{Type: wire.StreamTypeComputerCommandRelease, OperationID: r.CommandID}, 0); err != nil {
		return err
	}
	if err := frameio.WriteProtoFrame(conn, &computerv0.ComputerCommandReleaseRequest{Authority: &computerv0.ComputerCommandAuthority{OperationId: r.CommandID, ComputerId: release.ComputerID, ComputerInstanceId: r.ComputerInstanceID, WriterGeneration: r.WriterGeneration, ChannelToken: m.channelToken(mount), RequestFingerprint: release.RequestFingerprint}}); err != nil {
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
