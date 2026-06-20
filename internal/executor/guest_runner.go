package executor

import (
	"bytes"
	"context"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"os"
	"strings"
	"time"

	"github.com/helmrdotdev/helmr/internal/api"
	"github.com/helmrdotdev/helmr/internal/archive"
	"github.com/helmrdotdev/helmr/internal/cas"
	"github.com/helmrdotdev/helmr/internal/checkpoint"
	"github.com/helmrdotdev/helmr/internal/proto/run/v0"
	"github.com/helmrdotdev/helmr/internal/transport"
	"github.com/helmrdotdev/helmr/internal/vm"
	"github.com/helmrdotdev/helmr/internal/workspace"
	"golang.org/x/sync/errgroup"
	"google.golang.org/protobuf/proto"
)

var (
	restoreAttachTimeout     = 30 * time.Second
	checkpointSuspendTimeout = 5 * time.Minute
)

const (
	waitMetadataJSONMaxBytes = 64 * 1024
	waitTagsMaxCount         = 32
	waitTagMaxBytes          = 128
)

type GuestRunner struct {
	Connector           vm.Connector
	CAS                 cas.Store
	CheckpointEncryptor *checkpoint.Encryptor
	Events              RuntimeEventSink
	TempDir             string
	Stdout              io.Writer
	Stderr              io.Writer
}

type RuntimeEventSink interface {
	AppendLog(context.Context, api.WorkerRunLease, api.WorkerLogStream, uint64, []byte) (api.WorkerEventResponse, error)
	RecordLogEntry(context.Context, api.WorkerRunLease, string) (api.WorkerEventResponse, error)
	WriteOutput(context.Context, api.WorkerWriteOutputRequest) (api.WorkerEventResponse, error)
	UpdateRunMetadata(context.Context, api.WorkerUpdateRunMetadataRequest) (api.WorkerEventResponse, error)
	CreateRuntimeWaitpointToken(context.Context, api.WorkerCreateWaitpointTokenRequest) (api.WaitpointTokenResponse, error)
}

func (r GuestRunner) Run(ctx context.Context, request Request) (Result, error) {
	if r.Connector == nil {
		return Result{}, errors.New("guest connector is required")
	}
	if request.Leases == nil {
		return Result{}, errors.New("worker run lease provider is required")
	}
	if request.Run.Restore != nil {
		return r.restore(ctx, request)
	}
	if strings.TrimSpace(request.Artifact.ImageTarPath) == "" {
		return Result{}, errors.New("runtime image artifact is required")
	}
	if strings.TrimSpace(request.DeploymentSource.ProjectRoot) == "" {
		return Result{}, errors.New("checked-out deployment source project root is required")
	}
	deploymentSourceRoot, err := runtimeSourceRoot(request.DeploymentSource)
	if err != nil {
		return Result{}, err
	}
	if strings.TrimSpace(request.Workspace.Path) == "" {
		return Result{}, errors.New("workspace artifact path is required")
	}
	if strings.TrimSpace(request.Workspace.Digest) == "" {
		return Result{}, errors.New("workspace artifact digest is required")
	}
	session, err := r.Connector.Connect(ctx, request.Run.Requirements.Network)
	if err != nil {
		return Result{}, fmt.Errorf("connect guest runtime: %w", err)
	}
	defer session.Close(context.Background())
	stream := session.Stream()
	inputMetadata, err := r.writeRuntimeInput(ctx, stream, request, deploymentSourceRoot)
	if err != nil {
		return Result{}, err
	}
	return r.readRunEvents(ctx, session, request, inputMetadata)
}

func (r GuestRunner) restore(ctx context.Context, request Request) (Result, error) {
	if request.Leases == nil {
		return Result{}, errors.New("worker run lease provider is required")
	}
	restoring, ok := r.Connector.(vm.RestoringConnector)
	if !ok {
		return Result{}, errors.New("guest connector does not support checkpoint restore")
	}
	if r.CAS == nil {
		return Result{}, errors.New("checkpoint CAS is required")
	}
	restore := request.Run.Restore
	if strings.TrimSpace(restore.CheckpointID) == "" {
		return Result{}, errors.New("restore checkpoint_id is required")
	}
	if strings.TrimSpace(restore.Waitpoint.ID) == "" {
		return Result{}, errors.New("restore waitpoint id is required")
	}
	if err := validateRestoreIdentity(restore.Checkpoint); err != nil {
		return Result{}, err
	}
	runtimeState := restore.Checkpoint.RuntimeState
	if err := requireCheckpointArtifact(runtimeState.VMStateArtifact, "runtime_state.vm_state_artifact"); err != nil {
		return Result{}, err
	}
	if err := requireCheckpointArtifact(runtimeState.ScratchDiskArtifact, "runtime_state.scratch_disk_artifact"); err != nil {
		return Result{}, err
	}
	if len(runtimeState.MemoryArtifacts) != 1 {
		return Result{}, fmt.Errorf("restore checkpoint requires exactly one memory artifact, got %d", len(runtimeState.MemoryArtifacts))
	}
	if err := requireCheckpointArtifact(runtimeState.MemoryArtifacts[0], "runtime_state.memory_artifacts[0]"); err != nil {
		return Result{}, err
	}
	configArtifact := runtimeState.ConfigArtifact
	stateArtifact := runtimeState.VMStateArtifact
	scratchArtifact := runtimeState.ScratchDiskArtifact
	memoryArtifact := runtimeState.MemoryArtifacts[0]
	var manifestPath string
	var state string
	var scratchDisk string
	var memory string
	group, groupCtx := errgroup.WithContext(ctx)
	group.SetLimit(4)
	group.Go(func() error {
		path, err := r.materializeCheckpointObject(groupCtx, configArtifact.Digest, "manifest")
		if err != nil {
			return err
		}
		manifestPath = path
		return nil
	})
	group.Go(func() error {
		path, err := r.materializeCheckpointObject(groupCtx, stateArtifact.Digest, "vmstate")
		if err != nil {
			return err
		}
		state = path
		return nil
	})
	group.Go(func() error {
		path, err := r.materializeCheckpointObject(groupCtx, scratchArtifact.Digest, "scratch-disk")
		if err != nil {
			return err
		}
		scratchDisk = path
		return nil
	})
	group.Go(func() error {
		path, err := r.materializeCheckpointObject(groupCtx, memoryArtifact.Digest, "memory")
		if err != nil {
			return err
		}
		memory = path
		return nil
	})
	if err := group.Wait(); err != nil {
		removeFiles([]string{manifestPath, state, scratchDisk, memory})
		return Result{}, err
	}
	defer removeFiles([]string{manifestPath, state, scratchDisk, memory})
	manifest, err := os.ReadFile(manifestPath)
	if err != nil {
		return Result{}, fmt.Errorf("read checkpoint manifest: %w", err)
	}
	runtimeInfo := restore.Checkpoint.RecoveryPoint.Runtime
	session, err := restoring.Restore(ctx, vm.RestoreRequest{
		ID:                   restore.CheckpointID,
		VMState:              state,
		VMStateMediaType:     stateArtifact.MediaType,
		ScratchDisk:          scratchDisk,
		ScratchDiskMediaType: scratchArtifact.MediaType,
		Memory:               []string{memory},
		MemoryMediaTypes:     []string{memoryArtifact.MediaType},
		Manifest:             manifest,
		Checkpoint: vm.CheckpointIdentity{
			RuntimeBackend:      runtimeInfo.Backend,
			RuntimeID:           runtimeInfo.ID,
			RuntimeArch:         runtimeInfo.Arch,
			RuntimeABI:          runtimeInfo.ABI,
			KernelDigest:        runtimeInfo.KernelDigest,
			InitramfsDigest:     runtimeInfo.InitramfsDigest,
			RootfsDigest:        runtimeInfo.RootfsDigest,
			RuntimeConfigDigest: runtimeInfo.ConfigDigest,
		},
		Network: request.Run.Requirements.Network,
	})
	if err != nil {
		return Result{}, fmt.Errorf("restore guest runtime: %w", err)
	}
	defer session.Close(context.Background())
	if err := r.attachAndAcknowledgeRestore(ctx, session, request); err != nil {
		return Result{}, err
	}
	return r.readRunEvents(ctx, session, request, runtimeInputMetadata{workspaceBase: restore.Checkpoint.WorkspaceState.Base})
}

func (r GuestRunner) tempDir() string {
	if strings.TrimSpace(r.TempDir) != "" {
		return r.TempDir
	}
	return os.TempDir()
}

func (r GuestRunner) attachAndAcknowledgeRestore(ctx context.Context, session vm.Session, request Request) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	acknowledger, ok := request.WaitHandler.(RestoreAcknowledger)
	if !ok {
		return errors.New("restore acknowledger is required")
	}
	stream := session.Stream()
	restore := request.Run.Restore
	if err := transport.WriteProtoFrame(stream, &runv0.ResumeAttach{
		CheckpointId: restore.CheckpointID,
		WaitpointId:  restore.Waitpoint.ID,
		RunLeaseId:   currentRunLease(request).ID,
	}); err != nil {
		return fmt.Errorf("write resume attach: %w", err)
	}
	if err := transport.WriteProtoFrame(stream, &runv0.ResumeDecision{
		WaitpointId:        restore.Waitpoint.ID,
		Kind:               restore.Waitpoint.ResumeKind,
		DataJson:           string(restore.Waitpoint.ResumePayloadJSON),
		RequireConsumedAck: true,
	}); err != nil {
		return fmt.Errorf("write resume decision: %w", err)
	}
	ackCtx, cancelAck := context.WithTimeout(ctx, restoreAttachTimeout)
	ack, err := readResumeAck(ackCtx, session)
	cancelAck()
	if err != nil {
		return fmt.Errorf("read resume ack: %w", err)
	}
	if ack.WaitpointId != restore.Waitpoint.ID {
		return fmt.Errorf("resume ack waitpoint %q did not match expected %q", ack.WaitpointId, restore.Waitpoint.ID)
	}
	if err := acknowledger.AcknowledgeRestore(ctx, RestoreAcknowledgement{
		Lease:           currentRunLease(request),
		RunSuspensionID: restore.Waitpoint.RunSuspensionID,
		WaitpointID:     restore.Waitpoint.ID,
		CheckpointID:    restore.CheckpointID,
	}); err != nil {
		return fmt.Errorf("acknowledge restore: %w", err)
	}
	return nil
}

func readResumeAck(ctx context.Context, session vm.Session) (*runv0.ResumeAck, error) {
	var ack runv0.ResumeAck
	if err := readProtoFrameContext(ctx, session, &ack); err != nil {
		return nil, err
	}
	return &ack, nil
}

func removeFiles(paths []string) {
	for _, path := range paths {
		if strings.TrimSpace(path) == "" {
			continue
		}
		_ = os.Remove(path)
	}
}

func readProtoFrameContext(ctx context.Context, session vm.Session, message proto.Message) error {
	return readProtoFrameFromReaderContext(ctx, session, session.Stream(), message)
}

func readProtoFrameFromReaderContext(ctx context.Context, session vm.Session, reader io.Reader, message proto.Message) error {
	result := make(chan error, 1)
	go func() {
		result <- transport.ReadProtoFrame(reader, message)
	}()
	select {
	case err := <-result:
		return err
	case <-ctx.Done():
		_ = session.Close(context.Background())
		return ctx.Err()
	}
}

type runEventReadResult struct {
	event     *runv0.RunEvent
	workspace *workspace.WorkspaceArtifact
	err       error
}

func (r GuestRunner) readRunEventContext(ctx context.Context, session vm.Session, runID string) (*runv0.RunEvent, *workspace.WorkspaceArtifact, error) {
	result := make(chan runEventReadResult, 1)
	go func() {
		event, artifact, err := r.readRunEventOrWorkspaceFrame(ctx, session.Stream(), runID)
		result <- runEventReadResult{event: event, workspace: artifact, err: err}
	}()
	select {
	case value := <-result:
		return value.event, value.workspace, value.err
	case <-ctx.Done():
		_ = session.Close(context.Background())
		return nil, nil, ctx.Err()
	}
}

func (r GuestRunner) readRunEventOrWorkspaceFrame(ctx context.Context, reader io.Reader, runID string) (*runv0.RunEvent, *workspace.WorkspaceArtifact, error) {
	var prefix [4]byte
	if _, err := io.ReadFull(reader, prefix[:]); err != nil {
		return nil, nil, err
	}
	if transport.IsStreamFramePrefix(prefix[:]) {
		header, bodyLen, err := transport.ReadStreamFrameHeader(io.MultiReader(bytes.NewReader(prefix[:]), reader))
		if err != nil {
			return nil, nil, err
		}
		artifact, err := r.storeWorkspaceArtifactFrame(ctx, reader, header, bodyLen, runID)
		if err != nil {
			return nil, nil, err
		}
		return nil, &artifact, nil
	}
	size := binary.BigEndian.Uint32(prefix[:])
	if size > transport.MaxFrameBytes {
		return nil, nil, fmt.Errorf("transport message frame length %d exceeds max %d", size, transport.MaxFrameBytes)
	}
	body := make([]byte, size)
	if _, err := io.ReadFull(reader, body); err != nil {
		return nil, nil, err
	}
	var event runv0.RunEvent
	if err := proto.Unmarshal(body, &event); err != nil {
		return nil, nil, fmt.Errorf("unmarshal transport proto frame: %w", err)
	}
	return &event, nil, nil
}

func (r GuestRunner) storeWorkspaceArtifactFrame(ctx context.Context, reader io.Reader, header transport.StreamHeader, bodyLen uint64, runID string) (workspace.WorkspaceArtifact, error) {
	if header.Type != transport.StreamTypeWorkspaceArtifact {
		return workspace.WorkspaceArtifact{}, fmt.Errorf("unsupported runtime stream type %q", header.Type)
	}
	if strings.TrimSpace(header.RunID) != strings.TrimSpace(runID) {
		return workspace.WorkspaceArtifact{}, fmt.Errorf("workspace artifact run_id %q did not match run %q", header.RunID, runID)
	}
	if header.BodyDigest == nil || strings.TrimSpace(*header.BodyDigest) == "" {
		return workspace.WorkspaceArtifact{}, errors.New("workspace artifact frame body_digest is required")
	}
	if header.EntryCount == nil {
		return workspace.WorkspaceArtifact{}, errors.New("workspace artifact frame entry_count is required")
	}
	if *header.EntryCount < 0 {
		return workspace.WorkspaceArtifact{}, errors.New("workspace artifact frame entry_count must be non-negative")
	}
	if *header.EntryCount > workspace.MaxArtifactEntries {
		return workspace.WorkspaceArtifact{}, fmt.Errorf("workspace artifact entry_count %d exceeds max %d", *header.EntryCount, workspace.MaxArtifactEntries)
	}
	if bodyLen == 0 {
		return workspace.WorkspaceArtifact{}, errors.New("workspace artifact frame body is required")
	}
	if bodyLen > uint64(workspace.MaxArtifactArchiveBytes) {
		return workspace.WorkspaceArtifact{}, fmt.Errorf("workspace artifact size_bytes %d exceeds max %d", bodyLen, workspace.MaxArtifactArchiveBytes)
	}
	if r.CAS == nil {
		return workspace.WorkspaceArtifact{}, errors.New("workspace artifact CAS is required")
	}
	body := &io.LimitedReader{R: reader, N: int64(bodyLen)}
	object, err := r.CAS.Put(ctx, workspace.ArtifactMediaType, body)
	if err != nil {
		_, _ = io.Copy(io.Discard, body)
		return workspace.WorkspaceArtifact{}, fmt.Errorf("put workspace artifact: %w", err)
	}
	if body.N > 0 {
		if _, err := io.Copy(io.Discard, body); err != nil {
			return workspace.WorkspaceArtifact{}, fmt.Errorf("drain workspace artifact: %w", err)
		}
	}
	if object.Digest != strings.TrimSpace(*header.BodyDigest) {
		return workspace.WorkspaceArtifact{}, fmt.Errorf("workspace artifact digest mismatch: got %s, want %s", object.Digest, *header.BodyDigest)
	}
	if object.SizeBytes != int64(bodyLen) {
		return workspace.WorkspaceArtifact{}, fmt.Errorf("workspace artifact size mismatch: got %d, want %d", object.SizeBytes, bodyLen)
	}
	if strings.TrimSpace(object.MediaType) != workspace.ArtifactMediaType {
		return workspace.WorkspaceArtifact{}, fmt.Errorf("workspace artifact media_type mismatch: got %q, want %q", object.MediaType, workspace.ArtifactMediaType)
	}
	return workspace.WorkspaceArtifact{
		Digest:     object.Digest,
		MediaType:  object.MediaType,
		Encoding:   workspace.ArtifactEncoding,
		SizeBytes:  object.SizeBytes,
		EntryCount: *header.EntryCount,
	}, nil
}

type activeRuntimeClock struct {
	limit     time.Duration
	used      time.Duration
	startedAt time.Time
}

func newActiveRuntimeClock(limit time.Duration, used time.Duration) activeRuntimeClock {
	if used < 0 {
		used = 0
	}
	return activeRuntimeClock{limit: limit, used: used, startedAt: time.Now()}
}

func (c activeRuntimeClock) elapsed() time.Duration {
	return c.used + time.Since(c.startedAt)
}

func (c activeRuntimeClock) readContext(ctx context.Context) (context.Context, context.CancelFunc, bool, error) {
	if c.limit <= 0 {
		return ctx, func() {}, false, nil
	}
	remaining := c.limit - c.elapsed()
	if remaining <= 0 {
		return nil, nil, true, context.DeadlineExceeded
	}
	readCtx, cancel := context.WithTimeout(ctx, remaining)
	return readCtx, cancel, true, nil
}

type runtimeInputMetadata struct {
	workspaceBase api.WorkerCheckpointWorkspaceBase
}

func (r GuestRunner) writeRuntimeInput(ctx context.Context, stream io.Writer, request Request, deploymentSourceRoot string) (runtimeInputMetadata, error) {
	if err := ctx.Err(); err != nil {
		return runtimeInputMetadata{}, err
	}
	protocolRequest, err := runTaskRequest(request)
	if err != nil {
		return runtimeInputMetadata{}, err
	}
	if err := transport.WriteFileFrame(stream, transport.StreamHeader{Type: transport.StreamTypeRunImage, RunID: request.Run.RunID}, request.Artifact.ImageTarPath); err != nil {
		return runtimeInputMetadata{}, fmt.Errorf("write run image: %w", err)
	}
	deploymentSourceTar, cleanupDeploymentSource, err := archive.CreateTarWithOptions(deploymentSourceRoot, r.TempDir, archive.TarOptions{
		ExcludePatterns: []string{"**/.git/**"},
	})
	if err != nil {
		return runtimeInputMetadata{}, err
	}
	defer cleanupDeploymentSource()
	if err := transport.WriteFileFrame(stream, transport.StreamHeader{Type: transport.StreamTypeDeploymentSource, RunID: request.Run.RunID}, deploymentSourceTar.Path); err != nil {
		return runtimeInputMetadata{}, fmt.Errorf("write deployment source: %w", err)
	}
	if err := transport.WriteProtoFrame(stream, protocolRequest); err != nil {
		return runtimeInputMetadata{}, fmt.Errorf("write run request: %w", err)
	}
	if err := transport.WriteFileFrame(stream, transport.StreamHeader{Type: transport.StreamTypeWorkspaceArtifact, RunID: request.Run.RunID}, request.Workspace.Path); err != nil {
		return runtimeInputMetadata{}, fmt.Errorf("write workspace artifact: %w", err)
	}
	return runtimeInputMetadata{workspaceBase: checkpointWorkspaceBase(request, protocolRequest)}, nil
}

func checkpointWorkspaceBase(request Request, protocolRequest *runv0.RunTaskRequest) api.WorkerCheckpointWorkspaceBase {
	workspace := protocolRequest.GetWorkspace()
	base := api.WorkerCheckpointWorkspaceBase{
		ArtifactDigest:    request.Workspace.Digest,
		ArtifactSizeBytes: request.Workspace.SizeBytes,
		ArtifactMediaType: request.Workspace.MediaType,
		ArtifactEncoding:  request.Workspace.Encoding,
	}
	if workspace != nil {
		base.MountPath = workspace.Path
		if workspace.Artifact != nil {
			base.ArtifactDigest = workspace.Artifact.Digest
			base.ArtifactSizeBytes = int64(workspace.Artifact.SizeBytes)
			base.ArtifactMediaType = workspace.Artifact.MediaType
			base.ArtifactEncoding = workspace.Artifact.Encoding
		}
	}
	return base
}

func (r GuestRunner) readRunEvents(ctx context.Context, session vm.Session, request Request, inputMetadata runtimeInputMetadata) (Result, error) {
	stream := session.Stream()
	active := newActiveRuntimeClock(request.Run.MaxDuration, request.Run.ActiveUsed)
	var observedSeq uint64
	var finalWorkspace *workspace.WorkspaceArtifact
	for {
		if err := ctx.Err(); err != nil {
			return Result{}, err
		}
		readCtx, cancelRead, activeLimited, err := active.readContext(ctx)
		if err != nil {
			return Result{}, runtimeMaxDurationError(request.Run.MaxDuration)
		}
		event, workspaceArtifact, err := r.readRunEventContext(readCtx, session, request.Run.RunID)
		cancelRead()
		if err != nil {
			if activeLimited && errors.Is(err, context.DeadlineExceeded) && ctx.Err() == nil {
				return Result{}, runtimeMaxDurationError(request.Run.MaxDuration)
			}
			if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
				return Result{}, err
			}
			return Result{}, fmt.Errorf("read run event: %w", err)
		}
		if workspaceArtifact != nil {
			if finalWorkspace != nil {
				return Result{}, errors.New("guest published multiple final workspace artifacts")
			}
			finalWorkspace = workspaceArtifact
			continue
		}
		observedSeq++
		switch value := event.Event.(type) {
		case *runv0.RunEvent_StdoutChunk:
			if r.Stdout != nil {
				if _, err := r.Stdout.Write(value.StdoutChunk); err != nil {
					return Result{}, fmt.Errorf("write stdout event: %w", err)
				}
			}
			if err := r.appendLog(ctx, currentRunLease(request), api.WorkerLogStreamStdout, observedSeq, value.StdoutChunk); err != nil {
				return Result{}, err
			}
		case *runv0.RunEvent_StderrChunk:
			if r.Stderr != nil {
				if _, err := r.Stderr.Write(value.StderrChunk); err != nil {
					return Result{}, fmt.Errorf("write stderr event: %w", err)
				}
			}
			if err := r.appendLog(ctx, currentRunLease(request), api.WorkerLogStreamStderr, observedSeq, value.StderrChunk); err != nil {
				return Result{}, err
			}
		case *runv0.RunEvent_LogEntry:
			if err := r.recordLogEntry(ctx, currentRunLease(request), value.LogEntry); err != nil {
				return Result{}, err
			}
		case *runv0.RunEvent_ChannelOutputAppended:
			if err := r.writeChannelOutput(ctx, currentRunLease(request), value.ChannelOutputAppended); err != nil {
				return Result{}, err
			}
		case *runv0.RunEvent_MetadataUpdated:
			if err := r.updateRunMetadata(ctx, currentRunLease(request), value.MetadataUpdated); err != nil {
				return Result{}, err
			}
		case *runv0.RunEvent_WaitpointTokenCreateRequested:
			result := r.createWaitpointToken(ctx, currentRunLease(request), value.WaitpointTokenCreateRequested)
			if err := transport.WriteProtoFrame(stream, result); err != nil {
				return Result{}, fmt.Errorf("write waitpoint token create result: %w", err)
			}
		case *runv0.RunEvent_WaitpointRequested:
			if err := r.handleWaitpointRequested(ctx, stream, session, request, value.WaitpointRequested, active.elapsed(), inputMetadata, &observedSeq); err != nil {
				if errors.Is(err, ErrDetached) {
					return Result{Detached: true, ActiveDuration: active.elapsed()}, nil
				}
				return Result{}, err
			}
		case *runv0.RunEvent_TaskResult:
			if value.TaskResult == nil {
				return Result{}, errors.New("guest task_result event is empty")
			}
			if strings.TrimSpace(value.TaskResult.GetErrorMessage()) != "" {
				return Result{}, errors.New(value.TaskResult.GetErrorMessage())
			}
			result := Result{ExitCode: value.TaskResult.ExitCode, ActiveDuration: active.elapsed()}
			if value.TaskResult.ExitCode == 0 && value.TaskResult.OutputJson != nil {
				output := json.RawMessage(value.TaskResult.GetOutputJson())
				if !json.Valid(output) {
					return Result{}, errors.New("guest task_result output_json must be valid JSON")
				}
				result.Output = append(json.RawMessage(nil), output...)
			}
			if value.TaskResult.ExitCode == 0 {
				result.Workspace = finalWorkspace
			}
			return result, nil
		case nil:
			return Result{}, errors.New("guest run event is empty")
		default:
			return Result{}, fmt.Errorf("unsupported guest run event %T", value)
		}
	}
}

func (r GuestRunner) processCheckpointRunEvent(ctx context.Context, request Request, observedSeq *uint64, event *runv0.RunEvent) error {
	if observedSeq == nil {
		return errors.New("checkpoint run event sequence is required")
	}
	(*observedSeq)++
	switch value := event.GetEvent().(type) {
	case *runv0.RunEvent_StdoutChunk:
		if r.Stdout != nil {
			if _, err := r.Stdout.Write(value.StdoutChunk); err != nil {
				return fmt.Errorf("write stdout event: %w", err)
			}
		}
		return r.appendLog(ctx, currentRunLease(request), api.WorkerLogStreamStdout, *observedSeq, value.StdoutChunk)
	case *runv0.RunEvent_StderrChunk:
		if r.Stderr != nil {
			if _, err := r.Stderr.Write(value.StderrChunk); err != nil {
				return fmt.Errorf("write stderr event: %w", err)
			}
		}
		return r.appendLog(ctx, currentRunLease(request), api.WorkerLogStreamStderr, *observedSeq, value.StderrChunk)
	case *runv0.RunEvent_LogEntry:
		return r.recordLogEntry(ctx, currentRunLease(request), value.LogEntry)
	case *runv0.RunEvent_ChannelOutputAppended:
		return r.writeChannelOutput(ctx, currentRunLease(request), value.ChannelOutputAppended)
	case *runv0.RunEvent_MetadataUpdated:
		return r.updateRunMetadata(ctx, currentRunLease(request), value.MetadataUpdated)
	case *runv0.RunEvent_WaitpointTokenCreateRequested:
		return errors.New("waitpoint token creation is not supported while checkpointing")
	case *runv0.RunEvent_WaitpointRequested:
		appender, ok := request.WaitHandler.(WaitpointAppender)
		if !ok {
			return errors.New("aggregate waitpoint creation is not supported by this wait handler")
		}
		runtimeWait, err := runtimeWaitRequest(request, value.WaitpointRequested)
		if err != nil {
			return err
		}
		runtimeWait.Lease = currentRunLease(request)
		opened, err := appender.AddWaitpoint(ctx, runtimeWait)
		if err != nil {
			return fmt.Errorf("add aggregate waitpoint: %w", err)
		}
		if strings.TrimSpace(opened.ResolutionKind) != "" {
			return errors.New("aggregate waitpoint resolved before checkpoint was ready")
		}
		return nil
	case nil:
		return errors.New("guest run event is empty")
	default:
		return fmt.Errorf("unsupported checkpoint interleaved guest run event %T", value)
	}
}

type MaxDurationError struct {
	Limit time.Duration
}

func (e MaxDurationError) Error() string {
	return fmt.Sprintf("runtime max_duration exceeded after %s active time", e.Limit)
}

func runtimeMaxDurationError(limit time.Duration) error {
	return MaxDurationError{Limit: limit}
}

func (r GuestRunner) appendLog(ctx context.Context, claim api.WorkerRunLease, stream api.WorkerLogStream, observedSeq uint64, content []byte) error {
	if r.Events == nil {
		return nil
	}
	if _, err := r.Events.AppendLog(ctx, claim, stream, observedSeq, content); err != nil {
		return fmt.Errorf("append %s log: %w", stream, err)
	}
	return nil
}

func (r GuestRunner) recordLogEntry(ctx context.Context, claim api.WorkerRunLease, entry string) error {
	if r.Events == nil {
		return nil
	}
	if _, err := r.Events.RecordLogEntry(ctx, claim, entry); err != nil {
		return fmt.Errorf("record log entry: %w", err)
	}
	return nil
}

func (r GuestRunner) writeChannelOutput(ctx context.Context, claim api.WorkerRunLease, output *runv0.ChannelOutputAppended) error {
	if r.Events == nil {
		return nil
	}
	if output == nil {
		return errors.New("guest channel_output_appended is empty")
	}
	channel := strings.TrimSpace(output.GetChannel())
	if channel == "" {
		return errors.New("guest channel_output_appended channel is required")
	}
	payload := json.RawMessage(output.GetPayloadJson())
	if len(payload) == 0 {
		payload = json.RawMessage(`null`)
	}
	if !json.Valid(payload) {
		return errors.New("guest channel_output_appended payload_json must be valid JSON")
	}
	objectRef := json.RawMessage(output.GetObjectRefJson())
	if len(objectRef) > 0 && !json.Valid(objectRef) {
		return errors.New("guest channel_output_appended object_ref_json must be valid JSON")
	}
	if _, err := r.Events.WriteOutput(ctx, api.WorkerWriteOutputRequest{
		Lease:       claim,
		Channel:     channel,
		Payload:     payload,
		ContentType: output.GetContentType(),
		ObjectRef:   objectRef,
	}); err != nil {
		return fmt.Errorf("write channel output %q: %w", channel, err)
	}
	return nil
}

func (r GuestRunner) createWaitpointToken(ctx context.Context, lease api.WorkerRunLease, request *runv0.WaitpointTokenCreateRequested) *runv0.WaitpointTokenCreateResult {
	if r.Events == nil {
		return &runv0.WaitpointTokenCreateResult{ErrorMessage: ptrString("runtime event sink is required")}
	}
	if request == nil {
		return &runv0.WaitpointTokenCreateResult{ErrorMessage: ptrString("guest waitpoint_token_create_requested is empty")}
	}
	var timeoutAt *time.Time
	if strings.TrimSpace(request.GetTimeoutAt()) != "" {
		parsed, err := time.Parse(time.RFC3339, strings.TrimSpace(request.GetTimeoutAt()))
		if err != nil {
			return &runv0.WaitpointTokenCreateResult{ErrorMessage: ptrString("waitpoint token timeout_at must be an RFC3339 timestamp")}
		}
		timeoutAt = &parsed
	}
	var timeoutInSeconds *int32
	if request.TimeoutInSeconds != nil {
		if request.GetTimeoutInSeconds() > math.MaxInt32 {
			return &runv0.WaitpointTokenCreateResult{ErrorMessage: ptrString("waitpoint token timeout_in_seconds is too large")}
		}
		value := int32(request.GetTimeoutInSeconds())
		timeoutInSeconds = &value
	}
	metadata := json.RawMessage(request.GetMetadataJson())
	if len(metadata) == 0 {
		metadata = json.RawMessage(`{}`)
	}
	if !json.Valid(metadata) {
		return &runv0.WaitpointTokenCreateResult{ErrorMessage: ptrString("waitpoint token metadata_json must be valid JSON")}
	}
	token, err := r.Events.CreateRuntimeWaitpointToken(ctx, api.WorkerCreateWaitpointTokenRequest{
		Lease: lease,
		CreateWaitpointTokenRequest: api.CreateWaitpointTokenRequest{
			TimeoutAt:        timeoutAt,
			TimeoutInSeconds: timeoutInSeconds,
			Tags:             request.GetTags(),
			Metadata:         metadata,
		},
	})
	if err != nil {
		return &runv0.WaitpointTokenCreateResult{ErrorMessage: ptrString(err.Error())}
	}
	result := &runv0.WaitpointTokenCreateResult{
		Id:          token.ID,
		CallbackUrl: token.CallbackURL,
		TimeoutAt:   timePtrString(token.TimeoutAt),
		Status:      optionalString(token.Status),
		Tags:        token.Tags,
	}
	if strings.TrimSpace(token.PublicAccessToken) != "" {
		result.PublicAccessToken = ptrString(token.PublicAccessToken)
	}
	if len(token.Metadata) > 0 {
		result.MetadataJson = ptrString(string(token.Metadata))
	}
	return result
}

func ptrString(value string) *string {
	return &value
}

func optionalString(value string) *string {
	value = strings.TrimSpace(value)
	if value == "" {
		return nil
	}
	return &value
}

func timePtrString(value *time.Time) *string {
	if value == nil {
		return nil
	}
	formatted := value.UTC().Format(time.RFC3339Nano)
	return &formatted
}

func (r GuestRunner) updateRunMetadata(ctx context.Context, claim api.WorkerRunLease, metadata *runv0.MetadataUpdated) error {
	if r.Events == nil {
		return nil
	}
	if metadata == nil {
		return errors.New("guest metadata_updated is empty")
	}
	operation := strings.TrimSpace(metadata.GetOperation())
	if operation == "" {
		return errors.New("guest metadata_updated operation is required")
	}
	request := api.WorkerUpdateRunMetadataRequest{
		Lease:     claim,
		Operation: operation,
		Key:       strings.TrimSpace(metadata.GetKey()),
		Amount:    metadata.GetAmount(),
	}
	if metadata.GetValueJson() != "" {
		value := json.RawMessage(metadata.GetValueJson())
		if !json.Valid(value) {
			return errors.New("guest metadata_updated value_json must be valid JSON")
		}
		request.Value = value
	}
	if metadata.GetPatchJson() != "" {
		patch := json.RawMessage(metadata.GetPatchJson())
		if !json.Valid(patch) {
			return errors.New("guest metadata_updated patch_json must be valid JSON")
		}
		request.Patch = patch
	}
	if _, err := r.Events.UpdateRunMetadata(ctx, request); err != nil {
		return fmt.Errorf("update run metadata: %w", err)
	}
	return nil
}

func (r GuestRunner) handleWaitpointRequested(ctx context.Context, stream io.ReadWriteCloser, session vm.Session, request Request, wait *runv0.WaitpointRequested, activeDuration time.Duration, inputMetadata runtimeInputMetadata, observedSeq *uint64) error {
	if request.WaitHandler == nil {
		return errors.New("guest wait request requires a waitpoint handler")
	}
	runtimeWait, err := runtimeWaitRequest(request, wait)
	if err != nil {
		return err
	}
	runtimeWait.Lease = currentRunLease(request)
	runtimeWait.ActiveDuration = activeDuration
	resumeSent := false
	runtimeWait.Resume = func(resumeCtx context.Context, decision WaitResumeDecision) error {
		if strings.TrimSpace(decision.Kind) == "" {
			return errors.New("waitpoint resume kind is required")
		}
		if len(decision.Data) == 0 {
			decision.Data = json.RawMessage(`null`)
		}
		if err := transport.WriteProtoFrame(stream, &runv0.ResumeDecision{
			WaitpointId: wait.CorrelationId,
			Kind:        decision.Kind,
			DataJson:    string(decision.Data),
		}); err != nil {
			return fmt.Errorf("write immediate resume decision: %w", err)
		}
		resumeSent = true
		return resumeCtx.Err()
	}
	if checkpointable, ok := session.(vm.CheckpointableSession); ok {
		runtimeWait.Checkpointer = runtimeCheckpointer{
			session:   checkpointable,
			cas:       r.CAS,
			encryptor: r.CheckpointEncryptor,
			tempDir:   r.tempDir(),
			stream:    stream,
			workspace: inputMetadata.workspaceBase,
			runEvent: func(eventCtx context.Context, event *runv0.RunEvent) error {
				return r.processCheckpointRunEvent(eventCtx, request, observedSeq, event)
			},
		}
	}
	if err := request.WaitHandler.Wait(ctx, runtimeWait); err != nil {
		return err
	}
	if resumeSent {
		return nil
	}
	return errors.New("waitpoint handler returned without detaching runtime")
}

func currentRunLease(request Request) api.WorkerRunLease {
	return request.Leases.CurrentWorkerRunLease()
}

func runtimeWaitRequest(request Request, wait *runv0.WaitpointRequested) (WaitRequest, error) {
	if wait == nil {
		return WaitRequest{}, errors.New("guest wait request is empty")
	}
	correlationID := strings.TrimSpace(wait.GetCorrelationId())
	if correlationID == "" {
		return WaitRequest{}, errors.New("guest wait request correlation_id is required")
	}
	kind := api.WorkerWaitpointKind(strings.TrimSpace(wait.GetKind()))
	if kind == "" {
		return WaitRequest{}, errors.New("guest wait request kind is required")
	}
	paramsJSON := strings.TrimSpace(wait.GetParamsJson())
	if paramsJSON == "" {
		paramsJSON = "{}"
	}
	if !json.Valid([]byte(paramsJSON)) {
		return WaitRequest{}, errors.New("guest wait params_json must be valid JSON")
	}
	metadataJSON := strings.TrimSpace(wait.GetMetadataJson())
	if metadataJSON == "" {
		metadataJSON = "{}"
	}
	if !json.Valid([]byte(metadataJSON)) {
		return WaitRequest{}, errors.New("guest wait metadata_json must be valid JSON")
	}
	var metadataCompact bytes.Buffer
	if err := json.Compact(&metadataCompact, []byte(metadataJSON)); err != nil {
		return WaitRequest{}, fmt.Errorf("guest wait metadata_json must be valid JSON: %w", err)
	}
	if metadataCompact.Len() > waitMetadataJSONMaxBytes {
		return WaitRequest{}, fmt.Errorf("guest wait metadata_json is %d bytes, exceeds max %d", metadataCompact.Len(), waitMetadataJSONMaxBytes)
	}
	if !waitMetadataJSONObject([]byte(metadataJSON)) {
		return WaitRequest{}, errors.New("guest wait metadata_json must be a JSON object")
	}
	tags, err := normalizeRuntimeWaitTags(wait.GetTags())
	if err != nil {
		return WaitRequest{}, err
	}
	timeout, err := waitTimeoutSeconds(wait.Timeout)
	if err != nil {
		return WaitRequest{}, err
	}
	return WaitRequest{
		Lease:          currentRunLease(request),
		CorrelationID:  correlationID,
		Kind:           kind,
		Params:         []byte(paramsJSON),
		Metadata:       []byte(metadataJSON),
		Tags:           tags,
		TimeoutSeconds: timeout,
		Ordinal:        int32(wait.GetOrdinal()),
	}, nil
}

func normalizeRuntimeWaitTags(tags []string) ([]string, error) {
	if len(tags) > waitTagsMaxCount {
		return nil, fmt.Errorf("guest wait tags has %d entries, exceeds max %d", len(tags), waitTagsMaxCount)
	}
	normalized := make([]string, 0, len(tags))
	for _, tag := range tags {
		tag = strings.TrimSpace(tag)
		if tag == "" {
			return nil, errors.New("guest wait tags must be non-empty")
		}
		if len([]byte(tag)) > waitTagMaxBytes {
			return nil, fmt.Errorf("guest wait tag is %d bytes, exceeds max %d", len([]byte(tag)), waitTagMaxBytes)
		}
		normalized = append(normalized, tag)
	}
	return normalized, nil
}

func waitMetadataJSONObject(value []byte) bool {
	var decoded map[string]json.RawMessage
	if err := json.Unmarshal(value, &decoded); err != nil {
		return false
	}
	return decoded != nil
}

func waitTimeoutSeconds(value *uint32) (*int32, error) {
	if value == nil {
		return nil, nil
	}
	if *value > math.MaxInt32 {
		return nil, fmt.Errorf("wait timeout %d exceeds max %d", *value, int64(math.MaxInt32))
	}
	timeout := int32(*value)
	return &timeout, nil
}

var _ Runner = GuestRunner{}
