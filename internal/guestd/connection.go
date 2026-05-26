package guestd

import (
	"context"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"strings"

	runv0 "github.com/helmrdotdev/helmr/internal/proto/run/v0"
	"github.com/helmrdotdev/helmr/internal/transport"
	"github.com/helmrdotdev/helmr/internal/workspace"
	"google.golang.org/protobuf/proto"
)

type connectionStart struct {
	streamHeader transport.StreamHeader
	bodyLen      uint64
	attach       *runv0.ResumeAttach
}

func handleConnection(ctx context.Context, conn io.ReadWriter, cfg Config, logger *slog.Logger, registry *waitingRunRegistry) (bool, error) {
	start, err := readConnectionStart(conn)
	if err != nil {
		return false, err
	}
	if start.attach != nil {
		if err := registry.attach(start.attach.WaitpointId, start.attach.CheckpointId, conn); err != nil {
			return false, err
		}
		return true, nil
	}
	switch start.streamHeader.Type {
	case transport.StreamTypeCatalogDeployment:
		return false, handleCatalogDeployment(ctx, conn, cfg, start.streamHeader, start.bodyLen)
	case transport.StreamTypeCompileTaskBundle:
		return false, handleCompileTaskBundle(ctx, conn, cfg, start.streamHeader, start.bodyLen)
	case transport.StreamTypeRunImage:
		return false, handleRunConnection(ctx, conn, cfg, logger, registry, start.streamHeader, start.bodyLen)
	default:
		return false, fmt.Errorf("unsupported input stream type %q", start.streamHeader.Type)
	}
}

func readConnectionStart(conn io.Reader) (connectionStart, error) {
	var prefix [8]byte
	if _, err := io.ReadFull(conn, prefix[:]); err != nil {
		return connectionStart{}, fmt.Errorf("read initial connection frame: %w", err)
	}
	frameLen := binary.BigEndian.Uint32(prefix[:4])
	if frameLen < 4 {
		return connectionStart{}, fmt.Errorf("initial connection frame length %d is invalid", frameLen)
	}
	second := binary.BigEndian.Uint32(prefix[4:])
	if second <= frameLen && second <= transport.MaxFrameBytes {
		headerBytes := make([]byte, second)
		if _, err := io.ReadFull(conn, headerBytes); err != nil {
			return connectionStart{}, fmt.Errorf("read stream header: %w", err)
		}
		var header transport.StreamHeader
		if err := json.Unmarshal(headerBytes, &header); err == nil && strings.TrimSpace(string(header.Type)) != "" {
			return connectionStart{streamHeader: header, bodyLen: uint64(frameLen) - uint64(second)}, nil
		}
		if second > frameLen-4 {
			return connectionStart{}, errors.New("decode initial stream header")
		}
		remaining := int(frameLen) - 4 - len(headerBytes)
		body := append([]byte{}, prefix[4:]...)
		body = append(body, headerBytes...)
		if remaining > 0 {
			tail := make([]byte, remaining)
			if _, err := io.ReadFull(conn, tail); err != nil {
				return connectionStart{}, fmt.Errorf("read resume attach frame: %w", err)
			}
			body = append(body, tail...)
		}
		var attach runv0.ResumeAttach
		if err := proto.Unmarshal(body, &attach); err != nil {
			return connectionStart{}, fmt.Errorf("decode initial connection frame: %w", err)
		}
		return validateResumeAttach(&attach)
	}
	if frameLen > transport.MaxFrameBytes {
		return connectionStart{}, fmt.Errorf("resume attach frame length %d exceeds max %d", frameLen, transport.MaxFrameBytes)
	}
	body := make([]byte, int(frameLen))
	copy(body, prefix[4:])
	if _, err := io.ReadFull(conn, body[4:]); err != nil {
		return connectionStart{}, fmt.Errorf("read resume attach frame: %w", err)
	}
	var attach runv0.ResumeAttach
	if err := proto.Unmarshal(body, &attach); err != nil {
		return connectionStart{}, fmt.Errorf("decode resume attach: %w", err)
	}
	return validateResumeAttach(&attach)
}

func validateResumeAttach(attach *runv0.ResumeAttach) (connectionStart, error) {
	if strings.TrimSpace(attach.CheckpointId) == "" || strings.TrimSpace(attach.WaitpointId) == "" || strings.TrimSpace(attach.SessionId) == "" {
		return connectionStart{}, errors.New("resume attach is missing required fields")
	}
	return connectionStart{attach: attach}, nil
}

func handleCatalogDeployment(ctx context.Context, conn io.ReadWriter, cfg Config, header transport.StreamHeader, bodyLen uint64) error {
	runRoot, err := mkdirGuestdTemp("helmr-index-*")
	if err != nil {
		return fmt.Errorf("create index temp dir: %w", err)
	}
	defer os.RemoveAll(runRoot)
	sourceRoot := filepath.Join(runRoot, "source")
	if err := os.MkdirAll(sourceRoot, 0o755); err != nil {
		return fmt.Errorf("create index source dir: %w", err)
	}
	if strings.TrimSpace(header.RunID) == "" {
		return errors.New("index source run_id is required")
	}
	body := &io.LimitedReader{R: conn, N: int64(bodyLen)}
	if err := extractTar(body, sourceRoot); err != nil {
		if _, drainErr := io.Copy(io.Discard, body); drainErr != nil {
			return errors.Join(fmt.Errorf("extract index source: %w", err), fmt.Errorf("drain index source: %w", drainErr))
		}
		return transport.WriteParseErrorFrame(conn, "bad_request", fmt.Sprintf("extract index source: %s", err))
	}
	if _, err := io.Copy(io.Discard, body); err != nil {
		return transport.WriteParseErrorFrame(conn, "bad_request", fmt.Sprintf("drain index source: %s", err))
	}
	registry, err := indexAdapter(ctx, cfg, sourceRoot)
	if err != nil {
		var parseErr adapterParseError
		if errors.As(err, &parseErr) {
			return transport.WriteParseErrorFrame(conn, parseErr.Kind, parseErr.Message)
		}
		return err
	}
	return transport.WriteMessageFrame(conn, registry)
}

func handleCompileTaskBundle(ctx context.Context, conn io.ReadWriter, cfg Config, header transport.StreamHeader, bodyLen uint64) error {
	runRoot, err := mkdirGuestdTemp("helmr-run-*")
	if err != nil {
		return fmt.Errorf("create parse temp dir: %w", err)
	}
	defer os.RemoveAll(runRoot)
	sourceRoot := filepath.Join(runRoot, "source")
	if err := os.MkdirAll(sourceRoot, 0o755); err != nil {
		return fmt.Errorf("create parse source dir: %w", err)
	}
	runID := strings.TrimSpace(header.RunID)
	if runID == "" {
		return errors.New("parse source run_id is required")
	}
	taskID := strings.TrimSpace(header.TaskID)
	if taskID == "" {
		return errors.New("parse source task_id is required")
	}
	body := &io.LimitedReader{R: conn, N: int64(bodyLen)}
	if err := extractTar(body, sourceRoot); err != nil {
		if _, drainErr := io.Copy(io.Discard, body); drainErr != nil {
			return errors.Join(fmt.Errorf("extract parse source: %w", err), fmt.Errorf("drain parse source: %w", drainErr))
		}
		return transport.WriteParseErrorFrame(conn, "bad_request", fmt.Sprintf("extract parse source: %s", err))
	}
	if _, err := io.Copy(io.Discard, body); err != nil {
		return transport.WriteParseErrorFrame(conn, "bad_request", fmt.Sprintf("drain parse source: %s", err))
	}
	bundle, err := parseAdapter(ctx, cfg, sourceRoot, taskID)
	if err != nil {
		var parseErr adapterParseError
		if errors.As(err, &parseErr) {
			return transport.WriteParseErrorFrame(conn, parseErr.Kind, parseErr.Message)
		}
		return err
	}
	return transport.WriteMessageFrame(conn, bundle)
}

func handleRunConnection(ctx context.Context, conn io.ReadWriter, cfg Config, logger *slog.Logger, registry *waitingRunRegistry, header transport.StreamHeader, bodyLen uint64) error {
	if err := handleRunStream(ctx, conn, cfg, logger, registry, header, bodyLen); err != nil {
		if reportErr := writeRunSetupFailure(conn, err); reportErr != nil {
			return errors.Join(err, fmt.Errorf("write run setup failure: %w", reportErr))
		}
	}
	return nil
}

func handleRunStream(ctx context.Context, conn io.ReadWriter, cfg Config, logger *slog.Logger, registry *waitingRunRegistry, header transport.StreamHeader, bodyLen uint64) error {
	runRoot, err := mkdirGuestdTemp("helmr-run-*")
	if err != nil {
		return fmt.Errorf("create run temp dir: %w", err)
	}
	defer os.RemoveAll(runRoot)
	deploymentSourceRoot := filepath.Join(runRoot, "deployment-source")
	if err := os.MkdirAll(deploymentSourceRoot, 0o755); err != nil {
		return fmt.Errorf("create deployment source dir: %w", err)
	}
	imageRoot := filepath.Join(runRoot, "image")
	var image ociImage
	runID := header.RunID
	if strings.TrimSpace(runID) == "" {
		return errors.New("input stream run_id is required")
	}
	if header.Type != transport.StreamTypeRunImage {
		return fmt.Errorf("unsupported input stream type %q", header.Type)
	}
	body := &io.LimitedReader{R: conn, N: int64(bodyLen)}
	image, err = unpackOCIImage(body, imageRoot)
	if err != nil {
		if _, drainErr := io.Copy(io.Discard, body); drainErr != nil {
			return errors.Join(fmt.Errorf("unpack run image: %w", err), fmt.Errorf("drain run image: %w", drainErr))
		}
		return fmt.Errorf("unpack run image: %w", err)
	}
	if _, err := io.Copy(io.Discard, body); err != nil {
		return fmt.Errorf("drain run image: %w", err)
	}
	header, bodyLen, err = transport.ReadStreamFrameHeader(conn)
	if err != nil {
		return fmt.Errorf("read deployment source stream header: %w", err)
	}
	if header.RunID != runID {
		return fmt.Errorf("deployment source run_id %q does not match run image run_id %q", header.RunID, runID)
	}
	if header.Type != transport.StreamTypeDeploymentSource {
		return fmt.Errorf("unsupported input stream type %q", header.Type)
	}
	body = &io.LimitedReader{R: conn, N: int64(bodyLen)}
	if err := extractTar(body, deploymentSourceRoot); err != nil {
		if _, drainErr := io.Copy(io.Discard, body); drainErr != nil {
			return errors.Join(fmt.Errorf("extract deployment source: %w", err), fmt.Errorf("drain deployment source: %w", drainErr))
		}
		drainRemainingRunInput(conn, runID)
		return fmt.Errorf("extract deployment source: %w", err)
	}
	if _, err := io.Copy(io.Discard, body); err != nil {
		return fmt.Errorf("drain deployment source: %w", err)
	}
	var request runv0.RunTaskRequest
	if err := transport.ReadProtoFrame(conn, &request); err != nil {
		drainWorkspaceArtifact(conn, runID)
		return fmt.Errorf("read run request: %w", err)
	}
	if request.RunId != runID {
		drainWorkspaceArtifact(conn, runID)
		return fmt.Errorf("run request run_id %q does not match input stream run_id %q", request.RunId, runID)
	}
	mountPath, err := workspaceMountPath(&request)
	if err != nil {
		drainWorkspaceArtifact(conn, runID)
		return err
	}
	workspaceRoot, err := workspaceRootForImage(image.RootfsDir, mountPath)
	if err != nil {
		drainWorkspaceArtifact(conn, runID)
		return err
	}
	if err := os.MkdirAll(workspaceRoot, 0o755); err != nil {
		drainWorkspaceArtifact(conn, runID)
		return fmt.Errorf("create workspace root: %w", err)
	}
	header, bodyLen, err = transport.ReadStreamFrameHeader(conn)
	if err != nil {
		return fmt.Errorf("read workspace artifact stream header: %w", err)
	}
	if header.RunID != runID {
		return fmt.Errorf("workspace artifact run_id %q does not match run image run_id %q", header.RunID, runID)
	}
	if header.Type != transport.StreamTypeWorkspaceArtifact {
		return fmt.Errorf("unsupported input stream type %q", header.Type)
	}
	workspaceArtifactDigest := ""
	if header.BodyDigest != nil {
		workspaceArtifactDigest = strings.TrimSpace(*header.BodyDigest)
	}
	if err := validateWorkspaceArtifact(&request, workspaceArtifactDigest, bodyLen); err != nil {
		drainStreamBody(conn, bodyLen)
		return err
	}
	body = &io.LimitedReader{R: conn, N: int64(bodyLen)}
	if err := extractTarWithLimits(body, workspaceRoot, tarExtractLimits{
		MaxBytes:   workspace.MaxArtifactExtractedBytes,
		MaxEntries: int(request.GetWorkspace().GetArtifact().GetEntryCount()),
	}); err != nil {
		if _, drainErr := io.Copy(io.Discard, body); drainErr != nil {
			return errors.Join(fmt.Errorf("extract workspace artifact: %w", err), fmt.Errorf("drain workspace artifact: %w", drainErr))
		}
		return fmt.Errorf("extract workspace artifact: %w", err)
	}
	if _, err := io.Copy(io.Discard, body); err != nil {
		return fmt.Errorf("drain workspace artifact: %w", err)
	}
	runCwd := request.Cwd
	if strings.TrimSpace(runCwd) == "" {
		runCwd = mountPath
	}
	logger.Info("running task", "run_id", request.RunId, "task_id", request.TaskId)
	return runAdapter(ctx, conn, cfg, image.RootfsDir, deploymentSourceRoot, workspaceRoot, runCwd, image.Config, true, &request, registry)
}

func drainRunRequest(conn io.Reader) {
	_, _ = transport.ReadMessageFrame(conn)
}

func drainRemainingRunInput(conn io.Reader, runID string) {
	drainRunRequest(conn)
	drainWorkspaceArtifact(conn, runID)
}

func drainWorkspaceArtifact(conn io.Reader, runID string) {
	header, bodyLen, err := transport.ReadStreamFrameHeader(conn)
	if err != nil {
		return
	}
	if header.RunID != runID || header.Type != transport.StreamTypeWorkspaceArtifact {
		return
	}
	drainStreamBody(conn, bodyLen)
}

func drainStreamBody(conn io.Reader, bodyLen uint64) {
	_, _ = io.Copy(io.Discard, &io.LimitedReader{R: conn, N: int64(bodyLen)})
}

func validateWorkspaceArtifact(request *runv0.RunTaskRequest, frameDigest string, frameSize uint64) error {
	workspaceSpec := request.GetWorkspace()
	if workspaceSpec == nil {
		return errors.New("workspace volume is required")
	}
	artifact := workspaceSpec.GetArtifact()
	if artifact == nil {
		return errors.New("workspace artifact is required")
	}
	if strings.TrimSpace(artifact.Digest) == "" {
		return errors.New("workspace artifact digest is required")
	}
	if frameDigest != "" && strings.TrimSpace(artifact.Digest) != frameDigest {
		return fmt.Errorf("workspace artifact digest %q does not match frame digest %q", artifact.Digest, frameDigest)
	}
	if artifact.SizeBytes == 0 {
		return errors.New("workspace artifact size_bytes is required")
	}
	if artifact.SizeBytes != frameSize {
		return fmt.Errorf("workspace artifact size_bytes %d does not match frame size %d", artifact.SizeBytes, frameSize)
	}
	if artifact.SizeBytes > uint64(workspace.MaxArtifactArchiveBytes) {
		return fmt.Errorf("workspace artifact size_bytes %d exceeds max %d", artifact.SizeBytes, workspace.MaxArtifactArchiveBytes)
	}
	if artifact.EntryCount == 0 {
		return errors.New("workspace artifact entry_count is required")
	}
	if artifact.EntryCount > uint32(workspace.MaxArtifactEntries) {
		return fmt.Errorf("workspace artifact entry_count %d exceeds max %d", artifact.EntryCount, workspace.MaxArtifactEntries)
	}
	if strings.TrimSpace(artifact.MediaType) != workspace.ArtifactMediaType {
		return fmt.Errorf("unsupported workspace artifact media_type %q", artifact.MediaType)
	}
	if strings.TrimSpace(artifact.Encoding) != workspace.ArtifactEncoding {
		return fmt.Errorf("unsupported workspace artifact encoding %q", artifact.Encoding)
	}
	if strings.TrimSpace(workspaceSpec.VolumeKind) != workspace.VolumeKind {
		return fmt.Errorf("unsupported workspace volume_kind %q", workspaceSpec.VolumeKind)
	}
	if strings.TrimSpace(workspaceSpec.ProjectPath) != strings.TrimSpace(workspaceSpec.Path) {
		return fmt.Errorf("workspace project_path %q must match workspace path %q", workspaceSpec.ProjectPath, workspaceSpec.Path)
	}
	if !workspaceSpec.Writable {
		return errors.New("workspace volume must be writable")
	}
	return nil
}
