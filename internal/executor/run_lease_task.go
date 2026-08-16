package executor

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"sync"
	"time"

	"github.com/helmrdotdev/helmr/internal/api"
	"github.com/helmrdotdev/helmr/internal/cas"
	runv0 "github.com/helmrdotdev/helmr/internal/proto/run/v0"
	workspacev0 "github.com/helmrdotdev/helmr/internal/proto/workspace/v0"
	"github.com/helmrdotdev/helmr/internal/vm"
	"github.com/helmrdotdev/helmr/internal/wire"
	"github.com/helmrdotdev/helmr/internal/workerapi"
	"github.com/helmrdotdev/helmr/internal/workspace"
	"google.golang.org/protobuf/proto"
)

var (
	errRunLeaseAuthorityLapsed       = errors.New("run lease authority lapsed")
	errRunSourceOperationUnavailable = errors.New(
		"run lease task cannot perform run-sourced operation",
	)
)

type RunLeaseControlPlane interface {
	ClaimRunLease(context.Context, workerapi.RunLeaseWork) (workerapi.RunLeaseClaimResponse, error)
	AcknowledgeRunStart(context.Context, workerapi.RunStartRequest) (workerapi.RunStartResponse, error)
	AcknowledgeRunEntrypoint(context.Context, workerapi.RunEntrypointRequest) error
	RenewRunLease(context.Context, workerapi.RunLeaseAssignment) (workerapi.RunLeaseRenewResponse, error)
	BeginRunFinalization(context.Context, workerapi.BeginRunFinalizationRequest) (workerapi.BeginRunFinalizationResponse, error)
	CompleteTask(context.Context, workerapi.CompleteTaskRequest) error
	CompleteActor(context.Context, workerapi.CompleteActorRequest) error
	CommitActorTurn(context.Context, workerapi.CommitActorTurnRequest) (workerapi.CommitActorTurnResponse, error)
	SendRunActorInput(context.Context, workerapi.SendActorInputRequest) (workerapi.SendActorInputResponse, error)
	AppendActorOutput(context.Context, workerapi.AppendActorOutputRequest) (workerapi.AppendActorOutputResponse, error)
	CreateRuntimeToken(context.Context, workerapi.CreateTokenRequest) (api.TokenResponse, error)
	AppendRunLog(context.Context, workerapi.RunLeaseAssignment, workerapi.LogStream, uint64, []byte) error
}

type ActorRuntimeControlPlane interface {
	StartRunActor(context.Context, workerapi.StartActorRequest) (workerapi.StartActorResponse, error)
	GetRunSessionStatus(context.Context, workerapi.SessionReferenceRequest) (workerapi.SessionStatusResponse, error)
	CloseRunSession(context.Context, workerapi.CloseSessionRequest) (workerapi.CloseSessionResponse, error)
	ReadRunSessionOutputPage(context.Context, workerapi.ReadSessionOutputPageRequest) (workerapi.ReadSessionOutputPageResponse, error)
}

type WorkspaceRuntimeControlPlane interface {
	CreateRunWorkspace(context.Context, workerapi.CreateWorkspaceRequest) (workerapi.CreateWorkspaceResponse, error)
	RetrieveRunWorkspace(context.Context, workerapi.RetrieveWorkspaceRequest) (workerapi.RetrieveWorkspaceResponse, error)
	ReadRunWorkspaceFile(context.Context, workerapi.ReadWorkspaceFileRequest) (workerapi.ReadWorkspaceFileResponse, error)
	StatRunWorkspaceFile(context.Context, workerapi.ReadWorkspaceFileRequest) (workerapi.StatWorkspaceFileResponse, error)
	ListRunWorkspaceFiles(context.Context, workerapi.ListWorkspaceFilesRequest) (workerapi.ListWorkspaceFilesResponse, error)
	ExecuteRunWorkspace(context.Context, workerapi.ExecuteWorkspaceRequest) (workerapi.ExecuteWorkspaceResponse, error)
	PollRunWorkspaceExec(context.Context, workerapi.PollWorkspaceExecRequest) (workerapi.ExecuteWorkspaceResponse, error)
	DeleteRunWorkspace(context.Context, workerapi.DeleteWorkspaceRequest) (workerapi.DeleteWorkspaceResponse, error)
}

type RunLeaseTaskResult struct {
	Outcome         workerapi.TaskOutcome
	ActorOutcome    *workerapi.ActorOutcome
	ProgramQuiesced workerapi.RunQuiescenceProof
}

type RunLeaseTaskRenewal struct {
	Previous workerapi.RunLeaseAssignment
	Lease    workerapi.RunLeaseAssignment
}

type RunLeaseTask interface {
	Close()
	Wait(context.Context) (RunLeaseTaskResult, error)
	RenewRunLease(context.Context) (RunLeaseTaskRenewal, error)
	BeginWorkspaceFinalization(context.Context, workerapi.RunLeaseAssignment, workerapi.RunLeaseAssignment, string, workerapi.RunFinalizationKind) error
	CaptureWorkspace(context.Context) (workerapi.TaskWorkspaceCapture, error)
	CreateHandoffCheckpoint(context.Context, workerapi.RunFinalizationHandoff, string, workerapi.TaskWorkspaceCapture) (workerapi.CheckpointManifest, error)
	ResetWorkspace(context.Context) (workerapi.TaskWorkspaceRollback, error)
}

func (task *guestRunLeaseTask) Close() {
	task.mu.Lock()
	task.clearCapabilities()
	task.mu.Unlock()
}

type RunLeaseTaskRunner interface {
	StartRunLeaseTask(context.Context, *workerapi.RunLeaseClaimResponse, RunLeaseControlPlane) (RunLeaseTask, error)
}

type guestRunLeaseTask struct {
	program       freshProgram
	mounts        WorkspaceMountSessionRegistry
	store         cas.Store
	controlPlane  RunLeaseControlPlane
	resetTarget   workspace.ResetTarget
	waits         *ControlPlaneRunWaits
	checkpointer  Checkpointer
	waitWorkspace workerapi.Workspace
	orgID         string

	renewalGate      sync.Mutex
	mu               sync.Mutex
	lease            workerapi.RunLeaseAssignment
	authority        *workspacev0.WorkspaceRunAuthority
	operationID      string
	finalizingKind   workerapi.RunFinalizationKind
	checkpointFrozen bool
	finished         bool
}

func (task *guestRunLeaseTask) callRunSourceRuntime(
	ctx context.Context,
	call func(context.Context, workerapi.RunLeaseAssignment) error,
) error {
	return retryRunLeaseRequest(ctx, func(callCtx context.Context) error {
		// Keep the local assignment expiry stable while deriving this attempt's
		// deadline. Retry delays do not hold renewal.
		task.mu.Lock()
		defer task.mu.Unlock()
		if task.finished || task.finalizingKind != "" {
			return errRunSourceOperationUnavailable
		}
		lease := task.lease
		if !lease.ExpiresAt.After(time.Now()) {
			return errRunLeaseAuthorityLapsed
		}
		attemptCtx, cancel, err := runLeaseLogContext(callCtx, lease.ExpiresAt)
		if err != nil {
			return fmt.Errorf("prepare run-sourced operation: %w", err)
		}
		defer cancel()
		return call(attemptCtx, lease)
	})
}

func (r ProgramRunner) StartRunLeaseTask(
	ctx context.Context,
	claim *workerapi.RunLeaseClaimResponse,
	controlPlane RunLeaseControlPlane,
) (RunLeaseTask, error) {
	if r.CAS == nil {
		return nil, errors.New("run lease task CAS is required")
	}
	target, err := runLeaseResetTarget(claim)
	if err != nil {
		return nil, err
	}
	var program freshProgram
	if claim != nil &&
		(claim.Execution.Restore != nil ||
			(claim.Execution.Attach != nil && claim.Execution.Attach.Parent != nil)) {
		program, err = r.startResumedProgram(ctx, claim, controlPlane)
	} else {
		program, err = r.startNewProgram(
			ctx,
			claim,
			controlPlane,
			runLeaseProgramEventSink{controlPlane: controlPlane},
		)
	}
	if err != nil {
		return nil, err
	}
	authority := program.authority
	program.authority = nil
	task := &guestRunLeaseTask{
		program:      program,
		mounts:       r.WorkspaceMounts,
		store:        r.CAS,
		controlPlane: controlPlane,
		resetTarget:  target,
		lease:        program.lease,
		authority:    authority,
		orgID:        program.mount.OrgID,
		waitWorkspace: workerapi.Workspace{
			ID:                program.mount.WorkspaceID,
			WorkspaceMountID:  program.mount.ID,
			FencingGeneration: program.mount.FencingGeneration,
			BaseVersionID:     program.mount.BaseVersionID,
			MountPath:         program.mount.WorkspaceMountPath,
			Artifact:          &program.mount.WorkspaceArtifact,
		},
	}
	if waitClient, ok := controlPlane.(RunWaitClient); ok {
		task.waits = &ControlPlaneRunWaits{Client: waitClient}
	}
	if checkpointable, ok := program.session.(vm.CheckpointableSession); ok {
		task.checkpointer = &runtimeCheckpointer{
			session:   checkpointable,
			cas:       r.CAS,
			encryptor: r.CheckpointEncryptor,
			tempDir:   r.tempDir(),
			stream:    program.session.Stream(),
			workspace: workerapi.CheckpointWorkspaceBase{
				ArtifactDigest:    program.mount.WorkspaceArtifact.Digest,
				ArtifactSizeBytes: program.mount.WorkspaceArtifact.SizeBytes,
				ArtifactMediaType: program.mount.WorkspaceArtifact.MediaType,
				ArtifactEncoding:  program.mount.WorkspaceArtifact.Encoding,
				MountPath:         program.mount.WorkspaceMountPath,
			},
			runEvent:   task.processCheckpointRunEvent,
			freezeGate: &task.renewalGate,
			onFrozen:   task.markCheckpointFrozen,
		}
	}
	return task, nil
}

type runLeaseProgramEventSink struct {
	controlPlane RunLeaseControlPlane
}

func (sink runLeaseProgramEventSink) AppendRunLog(
	ctx context.Context,
	lease workerapi.RunLeaseAssignment,
	stream workerapi.LogStream,
	sequence uint64,
	content []byte,
) error {
	return sink.controlPlane.AppendRunLog(ctx, lease, stream, sequence, content)
}

func (sink runLeaseProgramEventSink) ApplyRunMetadata(
	ctx context.Context,
	lease workerapi.RunLeaseAssignment,
	request *runv0.MetadataUpdated,
) error {
	controlPlane, err := requireRunObservabilityControlPlane(sink.controlPlane)
	if err != nil {
		return err
	}
	return updateRunMetadata(ctx, controlPlane, lease, request)
}

func (sink runLeaseProgramEventSink) RecordStructuredRunLog(
	ctx context.Context,
	lease workerapi.RunLeaseAssignment,
	sequence uint64,
	request *runv0.StructuredLogRequested,
) error {
	controlPlane, err := requireRunObservabilityControlPlane(sink.controlPlane)
	if err != nil {
		return err
	}
	return appendStructuredRunLog(ctx, controlPlane, lease, sequence, request)
}

func (task *guestRunLeaseTask) CurrentWorkerRunLease() workerapi.RunLease {
	task.mu.Lock()
	defer task.mu.Unlock()
	return workerRunLeaseFromAssignment(task.orgID, task.lease)
}

func (task *guestRunLeaseTask) CurrentWorkerRunLeaseAssignment() workerapi.RunLeaseAssignment {
	task.mu.Lock()
	defer task.mu.Unlock()
	return task.lease
}

func workerRunLeaseFromAssignment(orgID string, assignment workerapi.RunLeaseAssignment) workerapi.RunLease {
	return workerapi.RunLease{
		ID: assignment.ID, OrgID: orgID, RunID: assignment.RunID,
		WorkerGroupID:     assignment.WorkerGroupID,
		WorkerInstanceID:  assignment.WorkerInstanceID,
		WorkerEpoch:       assignment.WorkerEpoch,
		LeaseSequence:     assignment.LeaseSequence,
		RuntimeInstanceID: assignment.RuntimeInstanceID,
		AttemptNumber:     assignment.AttemptNumber,
		Trace:             assignment.Trace,
		ExpiresAt:         assignment.ExpiresAt,
	}
}

func (task *guestRunLeaseTask) handleWait(ctx context.Context, wait *runv0.RunWaitRequested) error {
	if task.waits == nil {
		return errors.New("run lease task wait control plane is required")
	}
	runtimeWait, err := parseWaitRequest(task, wait)
	if err != nil {
		return err
	}
	runtimeWait.Leases = task
	runtimeWait.Workspace = task.waitWorkspace
	runtimeWait.Checkpointer = task.checkpointer
	runtimeWait.Resume = func(resumeCtx context.Context, decision WaitResumeDecision) error {
		if strings.TrimSpace(decision.Kind) == "" {
			return errors.New("program resume kind is required")
		}
		if len(decision.Data) == 0 {
			decision.Data = json.RawMessage(`null`)
		}
		if err := wire.WriteResumeDecision(task.program.session.Stream(), &runv0.ResumeDecision{
			RunWaitId:      wait.GetRunWaitId(),
			CorrelationId:  wait.GetCorrelationId(),
			ResumeAttachId: wait.GetResumeAttachId(),
			Kind:           decision.Kind,
			DataJson:       string(decision.Data),
		}); err != nil {
			return err
		}
		return nil
	}
	return task.waits.Wait(ctx, runtimeWait)
}

func (task *guestRunLeaseTask) processCheckpointRunEvent(ctx context.Context, event *runv0.RunEvent) error {
	if event == nil {
		return errors.New("checkpoint program event is required")
	}
	task.program.observedEventSeq++
	switch value := event.Event.(type) {
	case *runv0.RunEvent_StdoutChunk:
		return taskControlEvents{task: task}.AppendRunLog(ctx, workerapi.RunLeaseAssignment{}, workerapi.LogStreamStdout, task.program.observedEventSeq, value.StdoutChunk)
	case *runv0.RunEvent_StderrChunk:
		return taskControlEvents{task: task}.AppendRunLog(ctx, workerapi.RunLeaseAssignment{}, workerapi.LogStreamStderr, task.program.observedEventSeq, value.StderrChunk)
	case *runv0.RunEvent_MetadataUpdated:
		return processRunMetadataEvent(
			ctx,
			taskControlEvents{task: task},
			workerapi.RunLeaseAssignment{},
			task.program.session.Stream(),
			value.MetadataUpdated,
		)
	case *runv0.RunEvent_StructuredLogRequested:
		return processStructuredLogEvent(
			ctx,
			taskControlEvents{task: task},
			workerapi.RunLeaseAssignment{},
			task.program.session.Stream(),
			task.program.observedEventSeq,
			value.StructuredLogRequested,
		)
	case *runv0.RunEvent_TaskChildInvokeRequested:
		return task.handleChildTaskInvoke(ctx, value.TaskChildInvokeRequested)
	case *runv0.RunEvent_ActorStartRequested,
		*runv0.RunEvent_SessionStatusRequested,
		*runv0.RunEvent_SessionCloseRequested,
		*runv0.RunEvent_SessionOutputPageRequested:
		return task.handleActorRuntime(ctx, event)
	case *runv0.RunEvent_WorkspaceCreateRequested,
		*runv0.RunEvent_WorkspaceRetrieveRequested,
		*runv0.RunEvent_WorkspaceFileReadRequested,
		*runv0.RunEvent_WorkspaceFileStatRequested,
		*runv0.RunEvent_WorkspaceFileListRequested,
		*runv0.RunEvent_WorkspaceExecRequested,
		*runv0.RunEvent_WorkspaceDeleteRequested:
		return task.handleWorkspaceRuntime(ctx, event)
	default:
		return errors.New("unsupported program event while checkpoint pause is pending")
	}
}

func (task *guestRunLeaseTask) Wait(ctx context.Context) (RunLeaseTaskResult, error) {
	if task.program.entrypoint != nil && task.program.entrypoint.GetActor() != nil {
		outcome, quiesced, err := task.program.awaitActorCompletion(
			ctx,
			taskControlEvents{task: task},
			task.handleWait,
			task.handleActorTurnCommit,
			task.handleActorInputSend,
			task.handleActorOutputAppend,
			task.handleTokenCreate,
			task.handleChildTaskInvoke,
			task.handleResourceRuntime,
		)
		if err != nil {
			return RunLeaseTaskResult{}, err
		}
		converted, err := workerActorOutcome(outcome)
		if err != nil {
			return RunLeaseTaskResult{}, err
		}
		return RunLeaseTaskResult{
			ActorOutcome:    &converted,
			ProgramQuiesced: workerapi.RunQuiescenceProof{RunID: quiesced.GetRunId(), AttemptNumber: int32(quiesced.GetAttemptNumber()), RunLeaseID: quiesced.GetRunLeaseId()},
		}, nil
	}
	outcome, quiesced, err := task.program.awaitTaskCompletion(
		ctx,
		taskControlEvents{task: task},
		task.handleWait,
		task.handleActorInputSend,
		task.handleTokenCreate,
		task.handleChildTaskInvoke,
		task.handleResourceRuntime,
	)
	if err != nil {
		return RunLeaseTaskResult{}, err
	}
	converted, err := workerTaskOutcome(outcome)
	if err != nil {
		return RunLeaseTaskResult{}, err
	}
	return RunLeaseTaskResult{
		Outcome: converted,
		ProgramQuiesced: workerapi.RunQuiescenceProof{
			RunID:         quiesced.GetRunId(),
			AttemptNumber: int32(quiesced.GetAttemptNumber()),
			RunLeaseID:    quiesced.GetRunLeaseId(),
		},
	}, nil
}

func workerActorOutcome(outcome *runv0.ActorOutcome) (workerapi.ActorOutcome, error) {
	if err := validateFreshActorOutcome(outcome); err != nil {
		return workerapi.ActorOutcome{}, err
	}
	converted := workerapi.ActorOutcome{TerminalInputSequence: outcome.GetTerminalInputSequence()}
	switch value := outcome.GetOutcome().(type) {
	case *runv0.ActorOutcome_Succeeded:
		converted.Succeeded = &workerapi.ActorSucceeded{}
	case *runv0.ActorOutcome_Failed:
		failure := canonicalTaskFailure(value.Failed.GetMessage(), value.Failed.DetailsJson)
		converted.Failed = &failure
	default:
		return workerapi.ActorOutcome{}, errors.New("actor outcome variant is required")
	}
	return converted, nil
}

type taskControlEvents struct {
	task *guestRunLeaseTask
}

func (events taskControlEvents) AppendRunLog(
	ctx context.Context,
	_ workerapi.RunLeaseAssignment,
	stream workerapi.LogStream,
	sequence uint64,
	content []byte,
) error {
	events.task.mu.Lock()
	defer events.task.mu.Unlock()
	lease := events.task.lease
	logCtx, cancel, err := runLeaseLogContext(ctx, lease.ExpiresAt)
	if err != nil {
		return err
	}
	defer cancel()
	return events.task.controlPlane.AppendRunLog(logCtx, lease, stream, sequence, content)
}

func (events taskControlEvents) ApplyRunMetadata(
	ctx context.Context,
	_ workerapi.RunLeaseAssignment,
	request *runv0.MetadataUpdated,
) error {
	controlPlane, err := requireRunObservabilityControlPlane(events.task.controlPlane)
	if err != nil {
		return err
	}
	controlRequest, err := workerRunMetadataRequest(request)
	if err != nil {
		return err
	}
	return events.task.callRunSourceRuntime(ctx, func(
		callCtx context.Context,
		lease workerapi.RunLeaseAssignment,
	) error {
		controlRequest.Lease = lease.Fence()
		return sendRunMetadataRequest(callCtx, controlPlane, controlRequest)
	})
}

func (events taskControlEvents) RecordStructuredRunLog(
	ctx context.Context,
	_ workerapi.RunLeaseAssignment,
	sequence uint64,
	request *runv0.StructuredLogRequested,
) error {
	controlPlane, err := requireRunObservabilityControlPlane(events.task.controlPlane)
	if err != nil {
		return err
	}
	controlRequest, err := workerStructuredLogRequest(request, sequence)
	if err != nil {
		return err
	}
	return events.task.callRunSourceRuntime(ctx, func(
		callCtx context.Context,
		lease workerapi.RunLeaseAssignment,
	) error {
		controlRequest.Lease = lease.Fence()
		return sendStructuredRunLogRequest(callCtx, controlPlane, controlRequest)
	})
}

func (task *guestRunLeaseTask) RenewRunLease(
	ctx context.Context,
) (RunLeaseTaskRenewal, error) {
	task.renewalGate.Lock()
	defer task.renewalGate.Unlock()
	task.mu.Lock()
	defer task.mu.Unlock()
	if task.finished || task.finalizingKind != "" {
		return RunLeaseTaskRenewal{}, errors.New("run lease task is not renewable")
	}
	previous := task.lease
	if task.checkpointFrozen {
		renewed, err := renewControlPlaneRunLeaseAuthority(ctx, task.controlPlane, previous)
		if err != nil {
			return RunLeaseTaskRenewal{}, err
		}
		task.lease = renewed
		return RunLeaseTaskRenewal{Previous: previous, Lease: renewed}, nil
	}
	renewed, fence, err := renewRunLeaseAuthority(
		ctx,
		task.controlPlane,
		task.mounts,
		task.lease,
		task.authority,
	)
	if err != nil {
		return RunLeaseTaskRenewal{}, err
	}
	if fence != nil {
		task.authority.Fence = fence
	}
	task.lease = renewed
	return RunLeaseTaskRenewal{Previous: previous, Lease: renewed}, nil
}

func (task *guestRunLeaseTask) markCheckpointFrozen() {
	task.mu.Lock()
	task.checkpointFrozen = true
	task.mu.Unlock()
}

func renewRunLeaseAuthority(
	ctx context.Context,
	controlPlane interface {
		RenewRunLease(context.Context, workerapi.RunLeaseAssignment) (workerapi.RunLeaseRenewResponse, error)
	},
	mounts WorkspaceMountSessionRegistry,
	previous workerapi.RunLeaseAssignment,
	authority *workspacev0.WorkspaceRunAuthority,
) (workerapi.RunLeaseAssignment, *workspacev0.WorkspaceAuthorityFence, error) {
	renewed, err := renewControlPlaneRunLeaseAuthority(ctx, controlPlane, previous)
	if err != nil {
		return workerapi.RunLeaseAssignment{}, nil, err
	}
	if renewed.ExpiresAt.Equal(previous.ExpiresAt) {
		return previous, nil, nil
	}
	guestCtx, cancelGuest := context.WithDeadline(context.Background(), renewed.ExpiresAt)
	defer cancelGuest()
	var fence *workspacev0.WorkspaceAuthorityFence
	if err := retryWorkspaceAuthorityTransport(guestCtx, func(requestCtx context.Context) error {
		var requestErr error
		fence, requestErr = mounts.RenewWorkspaceAuthority(
			requestCtx,
			&workspacev0.RenewWorkspaceAuthorityRequest{
				Previous:             proto.Clone(authority).(*workspacev0.WorkspaceRunAuthority),
				NewExpiresAtUnixNano: renewed.ExpiresAt.UnixNano(),
			},
		)
		return requestErr
	}); err != nil {
		if !renewed.ExpiresAt.After(time.Now()) {
			return workerapi.RunLeaseAssignment{}, nil, fmt.Errorf("%w: %v", errRunLeaseAuthorityLapsed, err)
		}
		return workerapi.RunLeaseAssignment{}, nil, err
	}
	return renewed, proto.Clone(fence).(*workspacev0.WorkspaceAuthorityFence), nil
}

func renewControlPlaneRunLeaseAuthority(
	ctx context.Context,
	controlPlane interface {
		RenewRunLease(context.Context, workerapi.RunLeaseAssignment) (workerapi.RunLeaseRenewResponse, error)
	},
	previous workerapi.RunLeaseAssignment,
) (workerapi.RunLeaseAssignment, error) {
	controlCtx, cancelControlPlane := context.WithDeadline(ctx, previous.ExpiresAt)
	defer cancelControlPlane()
	var response workerapi.RunLeaseRenewResponse
	if err := retryRunLeaseRequest(controlCtx, func(requestCtx context.Context) error {
		var requestErr error
		response, requestErr = controlPlane.RenewRunLease(requestCtx, previous)
		return requestErr
	}); err != nil {
		if !previous.ExpiresAt.After(time.Now()) {
			return workerapi.RunLeaseAssignment{}, fmt.Errorf("%w: %v", errRunLeaseAuthorityLapsed, err)
		}
		return workerapi.RunLeaseAssignment{}, err
	}
	if response.Lease != previous.Fence() ||
		response.BaseWorkspaceVersionID != previous.BaseWorkspaceVersionID {
		return workerapi.RunLeaseAssignment{}, errors.New(
			"run lease renewal response changed its fence or workspace frontier",
		)
	}
	renewed := previous
	renewed.ExpiresAt = response.ExpiresAt
	if err := validateRunLeaseExpiryAdvance(previous, renewed); err != nil {
		return workerapi.RunLeaseAssignment{}, err
	}
	if renewed.ExpiresAt.Equal(previous.ExpiresAt) {
		return previous, nil
	}
	return renewed, nil
}

func retryWorkspaceAuthorityTransport(
	ctx context.Context,
	request func(context.Context) error,
) error {
	delay := runLeaseRetryEvery
	for {
		if err := ctx.Err(); err != nil {
			return err
		}
		requestCtx, cancel := context.WithTimeout(ctx, runLeaseRequestTimeout)
		err := request(requestCtx)
		cancel()
		if err == nil {
			return nil
		}
		if !errors.Is(err, errWorkspaceControlTransport) {
			return err
		}
		timer := time.NewTimer(delay)
		select {
		case <-ctx.Done():
			timer.Stop()
			return ctx.Err()
		case <-timer.C:
		}
		if delay < time.Second {
			delay *= 2
			if delay > time.Second {
				delay = time.Second
			}
		}
	}
}

func (task *guestRunLeaseTask) BeginWorkspaceFinalization(
	ctx context.Context,
	previous workerapi.RunLeaseAssignment,
	frozen workerapi.RunLeaseAssignment,
	operationID string,
	kind workerapi.RunFinalizationKind,
) error {
	task.mu.Lock()
	defer task.mu.Unlock()
	if task.finished {
		return errors.New("run lease task is already finalized")
	}
	if !equalRunLeaseAssignment(task.lease, previous) {
		return errors.New("workspace finalization previous receipt is not current")
	}
	if err := validateRunLeaseExpiryAdvance(previous, frozen); err != nil {
		return err
	}
	if !frozen.ExpiresAt.After(previous.ExpiresAt) {
		return errors.New("workspace finalization expiry did not advance")
	}
	if strings.TrimSpace(operationID) == "" ||
		(kind != workerapi.RunFinalizationCapture && kind != workerapi.RunFinalizationReset) {
		return errors.New("workspace finalization identity is invalid")
	}
	response, err := task.mounts.BeginWorkspaceFinalization(
		ctx,
		&workspacev0.BeginWorkspaceFinalizationRequest{
			Previous:                      proto.Clone(task.authority).(*workspacev0.WorkspaceRunAuthority),
			FinalizationExpiresAtUnixNano: frozen.ExpiresAt.UnixNano(),
			OperationId:                   operationID,
			Kind:                          string(kind),
		},
	)
	if err != nil {
		return err
	}
	task.authority.Fence = proto.Clone(response.GetFence()).(*workspacev0.WorkspaceAuthorityFence)
	task.lease = frozen
	task.operationID = operationID
	task.finalizingKind = kind
	return nil
}

func (task *guestRunLeaseTask) CaptureWorkspace(
	ctx context.Context,
) (workerapi.TaskWorkspaceCapture, error) {
	task.mu.Lock()
	defer task.mu.Unlock()
	if task.finished || task.finalizingKind != workerapi.RunFinalizationCapture {
		return workerapi.TaskWorkspaceCapture{}, errors.New("run lease task is not capturing")
	}
	envelope, err := task.finalizationEnvelope(workspace.FinalizationCaptureKind, nil)
	if err != nil {
		return workerapi.TaskWorkspaceCapture{}, err
	}
	result, err := task.mounts.CaptureWorkspace(
		ctx,
		&workspacev0.CaptureWorkspaceRequest{Envelope: envelope},
		task.store,
	)
	if err != nil {
		return workerapi.TaskWorkspaceCapture{}, err
	}
	task.finished = true
	task.clearCapabilities()
	return workerapi.TaskWorkspaceCapture{
		Receipt: workerWorkspaceFinalizationReceipt(result.Receipt),
		Tree: workerapi.WorkspaceTreeIdentity{
			Digest: result.ReportedTree.Digest, SizeBytes: result.ReportedTree.SizeBytes,
			EntryCount: int32(result.ReportedTree.EntryCount),
		},
		Artifact: workerapi.WorkspaceArtifact{
			Digest: result.Artifact.Digest, MediaType: result.Artifact.MediaType,
			Encoding: result.Artifact.Encoding, SizeBytes: result.Artifact.SizeBytes,
			EntryCount: int32(result.Artifact.EntryCount),
		},
	}, nil
}

func (task *guestRunLeaseTask) CreateHandoffCheckpoint(
	ctx context.Context,
	handoff workerapi.RunFinalizationHandoff,
	checkpointID string,
	capture workerapi.TaskWorkspaceCapture,
) (workerapi.CheckpointManifest, error) {
	task.mu.Lock()
	defer task.mu.Unlock()
	if !task.finished || task.finalizingKind != workerapi.RunFinalizationCapture {
		return workerapi.CheckpointManifest{}, errors.New("run lease task has not captured its workspace")
	}
	checkpointer, ok := task.checkpointer.(HandoffCheckpointer)
	if !ok {
		return workerapi.CheckpointManifest{}, errors.New("run lease task does not support handoff checkpoints")
	}
	return checkpointer.CreateHandoffCheckpoint(
		ctx,
		CheckpointRequest{
			RunID:          handoff.ParentRunID,
			AttemptNumber:  handoff.ParentAttemptNumber,
			RunLeaseID:     task.lease.ID,
			RunWaitID:      handoff.RunWaitID,
			CorrelationID:  handoff.CorrelationID,
			CheckpointID:   checkpointID,
			ResumeAttachID: handoff.ResumeAttachID,
		},
		workerapi.CheckpointWorkspaceBase{
			ArtifactDigest:    capture.Artifact.Digest,
			ArtifactSizeBytes: capture.Artifact.SizeBytes,
			ArtifactMediaType: capture.Artifact.MediaType,
			ArtifactEncoding:  capture.Artifact.Encoding,
			MountPath:         task.waitWorkspace.MountPath,
		},
	)
}

func (task *guestRunLeaseTask) ResetWorkspace(
	ctx context.Context,
) (workerapi.TaskWorkspaceRollback, error) {
	task.mu.Lock()
	defer task.mu.Unlock()
	if task.finished || task.finalizingKind != workerapi.RunFinalizationReset {
		return workerapi.TaskWorkspaceRollback{}, errors.New("run lease task is not resetting")
	}
	envelope, err := task.finalizationEnvelope(workspace.FinalizationResetKind, task.resetTarget)
	if err != nil {
		return workerapi.TaskWorkspaceRollback{}, err
	}
	result, err := task.mounts.ResetWorkspace(
		ctx,
		&workspacev0.ResetWorkspaceRequest{
			Envelope: envelope,
			Target:   workspace.ResetTargetProto(task.resetTarget),
		},
		task.store,
	)
	if err != nil {
		return workerapi.TaskWorkspaceRollback{}, err
	}
	task.finished = true
	task.clearCapabilities()
	return workerapi.TaskWorkspaceRollback{
		Receipt: workerWorkspaceFinalizationReceipt(result.Receipt),
		Target:  workerWorkspaceResetTarget(result.Target),
	}, nil
}

func (task *guestRunLeaseTask) finalizationEnvelope(
	kind string,
	target any,
) (*workspacev0.WorkspaceFinalizationEnvelope, error) {
	fence := executorFinalizationFence(task.authority.GetFence())
	fingerprint, err := workspace.FinalizationFingerprint(kind, workspace.FinalizationRequest{
		OperationID: task.operationID,
		Fence:       fence,
		Target:      target,
	})
	if err != nil {
		return nil, err
	}
	return &workspacev0.WorkspaceFinalizationEnvelope{
		OperationId:        task.operationID,
		RequestFingerprint: fingerprint,
		Authority:          proto.Clone(task.authority).(*workspacev0.WorkspaceRunAuthority),
	}, nil
}

func (task *guestRunLeaseTask) clearCapabilities() {
	if task.authority != nil {
		task.authority.ChannelToken = ""
		task.authority.WriteCapability = ""
	}
}

func validateRunLeaseExpiryAdvance(
	previous workerapi.RunLeaseAssignment,
	next workerapi.RunLeaseAssignment,
) error {
	previousExpiry := previous.ExpiresAt
	nextExpiry := next.ExpiresAt
	previous.ExpiresAt = time.Time{}
	next.ExpiresAt = time.Time{}
	if !equalRunLeaseAssignment(previous, next) {
		return errors.New("run lease renewal changed immutable authority")
	}
	if nextExpiry.Before(previousExpiry) {
		return errors.New("run lease expiry moved backwards")
	}
	return nil
}

func runLeaseResetTarget(
	claim *workerapi.RunLeaseClaimResponse,
) (workspace.ResetTarget, error) {
	if claim == nil {
		return workspace.ResetTarget{}, errors.New("run lease claim is required")
	}
	target := claim.Workspace.ResetTarget
	if target.BaseWorkspaceVersionID != claim.Lease.BaseWorkspaceVersionID {
		return workspace.ResetTarget{}, errors.New("run lease workspace reset target does not match its base version")
	}
	tree := workspace.TreeIdentity{
		Digest: target.Tree.Digest, SizeBytes: target.Tree.SizeBytes,
		EntryCount: int(target.Tree.EntryCount),
	}
	switch {
	case target.Empty != nil && target.Artifact == nil:
		return workspace.EmptyResetTarget(target.BaseWorkspaceVersionID, tree)
	case target.Empty == nil && target.Artifact != nil:
		return workspace.ArtifactResetTarget(
			target.BaseWorkspaceVersionID,
			tree,
			workspace.ArtifactIdentity{
				Digest: target.Artifact.Digest, MediaType: target.Artifact.MediaType,
				Encoding: target.Artifact.Encoding, SizeBytes: target.Artifact.SizeBytes,
				EntryCount: int(target.Artifact.EntryCount),
			},
		)
	default:
		return workspace.ResetTarget{}, errors.New("run lease workspace reset target is invalid")
	}
}

func workerTaskOutcome(outcome *runv0.TaskOutcome) (workerapi.TaskOutcome, error) {
	if err := validateFreshTaskOutcome(outcome); err != nil {
		return workerapi.TaskOutcome{}, err
	}
	switch value := outcome.GetOutcome().(type) {
	case *runv0.TaskOutcome_Succeeded:
		return workerapi.TaskOutcome{Succeeded: &workerapi.TaskSucceeded{
			Output: json.RawMessage(value.Succeeded.GetOutputJson()),
		}}, nil
	case *runv0.TaskOutcome_Failed:
		failure := canonicalTaskFailure(value.Failed.GetMessage(), value.Failed.DetailsJson)
		return workerapi.TaskOutcome{Failed: &failure}, nil
	case *runv0.TaskOutcome_PayloadInvalid:
		failure := canonicalTaskFailure(
			value.PayloadInvalid.GetMessage(),
			value.PayloadInvalid.DetailsJson,
		)
		return workerapi.TaskOutcome{PayloadInvalid: &failure}, nil
	default:
		return workerapi.TaskOutcome{}, errors.New("task outcome variant is required")
	}
}

func workerWorkspaceFinalizationReceipt(
	receipt *workspacev0.WorkspaceFinalizationReceipt,
) workerapi.WorkspaceFinalizationReceipt {
	if receipt == nil {
		return workerapi.WorkspaceFinalizationReceipt{}
	}
	fence := receipt.GetFence()
	return workerapi.WorkspaceFinalizationReceipt{
		OperationID: receipt.GetOperationId(), RequestFingerprint: receipt.GetRequestFingerprint(),
		Fence: workerapi.WorkspaceFinalizationFence{
			WorkerInstanceID: fence.GetWorkerInstanceId(), WorkerEpoch: fence.GetWorkerEpoch(),
			RuntimeInstanceID: fence.GetRuntimeInstanceId(), RuntimeIdentityID: fence.GetRuntimeIdentityId(),
			WorkspaceID: fence.GetWorkspaceId(), WorkspaceMountID: fence.GetWorkspaceMountId(),
			RunID: fence.GetRunId(), AttemptNumber: int32(fence.GetAttemptNumber()),
			RunLeaseID: fence.GetRunLeaseId(), LeaseSequence: fence.GetLeaseSequence(),
			WorkspaceLeaseID: fence.GetWorkspaceLeaseId(), OwnershipGeneration: fence.GetOwnershipGeneration(),
			WriterGeneration: fence.GetWriterGeneration(), MountFencingGeneration: fence.GetMountFencingGeneration(),
			ExpiresAt:              time.Unix(0, fence.GetExpiresAtUnixNano()).UTC(),
			BaseWorkspaceVersionID: fence.GetBaseWorkspaceVersionId(),
		},
	}
}

func workerWorkspaceResetTarget(target workspace.ResetTarget) workerapi.WorkspaceResetTarget {
	result := workerapi.WorkspaceResetTarget{
		BaseWorkspaceVersionID: target.BaseVersionID,
		Tree: workerapi.WorkspaceTreeIdentity{
			Digest: target.Tree.Digest, SizeBytes: target.Tree.SizeBytes,
			EntryCount: int32(target.Tree.EntryCount),
		},
	}
	if target.Kind == workspace.ResetTargetEmpty {
		result.Empty = &workerapi.EmptyWorkspace{}
	} else {
		result.Artifact = &workerapi.WorkspaceArtifact{
			Digest: target.Artifact.Digest, MediaType: target.Artifact.MediaType,
			Encoding: target.Artifact.Encoding, SizeBytes: target.Artifact.SizeBytes,
			EntryCount: int32(target.Artifact.EntryCount),
		}
	}
	return result
}

func canonicalTaskFailure(message string, details *string) workerapi.TaskFailure {
	failure := workerapi.TaskFailure{Message: message}
	if details != nil {
		failure.Details = json.RawMessage(*details)
	}
	return failure
}

var _ RunLeaseTaskRunner = ProgramRunner{}
