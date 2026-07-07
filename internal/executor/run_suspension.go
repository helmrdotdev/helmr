package executor

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/helmrdotdev/helmr/internal/api"
	"github.com/helmrdotdev/helmr/internal/workspace"
)

type RunWaitClient interface {
	CreateRunWait(context.Context, api.WorkerCreateRunWaitRequest) (api.WorkerCreateRunWaitResponse, error)
	FollowWorkerCommands(context.Context, int64, func(api.WorkerCommand) error) error
	AcceptWorkerCommand(context.Context, int64) (api.WorkerCommandAcceptResponse, error)
	AcknowledgeWorkerCommand(context.Context, int64) (api.WorkerCommandAckResponse, error)
	ClaimRunCheckpointWait(context.Context, api.WorkerCheckpointClaimRequest) (api.WorkerCheckpointClaimResponse, error)
	CaptureRunWaitWorkspace(context.Context, api.WorkerRunWaitWorkspaceCaptureRequest) (api.WorkerRunWaitWorkspaceCaptureResponse, error)
	AcknowledgeRestore(context.Context, api.WorkerAcknowledgeRestoreRequest) (api.WorkerAcknowledgeRestoreResponse, error)
	MarkCheckpointReady(context.Context, api.WorkerCheckpointReadyRequest) (api.WorkerCheckpointResponse, error)
	MarkCheckpointFailed(context.Context, api.WorkerCheckpointFailedRequest) (api.WorkerCheckpointResponse, error)
}

type ControlRunWaits struct {
	Client RunWaitClient
}

var errWorkerCommandHandled = errors.New("worker command handled")
var errStaleWorkerCommand = errors.New("stale worker command")
var errCheckpointAttemptRecorded = errors.New("checkpoint attempt failure recorded")

type RestoreAcknowledgement struct {
	Lease        api.WorkerRunLease
	RunWaitID    string
	CheckpointID string
	Phases       []api.WorkerCheckpointPhase
}

type RestoreAcknowledger interface {
	AcknowledgeRestore(context.Context, RestoreAcknowledgement) error
}

func (w ControlRunWaits) AcknowledgeRestore(ctx context.Context, request RestoreAcknowledgement) error {
	if w.Client == nil {
		return errors.New("run wait control client is required")
	}
	_, err := w.Client.AcknowledgeRestore(ctx, api.WorkerAcknowledgeRestoreRequest{
		Lease:        request.Lease,
		RunWaitID:    request.RunWaitID,
		CheckpointID: request.CheckpointID,
		Phases:       request.Phases,
	})
	return err
}

func (w ControlRunWaits) Wait(ctx context.Context, request WaitRequest) error {
	if w.Client == nil {
		return errors.New("run wait control client is required")
	}
	opened, err := w.AddRunWait(ctx, request)
	if err != nil {
		return fmt.Errorf("create run wait: %w", err)
	}
	if opened.ResolutionKind != "" {
		if request.Resume == nil {
			return errors.New("runtime resume support is required")
		}
		return request.Resume(ctx, WaitResumeDecision{
			Kind: opened.ResolutionKind,
			Data: opened.Resolution,
		})
	}
	if opened.RunWaitID == "" {
		return errors.New("run wait id is required")
	}
	var afterID int64
	reconnectDelay := 100 * time.Millisecond
	for {
		var handled error
		err = w.Client.FollowWorkerCommands(ctx, afterID, func(command api.WorkerCommand) error {
			if command.ID > afterID {
				afterID = command.ID
			}
			if command.RunWaitID != opened.RunWaitID {
				return nil
			}
			if err := validateRunWaitWorkerCommandFence(request.currentLease(), opened, command); err != nil {
				handled = err
				if errors.Is(err, errStaleWorkerCommand) {
					if _, ackErr := w.Client.AcknowledgeWorkerCommand(ctx, command.ID); ackErr != nil {
						handled = fmt.Errorf("acknowledge stale worker command: %w", ackErr)
						return errWorkerCommandHandled
					}
					handled = nil
					return nil
				}
				return errWorkerCommandHandled
			}
			switch command.Kind {
			case string(api.WorkerCommandKindRunResumeWait):
				if _, err := w.Client.AcceptWorkerCommand(ctx, command.ID); err != nil {
					handled = fmt.Errorf("accept worker command: %w", err)
					return errWorkerCommandHandled
				}
				handled = w.handleResumeDecision(ctx, request, command)
			case string(api.WorkerCommandKindRunCheckpointWait):
				if _, err := w.Client.AcceptWorkerCommand(ctx, command.ID); err != nil {
					handled = fmt.Errorf("accept worker command: %w", err)
					return errWorkerCommandHandled
				}
				handled = w.handleCheckpointDecision(ctx, request, opened, command)
				if errors.Is(handled, errStaleWorkerCommand) {
					if _, ackErr := w.Client.AcknowledgeWorkerCommand(ctx, command.ID); ackErr != nil {
						handled = fmt.Errorf("acknowledge stale worker command: %w", ackErr)
						return errWorkerCommandHandled
					}
					handled = nil
					return nil
				}
			default:
				return nil
			}
			if handled == nil || errors.Is(handled, ErrDetached) || errors.Is(handled, errCheckpointAttemptRecorded) {
				if _, ackErr := w.Client.AcknowledgeWorkerCommand(ctx, command.ID); ackErr != nil && !errors.Is(handled, ErrDetached) {
					if command.Kind != string(api.WorkerCommandKindRunResumeWait) {
						handled = fmt.Errorf("acknowledge worker command: %w", ackErr)
					}
				}
			}
			if errors.Is(handled, errCheckpointAttemptRecorded) {
				handled = nil
				return nil
			}
			return errWorkerCommandHandled
		})
		if errors.Is(err, errWorkerCommandHandled) {
			return handled
		}
		if ctxErr := ctx.Err(); ctxErr != nil {
			return ctxErr
		}
		if err != nil {
			return fmt.Errorf("follow worker commands: %w", err)
		}
		if err := sleepWithContext(ctx, reconnectDelay); err != nil {
			return err
		}
		if reconnectDelay < time.Second {
			reconnectDelay *= 2
		}
	}
}

func validateRunWaitWorkerCommandFence(lease api.WorkerRunLease, opened api.WorkerCreateRunWaitResponse, command api.WorkerCommand) error {
	if command.RunID != opened.RunID ||
		command.RunWaitID != opened.RunWaitID ||
		command.RunLeaseID != lease.ID ||
		command.WorkerInstanceID != lease.WorkerInstanceID ||
		command.RuntimeInstanceID != opened.RuntimeInstanceID ||
		command.RuntimeEpoch != opened.RuntimeEpoch {
		return errStaleWorkerCommand
	}
	return nil
}

type workerResumeDecisionPayload struct {
	ResumeKind    string          `json:"resume_kind"`
	ResumePayload json.RawMessage `json:"resume_payload"`
}

func (w ControlRunWaits) handleResumeDecision(ctx context.Context, request WaitRequest, command api.WorkerCommand) error {
	if request.Resume == nil {
		return errors.New("runtime resume support is required")
	}
	var payload workerResumeDecisionPayload
	if err := json.Unmarshal(command.Payload, &payload); err != nil {
		return fmt.Errorf("decode resume decision: %w", err)
	}
	if payload.ResumeKind == "" {
		return errors.New("resume decision kind is required")
	}
	if len(payload.ResumePayload) == 0 {
		payload.ResumePayload = json.RawMessage(`null`)
	}
	return request.Resume(ctx, WaitResumeDecision{
		Kind: payload.ResumeKind,
		Data: payload.ResumePayload,
	})
}

func (w ControlRunWaits) handleCheckpointDecision(ctx context.Context, request WaitRequest, opened api.WorkerCreateRunWaitResponse, command api.WorkerCommand) error {
	claim, err := w.Client.ClaimRunCheckpointWait(ctx, api.WorkerCheckpointClaimRequest{
		Lease:     request.currentLease(),
		RunWaitID: opened.RunWaitID,
	})
	if err != nil {
		return fmt.Errorf("claim run wait checkpoint: %w", err)
	}
	if claim.Status == "stale" {
		return errStaleWorkerCommand
	}
	if claim.Status != "" && claim.Status != "claimed" {
		return fmt.Errorf("unsupported checkpoint claim status %q", claim.Status)
	}
	if claim.CheckpointID == "" {
		return errors.New("checkpoint claim id is required")
	}
	failCheckpoint := func(err error) error {
		_, failErr := w.Client.MarkCheckpointFailed(ctx, api.WorkerCheckpointFailedRequest{
			Lease:           request.currentLease(),
			WorkerCommandID: command.ID,
			RunWaitID:       claim.RunWaitID,
			CheckpointID:    claim.CheckpointID,
			Error:           err.Error(),
		})
		if failErr != nil {
			return failErr
		}
		return errCheckpointAttemptRecorded
	}
	if request.Checkpointer == nil {
		err := errors.New("run checkpoint support is required")
		if failErr := failCheckpoint(err); failErr != nil {
			return fmt.Errorf("mark checkpoint failed after unsupported checkpoint: %w", failErr)
		}
		return nil
	}
	checkpoint, err := request.Checkpointer.CreateCheckpoint(ctx, CheckpointRequest{
		RunID:            request.currentLease().RunID,
		RunWaitID:        claim.RunWaitID,
		CheckpointID:     claim.CheckpointID,
		CaptureWorkspace: claim.CaptureWorkspace,
	})
	if err != nil {
		if failErr := failCheckpoint(err); failErr != nil {
			return fmt.Errorf("mark checkpoint failed after create checkpoint error: %w", failErr)
		}
		return errCheckpointAttemptRecorded
	}
	if claim.CaptureWorkspace {
		if checkpoint.WorkspaceCapture == nil {
			err := errors.New("workspace capture is required before parking")
			if failErr := failCheckpoint(err); failErr != nil {
				return fmt.Errorf("mark checkpoint failed after missing workspace capture: %w", failErr)
			}
			return errCheckpointAttemptRecorded
		}
		if _, err := w.Client.CaptureRunWaitWorkspace(ctx, api.WorkerRunWaitWorkspaceCaptureRequest{
			Lease:            request.currentLease(),
			RunWaitID:        claim.RunWaitID,
			CheckpointID:     claim.CheckpointID,
			WorkspaceCapture: *workerCheckpointWorkspaceCapture(checkpoint.WorkspaceCapture),
		}); err != nil {
			if failErr := failCheckpoint(err); failErr != nil {
				return fmt.Errorf("mark checkpoint failed after workspace capture error: %w", failErr)
			}
			return errCheckpointAttemptRecorded
		}
	}
	if _, err := w.Client.MarkCheckpointReady(ctx, api.WorkerCheckpointReadyRequest{
		Lease:            request.currentLease(),
		WorkerCommandID:  command.ID,
		RunWaitID:        claim.RunWaitID,
		CheckpointID:     claim.CheckpointID,
		ActiveDurationMs: durationMilliseconds(request.ActiveDuration),
		Manifest:         checkpoint.Manifest,
	}); err != nil {
		if failErr := failCheckpoint(err); failErr != nil {
			return fmt.Errorf("mark checkpoint failed after ready error: %w", failErr)
		}
		return errCheckpointAttemptRecorded
	}
	return ErrDetached
}

func workerCheckpointWorkspaceCapture(capture *workspace.WorkspaceArtifact) *api.WorkerWorkspaceArtifact {
	if capture == nil {
		return nil
	}
	return &api.WorkerWorkspaceArtifact{
		Digest:     capture.Digest,
		MediaType:  capture.MediaType,
		Encoding:   capture.Encoding,
		SizeBytes:  capture.SizeBytes,
		EntryCount: int32(capture.EntryCount),
	}
}

func (w ControlRunWaits) AddRunWait(ctx context.Context, request WaitRequest) (api.WorkerCreateRunWaitResponse, error) {
	if w.Client == nil {
		return api.WorkerCreateRunWaitResponse{}, errors.New("run wait control client is required")
	}
	return w.Client.CreateRunWait(ctx, api.WorkerCreateRunWaitRequest{
		Lease:              request.currentLease(),
		CorrelationID:      request.CorrelationID,
		Kind:               request.Kind,
		Params:             request.Params,
		Metadata:           request.Metadata,
		Tags:               request.Tags,
		TimeoutSeconds:     request.TimeoutSeconds,
		IdleTimeoutSeconds: request.IdleTimeoutSeconds,
	})
}

func (request WaitRequest) currentLease() api.WorkerRunLease {
	if request.Leases != nil {
		return request.Leases.CurrentWorkerRunLease()
	}
	return request.Lease
}

func sleepWithContext(ctx context.Context, duration time.Duration) error {
	timer := time.NewTimer(duration)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-timer.C:
		return nil
	}
}

func durationMilliseconds(value time.Duration) int64 {
	if value <= 0 {
		return 0
	}
	return value.Milliseconds()
}

var _ WaitHandler = ControlRunWaits{}
