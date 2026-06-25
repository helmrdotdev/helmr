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
	CaptureRunWaitWorkspace(context.Context, api.WorkerRunWaitWorkspaceCaptureRequest) (api.WorkerRunWaitWorkspaceCaptureResponse, error)
	AcknowledgeRestore(context.Context, api.WorkerAcknowledgeRestoreRequest) (api.WorkerAcknowledgeRestoreResponse, error)
	MarkCheckpointReady(context.Context, api.WorkerCheckpointReadyRequest) (api.WorkerCreateRunWaitResponse, error)
	MarkCheckpointFailed(context.Context, api.WorkerCheckpointFailedRequest) (api.WorkerCreateRunWaitResponse, error)
}

type ControlRunWaits struct {
	Client RunWaitClient
}

type RestoreAcknowledgement struct {
	Lease        api.WorkerRunLease
	RunWaitID    string
	CheckpointID string
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
	failCheckpoint := func(err error) {
		_, _ = w.Client.MarkCheckpointFailed(ctx, api.WorkerCheckpointFailedRequest{
			Lease:        request.currentLease(),
			RunWaitID:    opened.RunWaitID,
			CheckpointID: opened.CheckpointID,
			Error:        err.Error(),
		})
	}
	if request.Checkpointer == nil {
		err := errors.New("runtime checkpoint support is required")
		failCheckpoint(err)
		return err
	}
	checkpoint, err := request.Checkpointer.CreateCheckpoint(ctx, CheckpointRequest{
		RunID:            request.currentLease().RunID,
		RunWaitID:        opened.RunWaitID,
		CheckpointID:     opened.CheckpointID,
		CaptureWorkspace: opened.CaptureWorkspace,
	})
	if err != nil {
		failCheckpoint(err)
		return fmt.Errorf("create checkpoint: %w", err)
	}
	if opened.CaptureWorkspace {
		if checkpoint.WorkspaceCapture == nil {
			err := errors.New("workspace capture is required before parking")
			failCheckpoint(err)
			return err
		}
		if _, err := w.Client.CaptureRunWaitWorkspace(ctx, api.WorkerRunWaitWorkspaceCaptureRequest{
			Lease:            request.currentLease(),
			RunWaitID:        opened.RunWaitID,
			CheckpointID:     opened.CheckpointID,
			WorkspaceCapture: *workerCheckpointWorkspaceCapture(checkpoint.WorkspaceCapture),
		}); err != nil {
			failCheckpoint(err)
			return fmt.Errorf("capture workspace before checkpoint ready: %w", err)
		}
	}
	if _, err := w.Client.MarkCheckpointReady(ctx, api.WorkerCheckpointReadyRequest{
		Lease:            request.currentLease(),
		RunWaitID:        opened.RunWaitID,
		CheckpointID:     opened.CheckpointID,
		ActiveDurationMs: durationMilliseconds(request.ActiveDuration),
		Manifest:         checkpoint.Manifest,
	}); err != nil {
		failCheckpoint(err)
		return fmt.Errorf("mark checkpoint ready: %w", err)
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

func durationMilliseconds(value time.Duration) int64 {
	if value <= 0 {
		return 0
	}
	return value.Milliseconds()
}

var _ WaitHandler = ControlRunWaits{}
