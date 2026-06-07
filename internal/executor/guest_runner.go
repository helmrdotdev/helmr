package executor

import (
	"context"
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
	runv0 "github.com/helmrdotdev/helmr/internal/proto/run/v0"
	"github.com/helmrdotdev/helmr/internal/transport"
	"github.com/helmrdotdev/helmr/internal/vm"
	"golang.org/x/sync/errgroup"
	"google.golang.org/protobuf/proto"
)

var (
	restoreAttachTimeout     = 30 * time.Second
	checkpointSuspendTimeout = 5 * time.Minute
)

const maxWaitDisplayTextBytes = 16 * 1024

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
	EmitEvent(context.Context, api.WorkerRunLease, string, json.RawMessage) (api.WorkerEventResponse, error)
}

func (r GuestRunner) Run(ctx context.Context, request Request) (Result, error) {
	if r.Connector == nil {
		return Result{}, errors.New("guest connector is required")
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
	defer session.Close()
	stream := session.Stream()
	inputMetadata, err := r.writeRunInput(ctx, stream, request, deploymentSourceRoot)
	if err != nil {
		return Result{}, err
	}
	return r.readRunEvents(ctx, session, request, inputMetadata)
}

func (r GuestRunner) restore(ctx context.Context, request Request) (Result, error) {
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
	defer session.Close()
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
		SessionId:    request.Lease.ID,
	}); err != nil {
		return fmt.Errorf("write resume attach: %w", err)
	}
	if err := transport.WriteProtoFrame(stream, &runv0.ResumeDecision{
		WaitpointId:       restore.Waitpoint.ID,
		Kind:              restore.Waitpoint.ResumeKind,
		ResumePayloadJson: string(restore.Waitpoint.ResumePayloadJSON),
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
		Lease:        request.Lease,
		RunWaitID:    restore.Waitpoint.RunWaitID,
		WaitpointID:  restore.Waitpoint.ID,
		CheckpointID: restore.CheckpointID,
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
		_ = session.Close()
		return ctx.Err()
	}
}

type runEventReadResult struct {
	event *runv0.RunEvent
	err   error
}

func readRunEventContext(ctx context.Context, session vm.Session) (*runv0.RunEvent, error) {
	result := make(chan runEventReadResult, 1)
	go func() {
		event, err := transport.ReadRunEvent(session.Stream())
		result <- runEventReadResult{event: event, err: err}
	}()
	select {
	case value := <-result:
		return value.event, value.err
	case <-ctx.Done():
		_ = session.Close()
		return nil, ctx.Err()
	}
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

func (r GuestRunner) writeRunInput(ctx context.Context, stream io.Writer, request Request, deploymentSourceRoot string) (runtimeInputMetadata, error) {
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
		VolumeKind:        request.Workspace.VolumeKind,
	}
	if workspace != nil {
		base.MountPath = workspace.Path
		base.VolumeKind = workspace.VolumeKind
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
	for {
		if err := ctx.Err(); err != nil {
			return Result{}, err
		}
		readCtx, cancelRead, activeLimited, err := active.readContext(ctx)
		if err != nil {
			return Result{}, runtimeMaxDurationError(request.Run.MaxDuration)
		}
		event, err := readRunEventContext(readCtx, session)
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
		observedSeq++
		switch value := event.Event.(type) {
		case *runv0.RunEvent_StdoutChunk:
			if r.Stdout != nil {
				if _, err := r.Stdout.Write(value.StdoutChunk); err != nil {
					return Result{}, fmt.Errorf("write stdout event: %w", err)
				}
			}
			if err := r.appendLog(ctx, request.Lease, api.WorkerLogStreamStdout, observedSeq, value.StdoutChunk); err != nil {
				return Result{}, err
			}
		case *runv0.RunEvent_StderrChunk:
			if r.Stderr != nil {
				if _, err := r.Stderr.Write(value.StderrChunk); err != nil {
					return Result{}, fmt.Errorf("write stderr event: %w", err)
				}
			}
			if err := r.appendLog(ctx, request.Lease, api.WorkerLogStreamStderr, observedSeq, value.StderrChunk); err != nil {
				return Result{}, err
			}
		case *runv0.RunEvent_LogEntry:
			if err := r.recordLogEntry(ctx, request.Lease, value.LogEntry); err != nil {
				return Result{}, err
			}
		case *runv0.RunEvent_EmitEvent:
			if value.EmitEvent == nil {
				return Result{}, errors.New("guest emit_event is empty")
			}
			if strings.TrimSpace(value.EmitEvent.Type) == "" {
				return Result{}, errors.New("guest emit_event type is required")
			}
			content := normalizeEmitEventContent(value.EmitEvent.ContentJson)
			if err := r.emitEvent(ctx, request.Lease, value.EmitEvent.Type, content); err != nil {
				return Result{}, err
			}
		case *runv0.RunEvent_WaitRequested:
			if err := r.handleWaitRequested(ctx, stream, session, request, value.WaitRequested, active.elapsed(), inputMetadata, &observedSeq); err != nil {
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
		return r.appendLog(ctx, request.Lease, api.WorkerLogStreamStdout, *observedSeq, value.StdoutChunk)
	case *runv0.RunEvent_StderrChunk:
		if r.Stderr != nil {
			if _, err := r.Stderr.Write(value.StderrChunk); err != nil {
				return fmt.Errorf("write stderr event: %w", err)
			}
		}
		return r.appendLog(ctx, request.Lease, api.WorkerLogStreamStderr, *observedSeq, value.StderrChunk)
	case *runv0.RunEvent_LogEntry:
		return r.recordLogEntry(ctx, request.Lease, value.LogEntry)
	case *runv0.RunEvent_EmitEvent:
		if value.EmitEvent == nil {
			return errors.New("guest emit_event is empty")
		}
		if strings.TrimSpace(value.EmitEvent.Type) == "" {
			return errors.New("guest emit_event type is required")
		}
		return r.emitEvent(ctx, request.Lease, value.EmitEvent.Type, normalizeEmitEventContent(value.EmitEvent.ContentJson))
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

func normalizeEmitEventContent(raw string) json.RawMessage {
	if raw == "" {
		return json.RawMessage(`null`)
	}
	var value any
	if err := json.Unmarshal([]byte(raw), &value); err != nil {
		payload, marshalErr := json.Marshal(map[string]string{
			"parse_error": fmt.Sprintf("invalid emit event content_json: %v", err),
			"raw":         raw,
		})
		if marshalErr != nil {
			return json.RawMessage(`{"parse_error":"invalid emit event content_json"}`)
		}
		return json.RawMessage(payload)
	}
	return json.RawMessage(raw)
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

func (r GuestRunner) emitEvent(ctx context.Context, claim api.WorkerRunLease, eventType string, content json.RawMessage) error {
	if r.Events == nil {
		return nil
	}
	if _, err := r.Events.EmitEvent(ctx, claim, eventType, content); err != nil {
		return fmt.Errorf("emit event %q: %w", eventType, err)
	}
	return nil
}

func (r GuestRunner) handleWaitRequested(ctx context.Context, stream io.ReadWriteCloser, session vm.Session, request Request, wait *runv0.WaitRequested, activeDuration time.Duration, inputMetadata runtimeInputMetadata, observedSeq *uint64) error {
	if request.WaitHandler == nil {
		return errors.New("guest wait request requires a waitpoint handler")
	}
	runtimeWait, err := runtimeWaitRequest(request, wait)
	if err != nil {
		return err
	}
	runtimeWait.ActiveDuration = activeDuration
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
	return errors.New("waitpoint handler returned without detaching runtime")
}

func runtimeWaitRequest(request Request, wait *runv0.WaitRequested) (WaitRequest, error) {
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
	requestJSON := strings.TrimSpace(wait.GetRequestJson())
	if requestJSON == "" {
		requestJSON = "{}"
	}
	if !json.Valid([]byte(requestJSON)) {
		return WaitRequest{}, errors.New("guest wait request_json must be valid JSON")
	}
	timeout, err := waitTimeoutSeconds(wait.Timeout)
	if err != nil {
		return WaitRequest{}, err
	}
	displayText := strings.TrimSpace(wait.GetDisplayText())
	if err := validateWaitDisplayText("display_text", displayText); err != nil {
		return WaitRequest{}, err
	}
	return WaitRequest{
		Lease:          request.Lease,
		CorrelationID:  correlationID,
		Kind:           kind,
		Request:        []byte(requestJSON),
		DisplayText:    displayText,
		TimeoutSeconds: timeout,
		Policy:         strings.TrimSpace(wait.GetPolicy()),
	}, nil
}

func validateWaitDisplayText(field, value string) error {
	if len([]byte(value)) > maxWaitDisplayTextBytes {
		return fmt.Errorf("guest wait %s exceeds max %d bytes", field, maxWaitDisplayTextBytes)
	}
	return nil
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
