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
	"github.com/helmrdotdev/helmr/internal/workspace"
	"google.golang.org/protobuf/proto"
)

var errRunLeaseAuthorityLapsed = errors.New("Run Lease authority lapsed")

type RunLeaseControl interface {
	ClaimRunLease(context.Context, api.WorkerRunLeaseWork) (api.WorkerRunLeaseClaimResponse, error)
	AcknowledgeRunStart(context.Context, api.WorkerRunLeaseReceipt) (api.WorkerRunStartResponse, error)
	AcknowledgeRunEntrypoint(context.Context, api.WorkerRunEntrypointRequest) error
	RenewRunLease(context.Context, api.WorkerRunLeaseReceipt) (api.WorkerRunLeaseRenewResponse, error)
	BeginRunFinalization(context.Context, api.WorkerBeginRunFinalizationRequest) (api.WorkerBeginRunFinalizationResponse, error)
	CompleteTask(context.Context, api.WorkerCompleteTaskRequest) error
	AppendRunLog(context.Context, api.WorkerRunLeaseReceipt, api.WorkerLogStream, uint64, []byte) error
}

type RunLeaseTaskResult struct {
	Outcome         api.WorkerTaskOutcome
	ProgramQuiesced api.WorkerRunQuiescenceProof
}

type RunLeaseTask interface {
	Close()
	Wait(context.Context) (RunLeaseTaskResult, error)
	RenewRunLease(context.Context) (api.WorkerRunLeaseReceipt, error)
	BeginWorkspaceFinalization(context.Context, api.WorkerRunLeaseReceipt, api.WorkerRunLeaseReceipt, string, api.WorkerRunFinalizationKind) error
	CaptureWorkspace(context.Context) (api.WorkerTaskWorkspaceCapture, error)
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
	program     freshProgram
	mounts      WorkspaceMountSessionRegistry
	store       cas.Store
	control     RunLeaseControl
	resetTarget workspace.ResetTarget

	mu             sync.Mutex
	lease          api.WorkerRunLeaseReceipt
	authority      *workspacev0.WorkspaceRunAuthority
	operationID    string
	finalizingKind api.WorkerRunFinalizationKind
	finished       bool
}

func (r GuestRunner) StartRunLeaseTask(
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
	program, err := r.startFreshProgram(ctx, claim, control, control)
	if err != nil {
		return nil, err
	}
	authority := program.authority
	program.authority = nil
	return &guestRunLeaseTask{
		program:     program,
		mounts:      r.WorkspaceMounts,
		store:       r.CAS,
		control:     control,
		resetTarget: target,
		lease:       program.lease,
		authority:   authority,
	}, nil
}

func (task *guestRunLeaseTask) Wait(ctx context.Context) (RunLeaseTaskResult, error) {
	outcome, quiesced, err := task.program.awaitTaskCompletion(ctx, taskControlEvents{task: task})
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

type taskControlEvents struct {
	task *guestRunLeaseTask
}

func (events taskControlEvents) AppendRunLog(
	ctx context.Context,
	_ api.WorkerRunLeaseReceipt,
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

func (task *guestRunLeaseTask) RenewRunLease(
	ctx context.Context,
) (api.WorkerRunLeaseReceipt, error) {
	task.mu.Lock()
	defer task.mu.Unlock()
	if task.finished || task.finalizingKind != "" {
		return api.WorkerRunLeaseReceipt{}, errors.New("Run Lease Task is not renewable")
	}
	renewed, fence, err := renewRunLeaseAuthority(
		ctx,
		task.control,
		task.mounts,
		task.lease,
		task.authority,
	)
	if err != nil {
		return api.WorkerRunLeaseReceipt{}, err
	}
	if fence != nil {
		task.authority.Fence = fence
	}
	task.lease = renewed
	return renewed, nil
}

func renewRunLeaseAuthority(
	ctx context.Context,
	control interface {
		RenewRunLease(context.Context, api.WorkerRunLeaseReceipt) (api.WorkerRunLeaseRenewResponse, error)
	},
	mounts WorkspaceMountSessionRegistry,
	previous api.WorkerRunLeaseReceipt,
	authority *workspacev0.WorkspaceRunAuthority,
) (api.WorkerRunLeaseReceipt, *workspacev0.WorkspaceAuthorityFence, error) {
	controlCtx, cancelControl := context.WithDeadline(ctx, previous.ExpiresAt)
	defer cancelControl()
	var response api.WorkerRunLeaseRenewResponse
	if err := retryRunLeaseRequest(controlCtx, func(requestCtx context.Context) error {
		var requestErr error
		response, requestErr = control.RenewRunLease(requestCtx, previous)
		return requestErr
	}); err != nil {
		if !previous.ExpiresAt.After(time.Now()) {
			return api.WorkerRunLeaseReceipt{}, nil, fmt.Errorf("%w: %v", errRunLeaseAuthorityLapsed, err)
		}
		return api.WorkerRunLeaseReceipt{}, nil, err
	}
	if err := validateReceiptExpiryAdvance(previous, response.Lease); err != nil {
		return api.WorkerRunLeaseReceipt{}, nil, err
	}
	if response.Lease.ExpiresAt.Equal(previous.ExpiresAt) {
		return previous, nil, nil
	}
	guestCtx, cancelGuest := context.WithDeadline(context.Background(), response.Lease.ExpiresAt)
	defer cancelGuest()
	var fence *workspacev0.WorkspaceAuthorityFence
	if err := retryWorkspaceAuthorityTransport(guestCtx, func(requestCtx context.Context) error {
		var requestErr error
		fence, requestErr = mounts.RenewWorkspaceAuthority(
			requestCtx,
			&workspacev0.RenewWorkspaceAuthorityRequest{
				Previous:             proto.Clone(authority).(*workspacev0.WorkspaceRunAuthority),
				NewExpiresAtUnixNano: response.Lease.ExpiresAt.UnixNano(),
			},
		)
		return requestErr
	}); err != nil {
		if !response.Lease.ExpiresAt.After(time.Now()) {
			return api.WorkerRunLeaseReceipt{}, nil, fmt.Errorf("%w: %v", errRunLeaseAuthorityLapsed, err)
		}
		return api.WorkerRunLeaseReceipt{}, nil, err
	}
	return response.Lease, proto.Clone(fence).(*workspacev0.WorkspaceAuthorityFence), nil
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
	previous api.WorkerRunLeaseReceipt,
	frozen api.WorkerRunLeaseReceipt,
	operationID string,
	kind api.WorkerRunFinalizationKind,
) error {
	task.mu.Lock()
	defer task.mu.Unlock()
	if task.finished {
		return errors.New("Run Lease Task is already finalized")
	}
	if !equalRunLeaseReceipt(task.lease, previous) {
		return errors.New("Workspace finalization previous receipt is not current")
	}
	if err := validateReceiptExpiryAdvance(previous, frozen); err != nil {
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

func validateReceiptExpiryAdvance(
	previous api.WorkerRunLeaseReceipt,
	next api.WorkerRunLeaseReceipt,
) error {
	previousExpiry := previous.ExpiresAt
	nextExpiry := next.ExpiresAt
	previous.ExpiresAt = time.Time{}
	next.ExpiresAt = time.Time{}
	if !equalRunLeaseReceipt(previous, next) {
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

var _ RunLeaseTaskRunner = GuestRunner{}
