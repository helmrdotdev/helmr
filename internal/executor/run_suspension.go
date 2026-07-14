package executor

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/helmrdotdev/helmr/internal/api"
	"github.com/helmrdotdev/helmr/internal/workspace"
)

type RunWaitClient interface {
	CreateRunWait(context.Context, api.WorkerCreateRunWaitRequest) (api.WorkerCreateRunWaitResponse, error)
	PollRunWait(context.Context, api.WorkerRunWaitPollRequest) (api.WorkerRunWaitPollResponse, error)
	AcknowledgeRunWaitResume(context.Context, api.WorkerRunWaitResumeAckRequest) (api.WorkerRunWaitResumeAckResponse, error)
	CaptureRunWaitWorkspace(context.Context, api.WorkerRunWaitWorkspaceCaptureRequest) (api.WorkerRunWaitWorkspaceCaptureResponse, error)
	AcknowledgeRestore(context.Context, api.WorkerAcknowledgeRestoreRequest) (api.WorkerAcknowledgeRestoreResponse, error)
	MarkCheckpointReady(context.Context, api.WorkerCheckpointReadyRequest) (api.WorkerCheckpointResponse, error)
	MarkCheckpointFailed(context.Context, api.WorkerCheckpointFailedRequest) (api.WorkerCheckpointResponse, error)
}

type ControlRunWaits struct {
	Client RunWaitClient
}

var errCheckpointAttemptRecorded = errors.New("checkpoint attempt failure recorded")

type RestoreAcknowledgement struct {
	Lease                api.WorkerRunLease
	RunWaitID            string
	CheckpointID         string
	ResumeRequestVersion int64
	Phases               []api.WorkerCheckpointPhase
}

type RestoreAcknowledger interface {
	AcknowledgeRestore(context.Context, RestoreAcknowledgement) error
}

func (w ControlRunWaits) AcknowledgeRestore(ctx context.Context, request RestoreAcknowledgement) error {
	if w.Client == nil {
		return errors.New("run wait control client is required")
	}
	_, err := w.Client.AcknowledgeRestore(ctx, api.WorkerAcknowledgeRestoreRequest{
		Lease:                request.Lease,
		RunWaitID:            request.RunWaitID,
		CheckpointID:         request.CheckpointID,
		ResumeRequestVersion: request.ResumeRequestVersion,
		Phases:               request.Phases,
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
	pollDelay := 100 * time.Millisecond
	for {
		intent, pollErr := w.Client.PollRunWait(ctx, api.WorkerRunWaitPollRequest{
			Lease:     request.currentLease(),
			RunWaitID: opened.RunWaitID,
		})
		if ctxErr := ctx.Err(); ctxErr != nil {
			return ctxErr
		}
		if pollErr != nil {
			return fmt.Errorf("poll run wait: %w", pollErr)
		}
		if intent.RunID != opened.RunID || intent.RunWaitID != opened.RunWaitID {
			return errors.New("run wait poll returned a mismatched fence")
		}
		switch intent.Status {
		case api.WorkerRunWaitPollStatusWaiting:
		case api.WorkerRunWaitPollStatusResumeRequested:
			if intent.RequestVersion <= 0 || intent.ResumeKind == "" {
				return errors.New("run wait resume request version and kind are required")
			}
			if request.Resume == nil {
				return errors.New("runtime resume support is required")
			}
			payload := intent.ResumePayload
			if len(payload) == 0 {
				payload = []byte("null")
			}
			if err := request.Resume(ctx, WaitResumeDecision{Kind: intent.ResumeKind, Data: payload}); err != nil {
				return err
			}
			if _, err := w.Client.AcknowledgeRunWaitResume(ctx, api.WorkerRunWaitResumeAckRequest{
				Lease:                request.currentLease(),
				RunWaitID:            opened.RunWaitID,
				ResumeRequestVersion: intent.RequestVersion,
			}); err != nil {
				return fmt.Errorf("acknowledge run wait resume: %w", err)
			}
			return nil
		case api.WorkerRunWaitPollStatusCheckpointRequested:
			handled := w.handleCheckpointDecision(ctx, request, intent)
			if errors.Is(handled, errCheckpointAttemptRecorded) {
				return nil
			}
			return handled
		case api.WorkerRunWaitPollStatusTerminal:
			return errors.New("run wait became terminal before resume")
		default:
			return fmt.Errorf("unsupported run wait poll status %q", intent.Status)
		}
		if err := sleepWithContext(ctx, pollDelay); err != nil {
			return err
		}
		if pollDelay < time.Second {
			pollDelay *= 2
		}
	}
}

func (w ControlRunWaits) handleCheckpointDecision(ctx context.Context, request WaitRequest, intent api.WorkerRunWaitPollResponse) error {
	if intent.CheckpointID == "" || intent.RequestVersion <= 0 {
		return errors.New("checkpoint request id and version are required")
	}
	failCheckpoint := func(err error) error {
		_, failErr := w.Client.MarkCheckpointFailed(ctx, api.WorkerCheckpointFailedRequest{
			Lease: request.currentLease(), RequestVersion: intent.RequestVersion,
			RunWaitID: intent.RunWaitID, CheckpointID: intent.CheckpointID, Error: err.Error(),
			ActiveDurationMs: durationMilliseconds(request.ActiveDuration),
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
		RunWaitID:        intent.RunWaitID,
		CheckpointID:     intent.CheckpointID,
		CaptureWorkspace: intent.CaptureWorkspace,
	})
	if err != nil {
		if failErr := failCheckpoint(err); failErr != nil {
			return fmt.Errorf("mark checkpoint failed after create checkpoint error: %w", failErr)
		}
		return errCheckpointAttemptRecorded
	}
	if intent.CaptureWorkspace {
		if checkpoint.WorkspaceCapture == nil {
			err := errors.New("workspace capture is required before parking")
			if failErr := failCheckpoint(err); failErr != nil {
				return fmt.Errorf("mark checkpoint failed after missing workspace capture: %w", failErr)
			}
			return errCheckpointAttemptRecorded
		}
		capture, err := w.Client.CaptureRunWaitWorkspace(ctx, api.WorkerRunWaitWorkspaceCaptureRequest{
			Lease: request.currentLease(), RequestVersion: intent.RequestVersion,
			RunWaitID: intent.RunWaitID, CheckpointID: intent.CheckpointID, Workspace: request.Workspace,
			WorkspaceCapture: *workerCheckpointWorkspaceCapture(checkpoint.WorkspaceCapture),
		})
		if err != nil {
			if failErr := failCheckpoint(err); failErr != nil {
				return fmt.Errorf("mark checkpoint failed after workspace capture error: %w", failErr)
			}
			return errCheckpointAttemptRecorded
		}
		request.Workspace.BaseVersionID = capture.WorkspaceVersionID
	}
	if _, err := w.Client.MarkCheckpointReady(ctx, api.WorkerCheckpointReadyRequest{
		Lease: request.currentLease(), RequestVersion: intent.RequestVersion,
		RunWaitID: intent.RunWaitID, CheckpointID: intent.CheckpointID,
		Workspace: request.Workspace, WorkspaceVersionID: request.Workspace.BaseVersionID,
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
