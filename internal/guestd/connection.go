package guestd

import (
	"context"

	"errors"
	"fmt"
	"io"
	"log/slog"
	"time"

	"github.com/helmrdotdev/helmr/internal/wire"
)

const computerControlTimeout = 30 * time.Second

type connectionStart struct {
	streamHeader wire.StreamHeader
	bodyLen      uint64
}

func handleConnection(ctx context.Context, conn io.ReadWriteCloser, logger *slog.Logger, computerRegistry *computerOperationRegistry) error {
	start, err := readConnectionStart(conn)
	if err != nil {
		return err
	}
	switch start.streamHeader.Type {
	case wire.StreamTypePreparationControl:
		connection, ok := conn.(programConnection)
		if !ok || computerRegistry == nil {
			return errors.New("preparation requires deadlines and Computer registry")
		}
		return handlePreparationControl(ctx, connection, start.bodyLen, computerRegistry)
	case wire.StreamTypeComputerFlush:
		programConn, ok := conn.(programConnection)
		if !ok || computerRegistry == nil {
			return errors.New("computer flush requires deadlines and registry")
		}
		return handleComputerFlush(ctx, programConn, start.streamHeader, start.bodyLen, computerRegistry.writeback)
	case wire.StreamTypeComputerMaterialize:
		return handleComputerMaterializeConnection(ctx, conn, logger, computerRegistry)
	case wire.StreamTypeComputerRuntimePrepare:
		return handleComputerRuntimePrepareConnection(ctx, conn, logger, computerRegistry)
	case wire.StreamTypeAgentComputer:
		connection, ok := conn.(programConnection)
		if !ok {
			return errors.New("computer continuation requires an owned connection")
		}
		return handleAgentComputerConnection(ctx, connection, start.bodyLen, computerRegistry)
	case wire.StreamTypeAgentSession:
		connection, ok := conn.(programConnection)
		if !ok || computerRegistry == nil {
			return errors.New("session connection requires deadlines and Computer registry")
		}
		return handleAgentSessionConnection(ctx, connection, start.bodyLen, computerRegistry)
	case wire.StreamTypeComputerCommandCancel:
		return handleComputerCommandCancelConnection(ctx, conn, computerRegistry)
	case wire.StreamTypeComputerCommandRelease:
		return handleComputerCommandReleaseConnection(conn, computerRegistry)
	case wire.StreamTypeComputerBasicExec:
		return handleComputerBasicExecConnection(ctx, conn, computerRegistry)
	default:
		return fmt.Errorf("unsupported runtime input type %q", start.streamHeader.Type)
	}
}

func readConnectionStart(conn io.Reader) (connectionStart, error) {
	header, bodyLen, err := wire.ReadStreamFrameHeader(conn)
	if err != nil {
		return connectionStart{}, fmt.Errorf("read stream header: %w", err)
	}
	return connectionStart{streamHeader: header, bodyLen: bodyLen}, nil
}
