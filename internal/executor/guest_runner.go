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
	"github.com/helmrdotdev/helmr/internal/cas"
	"github.com/helmrdotdev/helmr/internal/checkpoint"
	runv0 "github.com/helmrdotdev/helmr/internal/proto/run/v0"
	"github.com/helmrdotdev/helmr/internal/sourcetar"
	"github.com/helmrdotdev/helmr/internal/transport"
	"github.com/helmrdotdev/helmr/internal/vm"
	"google.golang.org/protobuf/proto"
)

var (
	restoreAttachTimeout     = 30 * time.Second
	checkpointSuspendTimeout = 30 * time.Second
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
	if strings.TrimSpace(request.WorkspaceSource.ProjectRoot) == "" {
		return Result{}, errors.New("checked-out workspace source project root is required")
	}
	workspaceSourceRoot, err := runtimeSourceRoot(request.WorkspaceSource)
	if err != nil {
		return Result{}, err
	}
	session, err := r.Connector.Connect(ctx)
	if err != nil {
		return Result{}, fmt.Errorf("connect guest runtime: %w", err)
	}
	defer session.Close()
	stream := session.Stream()
	if err := r.writeRunInput(ctx, stream, request, deploymentSourceRoot, workspaceSourceRoot); err != nil {
		return Result{}, err
	}
	return r.readRunEvents(ctx, session, request)
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
	if restore.Checkpoint.VMStateDigest == nil || strings.TrimSpace(*restore.Checkpoint.VMStateDigest) == "" {
		return Result{}, errors.New("restore checkpoint vm_state_digest is required")
	}
	if len(restore.Checkpoint.MemoryDigests) != 1 {
		return Result{}, fmt.Errorf("restore checkpoint requires exactly one memory digest, got %d", len(restore.Checkpoint.MemoryDigests))
	}
	if err := validateRestoreIdentity(restore.Checkpoint); err != nil {
		return Result{}, err
	}
	state, err := r.materializeCheckpointObject(ctx, *restore.Checkpoint.VMStateDigest, "vmstate")
	if err != nil {
		return Result{}, err
	}
	defer os.Remove(state)
	memory, err := r.materializeCheckpointObject(ctx, restore.Checkpoint.MemoryDigests[0], "memory")
	if err != nil {
		return Result{}, err
	}
	defer os.Remove(memory)
	session, err := restoring.Restore(ctx, vm.RestoreRequest{
		ID:       restore.CheckpointID,
		VMState:  state,
		Memory:   []string{memory},
		Manifest: restore.Checkpoint.Manifest,
		Checkpoint: vm.CheckpointIdentity{
			RuntimeBackend:      restore.Checkpoint.RuntimeBackend,
			RuntimeArch:         restore.Checkpoint.RuntimeArch,
			RuntimeABI:          restore.Checkpoint.RuntimeABI,
			KernelDigest:        derefString(restore.Checkpoint.KernelDigest),
			RootfsDigest:        derefString(restore.Checkpoint.RootfsDigest),
			RuntimeConfigDigest: derefString(restore.Checkpoint.RuntimeConfigDigest),
		},
	})
	if err != nil {
		return Result{}, fmt.Errorf("restore guest runtime: %w", err)
	}
	defer session.Close()
	if err := r.attachRestoredWaitpoint(ctx, session, request); err != nil {
		return Result{}, err
	}
	return r.readRunEvents(ctx, session, request)
}

func (r GuestRunner) tempDir() string {
	if strings.TrimSpace(r.TempDir) != "" {
		return r.TempDir
	}
	return os.TempDir()
}

func (r GuestRunner) attachRestoredWaitpoint(ctx context.Context, session vm.Session, request Request) error {
	if err := ctx.Err(); err != nil {
		return err
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
		WaitpointId:           restore.Waitpoint.ID,
		Kind:                  restore.Waitpoint.ResolutionKind,
		ResolutionPayloadJson: string(restore.Waitpoint.ResolutionPayloadJSON),
		TimedOut:              restore.Waitpoint.ResolutionKind == "timed_out",
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
	return nil
}

func readResumeAck(ctx context.Context, session vm.Session) (*runv0.ResumeAck, error) {
	var ack runv0.ResumeAck
	if err := readProtoFrameContext(ctx, session, &ack); err != nil {
		return nil, err
	}
	return &ack, nil
}

func readProtoFrameContext(ctx context.Context, session vm.Session, message proto.Message) error {
	result := make(chan error, 1)
	go func() {
		result <- transport.ReadProtoFrame(session.Stream(), message)
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

func (r GuestRunner) writeRunInput(ctx context.Context, stream io.Writer, request Request, deploymentSourceRoot string, workspaceSourceRoot string) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if err := transport.WriteFileFrame(stream, transport.StreamHeader{Type: transport.StreamTypeRunImage, RunID: request.Run.RunID}, request.Artifact.ImageTarPath); err != nil {
		return fmt.Errorf("write run image: %w", err)
	}
	deploymentSourceTar, cleanupDeploymentSource, err := sourcetar.CreateTar(deploymentSourceRoot, r.TempDir)
	if err != nil {
		return err
	}
	defer cleanupDeploymentSource()
	if err := transport.WriteFileFrame(stream, transport.StreamHeader{Type: transport.StreamTypeDeploymentSource, RunID: request.Run.RunID}, deploymentSourceTar.Path); err != nil {
		return fmt.Errorf("write deployment source: %w", err)
	}
	workspaceSourceTar, cleanupWorkspace, err := sourcetar.CreateTar(workspaceSourceRoot, r.TempDir)
	if err != nil {
		return err
	}
	defer cleanupWorkspace()
	if err := transport.WriteFileFrame(stream, transport.StreamHeader{Type: transport.StreamTypeWorkspaceSource, RunID: request.Run.RunID}, workspaceSourceTar.Path); err != nil {
		return fmt.Errorf("write workspace source: %w", err)
	}
	protocolRequest, err := runTaskRequest(request, workspaceSourceTar.Digest)
	if err != nil {
		return err
	}
	if err := transport.WriteProtoFrame(stream, protocolRequest); err != nil {
		return fmt.Errorf("write run request: %w", err)
	}
	return nil
}

func (r GuestRunner) readRunEvents(ctx context.Context, session vm.Session, request Request) (Result, error) {
	stream := session.Stream()
	active := newActiveRuntimeClock(request.Run.MaxDuration, request.Run.ActiveUsed)
	var observedSeq uint64
	var taskOutput json.RawMessage
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
		case *runv0.RunEvent_TaskOutput:
			if value.TaskOutput == nil {
				return Result{}, errors.New("guest task_output event is empty")
			}
			output := json.RawMessage(value.TaskOutput.OutputJson)
			if !json.Valid(output) {
				return Result{}, errors.New("guest task_output output_json must be valid JSON")
			}
			taskOutput = append(taskOutput[:0], output...)
		case *runv0.RunEvent_WaitRequested:
			if err := r.handleWaitRequested(ctx, stream, session, request, value.WaitRequested, active.elapsed()); err != nil {
				if errors.Is(err, ErrDetached) {
					return Result{Detached: true}, nil
				}
				return Result{}, err
			}
		case *runv0.RunEvent_TaskComplete:
			if value.TaskComplete == nil {
				return Result{}, errors.New("guest task_complete event is empty")
			}
			if strings.TrimSpace(value.TaskComplete.GetErrorMessage()) != "" {
				return Result{}, errors.New(value.TaskComplete.GetErrorMessage())
			}
			result := Result{ExitCode: value.TaskComplete.ExitCode}
			if value.TaskComplete.ExitCode == 0 && len(taskOutput) > 0 {
				result.Output = append(json.RawMessage(nil), taskOutput...)
			}
			return result, nil
		case nil:
			return Result{}, errors.New("guest run event is empty")
		default:
			return Result{}, fmt.Errorf("unsupported guest run event %T", value)
		}
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

func (r GuestRunner) handleWaitRequested(ctx context.Context, stream io.ReadWriteCloser, session vm.Session, request Request, wait *runv0.WaitRequested, activeDuration time.Duration) error {
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
	base := WaitRequest{
		Lease:         request.Lease,
		CorrelationID: correlationID,
	}
	switch value := wait.GetKind().(type) {
	case *runv0.WaitRequested_Approval:
		if value.Approval == nil {
			return WaitRequest{}, errors.New("guest approval wait request is empty")
		}
		timeout, err := waitTimeoutSeconds(value.Approval.Timeout)
		if err != nil {
			return WaitRequest{}, err
		}
		if err := validateWaitDisplayText("approval message", value.Approval.Message); err != nil {
			return WaitRequest{}, err
		}
		payload, err := json.Marshal(map[string]any{"message": value.Approval.Message})
		if err != nil {
			return WaitRequest{}, err
		}
		base.Kind = api.WorkerWaitpointKindApproval
		base.Request = payload
		base.DisplayText = value.Approval.Message
		base.TimeoutSeconds = timeout
		base.Policy = strings.TrimSpace(value.Approval.GetPolicy())
		return base, nil
	case *runv0.WaitRequested_Message:
		if value.Message == nil {
			return WaitRequest{}, errors.New("guest message wait request is empty")
		}
		timeout, err := waitTimeoutSeconds(value.Message.Timeout)
		if err != nil {
			return WaitRequest{}, err
		}
		prompt := value.Message.GetPrompt()
		if err := validateWaitDisplayText("message prompt", prompt); err != nil {
			return WaitRequest{}, err
		}
		payload, err := json.Marshal(map[string]any{"prompt": prompt})
		if err != nil {
			return WaitRequest{}, err
		}
		base.Kind = api.WorkerWaitpointKindMessage
		base.Request = payload
		base.DisplayText = prompt
		base.TimeoutSeconds = timeout
		base.Policy = strings.TrimSpace(value.Message.GetPolicy())
		return base, nil
	default:
		return WaitRequest{}, fmt.Errorf("unsupported guest wait request kind %T", value)
	}
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
