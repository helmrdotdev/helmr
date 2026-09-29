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

func (m ComputerMaterializer) cancelComputerCommand(ctx context.Context, session vm.Machine, mount workerapi.ComputerInstanceAssignment, request workerapi.ComputerCommandCancellation) error {
	r := request
	if request.ComputerID != mount.ComputerID || r.ComputerInstanceID != mount.ComputerInstanceID || r.WriterGeneration != mount.WriterGeneration || request.RequestFingerprint == "" {
		return computerBasicExecProtocol(errors.New("Command cancellation does not match the Instance"))
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
	if err := wire.WriteStreamFrameHeader(conn, wire.StreamHeader{Type: wire.StreamTypeComputerCommandCancel, OperationID: r.CommandID}, 0); err != nil {
		return err
	}
	if err := frameio.WriteProtoFrame(conn, &computerv0.ComputerCommandCancelRequest{Authority: &computerv0.ComputerCommandAuthority{OperationId: r.CommandID, ComputerId: request.ComputerID, ComputerInstanceId: r.ComputerInstanceID, WriterGeneration: r.WriterGeneration, ChannelToken: m.channelToken(mount), OperationExpiresAtUnixNano: request.ExpiresAt.UnixNano(), RequestFingerprint: request.RequestFingerprint}}); err != nil {
		return err
	}
	var response computerv0.ComputerCommandCancelResponse
	if err := frameio.ReadProtoFrame(conn, &response); err != nil {
		return err
	}
	if response.Error != "" || !response.Accepted {
		return computerBasicExecProtocol(errors.New(response.Error))
	}
	return nil
}
