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
	"github.com/helmrdotdev/helmr/internal/computerhost"
	computerv0 "github.com/helmrdotdev/helmr/internal/proto/computer/v0"
	programv0 "github.com/helmrdotdev/helmr/internal/proto/program/v0"
	"github.com/helmrdotdev/helmr/internal/wire"
	"github.com/helmrdotdev/helmr/internal/workerapi"
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
	CreateRuntimeToken(context.Context, workerapi.CreateTokenRequest) (api.TokenResponse, error)
	AppendRunLog(context.Context, workerapi.RunLeaseAssignment, workerapi.LogStream, uint64, []byte) error
}

type ActorRuntimeControlPlane interface {
	StartRunActor(context.Context, workerapi.StartActorRequest) (workerapi.StartActorResponse, error)
	GetRunSessionStatus(context.Context, workerapi.SessionReferenceRequest) (workerapi.SessionStatusResponse, error)
	CloseRunSession(context.Context, workerapi.CloseSessionRequest) (workerapi.CloseSessionResponse, error)
	CancelRunSession(context.Context, workerapi.CancelSessionRequest) (workerapi.CancelSessionResponse, error)
	ReadRunSessionEvents(context.Context, workerapi.ReadSessionEventsRequest) (workerapi.ReadSessionEventsResponse, error)
}

type ComputerRuntimeControlPlane interface {
	CreateRunComputer(context.Context, workerapi.CreateComputerRequest) (workerapi.CreateComputerResponse, error)
	RetrieveRunComputer(context.Context, workerapi.RetrieveComputerRequest) (workerapi.RetrieveComputerResponse, error)
	ListRunComputerMembers(context.Context, workerapi.ComputerMembersRequest) (workerapi.ComputerMembersResponse, error)
	DeleteRunComputer(context.Context, workerapi.DeleteComputerRequest) (workerapi.DeleteComputerResponse, error)
}

// ControlPlane is the complete set of Control Plane capabilities a run lease
// task uses. Every capability is required: NewProgramRunner validates it, and
// StartRunLeaseTask validates it again before starting any work.
type ControlPlane struct {
	Leases        RunLeaseControlPlane
	Waits         RunWaitClient
	Observability RunObservabilityControlPlane
	Sessions      SessionControlPlane
	Actors        ActorRuntimeControlPlane
	Computers     ComputerRuntimeControlPlane
	Children      ChildTaskControlPlane
}

// Validate reports the first missing Control Plane capability.
func (c ControlPlane) Validate() error {
	switch {
	case c.Leases == nil:
		return errors.New("run lease control plane is required")
	case c.Waits == nil:
		return errors.New("run wait control plane is required")
	case c.Observability == nil:
		return errors.New("run observability control plane is required")
	case c.Sessions == nil:
		return errors.New("session control plane is required")
	case c.Actors == nil:
		return errors.New("actor runtime control plane is required")
	case c.Computers == nil:
		return errors.New("computer runtime control plane is required")
	case c.Children == nil:
		return errors.New("child task control plane is required")
	}
	return nil
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
}

func (task *guestRunLeaseTask) Close() {
	if task.program.protocol != nil {
		_ = task.program.protocol.Close()
	}
	task.mu.Lock()
	task.finished = true
	task.clearCapabilities()
	task.mu.Unlock()
}

type RunLeaseTaskRunner interface {
	StartRunLeaseTask(context.Context, *workerapi.RunLeaseClaimResponse) (RunLeaseTask, error)
}

type guestRunLeaseTask struct {
	resumeWait   *programv0.ResumeAttach
	captures     *computerhost.CaptureRuns
	stopMu       sync.Mutex
	stopDeadline time.Time
	program      freshProgram
	mounts       MountRegistry
	store        cas.Store
	controlPlane ControlPlane
	waitComputer workerapi.Computer
	orgID        string

	renewalGate             sync.Mutex
	mu                      sync.Mutex
	lease                   workerapi.RunLeaseAssignment
	authority               *computerv0.ComputerRunAuthority
	checkpointFrozen        bool
	capturePaused           bool
	captureCheckpoint       string
	captureAttachSequence   uint64
	capturePreparedSequence uint64
	finished                bool
}

func (task *guestRunLeaseTask) callRunSourceRuntime(
	ctx context.Context,
	call func(context.Context, workerapi.RunLeaseAssignment) error,
) error {
	return retryRunLeaseRequest(ctx, func(callCtx context.Context) error {
		// Snapshot the current local authority for this bounded attempt. The
		// immutable fence identifies the operation while a concurrent renewal may
		// advance only its expiry. Neither external Control Plane I/O nor retry
		// delays may hold the lock needed by that renewal.
		task.mu.Lock()
		if task.finished {
			task.mu.Unlock()
			return errRunSourceOperationUnavailable
		}
		lease := task.lease
		task.mu.Unlock()
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
) (RunLeaseTask, error) {
	// A runner built without NewProgramRunner fails the lease here instead of
	// when a guest event first needs the missing collaborator.
	if err := r.validate(); err != nil {
		return nil, err
	}
	target, err := runLeaseMountTarget(claim)
	if err != nil {
		return nil, err
	}
	_, err = checkpointComputerBase(target)
	if err != nil {
		return nil, err
	}
	var program freshProgram
	var resumedWait *programv0.ResumeAttach
	if claim.ProgramResume != nil {
		program, resumedWait, err = r.startRestoredProgram(ctx, claim)
	} else {
		// Admission takes its lease capability as a narrow argument so that
		// admission can be exercised with a lease-only fake.
		program, err = r.startNewProgram(ctx, claim, r.ControlPlane.Leases, runLeaseProgramEventSink{controlPlane: r.ControlPlane})
	}
	if err != nil {
		return nil, err
	}
	authority := program.authority
	program.authority = nil
	task := &guestRunLeaseTask{
		resumeWait:   resumedWait,
		captures:     r.ComputerCaptures,
		program:      program,
		mounts:       r.Mounts,
		store:        r.CAS,
		controlPlane: r.ControlPlane,
		lease:        program.lease,
		authority:    authority,
		orgID:        program.mount.OrgID,
		waitComputer: waitComputerForRun(
			program.mount,
			claim.Computer.Target,
		),
	}

	task.program.protocol = newProgramProtocol(program.channel.Stream())
	return task, nil
}

func waitComputerForRun(
	mount workerapi.ComputerInstanceAssignment,
	target workerapi.ComputerMountTarget,
) workerapi.Computer {
	return workerapi.Computer{
		ID:                        mount.ComputerID,
		ComputerInstanceID:        mount.ComputerInstanceID,
		WriterGeneration:          mount.WriterGeneration,
		BaseComputerDiskVersionID: target.BaseComputerDiskVersionID,
		MountPath:                 mount.ComputerMountPath,
	}
}

type runLeaseProgramEventSink struct {
	controlPlane ControlPlane
}

func (sink runLeaseProgramEventSink) AppendRunLog(
	ctx context.Context,
	lease workerapi.RunLeaseAssignment,
	stream workerapi.LogStream,
	sequence uint64,
	content []byte,
) error {
	return sink.controlPlane.Leases.AppendRunLog(ctx, lease, stream, sequence, content)
}

func (sink runLeaseProgramEventSink) ApplyRunMetadata(
	ctx context.Context,
	lease workerapi.RunLeaseAssignment,
	request *programv0.MetadataUpdated,
) error {
	return updateRunMetadata(ctx, sink.controlPlane.Observability, lease, request)
}

func (sink runLeaseProgramEventSink) RecordStructuredRunLog(
	ctx context.Context,
	lease workerapi.RunLeaseAssignment,
	sequence uint64,
	request *programv0.StructuredLogRequested,
) error {
	return appendStructuredRunLog(ctx, sink.controlPlane.Observability, lease, sequence, request)
}

func (task *guestRunLeaseTask) runWaits() ControlPlaneRunWaits {
	return ControlPlaneRunWaits{Client: task.controlPlane.Waits}
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
		WorkerGroupID:      assignment.WorkerGroupID,
		WorkerHostID:       assignment.WorkerHostID,
		WorkerEpoch:        assignment.WorkerEpoch,
		LeaseSequence:      assignment.LeaseSequence,
		ComputerInstanceID: assignment.ComputerInstanceID,
		AttemptNumber:      assignment.AttemptNumber,
		Trace:              assignment.Trace,
		ExpiresAt:          assignment.ExpiresAt,
	}
}

func (task *guestRunLeaseTask) handleWait(ctx context.Context, wait *programv0.RunWaitRequested) error {
	if err := task.validateWaitScope(wait.GetExecution(), wait.TurnId); err != nil {
		return err
	}
	runtimeWait, err := parseWaitRequest(task, wait)
	if err != nil {
		return err
	}
	runtimeWait.Leases = task
	runtimeWait.Computer = task.waitComputer
	runtimeWait.Resume = func(resumeCtx context.Context, decision WaitResumeDecision) error {
		if err := task.beforeWaitResume(resumeCtx, decision); err != nil {
			return err
		}
		if strings.TrimSpace(decision.Kind) == "" {
			return errors.New("program resume kind is required")
		}
		if len(decision.Data) == 0 {
			decision.Data = json.RawMessage(`null`)
		}
		if err := wire.WriteResumeDecision(task.programStream(), &programv0.ResumeDecision{
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
	return task.runHotWait(ctx, runtimeWait, task.runWaits().Wait)
}

func (task *guestRunLeaseTask) processCheckpointRunEvent(ctx context.Context, event *programv0.RunEvent) error {
	if event == nil {
		return errors.New("checkpoint program event is required")
	}
	task.program.observedEventSeq++
	switch value := event.Event.(type) {
	case *programv0.RunEvent_StdoutChunk:
		return taskControlEvents{task: task}.AppendRunLog(ctx, workerapi.RunLeaseAssignment{}, workerapi.LogStreamStdout, task.program.observedEventSeq, value.StdoutChunk)
	case *programv0.RunEvent_StderrChunk:
		return taskControlEvents{task: task}.AppendRunLog(ctx, workerapi.RunLeaseAssignment{}, workerapi.LogStreamStderr, task.program.observedEventSeq, value.StderrChunk)
	case *programv0.RunEvent_MetadataUpdated:
		return processRunMetadataEvent(
			ctx,
			taskControlEvents{task: task},
			workerapi.RunLeaseAssignment{},
			task.programStream(),
			value.MetadataUpdated,
		)
	case *programv0.RunEvent_StructuredLogRequested:
		return processStructuredLogEvent(
			ctx,
			taskControlEvents{task: task},
			workerapi.RunLeaseAssignment{},
			task.programStream(),
			task.program.observedEventSeq,
			value.StructuredLogRequested,
		)
	case *programv0.RunEvent_TaskChildInvokeRequested:
		if value.TaskChildInvokeRequested.GetMethod() != "start" {
			return errors.New("concurrent consuming child wait")
		}
		return task.handleChildTaskInvoke(ctx, value.TaskChildInvokeRequested)
	case *programv0.RunEvent_TurnOutputWriteRequested:
		return task.handleTurnOutput(ctx, value.TurnOutputWriteRequested)
	case *programv0.RunEvent_SessionSubmitRequested:
		return task.handleSessionSubmit(ctx, value.SessionSubmitRequested)
	case *programv0.RunEvent_TokenCreateRequested:
		return task.handleTokenCreate(ctx, value.TokenCreateRequested)
	case *programv0.RunEvent_TurnReadyRequested,
		*programv0.RunEvent_TurnSettlementBeginRequested,
		*programv0.RunEvent_TurnMessageClaimRequested,
		*programv0.RunEvent_TurnMessageCompleteRequested,
		*programv0.RunEvent_SessionOutputWriteRequested,
		*programv0.RunEvent_SessionTurnRetrieveRequested,
		*programv0.RunEvent_SessionTurnInterruptRequested,
		*programv0.RunEvent_SessionResumeRequested,
		*programv0.RunEvent_ActorStartRequested,
		*programv0.RunEvent_SessionStatusRequested,
		*programv0.RunEvent_SessionCloseRequested,
		*programv0.RunEvent_SessionEventsRequested:
		return task.handleResourceRuntime(ctx, event)
	case *programv0.RunEvent_ComputerCreateRequested,
		*programv0.RunEvent_ComputerRetrieveRequested,
		*programv0.RunEvent_ComputerMembersRequested,
		*programv0.RunEvent_ComputerDeleteRequested:
		return task.handleComputerRuntime(ctx, event)
	default:
		return errors.New("unsupported program event while checkpoint pause is pending")
	}
}

func (task *guestRunLeaseTask) Wait(ctx context.Context) (RunLeaseTaskResult, error) {
	if task.program.protocol != nil {
		defer task.program.protocol.Close()
	}
	stopCtx, cancelStop := context.WithCancel(ctx)
	defer cancelStop()
	if task.program.execution != nil {
		go func() {
			if err := task.pollSessionStop(stopCtx); err != nil && stopCtx.Err() == nil {
				_ = task.programStream().Close()
			}
		}()
	}

	if task.resumeWait != nil {
		if err := task.continueRestoredWait(ctx, task.resumeWait); err != nil {
			return RunLeaseTaskResult{}, err
		}
		task.resumeWait = nil
	}
	if task.program.entrypoint != nil && task.program.entrypoint.GetActor() != nil {
		outcome, quiesced, err := task.program.awaitActorCompletion(
			ctx,
			taskControlEvents{task: task},
			task.handleWait,
			task.handleTurnSettle,
			task.handleSessionSubmit,
			task.handleTurnOutput,
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
		task.handleSessionSubmit,
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

func workerActorOutcome(outcome *programv0.ActorOutcome) (workerapi.ActorOutcome, error) {
	if err := validateFreshActorOutcome(outcome); err != nil {
		return workerapi.ActorOutcome{}, err
	}
	converted := workerapi.ActorOutcome{RunGeneration: outcome.GetRunGeneration()}
	switch value := outcome.GetOutcome().(type) {
	case *programv0.ActorOutcome_Succeeded:
		converted.Succeeded = &workerapi.ActorSucceeded{}
	case *programv0.ActorOutcome_Interrupted:
		converted.Interrupted = &workerapi.ActorInterrupted{HoldID: value.Interrupted.GetHoldId(), TurnID: value.Interrupted.TurnId}
	case *programv0.ActorOutcome_Failed:
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
	return events.task.callRunSourceRuntime(ctx, func(callCtx context.Context, lease workerapi.RunLeaseAssignment) error {
		return events.task.controlPlane.Leases.AppendRunLog(callCtx, lease, stream, sequence, content)
	})
}

func (events taskControlEvents) ApplyRunMetadata(
	ctx context.Context,
	_ workerapi.RunLeaseAssignment,
	request *programv0.MetadataUpdated,
) error {
	controlPlane := events.task.controlPlane.Observability
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
	request *programv0.StructuredLogRequested,
) error {
	controlPlane := events.task.controlPlane.Observability
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
	if task.finished {
		return RunLeaseTaskRenewal{}, errors.New("run lease task is not renewable")
	}
	previous := task.lease
	if task.checkpointFrozen {
		renewed, err := renewControlPlaneRunLeaseAuthority(ctx, task.controlPlane.Leases, previous)
		if err != nil {
			return RunLeaseTaskRenewal{}, err
		}
		task.lease = renewed
		return RunLeaseTaskRenewal{Previous: previous, Lease: renewed}, nil
	}
	renewed, fence, err := renewRunLeaseAuthority(
		ctx,
		task.controlPlane.Leases,
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
	mounts MountRegistry,
	previous workerapi.RunLeaseAssignment,
	authority *computerv0.ComputerRunAuthority,
) (workerapi.RunLeaseAssignment, *computerv0.ComputerAuthorityFence, error) {
	renewed, err := renewControlPlaneRunLeaseAuthority(ctx, controlPlane, previous)
	if err != nil {
		return workerapi.RunLeaseAssignment{}, nil, err
	}
	if renewed.ExpiresAt.Equal(previous.ExpiresAt) {
		return previous, nil, nil
	}
	guestCtx, cancelGuest := context.WithDeadline(context.Background(), renewed.ExpiresAt)
	defer cancelGuest()
	var fence *computerv0.ComputerAuthorityFence
	if err := retryComputerAuthorityTransport(guestCtx, func(requestCtx context.Context) error {
		var requestErr error
		fence, requestErr = mounts.RenewComputerAuthority(
			requestCtx,
			&computerv0.RenewComputerAuthorityRequest{
				Previous:             proto.Clone(authority).(*computerv0.ComputerRunAuthority),
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
	return renewed, proto.Clone(fence).(*computerv0.ComputerAuthorityFence), nil
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
		response.BaseComputerDiskVersionID != previous.BaseComputerDiskVersionID {
		return workerapi.RunLeaseAssignment{}, errors.New(
			"run lease renewal response changed its fence or computer frontier",
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

func retryComputerAuthorityTransport(
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
		if !errors.Is(err, computerhost.ErrControlTransport) {
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

func (task *guestRunLeaseTask) clearCapabilities() {
	if task.authority != nil {
		task.authority.ChannelCredential = ""
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

func runLeaseMountTarget(claim *workerapi.RunLeaseClaimResponse) (workerapi.ComputerMountTarget, error) {
	if claim == nil {
		return workerapi.ComputerMountTarget{}, errors.New("run lease claim is required")
	}
	target := claim.Computer.Target
	if target.BaseComputerDiskVersionID != claim.Lease.BaseComputerDiskVersionID {
		return workerapi.ComputerMountTarget{}, errors.New("computer mount target does not match lease base")
	}
	return target, validateComputerMountTarget(target)
}

func checkpointComputerBase(target workerapi.ComputerMountTarget) (workerapi.CheckpointComputerBase, error) {
	if err := validateComputerMountTarget(target); err != nil {
		return workerapi.CheckpointComputerBase{}, err
	}
	return workerapi.CheckpointComputerBase{MountPath: "/workspace"}, nil
}

func workerTaskOutcome(outcome *programv0.TaskOutcome) (workerapi.TaskOutcome, error) {
	if err := validateFreshTaskOutcome(outcome); err != nil {
		return workerapi.TaskOutcome{}, err
	}
	switch value := outcome.GetOutcome().(type) {
	case *programv0.TaskOutcome_Succeeded:
		return workerapi.TaskOutcome{Succeeded: &workerapi.TaskSucceeded{
			Output: json.RawMessage(value.Succeeded.GetOutputJson()),
		}}, nil
	case *programv0.TaskOutcome_Failed:
		failure := canonicalTaskFailure(value.Failed.GetMessage(), value.Failed.DetailsJson)
		return workerapi.TaskOutcome{Failed: &failure}, nil
	case *programv0.TaskOutcome_PayloadInvalid:
		failure := canonicalTaskFailure(
			value.PayloadInvalid.GetMessage(),
			value.PayloadInvalid.DetailsJson,
		)
		return workerapi.TaskOutcome{PayloadInvalid: &failure}, nil
	default:
		return workerapi.TaskOutcome{}, errors.New("task outcome variant is required")
	}
}

func canonicalTaskFailure(message string, details *string) workerapi.TaskFailure {
	failure := workerapi.TaskFailure{Message: message}
	if details != nil {
		failure.Details = json.RawMessage(*details)
	}
	return failure
}

var _ RunLeaseTaskRunner = ProgramRunner{}
