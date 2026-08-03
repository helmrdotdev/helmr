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
	"github.com/helmrdotdev/helmr/internal/proto/run/v0"
	"github.com/helmrdotdev/helmr/internal/wire"
	"google.golang.org/protobuf/proto"
)

type connectionStart struct {
	streamHeader wire.StreamHeader
	bodyLen      uint64
	attach       *runv0.ResumeAttach
}

type guestProfile uint8

const (
	ordinaryGuestProfile guestProfile = iota
	buildGuestProfile
	imageBuildGuestProfile
)

func parseGuestProfile(value string) (guestProfile, error) {
	switch value {
	case "":
		return ordinaryGuestProfile, nil
	case "build":
		return buildGuestProfile, nil
	case "image-build":
		return imageBuildGuestProfile, nil
	default:
		return 0, fmt.Errorf("unsupported guest profile %q", value)
	}
}

func handleConnection(ctx context.Context, conn io.ReadWriteCloser, cfg Config, logger *slog.Logger, registry *waitingRunRegistry, workspaceRegistry *workspaceOperationRegistry) (bool, error) {
	profile, err := parseGuestProfile(cfg.Profile)
	if err != nil {
		return false, err
	}
	start, err := readConnectionStart(conn)
	if err != nil {
		return false, err
	}
	if profile != ordinaryGuestProfile {
		if start.attach != nil {
			return false, errors.New("one-shot guest rejects resume attach")
		}
		switch profile {
		case buildGuestProfile:
			if start.streamHeader.Type != wire.StreamTypeBuild {
				return false, fmt.Errorf(
					"build guest rejects input type %q",
					start.streamHeader.Type,
				)
			}
			return false, handleBuild(ctx, conn, start.bodyLen)
		case imageBuildGuestProfile:
			if start.streamHeader.Type != wire.StreamTypeImageBuild {
				return false, fmt.Errorf(
					"image-build guest rejects input type %q",
					start.streamHeader.Type,
				)
			}
			return false, handleImageBuild(ctx, conn, start.bodyLen)
		}
	}
	if start.attach != nil {
		if err := registry.attachResume(start.attach, conn); err != nil {
			return false, err
		}
		return true, nil
	}
	switch start.streamHeader.Type {
	case wire.StreamTypeWorkspaceMaterialize:
		return false, handleWorkspaceMaterializeConnection(ctx, conn, logger, workspaceRegistry, registry)
	case wire.StreamTypeWorkspaceRuntimePrepare:
		return false, handleWorkspaceRuntimePrepareConnection(ctx, conn, logger, workspaceRegistry)
	case wire.StreamTypeProgramRun:
		return false, handleProgramRunConnection(ctx, conn, logger, registry, workspaceRegistry, start.streamHeader, start.bodyLen)
	case wire.StreamTypeWorkspaceBasicExec:
		return false, handleWorkspaceBasicExecConnection(ctx, conn, workspaceRegistry)
	case wire.StreamTypeWorkspaceStop:
		return false, handleWorkspaceStopConnection(ctx, conn, workspaceRegistry)
	case wire.StreamTypeWorkspaceAuthorityRenew:
		return false, handleWorkspaceAuthorityRenewConnection(ctx, conn, workspaceRegistry)
	case wire.StreamTypeProgramResumeGrant:
		programConn, ok := conn.(programConnection)
		if !ok {
			return false, errors.New("Program resume grant connection does not support deadlines")
		}
		return false, handleProgramResumeGrantConnection(programConn, start.bodyLen, workspaceRegistry, registry, time.Now)
	case wire.StreamTypeProgramRestoreVerify:
		programConn, ok := conn.(programConnection)
		if !ok {
			return false, errors.New("Program restore verification connection does not support deadlines")
		}
		return false, handleProgramRestoreVerifyConnection(programConn, start.bodyLen, workspaceRegistry, registry)
	case wire.StreamTypeWorkspaceFinalizationBegin:
		return false, handleWorkspaceFinalizationBeginConnection(conn, workspaceRegistry)
	case wire.StreamTypeWorkspaceCapture:
		return false, handleWorkspaceCaptureConnection(ctx, conn, workspaceRegistry)
	case wire.StreamTypeWorkspaceReset:
		return false, handleWorkspaceResetConnection(ctx, conn, workspaceRegistry)
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
	var attach runv0.ResumeAttach
	if err := proto.Unmarshal(body, &attach); err != nil {
		return connectionStart{}, fmt.Errorf("decode resume attach: %w", err)
	}
	return validateResumeAttach(&attach)
}

func validateResumeAttach(attach *runv0.ResumeAttach) (connectionStart, error) {
	if strings.TrimSpace(attach.CheckpointId) == "" || strings.TrimSpace(attach.RunWaitId) == "" || strings.TrimSpace(attach.RunLeaseId) == "" {
		return connectionStart{}, errors.New("resume attach is missing required fields")
	}
	return connectionStart{attach: attach}, nil
}

func drainStreamBody(conn io.Reader, bodyLen uint64) {
	_, _ = io.Copy(io.Discard, &io.LimitedReader{R: conn, N: int64(bodyLen)})
}
