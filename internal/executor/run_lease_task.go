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
	"github.com/helmrdotdev/helmr/internal/workspace"
	"google.golang.org/protobuf/proto"
)

var (
	errRunLeaseAuthorityLapsed       = errors.New("Run Lease authority lapsed")
	errRunSourceOperationUnavailable = errors.New(
		"Run Lease Task cannot perform run-sourced operation",
	)
)

type RunLeaseControl interface {
	ClaimRunLease(context.Context, api.WorkerRunLeaseWork) (api.WorkerRunLeaseClaimResponse, error)
	AcknowledgeRunStart(context.Context, api.WorkerRunStartRequest) (api.WorkerRunStartResponse, error)
	AcknowledgeRunEntrypoint(context.Context, api.WorkerRunEntrypointRequest) error
	RenewRunLease(context.Context, api.WorkerRunLeaseAssignment) (api.WorkerRunLeaseRenewResponse, error)
	BeginRunFinalization(context.Context, api.WorkerBeginRunFinalizationRequest) (api.WorkerBeginRunFinalizationResponse, error)
	CompleteTask(context.Context, api.WorkerCompleteTaskRequest) error
	CompleteActor(context.Context, api.WorkerCompleteActorRequest) error
	CommitActorTurn(context.Context, api.WorkerCommitActorTurnRequest) (api.WorkerCommitActorTurnResponse, error)
	SendRunActorInput(context.Context, api.WorkerSendActorInputRequest) (api.WorkerSendActorInputResponse, error)
	AppendActorOutput(context.Context, api.WorkerAppendActorOutputRequest) (api.WorkerAppendActorOutputResponse, error)
	CreateRuntimeToken(context.Context, api.WorkerCreateTokenRequest) (api.TokenResponse, error)
	AppendRunLog(context.Context, api.WorkerRunLeaseAssignment, api.WorkerLogStream, uint64, []byte) error
}

type ActorRuntimeControl interface {
	StartRunActor(context.Context, api.WorkerStartActorRequest) (api.WorkerStartActorResponse, error)
	GetRunActorStatus(context.Context, api.WorkerActorReferenceRequest) (api.WorkerActorStatusResponse, error)
	CloseRunActor(context.Context, api.WorkerCloseActorRequest) (api.WorkerCloseActorResponse, error)
	ReadRunActorOutputPage(context.Context, api.WorkerReadActorOutputPageRequest) (api.WorkerReadActorOutputPageResponse, error)
}

type WorkspaceRuntimeControl interface {
	CreateRunWorkspace(context.Context, api.WorkerCreateWorkspaceRequest) (api.WorkerCreateWorkspaceResponse, error)
	RetrieveRunWorkspace(context.Context, api.WorkerRetrieveWorkspaceRequest) (api.WorkerRetrieveWorkspaceResponse, error)
	ReadRunWorkspaceFile(context.Context, api.WorkerReadWorkspaceFileRequest) (api.WorkerReadWorkspaceFileResponse, error)
	StatRunWorkspaceFile(context.Context, api.WorkerReadWorkspaceFileRequest) (api.WorkerStatWorkspaceFileResponse, error)
	ListRunWorkspaceFiles(context.Context, api.WorkerListWorkspaceFilesRequest) (api.WorkerListWorkspaceFilesResponse, error)
	ExecuteRunWorkspace(context.Context, api.WorkerExecuteWorkspaceRequest) (api.WorkerExecuteWorkspaceResponse, error)
	PollRunWorkspaceExec(context.Context, api.WorkerPollWorkspaceExecRequest) (api.WorkerExecuteWorkspaceResponse, error)
	DeleteRunWorkspace(context.Context, api.WorkerDeleteWorkspaceRequest) (api.WorkerDeleteWorkspaceResponse, error)
}

type RunLeaseTaskResult struct {
	Outcome         api.WorkerTaskOutcome
	ActorOutcome    *api.WorkerActorOutcome
	ProgramQuiesced api.WorkerRunQuiescenceProof
}

type RunLeaseTaskRenewal struct {
	Previous api.WorkerRunLeaseAssignment
	Lease    api.WorkerRunLeaseAssignment
}

type RunLeaseTask interface {
	Close()
	Wait(context.Context) (RunLeaseTaskResult, error)
	RenewRunLease(context.Context) (RunLeaseTaskRenewal, error)
	BeginWorkspaceFinalization(context.Context, api.WorkerRunLeaseAssignment, api.WorkerRunLeaseAssignment, string, api.WorkerRunFinalizationKind) error
	CaptureWorkspace(context.Context) (api.WorkerTaskWorkspaceCapture, error)
	CreateHandoffCheckpoint(context.Context, api.WorkerRunFinalizationHandoff, string, api.WorkerTaskWorkspaceCapture) (api.WorkerCheckpointManifest, error)
	ResetWorkspace(context.Context) (api.WorkerTaskWorkspaceRollback, error)
}

func (task *guestRunLeaseTask) Close() {
	task.mu.Lock()
	task.clearCapabilities()
	task.mu.Unlock()
}

type RunLeaseTaskRunner interface {
	StartRunLeaseTask(context.Context, *api.WorkerRunLeaseClaimResponse, RunLeaseControl) (RunLeaseTask, error)
}

type guestRunLeaseTask struct {
	program       freshProgram
	mounts        WorkspaceMountSessionRegistry
	store         cas.Store
	control       RunLeaseControl
	resetTarget   workspace.ResetTarget
	waits         *ControlRunWaits
	checkpointer  Checkpointer
	waitWorkspace api.WorkerWorkspace
	orgID         string

	mu             sync.Mutex
	lease          api.WorkerRunLeaseAssignment
	authority      *workspacev0.WorkspaceRunAuthority
	operationID    string
	finalizingKind api.WorkerRunFinalizationKind
	finished       bool
}

func (task *guestRunLeaseTask) callRunSourceRuntime(
	ctx context.Context,
	call func(context.Context, api.WorkerRunLeaseAssignment) error,
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
	claim *api.WorkerRunLeaseClaimResponse,
	control RunLeaseControl,
) (RunLeaseTask, error) {
	if r.CAS == nil {
		return nil, errors.New("Run Lease Task CAS is required")
	}
	target, err := runLeaseResetTarget(claim)
	if err != nil {
		return nil, err
	}
	var program freshProgram
	if claim != nil &&
		(claim.Execution.Restore != nil ||
			(claim.Execution.Attach != nil && claim.Execution.Attach.Parent != nil)) {
		program, err = r.startResumedProgram(ctx, claim, control)
	} else {
		program, err = r.startNewProgram(
			ctx,
			claim,
			control,
			runLeaseProgramEventSink{control: control},
		)
	}
	if err != nil {
		return nil, err
	}
	authority := program.authority
	program.authority = nil
	task := &guestRunLeaseTask{
		program:     program,
		mounts:      r.WorkspaceMounts,
		store:       r.CAS,
		control:     control,
		resetTarget: target,
		lease:       program.lease,
		authority:   authority,
		orgID:       program.mount.OrgID,
		waitWorkspace: api.WorkerWorkspace{
			ID:                program.mount.WorkspaceID,
			WorkspaceMountID:  program.mount.ID,
			FencingGeneration: program.mount.FencingGeneration,
			BaseVersionID:     program.mount.BaseVersionID,
			MountPath:         program.mount.WorkspaceMountPath,
			Artifact:          &program.mount.WorkspaceArtifact,
		},
	}
	if waitClient, ok := control.(RunWaitClient); ok {
		task.waits = &ControlRunWaits{Client: waitClient}
	}
	if checkpointable, ok := program.session.(vm.CheckpointableSession); ok {
		task.checkpointer = &runtimeCheckpointer{
			session:   checkpointable,
			cas:       r.CAS,
			encryptor: r.CheckpointEncryptor,
			tempDir:   r.tempDir(),
			stream:    program.session.Stream(),
			workspace: api.WorkerCheckpointWorkspaceBase{
				ArtifactDigest:    program.mount.WorkspaceArtifact.Digest,
				ArtifactSizeBytes: program.mount.WorkspaceArtifact.SizeBytes,
				ArtifactMediaType: program.mount.WorkspaceArtifact.MediaType,
				ArtifactEncoding:  program.mount.WorkspaceArtifact.Encoding,
				MountPath:         program.mount.WorkspaceMountPath,
			},
			runEvent: task.processCheckpointRunEvent,
		}
	}
	return task, nil
}

type runLeaseProgramEventSink struct {
	control RunLeaseControl
}

func (sink runLeaseProgramEventSink) AppendRunLog(
	ctx context.Context,
	lease api.WorkerRunLeaseAssignment,
	stream api.WorkerLogStream,
	sequence uint64,
	content []byte,
) error {
	return sink.control.AppendRunLog(ctx, lease, stream, sequence, content)
}

func (sink runLeaseProgramEventSink) ApplyRunMetadata(
	ctx context.Context,
	lease api.WorkerRunLeaseAssignment,
	request *runv0.MetadataUpdated,
) error {
	control, err := requireRunObservabilityControl(sink.control)
	if err != nil {
		return err
	}
	return updateRunMetadata(ctx, control, lease, request)
}

func (sink runLeaseProgramEventSink) RecordStructuredRunLog(
	ctx context.Context,
	lease api.WorkerRunLeaseAssignment,
	sequence uint64,
	request *runv0.StructuredLogRequested,
) error {
	control, err := requireRunObservabilityControl(sink.control)
	if err != nil {
		return err
	}
	return appendStructuredRunLog(ctx, control, lease, sequence, request)
}

func (task *guestRunLeaseTask) CurrentWorkerRunLease() api.WorkerRunLease {
	task.mu.Lock()
	defer task.mu.Unlock()
	return workerRunLeaseFromAssignment(task.orgID, task.lease)
}

func (task *guestRunLeaseTask) CurrentWorkerRunLeaseAssignment() api.WorkerRunLeaseAssignment {
	task.mu.Lock()
	defer task.mu.Unlock()
	return task.lease
}

func workerRunLeaseFromAssignment(orgID string, assignment api.WorkerRunLeaseAssignment) api.WorkerRunLease {
	return api.WorkerRunLease{
		ID: assignment.ID, OrgID: orgID, RunID: assignment.RunID,
		WorkerGroupID:     assignment.WorkerGroupID,
		WorkerInstanceID:  assignment.WorkerInstanceID,
		WorkerEpoch:       assignment.WorkerEpoch,
		LeaseSequence:     assignment.LeaseSequence,
		RuntimeInstanceID: assignment.RuntimeInstanceID,
		ProtocolVersion:   assignment.WorkerProtocolVersion,
		AttemptNumber:     assignment.AttemptNumber,
		Trace:             assignment.Trace,
		ExpiresAt:         assignment.ExpiresAt,
	}
}

func (task *guestRunLeaseTask) handleWait(ctx context.Context, wait *runv0.RunWaitRequested) error {
	if task.waits == nil {
		return errors.New("Run Lease Task wait control is required")
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
			return errors.New("Program resume kind is required")
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
		return errors.New("checkpoint Program event is required")
	}
	task.program.observedEventSeq++
	switch value := event.Event.(type) {
	case *runv0.RunEvent_StdoutChunk:
		return taskControlEvents{task: task}.AppendRunLog(ctx, api.WorkerRunLeaseAssignment{}, api.WorkerLogStreamStdout, task.program.observedEventSeq, value.StdoutChunk)
	case *runv0.RunEvent_StderrChunk:
		return taskControlEvents{task: task}.AppendRunLog(ctx, api.WorkerRunLeaseAssignment{}, api.WorkerLogStreamStderr, task.program.observedEventSeq, value.StderrChunk)
	case *runv0.RunEvent_MetadataUpdated:
		return processRunMetadataEvent(
			ctx,
			taskControlEvents{task: task},
			api.WorkerRunLeaseAssignment{},
			task.program.session.Stream(),
			value.MetadataUpdated,
		)
	case *runv0.RunEvent_StructuredLogRequested:
		return processStructuredLogEvent(
			ctx,
			taskControlEvents{task: task},
			api.WorkerRunLeaseAssignment{},
			task.program.session.Stream(),
			task.program.observedEventSeq,
			value.StructuredLogRequested,
		)
	case *runv0.RunEvent_TaskChildInvokeRequested:
		return task.handleChildTaskInvoke(ctx, value.TaskChildInvokeRequested)
	case *runv0.RunEvent_ActorStartRequested,
		*runv0.RunEvent_ActorStatusRequested,
		*runv0.RunEvent_ActorCloseRequested,
		*runv0.RunEvent_ActorOutputPageRequested:
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
		return errors.New("unsupported Program event while checkpoint pause is pending")
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
			ProgramQuiesced: api.WorkerRunQuiescenceProof{RunID: quiesced.GetRunId(), AttemptNumber: int32(quiesced.GetAttemptNumber()), RunLeaseID: quiesced.GetRunLeaseId()},
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
		ProgramQuiesced: api.WorkerRunQuiescenceProof{
			RunID:         quiesced.GetRunId(),
			AttemptNumber: int32(quiesced.GetAttemptNumber()),
			RunLeaseID:    quiesced.GetRunLeaseId(),
		},
	}, nil
}

func workerActorOutcome(outcome *runv0.ActorOutcome) (api.WorkerActorOutcome, error) {
	if err := validateFreshActorOutcome(outcome); err != nil {
		return api.WorkerActorOutcome{}, err
	}
	converted := api.WorkerActorOutcome{TerminalInputSequence: outcome.GetTerminalInputSequence()}
	switch value := outcome.GetOutcome().(type) {
	case *runv0.ActorOutcome_Succeeded:
		converted.Succeeded = &api.WorkerActorSucceeded{}
	case *runv0.ActorOutcome_Failed:
		failure := canonicalTaskFailure(value.Failed.GetMessage(), value.Failed.DetailsJson)
		converted.Failed = &failure
	default:
		return api.WorkerActorOutcome{}, errors.New("Actor outcome variant is required")
	}
	return converted, nil
}

type taskControlEvents struct {
	task *guestRunLeaseTask
}

func (events taskControlEvents) AppendRunLog(
	ctx context.Context,
	_ api.WorkerRunLeaseAssignment,
	stream api.WorkerLogStream,
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
	return events.task.control.AppendRunLog(logCtx, lease, stream, sequence, content)
}

func (events taskControlEvents) ApplyRunMetadata(
	ctx context.Context,
	_ api.WorkerRunLeaseAssignment,
	request *runv0.MetadataUpdated,
) error {
	control, err := requireRunObservabilityControl(events.task.control)
	if err != nil {
		return err
	}
	controlRequest, err := workerRunMetadataRequest(request)
	if err != nil {
		return err
	}
	return events.task.callRunSourceRuntime(ctx, func(
		callCtx context.Context,
		lease api.WorkerRunLeaseAssignment,
	) error {
		controlRequest.Lease = lease.Fence()
		return sendRunMetadataRequest(callCtx, control, controlRequest)
	})
}

func (events taskControlEvents) RecordStructuredRunLog(
	ctx context.Context,
	_ api.WorkerRunLeaseAssignment,
	sequence uint64,
	request *runv0.StructuredLogRequested,
) error {
	control, err := requireRunObservabilityControl(events.task.control)
	if err != nil {
		return err
	}
	controlRequest, err := workerStructuredLogRequest(request, sequence)
	if err != nil {
		return err
	}
	return events.task.callRunSourceRuntime(ctx, func(
		callCtx context.Context,
		lease api.WorkerRunLeaseAssignment,
	) error {
		controlRequest.Lease = lease.Fence()
		return sendStructuredRunLogRequest(callCtx, control, controlRequest)
	})
}

func (task *guestRunLeaseTask) RenewRunLease(
	ctx context.Context,
) (RunLeaseTaskRenewal, error) {
	task.mu.Lock()
	defer task.mu.Unlock()
	if task.finished || task.finalizingKind != "" {
		return RunLeaseTaskRenewal{}, errors.New("Run Lease Task is not renewable")
	}
	previous := task.lease
	renewed, fence, err := renewRunLeaseAuthority(
		ctx,
		task.control,
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

func renewRunLeaseAuthority(
	ctx context.Context,
	control interface {
		RenewRunLease(context.Context, api.WorkerRunLeaseAssignment) (api.WorkerRunLeaseRenewResponse, error)
	},
	mounts WorkspaceMountSessionRegistry,
	previous api.WorkerRunLeaseAssignment,
	authority *workspacev0.WorkspaceRunAuthority,
) (api.WorkerRunLeaseAssignment, *workspacev0.WorkspaceAuthorityFence, error) {
	controlCtx, cancelControl := context.WithDeadline(ctx, previous.ExpiresAt)
	defer cancelControl()
	var response api.WorkerRunLeaseRenewResponse
	if err := retryRunLeaseRequest(controlCtx, func(requestCtx context.Context) error {
		var requestErr error
		response, requestErr = control.RenewRunLease(requestCtx, previous)
		return requestErr
	}); err != nil {
		if !previous.ExpiresAt.After(time.Now()) {
			return api.WorkerRunLeaseAssignment{}, nil, fmt.Errorf("%w: %v", errRunLeaseAuthorityLapsed, err)
		}
		return api.WorkerRunLeaseAssignment{}, nil, err
	}
	if response.Lease != previous.Fence() ||
		response.BaseWorkspaceVersionID != previous.BaseWorkspaceVersionID {
		return api.WorkerRunLeaseAssignment{}, nil, errors.New(
			"Run Lease renewal response changed its fence or Workspace frontier",
		)
	}
	renewed := previous
	renewed.ExpiresAt = response.ExpiresAt
	if err := validateRunLeaseExpiryAdvance(previous, renewed); err != nil {
		return api.WorkerRunLeaseAssignment{}, nil, err
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
			return api.WorkerRunLeaseAssignment{}, nil, fmt.Errorf("%w: %v", errRunLeaseAuthorityLapsed, err)
		}
		return api.WorkerRunLeaseAssignment{}, nil, err
	}
	return renewed, proto.Clone(fence).(*workspacev0.WorkspaceAuthorityFence), nil
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
	previous api.WorkerRunLeaseAssignment,
	frozen api.WorkerRunLeaseAssignment,
	operationID string,
	kind api.WorkerRunFinalizationKind,
) error {
	task.mu.Lock()
	defer task.mu.Unlock()
	if task.finished {
		return errors.New("Run Lease Task is already finalized")
	}
	if !equalRunLeaseAssignment(task.lease, previous) {
		return errors.New("Workspace finalization previous receipt is not current")
	}
	if err := validateRunLeaseExpiryAdvance(previous, frozen); err != nil {
		return err
	}
	if !frozen.ExpiresAt.After(previous.ExpiresAt) {
		return errors.New("Workspace finalization expiry did not advance")
	}
	if strings.TrimSpace(operationID) == "" ||
		(kind != api.WorkerRunFinalizationCapture && kind != api.WorkerRunFinalizationReset) {
		return errors.New("Workspace finalization identity is invalid")
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
) (api.WorkerTaskWorkspaceCapture, error) {
	task.mu.Lock()
	defer task.mu.Unlock()
	if task.finished || task.finalizingKind != api.WorkerRunFinalizationCapture {
		return api.WorkerTaskWorkspaceCapture{}, errors.New("Run Lease Task is not capturing")
	}
	envelope, err := task.finalizationEnvelope(workspace.FinalizationCaptureKind, nil)
	if err != nil {
		return api.WorkerTaskWorkspaceCapture{}, err
	}
	result, err := task.mounts.CaptureWorkspace(
		ctx,
		&workspacev0.CaptureWorkspaceRequest{Envelope: envelope},
		task.store,
	)
	if err != nil {
		return api.WorkerTaskWorkspaceCapture{}, err
	}
	task.finished = true
	task.clearCapabilities()
	return api.WorkerTaskWorkspaceCapture{
		Receipt: workerWorkspaceFinalizationReceipt(result.Receipt),
		Tree: api.WorkerWorkspaceTreeIdentity{
			Digest: result.ReportedTree.Digest, SizeBytes: result.ReportedTree.SizeBytes,
			EntryCount: int32(result.ReportedTree.EntryCount),
		},
		Artifact: api.WorkerWorkspaceArtifact{
			Digest: result.Artifact.Digest, MediaType: result.Artifact.MediaType,
			Encoding: result.Artifact.Encoding, SizeBytes: result.Artifact.SizeBytes,
			EntryCount: int32(result.Artifact.EntryCount),
		},
	}, nil
}

func (task *guestRunLeaseTask) CreateHandoffCheckpoint(
	ctx context.Context,
	handoff api.WorkerRunFinalizationHandoff,
	checkpointID string,
	capture api.WorkerTaskWorkspaceCapture,
) (api.WorkerCheckpointManifest, error) {
	task.mu.Lock()
	defer task.mu.Unlock()
	if !task.finished || task.finalizingKind != api.WorkerRunFinalizationCapture {
		return api.WorkerCheckpointManifest{}, errors.New("Run Lease Task has not captured its Workspace")
	}
	checkpointer, ok := task.checkpointer.(HandoffCheckpointer)
	if !ok {
		return api.WorkerCheckpointManifest{}, errors.New("Run Lease Task does not support handoff checkpoints")
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
		api.WorkerCheckpointWorkspaceBase{
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
) (api.WorkerTaskWorkspaceRollback, error) {
	task.mu.Lock()
	defer task.mu.Unlock()
	if task.finished || task.finalizingKind != api.WorkerRunFinalizationReset {
		return api.WorkerTaskWorkspaceRollback{}, errors.New("Run Lease Task is not resetting")
	}
	envelope, err := task.finalizationEnvelope(workspace.FinalizationResetKind, task.resetTarget)
	if err != nil {
		return api.WorkerTaskWorkspaceRollback{}, err
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
		return api.WorkerTaskWorkspaceRollback{}, err
	}
	task.finished = true
	task.clearCapabilities()
	return api.WorkerTaskWorkspaceRollback{
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
	previous api.WorkerRunLeaseAssignment,
	next api.WorkerRunLeaseAssignment,
) error {
	previousExpiry := previous.ExpiresAt
	nextExpiry := next.ExpiresAt
	previous.ExpiresAt = time.Time{}
	next.ExpiresAt = time.Time{}
	if !equalRunLeaseAssignment(previous, next) {
		return errors.New("Run Lease renewal changed immutable authority")
	}
	if nextExpiry.Before(previousExpiry) {
		return errors.New("Run Lease expiry moved backwards")
	}
	return nil
}

func runLeaseResetTarget(
	claim *api.WorkerRunLeaseClaimResponse,
) (workspace.ResetTarget, error) {
	if claim == nil {
		return workspace.ResetTarget{}, errors.New("Run Lease claim is required")
	}
	target := claim.Workspace.ResetTarget
	if target.BaseWorkspaceVersionID != claim.Lease.BaseWorkspaceVersionID {
		return workspace.ResetTarget{}, errors.New("Run Lease Workspace Reset target does not match its base version")
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
		return workspace.ResetTarget{}, errors.New("Run Lease Workspace Reset target is invalid")
	}
}

func workerTaskOutcome(outcome *runv0.TaskOutcome) (api.WorkerTaskOutcome, error) {
	if err := validateFreshTaskOutcome(outcome); err != nil {
		return api.WorkerTaskOutcome{}, err
	}
	switch value := outcome.GetOutcome().(type) {
	case *runv0.TaskOutcome_Succeeded:
		return api.WorkerTaskOutcome{Succeeded: &api.WorkerTaskSucceeded{
			Output: json.RawMessage(value.Succeeded.GetOutputJson()),
		}}, nil
	case *runv0.TaskOutcome_Failed:
		failure := canonicalTaskFailure(value.Failed.GetMessage(), value.Failed.DetailsJson)
		return api.WorkerTaskOutcome{Failed: &failure}, nil
	case *runv0.TaskOutcome_PayloadInvalid:
		failure := canonicalTaskFailure(
			value.PayloadInvalid.GetMessage(),
			value.PayloadInvalid.DetailsJson,
		)
		return api.WorkerTaskOutcome{PayloadInvalid: &failure}, nil
	default:
		return api.WorkerTaskOutcome{}, errors.New("Task outcome variant is required")
	}
}

func workerWorkspaceFinalizationReceipt(
	receipt *workspacev0.WorkspaceFinalizationReceipt,
) api.WorkerWorkspaceFinalizationReceipt {
	if receipt == nil {
		return api.WorkerWorkspaceFinalizationReceipt{}
	}
	fence := receipt.GetFence()
	return api.WorkerWorkspaceFinalizationReceipt{
		OperationID: receipt.GetOperationId(), RequestFingerprint: receipt.GetRequestFingerprint(),
		Fence: api.WorkerWorkspaceFinalizationFence{
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

func workerWorkspaceResetTarget(target workspace.ResetTarget) api.WorkerWorkspaceResetTarget {
	result := api.WorkerWorkspaceResetTarget{
		BaseWorkspaceVersionID: target.BaseVersionID,
		Tree: api.WorkerWorkspaceTreeIdentity{
			Digest: target.Tree.Digest, SizeBytes: target.Tree.SizeBytes,
			EntryCount: int32(target.Tree.EntryCount),
		},
	}
	if target.Kind == workspace.ResetTargetEmpty {
		result.Empty = &api.WorkerEmptyWorkspace{}
	} else {
		result.Artifact = &api.WorkerWorkspaceArtifact{
			Digest: target.Artifact.Digest, MediaType: target.Artifact.MediaType,
			Encoding: target.Artifact.Encoding, SizeBytes: target.Artifact.SizeBytes,
			EntryCount: int32(target.Artifact.EntryCount),
		}
	}
	return result
}

func canonicalTaskFailure(message string, details *string) api.WorkerTaskFailure {
	failure := api.WorkerTaskFailure{Message: message}
	if details != nil {
		failure.Details = json.RawMessage(*details)
	}
	return failure
}

var _ RunLeaseTaskRunner = ProgramRunner{}
