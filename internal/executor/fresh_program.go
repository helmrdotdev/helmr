package executor

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"sync"
	"time"
	"unicode/utf8"

	"github.com/helmrdotdev/helmr/internal/frameio"
	"github.com/helmrdotdev/helmr/internal/jsoncanon"
	runv0 "github.com/helmrdotdev/helmr/internal/proto/run/v0"
	workspacev0 "github.com/helmrdotdev/helmr/internal/proto/workspace/v0"
	"github.com/helmrdotdev/helmr/internal/vm"
	"github.com/helmrdotdev/helmr/internal/wire"
	"github.com/helmrdotdev/helmr/internal/workerapi"
)

const (
	maxFreshProgramSecrets     = 64
	maxFreshProgramSecretBytes = 128 << 20
	maxFreshTaskOutputBytes    = 16 << 20
	maxFreshTaskErrorBytes     = 16 << 10
	maxFreshTaskMessageBytes   = 1024
	maxFreshProofFrameBytes    = 64 << 10
	maxFreshOutcomeFrameBytes  = maxFreshTaskOutputBytes + 64<<10
)

type FreshProgramControlPlane interface {
	AcknowledgeRunStart(
		context.Context,
		workerapi.RunStartRequest,
	) (workerapi.RunStartResponse, error)
	AcknowledgeRunEntrypoint(
		context.Context,
		workerapi.RunEntrypointRequest,
	) error
	RenewRunLease(
		context.Context,
		workerapi.RunLeaseAssignment,
	) (workerapi.RunLeaseRenewResponse, error)
}

type freshProgramEventSink interface {
	AppendRunLog(
		context.Context,
		workerapi.RunLeaseAssignment,
		workerapi.LogStream,
		uint64,
		[]byte,
	) error
	ApplyRunMetadata(
		context.Context,
		workerapi.RunLeaseAssignment,
		*runv0.MetadataUpdated,
	) error
	RecordStructuredRunLog(
		context.Context,
		workerapi.RunLeaseAssignment,
		uint64,
		*runv0.StructuredLogRequested,
	) error
}

type freshProgram struct {
	session          vm.Session
	mount            workerapi.WorkspaceMount
	lease            workerapi.RunLeaseAssignment
	authority        *workspacev0.WorkspaceRunAuthority
	entrypoint       *runv0.EntrypointIdentity
	observedEventSeq uint64
}

type newProgramAdmission struct {
	programStart []byte
	start        workerapi.RunStartRequest
	parent       *workspacev0.VerifyProgramRestoreRequest
}

type freshAdmissionState struct {
	mu           sync.Mutex
	lease        workerapi.RunLeaseAssignment
	authority    *workspacev0.WorkspaceRunAuthority
	mounts       WorkspaceMountSessionRegistry
	controlPlane FreshProgramControlPlane
	events       freshProgramEventSink
}

func (state *freshAdmissionState) AppendRunLog(
	ctx context.Context,
	_ workerapi.RunLeaseAssignment,
	stream workerapi.LogStream,
	sequence uint64,
	content []byte,
) error {
	state.mu.Lock()
	defer state.mu.Unlock()
	logCtx, cancel, err := runLeaseLogContext(ctx, state.lease.ExpiresAt)
	if err != nil {
		return err
	}
	defer cancel()
	return state.events.AppendRunLog(
		logCtx,
		state.lease,
		stream,
		sequence,
		content,
	)
}

func (state *freshAdmissionState) ApplyRunMetadata(
	ctx context.Context,
	_ workerapi.RunLeaseAssignment,
	request *runv0.MetadataUpdated,
) error {
	state.mu.Lock()
	defer state.mu.Unlock()
	controlPlane, err := requireRunObservabilityControlPlane(state.controlPlane)
	if err != nil {
		return err
	}
	return updateRunMetadata(ctx, controlPlane, state.lease, request)
}

func (state *freshAdmissionState) RecordStructuredRunLog(
	ctx context.Context,
	_ workerapi.RunLeaseAssignment,
	sequence uint64,
	request *runv0.StructuredLogRequested,
) error {
	state.mu.Lock()
	defer state.mu.Unlock()
	controlPlane, err := requireRunObservabilityControlPlane(state.controlPlane)
	if err != nil {
		return err
	}
	return appendStructuredRunLog(ctx, controlPlane, state.lease, sequence, request)
}

func (state *freshAdmissionState) expiresAt() time.Time {
	state.mu.Lock()
	defer state.mu.Unlock()
	return state.lease.ExpiresAt
}

func (state *freshAdmissionState) renew(ctx context.Context) error {
	state.mu.Lock()
	defer state.mu.Unlock()
	renewed, fence, err := renewRunLeaseAuthority(
		ctx,
		state.controlPlane,
		state.mounts,
		state.lease,
		state.authority,
	)
	if err != nil {
		return err
	}
	state.lease = renewed
	if fence != nil {
		state.authority.Fence = fence
	}
	return nil
}

func (program *freshProgram) awaitTaskCompletion(
	ctx context.Context,
	events freshProgramEventSink,
	wait func(context.Context, *runv0.RunWaitRequested) error,
	sendActorInput func(context.Context, *runv0.ActorInputSendRequested) error,
	createToken func(context.Context, *runv0.TokenCreateRequested) error,
	invokeChildTask func(context.Context, *runv0.TaskChildInvokeRequested) error,
	resourceRuntime ...func(context.Context, *runv0.RunEvent) error,
) (*runv0.TaskOutcome, *runv0.ProgramQuiesced, error) {
	if program == nil || program.session == nil {
		return nil, nil, errors.New("fresh Program session is required")
	}
	defer program.session.Close(context.Background())
	if events == nil {
		return nil, nil, errors.New("fresh Program event sink is required")
	}
	if program.entrypoint == nil || program.entrypoint.GetTask() == nil {
		return nil, nil, errors.New("fresh Program entrypoint is not a Task")
	}
	var outcome *runv0.TaskOutcome
	for {
		var event runv0.RunEvent
		err := readProtoFrameBoundedContext(
			ctx,
			program.session,
			maxFreshOutcomeFrameBytes,
			&event,
		)
		if err != nil {
			return nil, nil, fmt.Errorf("read Task completion event: %w", err)
		}
		program.observedEventSeq++
		switch value := event.Event.(type) {
		case *runv0.RunEvent_StdoutChunk:
			if err := events.AppendRunLog(
				ctx,
				program.lease,
				workerapi.LogStreamStdout,
				program.observedEventSeq,
				value.StdoutChunk,
			); err != nil {
				return nil, nil, fmt.Errorf("append Task stdout: %w", err)
			}
		case *runv0.RunEvent_StderrChunk:
			if err := events.AppendRunLog(
				ctx,
				program.lease,
				workerapi.LogStreamStderr,
				program.observedEventSeq,
				value.StderrChunk,
			); err != nil {
				return nil, nil, fmt.Errorf("append Task stderr: %w", err)
			}
		case *runv0.RunEvent_MetadataUpdated:
			if outcome != nil {
				return nil, nil, errors.New("Program emitted a metadata update after Task outcome")
			}
			if err := processRunMetadataEvent(
				ctx,
				events,
				program.lease,
				program.session.Stream(),
				value.MetadataUpdated,
			); err != nil {
				return nil, nil, fmt.Errorf("update Task Run metadata: %w", err)
			}
		case *runv0.RunEvent_StructuredLogRequested:
			if outcome != nil {
				return nil, nil, errors.New("Program emitted a structured log after Task outcome")
			}
			if err := processStructuredLogEvent(
				ctx,
				events,
				program.lease,
				program.session.Stream(),
				program.observedEventSeq,
				value.StructuredLogRequested,
			); err != nil {
				return nil, nil, fmt.Errorf("append Task structured log: %w", err)
			}
		case *runv0.RunEvent_TaskOutcome:
			if outcome != nil {
				return nil, nil, errors.New("Program emitted more than one Task outcome")
			}
			if err := validateFreshTaskOutcome(value.TaskOutcome); err != nil {
				return nil, nil, err
			}
			outcome = value.TaskOutcome
		case *runv0.RunEvent_RunWaitRequested:
			if outcome != nil {
				return nil, nil, errors.New("Program emitted a Wait after Task outcome")
			}
			if wait == nil {
				return nil, nil, errors.New("fresh Program Wait support is required")
			}
			if err := wait(ctx, value.RunWaitRequested); err != nil {
				return nil, nil, err
			}
		case *runv0.RunEvent_ActorInputSendRequested:
			if outcome != nil {
				return nil, nil, errors.New("Program emitted an Actor input send after Task outcome")
			}
			if sendActorInput == nil {
				return nil, nil, errors.New("fresh Program Actor input send support is required")
			}
			if err := sendActorInput(ctx, value.ActorInputSendRequested); err != nil {
				return nil, nil, err
			}
		case *runv0.RunEvent_TokenCreateRequested:
			if outcome != nil {
				return nil, nil, errors.New("Program emitted a Token create after Task outcome")
			}
			if createToken == nil {
				return nil, nil, errors.New("fresh Program Token create support is required")
			}
			if err := createToken(ctx, value.TokenCreateRequested); err != nil {
				return nil, nil, err
			}
		case *runv0.RunEvent_TaskChildInvokeRequested:
			if outcome != nil {
				return nil, nil, errors.New("Program emitted a child Task invocation after Task outcome")
			}
			if invokeChildTask == nil {
				return nil, nil, errors.New("fresh Program child Task invocation support is required")
			}
			if err := invokeChildTask(ctx, value.TaskChildInvokeRequested); err != nil {
				return nil, nil, err
			}
		case *runv0.RunEvent_ActorStartRequested,
			*runv0.RunEvent_ActorStatusRequested,
			*runv0.RunEvent_ActorCloseRequested,
			*runv0.RunEvent_ActorOutputPageRequested,
			*runv0.RunEvent_WorkspaceCreateRequested,
			*runv0.RunEvent_WorkspaceRetrieveRequested,
			*runv0.RunEvent_WorkspaceFileReadRequested,
			*runv0.RunEvent_WorkspaceFileStatRequested,
			*runv0.RunEvent_WorkspaceFileListRequested,
			*runv0.RunEvent_WorkspaceExecRequested,
			*runv0.RunEvent_WorkspaceDeleteRequested:
			if outcome != nil {
				return nil, nil, errors.New("Program emitted an Actor operation after Task outcome")
			}
			if len(resourceRuntime) != 1 || resourceRuntime[0] == nil {
				return nil, nil, errors.New("fresh Program resource runtime support is required")
			}
			if err := resourceRuntime[0](ctx, &event); err != nil {
				return nil, nil, err
			}
		case *runv0.RunEvent_ProgramQuiesced:
			if outcome == nil {
				return nil, nil, errors.New("Program quiesced before emitting a Task outcome")
			}
			proof := value.ProgramQuiesced
			if proof == nil ||
				proof.GetRunId() != program.lease.RunID ||
				proof.GetAttemptNumber() != uint32(program.lease.AttemptNumber) ||
				proof.GetRunLeaseId() != program.lease.ID {
				return nil, nil, errors.New("Program quiescence proof does not match Run Lease")
			}
			return outcome, proof, nil
		default:
			return nil, nil, errors.New("Program emitted an unsupported Task completion event")
		}
	}
}

func (program *freshProgram) awaitActorCompletion(
	ctx context.Context,
	events freshProgramEventSink,
	wait func(context.Context, *runv0.RunWaitRequested) error,
	turnCommit func(context.Context, *runv0.ActorTurnCommitRequested) error,
	sendActorInput func(context.Context, *runv0.ActorInputSendRequested) error,
	appendActorOutput func(context.Context, *runv0.ActorOutputAppendRequested) error,
	createToken func(context.Context, *runv0.TokenCreateRequested) error,
	invokeChildTask func(context.Context, *runv0.TaskChildInvokeRequested) error,
	resourceRuntime ...func(context.Context, *runv0.RunEvent) error,
) (*runv0.ActorOutcome, *runv0.ProgramQuiesced, error) {
	if program == nil || program.session == nil {
		return nil, nil, errors.New("fresh Program session is required")
	}
	defer program.session.Close(context.Background())
	if events == nil {
		return nil, nil, errors.New("fresh Program event sink is required")
	}
	if program.entrypoint == nil || program.entrypoint.GetActor() == nil {
		return nil, nil, errors.New("fresh Program entrypoint is not an Actor")
	}
	var outcome *runv0.ActorOutcome
	for {
		var event runv0.RunEvent
		if err := readProtoFrameBoundedContext(ctx, program.session, maxFreshOutcomeFrameBytes, &event); err != nil {
			return nil, nil, fmt.Errorf("read Actor completion event: %w", err)
		}
		program.observedEventSeq++
		switch value := event.Event.(type) {
		case *runv0.RunEvent_StdoutChunk:
			if err := events.AppendRunLog(ctx, program.lease, workerapi.LogStreamStdout, program.observedEventSeq, value.StdoutChunk); err != nil {
				return nil, nil, fmt.Errorf("append Actor stdout: %w", err)
			}
		case *runv0.RunEvent_StderrChunk:
			if err := events.AppendRunLog(ctx, program.lease, workerapi.LogStreamStderr, program.observedEventSeq, value.StderrChunk); err != nil {
				return nil, nil, fmt.Errorf("append Actor stderr: %w", err)
			}
		case *runv0.RunEvent_MetadataUpdated:
			if outcome != nil {
				return nil, nil, errors.New("Program emitted a metadata update after Actor outcome")
			}
			if err := processRunMetadataEvent(
				ctx,
				events,
				program.lease,
				program.session.Stream(),
				value.MetadataUpdated,
			); err != nil {
				return nil, nil, fmt.Errorf("update Actor Run metadata: %w", err)
			}
		case *runv0.RunEvent_StructuredLogRequested:
			if outcome != nil {
				return nil, nil, errors.New("Program emitted a structured log after Actor outcome")
			}
			if err := processStructuredLogEvent(
				ctx,
				events,
				program.lease,
				program.session.Stream(),
				program.observedEventSeq,
				value.StructuredLogRequested,
			); err != nil {
				return nil, nil, fmt.Errorf("append Actor structured log: %w", err)
			}
		case *runv0.RunEvent_ActorOutcome:
			if outcome != nil {
				return nil, nil, errors.New("Program emitted more than one Actor outcome")
			}
			if err := validateFreshActorOutcome(value.ActorOutcome); err != nil {
				return nil, nil, err
			}
			outcome = value.ActorOutcome
		case *runv0.RunEvent_RunWaitRequested:
			if outcome != nil {
				return nil, nil, errors.New("Program emitted a Wait after Actor outcome")
			}
			if wait == nil {
				return nil, nil, errors.New("fresh Program Wait support is required")
			}
			if err := wait(ctx, value.RunWaitRequested); err != nil {
				return nil, nil, err
			}
		case *runv0.RunEvent_ActorTurnCommitRequested:
			if outcome != nil {
				return nil, nil, errors.New("Program emitted a turn commit after Actor outcome")
			}
			if turnCommit == nil {
				return nil, nil, errors.New("fresh Actor turn commit support is required")
			}
			if err := turnCommit(ctx, value.ActorTurnCommitRequested); err != nil {
				return nil, nil, err
			}
		case *runv0.RunEvent_ActorInputSendRequested:
			if outcome != nil {
				return nil, nil, errors.New("Program emitted an Actor input send after Actor outcome")
			}
			if sendActorInput == nil {
				return nil, nil, errors.New("fresh Program Actor input send support is required")
			}
			if err := sendActorInput(ctx, value.ActorInputSendRequested); err != nil {
				return nil, nil, err
			}
		case *runv0.RunEvent_ActorOutputAppendRequested:
			if outcome != nil {
				return nil, nil, errors.New("Program emitted an Actor output append after Actor outcome")
			}
			if appendActorOutput == nil {
				return nil, nil, errors.New("fresh Program Actor output append support is required")
			}
			if err := appendActorOutput(ctx, value.ActorOutputAppendRequested); err != nil {
				return nil, nil, err
			}
		case *runv0.RunEvent_TokenCreateRequested:
			if outcome != nil {
				return nil, nil, errors.New("Program emitted a Token create after Actor outcome")
			}
			if createToken == nil {
				return nil, nil, errors.New("fresh Program Token create support is required")
			}
			if err := createToken(ctx, value.TokenCreateRequested); err != nil {
				return nil, nil, err
			}
		case *runv0.RunEvent_TaskChildInvokeRequested:
			if outcome != nil {
				return nil, nil, errors.New("Program emitted a child Task invocation after Actor outcome")
			}
			if invokeChildTask == nil {
				return nil, nil, errors.New("fresh Program child Task invocation support is required")
			}
			if err := invokeChildTask(ctx, value.TaskChildInvokeRequested); err != nil {
				return nil, nil, err
			}
		case *runv0.RunEvent_ActorStartRequested,
			*runv0.RunEvent_ActorStatusRequested,
			*runv0.RunEvent_ActorCloseRequested,
			*runv0.RunEvent_ActorOutputPageRequested,
			*runv0.RunEvent_WorkspaceCreateRequested,
			*runv0.RunEvent_WorkspaceRetrieveRequested,
			*runv0.RunEvent_WorkspaceFileReadRequested,
			*runv0.RunEvent_WorkspaceFileStatRequested,
			*runv0.RunEvent_WorkspaceFileListRequested,
			*runv0.RunEvent_WorkspaceExecRequested,
			*runv0.RunEvent_WorkspaceDeleteRequested:
			if outcome != nil {
				return nil, nil, errors.New("Program emitted an Actor operation after Actor outcome")
			}
			if len(resourceRuntime) != 1 || resourceRuntime[0] == nil {
				return nil, nil, errors.New("fresh Program resource runtime support is required")
			}
			if err := resourceRuntime[0](ctx, &event); err != nil {
				return nil, nil, err
			}
		case *runv0.RunEvent_ProgramQuiesced:
			if outcome == nil {
				return nil, nil, errors.New("Program quiesced before emitting an Actor outcome")
			}
			proof := value.ProgramQuiesced
			if proof == nil || proof.GetRunId() != program.lease.RunID || proof.GetAttemptNumber() != uint32(program.lease.AttemptNumber) || proof.GetRunLeaseId() != program.lease.ID {
				return nil, nil, errors.New("Program quiescence proof does not match Run Lease")
			}
			return outcome, proof, nil
		default:
			return nil, nil, errors.New("Program emitted an unsupported Actor completion event")
		}
	}
}

func validateFreshActorOutcome(outcome *runv0.ActorOutcome) error {
	if outcome == nil {
		return errors.New("Actor outcome is required")
	}
	if outcome.TerminalInputSequence == nil || outcome.GetTerminalInputSequence() < 0 {
		return errors.New("Actor terminal input sequence is negative")
	}
	switch value := outcome.GetOutcome().(type) {
	case *runv0.ActorOutcome_Succeeded:
		if value.Succeeded == nil {
			return errors.New("Actor succeeded outcome is empty")
		}
	case *runv0.ActorOutcome_Failed:
		if value.Failed == nil {
			return errors.New("Actor failed outcome is empty")
		}
		if err := validateFreshTaskFailure(value.Failed.GetMessage(), value.Failed.DetailsJson); err != nil {
			return fmt.Errorf("invalid Actor failure: %w", err)
		}
	default:
		return errors.New("Actor outcome variant is required")
	}
	return nil
}

func validateFreshTaskOutcome(outcome *runv0.TaskOutcome) error {
	if outcome == nil {
		return errors.New("Task outcome is required")
	}
	switch value := outcome.GetOutcome().(type) {
	case *runv0.TaskOutcome_Succeeded:
		if value.Succeeded == nil {
			return errors.New("Task succeeded outcome is empty")
		}
		raw := []byte(value.Succeeded.GetOutputJson())
		if len(raw) == 0 || len(raw) > maxFreshTaskOutputBytes || !utf8.Valid(raw) {
			return errors.New("Task succeeded output is not bounded UTF-8 JSON")
		}
		if _, err := jsoncanon.Transform(raw); err != nil {
			return errors.New("Task succeeded output is not unambiguous JSON")
		}
	case *runv0.TaskOutcome_Failed:
		if value.Failed == nil {
			return errors.New("Task failed outcome is empty")
		}
		if err := validateFreshTaskFailure(
			value.Failed.GetMessage(),
			value.Failed.DetailsJson,
		); err != nil {
			return fmt.Errorf("invalid Task failure: %w", err)
		}
	case *runv0.TaskOutcome_PayloadInvalid:
		if value.PayloadInvalid == nil {
			return errors.New("Task payload-invalid outcome is empty")
		}
		if err := validateFreshTaskFailure(
			value.PayloadInvalid.GetMessage(),
			value.PayloadInvalid.DetailsJson,
		); err != nil {
			return fmt.Errorf("invalid Task payload failure: %w", err)
		}
	default:
		return errors.New("Task outcome variant is required")
	}
	return nil
}

func validateFreshTaskFailure(message string, details *string) error {
	if message == "" || !utf8.ValidString(message) ||
		len(message) > maxFreshTaskMessageBytes {
		return errors.New("message is not bounded UTF-8")
	}
	value := map[string]any{"message": message}
	if details != nil {
		raw := []byte(*details)
		if len(raw) == 0 || !utf8.Valid(raw) {
			return errors.New("details_json is not valid UTF-8 JSON")
		}
		canonical, err := jsoncanon.Transform(raw)
		if err != nil {
			return errors.New("details_json is not unambiguous JSON")
		}
		var parsed any
		if err := json.Unmarshal(canonical, &parsed); err != nil {
			return errors.New("details_json is not valid JSON")
		}
		value["details"] = parsed
	}
	normalizedJSON, err := json.Marshal(value)
	if err != nil {
		return fmt.Errorf("marshal normalized Task error: %w", err)
	}
	normalized, err := jsoncanon.Transform(normalizedJSON)
	if err != nil {
		return fmt.Errorf("canonicalize normalized Task error: %w", err)
	}
	if len(normalized) > maxFreshTaskErrorBytes {
		return errors.New("normalized error exceeds its bound")
	}
	return nil
}

func (r ProgramRunner) startNewProgram(
	ctx context.Context,
	claim *workerapi.RunLeaseClaimResponse,
	controlPlane FreshProgramControlPlane,
	events freshProgramEventSink,
) (freshProgram, error) {
	if claim == nil {
		return freshProgram{}, errors.New("Run Lease claim is required")
	}
	defer clearFreshProgramDelivery(claim)
	if controlPlane == nil {
		return freshProgram{}, errors.New("fresh Program Control Plane is required")
	}
	if events == nil {
		return freshProgram{}, errors.New("fresh Program event sink is required")
	}
	if r.WorkspaceMounts == nil {
		return freshProgram{}, errors.New(
			"workspace mount session registry is required",
		)
	}
	admission, err := validateNewProgramClaim(claim)
	if err != nil {
		return freshProgram{}, err
	}
	admissionCtx, cancelAdmission := context.WithDeadline(
		ctx,
		claim.Lease.StartDeadlineAt,
	)
	defer cancelAdmission()
	opened, err := r.WorkspaceMounts.OpenWorkspaceMountSession(
		admissionCtx,
		claim.Lease.WorkspaceMountID,
	)
	if err != nil {
		return freshProgram{}, err
	}
	keepSession := false
	defer func() {
		if !keepSession {
			_ = opened.Session.Close(context.Background())
		}
	}()
	if opened.Session.Stream() == nil {
		return freshProgram{}, errors.New("Workspace mount stream is required")
	}
	if err := validateNewProgramMount(claim.Lease, opened.Mount); err != nil {
		return freshProgram{}, err
	}
	if admission.parent != nil {
		if opened.ControlSession == nil {
			return freshProgram{}, errors.New(
				"child-attached Program mount control session is required",
			)
		}
		if err := verifyRestoredProgramOnSession(
			admissionCtx,
			opened.ControlSession,
			admission.parent,
		); err != nil {
			return freshProgram{}, fmt.Errorf(
				"verify frozen parent Program: %w",
				err,
			)
		}
	}
	if err := writeFreshProgramContext(
		admissionCtx,
		opened.Session,
		func(stream vm.Stream) error {
			return writeFreshProgramAdmission(
				stream,
				opened.ChannelToken,
				claim,
				admission.programStart,
			)
		},
	); err != nil {
		return freshProgram{}, err
	}
	var event runv0.RunEvent
	if err := readProtoFrameBoundedContext(
		admissionCtx,
		opened.Session,
		maxFreshProofFrameBytes,
		&event,
	); err != nil {
		return freshProgram{}, fmt.Errorf(
			"read Program process-started proof: %w",
			err,
		)
	}
	started := event.GetProgramProcessStarted()
	if started == nil ||
		started.GetRunId() != claim.Lease.RunID ||
		started.GetAttemptNumber() != uint32(claim.Lease.AttemptNumber) ||
		started.GetRunLeaseId() != claim.Lease.ID {
		return freshProgram{}, errors.New(
			"Program process-started proof does not match Run Lease",
		)
	}
	observedEventSeq := uint64(1)
	ackCtx, cancelAck := context.WithDeadline(ctx, claim.Lease.ExpiresAt)
	defer cancelAck()
	var startResponse workerapi.RunStartResponse
	if err := retryRunLeaseRequest(ackCtx, func(requestCtx context.Context) error {
		var requestErr error
		admission.start.Lease = claim.Lease.Fence()
		startResponse, requestErr = controlPlane.AcknowledgeRunStart(
			requestCtx,
			admission.start,
		)
		return requestErr
	}); err != nil {
		return freshProgram{}, fmt.Errorf("acknowledge new Program Run start: %w", err)
	}
	if startResponse.Lease != claim.Lease.Fence() {
		return freshProgram{}, errors.New(
			"Run start acknowledgement changed the Run Lease fence",
		)
	}
	state := &freshAdmissionState{
		lease:        claim.Lease,
		authority:    freshWorkspaceAuthority(claim, opened.ChannelToken),
		mounts:       r.WorkspaceMounts,
		controlPlane: controlPlane,
		events:       events,
	}
	retainAuthority := false
	defer func() {
		if !retainAuthority {
			state.authority.ChannelToken = ""
			state.authority.WriteCapability = ""
		}
	}()
	if err := state.renew(ctx); err != nil {
		return freshProgram{}, fmt.Errorf(
			"renew Run Lease before Program-start release: %w",
			err,
		)
	}
	if !state.expiresAt().After(time.Now()) {
		return freshProgram{}, errors.New(
			"Run Lease expired before Program-start release",
		)
	}
	startReleaseCtx, cancelStartRelease := context.WithDeadline(
		ctx,
		state.expiresAt(),
	)
	defer cancelStartRelease()
	if err := writeFreshProgramContext(
		startReleaseCtx,
		opened.Session,
		func(stream vm.Stream) error {
			return frameio.WriteProtoFrame(
				stream,
				programStartRelease(state.lease),
			)
		},
	); err != nil {
		return freshProgram{}, fmt.Errorf(
			"write Program-start release: %w",
			err,
		)
	}
	entrypointCtx, cancelEntrypoint := context.WithCancel(ctx)
	defer cancelEntrypoint()
	type entrypointResult struct {
		ready *runv0.EntrypointReady
		err   error
	}
	entrypointDone := make(chan entrypointResult, 1)
	go func() {
		ready, readErr := readFreshEntrypointReady(
			entrypointCtx,
			opened.Session,
			state.lease,
			state,
			&observedEventSeq,
		)
		entrypointDone <- entrypointResult{ready: ready, err: readErr}
	}()
	renewTimer := time.NewTimer(runLeaseRenewDelay(state.expiresAt()))
	defer renewTimer.Stop()
	var ready *runv0.EntrypointReady
	for ready == nil {
		select {
		case result := <-entrypointDone:
			if result.err != nil {
				return freshProgram{}, result.err
			}
			ready = result.ready
		case <-renewTimer.C:
			if err := state.renew(ctx); err != nil {
				cancelEntrypoint()
				<-entrypointDone
				return freshProgram{}, fmt.Errorf("renew pre-entrypoint Run Lease: %w", err)
			}
			renewTimer.Reset(runLeaseRenewDelay(state.expiresAt()))
		case <-ctx.Done():
			cancelEntrypoint()
			<-entrypointDone
			return freshProgram{}, ctx.Err()
		}
	}
	state.mu.Lock()
	defer state.mu.Unlock()
	kind, err := validateFreshEntrypoint(ready, state.lease)
	if err != nil {
		return freshProgram{}, err
	}
	entrypointAckCtx, cancelEntrypointAck := context.WithDeadline(ctx, state.lease.ExpiresAt)
	defer cancelEntrypointAck()
	entrypointRequest := workerapi.RunEntrypointRequest{
		Lease:                state.lease.Fence(),
		EntrypointKind:       kind,
		EntrypointDeclaredID: ready.GetEntrypoint().GetDeclaredId(),
	}
	if err := retryRunLeaseRequest(entrypointAckCtx, func(requestCtx context.Context) error {
		return controlPlane.AcknowledgeRunEntrypoint(requestCtx, entrypointRequest)
	}); err != nil {
		return freshProgram{}, fmt.Errorf("acknowledge Run entrypoint: %w", err)
	}
	if err := writeFreshProgramContext(
		entrypointAckCtx,
		opened.Session,
		func(stream vm.Stream) error {
			return frameio.WriteProtoFrame(
				stream,
				&runv0.ProgramSupervisorCommand{
					Command: &runv0.ProgramSupervisorCommand_EntrypointRelease{
						EntrypointRelease: &runv0.EntrypointRelease{
							RunId: state.lease.RunID,
							AttemptNumber: uint32(
								state.lease.AttemptNumber,
							),
							Entrypoint: ready.GetEntrypoint(),
						},
					},
				},
			)
		},
	); err != nil {
		return freshProgram{}, fmt.Errorf("write entrypoint release: %w", err)
	}
	keepSession = true
	retainAuthority = true
	return freshProgram{
		session:          opened.Session,
		mount:            opened.Mount,
		lease:            state.lease,
		authority:        state.authority,
		entrypoint:       ready.GetEntrypoint(),
		observedEventSeq: observedEventSeq,
	}, nil
}

func writeFreshProgramContext(
	ctx context.Context,
	session vm.Session,
	write func(vm.Stream) error,
) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	closed := make(chan struct{})
	stop := context.AfterFunc(ctx, func() {
		_ = session.Close(context.Background())
		close(closed)
	})
	err := write(session.Stream())
	if !stop() {
		<-closed
	}
	if contextErr := ctx.Err(); contextErr != nil {
		return contextErr
	}
	return err
}

func readFreshEntrypointReady(
	ctx context.Context,
	session vm.Session,
	lease workerapi.RunLeaseAssignment,
	events freshProgramEventSink,
	observedEventSeq *uint64,
) (*runv0.EntrypointReady, error) {
	for {
		var event runv0.RunEvent
		if err := readProtoFrameBoundedContext(
			ctx,
			session,
			maxFreshProofFrameBytes,
			&event,
		); err != nil {
			return nil, fmt.Errorf("read entrypoint-ready event: %w", err)
		}
		*observedEventSeq = *observedEventSeq + 1
		switch value := event.Event.(type) {
		case *runv0.RunEvent_StdoutChunk:
			if err := events.AppendRunLog(
				ctx,
				lease,
				workerapi.LogStreamStdout,
				*observedEventSeq,
				value.StdoutChunk,
			); err != nil {
				return nil, fmt.Errorf("append pre-entrypoint stdout: %w", err)
			}
		case *runv0.RunEvent_StderrChunk:
			if err := events.AppendRunLog(
				ctx,
				lease,
				workerapi.LogStreamStderr,
				*observedEventSeq,
				value.StderrChunk,
			); err != nil {
				return nil, fmt.Errorf("append pre-entrypoint stderr: %w", err)
			}
		case *runv0.RunEvent_EntrypointReady:
			return value.EntrypointReady, nil
		default:
			return nil, errors.New(
				"Program emitted an unsupported event before entrypoint-ready",
			)
		}
	}
}

func validateNewProgramClaim(
	claim *workerapi.RunLeaseClaimResponse,
) (newProgramAdmission, error) {
	lease := claim.Lease
	if strings.TrimSpace(lease.ID) == "" ||
		strings.TrimSpace(lease.RunID) == "" ||
		lease.AttemptNumber <= 0 ||
		lease.LeaseSequence <= 0 ||
		strings.TrimSpace(lease.WorkerInstanceID) == "" ||
		lease.WorkerEpoch <= 0 ||
		strings.TrimSpace(lease.RuntimeInstanceID) == "" ||
		strings.TrimSpace(lease.RuntimeIdentityID) == "" ||
		strings.TrimSpace(lease.WorkspaceID) == "" ||
		strings.TrimSpace(lease.WorkspaceMountID) == "" ||
		strings.TrimSpace(lease.WorkspaceLeaseID) == "" ||
		strings.TrimSpace(lease.BaseWorkspaceVersionID) == "" ||
		lease.OwnershipGeneration <= 0 ||
		lease.WriterGeneration <= 0 ||
		lease.MountFencingGeneration <= 0 ||
		lease.StartDeadlineAt.IsZero() ||
		!lease.StartDeadlineAt.After(time.Now()) ||
		lease.ExpiresAt.IsZero() ||
		!lease.ExpiresAt.After(time.Now()) {
		return newProgramAdmission{}, errors.New("new Program Run Lease assignment is incomplete")
	}
	if strings.TrimSpace(claim.Workspace.WriteCapability) == "" {
		return newProgramAdmission{}, errors.New(
			"new Program Workspace write capability is required",
		)
	}
	execution := claim.Execution
	var admission newProgramAdmission
	switch {
	case execution.Fresh != nil &&
		execution.Restore == nil &&
		execution.Attach == nil &&
		len(execution.Fresh.ProgramStart) > 0:
		admission = newProgramAdmission{
			programStart: execution.Fresh.ProgramStart,
			start: workerapi.RunStartRequest{
				Fresh: &workerapi.RunStartFresh{},
			},
		}
	case execution.Fresh == nil &&
		execution.Restore == nil &&
		execution.Attach != nil &&
		execution.Attach.Child != nil &&
		execution.Attach.Parent == nil:
		child := execution.Attach.Child
		if strings.TrimSpace(child.RunWaitID) == "" ||
			strings.TrimSpace(child.ParentRunID) == "" ||
			child.ParentAttemptNumber <= 0 ||
			strings.TrimSpace(child.CheckpointID) == "" ||
			strings.TrimSpace(child.ResumeAttachID) == "" ||
			strings.TrimSpace(child.CorrelationID) == "" ||
			len(child.ProgramStart) == 0 {
			return newProgramAdmission{}, errors.New(
				"child-attached Program authority is incomplete",
			)
		}
		admission = newProgramAdmission{
			programStart: child.ProgramStart,
			parent: &workspacev0.VerifyProgramRestoreRequest{
				RunId:         child.ParentRunID,
				AttemptNumber: uint32(child.ParentAttemptNumber),
				RunWaitId:     child.RunWaitID,
				CheckpointId:  child.CheckpointID,
				CorrelationId: child.CorrelationID,
			},
			start: workerapi.RunStartRequest{
				Attach: &workerapi.RunStartAttach{
					Child: &workerapi.RunStartChildAttach{
						RunWaitID:      child.RunWaitID,
						CheckpointID:   child.CheckpointID,
						ResumeAttachID: child.ResumeAttachID,
					},
				},
			},
		}
	default:
		return newProgramAdmission{}, errors.New(
			"Run Lease execution must contain exactly one fresh or child-attached Program",
		)
	}
	if len(claim.Secrets) > maxFreshProgramSecrets {
		return newProgramAdmission{}, fmt.Errorf(
			"new Program has %d Secrets, exceeds max %d",
			len(claim.Secrets),
			maxFreshProgramSecrets,
		)
	}
	totalSecretBytes := 0
	for _, secret := range claim.Secrets {
		if len(secret.Value) > maxFreshProgramSecretBytes-totalSecretBytes {
			return newProgramAdmission{}, fmt.Errorf(
				"new Program Secret plaintext exceeds max %d bytes",
				maxFreshProgramSecretBytes,
			)
		}
		totalSecretBytes += len(secret.Value)
	}
	return admission, nil
}

func validateNewProgramMount(
	lease workerapi.RunLeaseAssignment,
	mount workerapi.WorkspaceMount,
) error {
	if mount.ID != lease.WorkspaceMountID ||
		mount.WorkspaceID != lease.WorkspaceID ||
		mount.RuntimeInstanceID != lease.RuntimeInstanceID ||
		mount.BaseVersionID != lease.BaseWorkspaceVersionID ||
		mount.FencingGeneration != lease.MountFencingGeneration {
		return errors.New(
			"new Program Workspace mount does not match the claimed physical authority",
		)
	}
	return nil
}

func writeFreshProgramAdmission(
	stream vm.Stream,
	channelToken string,
	claim *workerapi.RunLeaseClaimResponse,
	programStart []byte,
) error {
	lease := claim.Lease
	channelToken = strings.TrimSpace(channelToken)
	if channelToken == "" {
		return errors.New("Workspace mount guest channel token is required")
	}
	if err := wire.WriteStreamFrameHeader(
		stream,
		wire.StreamHeader{
			Type:             wire.StreamTypeProgramRun,
			RunID:            lease.RunID,
			WorkspaceID:      lease.WorkspaceID,
			WorkspaceMountID: lease.WorkspaceMountID,
		},
		0,
	); err != nil {
		return fmt.Errorf("write Program run header: %w", err)
	}
	if err := frameio.WriteProtoFrame(
		stream,
		freshWorkspaceAuthority(claim, channelToken),
	); err != nil {
		return fmt.Errorf("write Program Workspace authority: %w", err)
	}
	if err := frameio.WriteProtoFrame(
		stream,
		&runv0.ProgramRunRequest{
			RunId:               lease.RunID,
			AttemptNumber:       uint32(lease.AttemptNumber),
			RunLeaseId:          lease.ID,
			ProgramStartFrame:   programStart,
			SecretCount:         uint32(len(claim.Secrets)),
			StartDeadlineUnixMs: lease.StartDeadlineAt.UnixMilli(),
		},
	); err != nil {
		return fmt.Errorf("write Program run request: %w", err)
	}
	for index := range claim.Secrets {
		secret, err := freshProgramSecret(claim.Secrets[index])
		if err != nil {
			return err
		}
		writeErr := frameio.WriteProtoFrame(
			stream,
			&runv0.ProgramSupervisorCommand{
				Command: &runv0.ProgramSupervisorCommand_SecretDelivery{
					SecretDelivery: secret,
				},
			},
		)
		clearBytes(secret.Value)
		secret.Value = nil
		if writeErr != nil {
			return fmt.Errorf("write Program Secret delivery: %w", writeErr)
		}
		clearBytes(claim.Secrets[index].Value)
		claim.Secrets[index].Value = nil
	}
	if err := frameio.WriteProtoFrame(
		stream,
		&runv0.ProgramSupervisorCommand{
			Command: &runv0.ProgramSupervisorCommand_SecretsComplete{
				SecretsComplete: &runv0.ProgramSecretsComplete{
					RunId:         lease.RunID,
					AttemptNumber: uint32(lease.AttemptNumber),
					RunLeaseId:    lease.ID,
					SecretCount:   uint32(len(claim.Secrets)),
				},
			},
		},
	); err != nil {
		return fmt.Errorf("write Program Secret completion: %w", err)
	}
	return nil
}

func freshWorkspaceAuthority(
	claim *workerapi.RunLeaseClaimResponse,
	channelToken string,
) *workspacev0.WorkspaceRunAuthority {
	lease := claim.Lease
	return &workspacev0.WorkspaceRunAuthority{
		Fence: &workspacev0.WorkspaceAuthorityFence{
			WorkerInstanceId:       lease.WorkerInstanceID,
			WorkerEpoch:            lease.WorkerEpoch,
			RuntimeInstanceId:      lease.RuntimeInstanceID,
			RuntimeIdentityId:      lease.RuntimeIdentityID,
			WorkspaceId:            lease.WorkspaceID,
			WorkspaceMountId:       lease.WorkspaceMountID,
			RunId:                  lease.RunID,
			AttemptNumber:          uint32(lease.AttemptNumber),
			RunLeaseId:             lease.ID,
			LeaseSequence:          lease.LeaseSequence,
			WorkspaceLeaseId:       lease.WorkspaceLeaseID,
			OwnershipGeneration:    lease.OwnershipGeneration,
			WriterGeneration:       lease.WriterGeneration,
			MountFencingGeneration: lease.MountFencingGeneration,
			ExpiresAtUnixNano:      lease.ExpiresAt.UnixNano(),
			BaseWorkspaceVersionId: lease.BaseWorkspaceVersionID,
		},
		ChannelToken:    channelToken,
		WriteCapability: claim.Workspace.WriteCapability,
	}
}

func freshProgramSecret(
	delivery workerapi.SecretDelivery,
) (*runv0.ProgramSecret, error) {
	secret := &runv0.ProgramSecret{
		Value: append([]byte(nil), delivery.Value...),
	}
	switch {
	case delivery.Env != nil && delivery.File == nil:
		secret.Placement = &runv0.ProgramSecret_Env{
			Env: delivery.Env.Name,
		}
	case delivery.Env == nil && delivery.File != nil:
		secret.Placement = &runv0.ProgramSecret_File{
			File: delivery.File.Path,
		}
	default:
		clearBytes(secret.Value)
		return nil, errors.New(
			"Program Secret must contain exactly one placement",
		)
	}
	return secret, nil
}

func programStartRelease(
	lease workerapi.RunLeaseAssignment,
) *runv0.ProgramSupervisorCommand {
	return &runv0.ProgramSupervisorCommand{
		Command: &runv0.ProgramSupervisorCommand_StartRelease{
			StartRelease: &runv0.ProgramStartRelease{
				RunId:         lease.RunID,
				AttemptNumber: uint32(lease.AttemptNumber),
				RunLeaseId:    lease.ID,
			},
		},
	}
}

func validateFreshEntrypoint(
	ready *runv0.EntrypointReady,
	lease workerapi.RunLeaseAssignment,
) (string, error) {
	if ready == nil ||
		ready.GetRunId() != lease.RunID ||
		ready.GetAttemptNumber() != uint32(lease.AttemptNumber) ||
		ready.GetEntrypoint() == nil ||
		strings.TrimSpace(ready.GetEntrypoint().GetDeclaredId()) == "" {
		return "", errors.New(
			"entrypoint-ready event does not match Run Lease",
		)
	}
	switch ready.GetEntrypoint().GetKind().(type) {
	case *runv0.EntrypointIdentity_Task:
		return "task", nil
	case *runv0.EntrypointIdentity_Actor:
		return "actor", nil
	default:
		return "", errors.New("entrypoint-ready kind is unsupported")
	}
}

func equalRunLeaseAssignment(
	left workerapi.RunLeaseAssignment,
	right workerapi.RunLeaseAssignment,
) bool {
	if !left.StartDeadlineAt.Equal(right.StartDeadlineAt) ||
		!left.ExpiresAt.Equal(right.ExpiresAt) {
		return false
	}
	left.StartDeadlineAt = time.Time{}
	left.ExpiresAt = time.Time{}
	right.StartDeadlineAt = time.Time{}
	right.ExpiresAt = time.Time{}
	return left == right
}

func clearFreshProgramDelivery(claim *workerapi.RunLeaseClaimResponse) {
	if claim == nil {
		return
	}
	if claim.Execution.Fresh != nil {
		clearBytes(claim.Execution.Fresh.ProgramStart)
		claim.Execution.Fresh.ProgramStart = nil
	}
	if claim.Execution.Attach != nil && claim.Execution.Attach.Child != nil {
		clearBytes(claim.Execution.Attach.Child.ProgramStart)
		claim.Execution.Attach.Child.ProgramStart = nil
	}
	for index := range claim.Secrets {
		clearBytes(claim.Secrets[index].Value)
		claim.Secrets[index].Value = nil
	}
	claim.Workspace.WriteCapability = ""
}

func clearBytes(value []byte) {
	for index := range value {
		value[index] = 0
	}
}
