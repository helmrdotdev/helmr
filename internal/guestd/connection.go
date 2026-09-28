package guestd

import (
	"bytes"
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"strings"
	"time"

	"github.com/helmrdotdev/helmr/internal/frameio"
	programv0 "github.com/helmrdotdev/helmr/internal/proto/program/v0"
	"github.com/helmrdotdev/helmr/internal/wire"
	"google.golang.org/protobuf/proto"
)

type connectionStart struct {
	streamHeader wire.StreamHeader
	bodyLen      uint64
	attach       *programv0.ResumeAttach
}

func handleConnection(ctx context.Context, conn io.ReadWriteCloser, logger *slog.Logger, registry *waitingRunRegistry, computerRegistry *computerOperationRegistry) (bool, error) {
	start, err := readConnectionStart(conn)
	if err != nil {
		return false, err
	}
	if start.attach != nil {
		if err := registry.attachResume(start.attach, conn); err != nil {
			return false, err
		}
		return true, nil
	}
	switch start.streamHeader.Type {
	case wire.StreamTypeComputerMaterialize:
		return false, handleComputerMaterializeConnection(ctx, conn, logger, computerRegistry, registry)
	case wire.StreamTypeComputerRuntimePrepare:
		return false, handleComputerRuntimePrepareConnection(ctx, conn, logger, computerRegistry)
	case wire.StreamTypeProgramRun:
		return false, handleProgramRunConnection(ctx, conn, logger, registry, computerRegistry, start.streamHeader, start.bodyLen)
	case wire.StreamTypeComputerRunCleanup:
		return false, handleComputerRunCleanupConnection(ctx, conn, computerRegistry)
	case wire.StreamTypeComputerCommandCancel:
		return false, handleComputerCommandCancelConnection(ctx, conn, computerRegistry)
	case wire.StreamTypeComputerCommandRelease:
		return false, handleComputerCommandReleaseConnection(conn, computerRegistry)
	case wire.StreamTypeComputerBasicExec:
		return false, handleComputerBasicExecConnection(ctx, conn, computerRegistry)
	case wire.StreamTypeComputerAuthorityRenew:
		return false, handleComputerAuthorityRenewConnection(ctx, conn, computerRegistry)
	case wire.StreamTypeProgramResumeGrant:
		programConn, ok := conn.(programConnection)
		if !ok {
			return false, errors.New("program resume grant connection does not support deadlines")
		}
		return false, handleProgramResumeGrantConnection(programConn, start.bodyLen, computerRegistry, registry, time.Now)
	case wire.StreamTypeComputerFreeze:
		programConn, ok := conn.(programConnection)
		if !ok {
			return false, errors.New("computer freeze connection does not support deadlines")
		}
		return false, handleComputerFreezeConnection(ctx, programConn, start.bodyLen, computerRegistry, registry)
	case wire.StreamTypeComputerRestoreInstall, wire.StreamTypeComputerRestoreActivate:
		programConn, ok := conn.(programConnection)
		if !ok {
			return false, errors.New("computer restore control connection does not support deadlines")
		}
		return false, handleComputerRestoreInstallation(programConn, start.bodyLen, computerRegistry, registry, start.streamHeader.Type == wire.StreamTypeComputerRestoreActivate)
	case wire.StreamTypeComputerRestoreVerify:
		programConn, ok := conn.(programConnection)
		if !ok {
			return false, errors.New("computer restore verification connection does not support deadlines")
		}
		return false, handleComputerRestoreVerifyConnection(programConn, start.bodyLen, computerRegistry, registry)
	default:
		return false, fmt.Errorf("unsupported runtime input type %q", start.streamHeader.Type)
	}
}

func readConnectionStart(conn io.Reader) (connectionStart, error) {
	var prefix [4]byte
	if _, err := io.ReadFull(conn, prefix[:]); err != nil {
		return connectionStart{}, fmt.Errorf("read initial connection frame: %w", err)
	}
	if frameio.IsStreamFramePrefix(prefix[:]) {
		header, bodyLen, err := wire.ReadStreamFrameHeader(io.MultiReader(bytes.NewReader(prefix[:]), conn))
		if err != nil {
			return connectionStart{}, fmt.Errorf("read stream header: %w", err)
		}
		return connectionStart{streamHeader: header, bodyLen: bodyLen}, nil
	}
	frameLen := binary.BigEndian.Uint32(prefix[:4])
	if frameLen < 4 {
		return connectionStart{}, fmt.Errorf("initial connection frame length %d is invalid", frameLen)
	}
	if frameLen > frameio.MaxFrameBytes {
		return connectionStart{}, fmt.Errorf("resume attach frame length %d exceeds max %d", frameLen, frameio.MaxFrameBytes)
	}
	body := make([]byte, int(frameLen))
	if _, err := io.ReadFull(conn, body); err != nil {
		return connectionStart{}, fmt.Errorf("read resume attach frame: %w", err)
	}
	var attach programv0.ResumeAttach
	if err := proto.Unmarshal(body, &attach); err != nil {
		return connectionStart{}, fmt.Errorf("decode resume attach: %w", err)
	}
	return validateResumeAttach(&attach)
}

func validateResumeAttach(attach *programv0.ResumeAttach) (connectionStart, error) {
	if strings.TrimSpace(attach.CheckpointId) == "" || strings.TrimSpace(attach.RunWaitId) == "" || strings.TrimSpace(attach.RunLeaseId) == "" {
		return connectionStart{}, errors.New("resume attach is missing required fields")
	}
	return connectionStart{attach: attach}, nil
}

func drainStreamBody(conn io.Reader, bodyLen uint64) {
	_, _ = io.Copy(io.Discard, &io.LimitedReader{R: conn, N: int64(bodyLen)})
}
