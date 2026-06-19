package executor

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/helmrdotdev/helmr/internal/api"
)

type WaitpointClient interface {
	CreateWaitpoint(context.Context, api.WorkerCreateWaitpointRequest) (api.WorkerCreateWaitpointResponse, error)
	AcknowledgeRestore(context.Context, api.WorkerAcknowledgeRestoreRequest) (api.WorkerAcknowledgeRestoreResponse, error)
	MarkCheckpointReady(context.Context, api.WorkerCheckpointReadyRequest) (api.WorkerCreateWaitpointResponse, error)
	MarkCheckpointFailed(context.Context, api.WorkerCheckpointFailedRequest) (api.WorkerCreateWaitpointResponse, error)
}

type ControlWaitpoints struct {
	Client WaitpointClient
}

type RestoreAcknowledgement struct {
	Lease           api.WorkerRunLease
	RunSuspensionID string
	WaitpointID     string
	CheckpointID    string
}

type RestoreAcknowledger interface {
	AcknowledgeRestore(context.Context, RestoreAcknowledgement) error
}

func (w ControlWaitpoints) AcknowledgeRestore(ctx context.Context, request RestoreAcknowledgement) error {
	if w.Client == nil {
		return errors.New("waitpoint control client is required")
	}
	_, err := w.Client.AcknowledgeRestore(ctx, api.WorkerAcknowledgeRestoreRequest{
		Lease:           request.Lease,
		RunSuspensionID: request.RunSuspensionID,
		WaitpointID:     request.WaitpointID,
		CheckpointID:    request.CheckpointID,
	})
	return err
}

func (w ControlWaitpoints) Wait(ctx context.Context, request WaitRequest) error {
	if w.Client == nil {
		return errors.New("waitpoint control client is required")
	}
	opened, err := w.AddWaitpoint(ctx, request)
	if err != nil {
		return fmt.Errorf("create waitpoint: %w", err)
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
	if request.Checkpointer == nil {
		err := errors.New("runtime checkpoint support is required")
		_, _ = w.Client.MarkCheckpointFailed(ctx, api.WorkerCheckpointFailedRequest{
			Lease:           request.Lease,
			RunSuspensionID: opened.RunSuspensionID,
			WaitpointID:     opened.WaitpointID,
			CheckpointID:    opened.CheckpointID,
			Error:           err.Error(),
		})
		return err
	}
	manifest, err := request.Checkpointer.CreateCheckpoint(ctx, CheckpointRequest{
		RunID:        request.Lease.RunID,
		WaitpointID:  opened.WaitpointID,
		CheckpointID: opened.CheckpointID,
	})
	if err != nil {
		_, _ = w.Client.MarkCheckpointFailed(ctx, api.WorkerCheckpointFailedRequest{
			Lease:           request.Lease,
			RunSuspensionID: opened.RunSuspensionID,
			WaitpointID:     opened.WaitpointID,
			CheckpointID:    opened.CheckpointID,
			Error:           err.Error(),
		})
		return fmt.Errorf("create checkpoint: %w", err)
	}
	if _, err := w.Client.MarkCheckpointReady(ctx, api.WorkerCheckpointReadyRequest{
		Lease:            request.Lease,
		RunSuspensionID:  opened.RunSuspensionID,
		WaitpointID:      opened.WaitpointID,
		CheckpointID:     opened.CheckpointID,
		ActiveDurationMs: durationMilliseconds(request.ActiveDuration),
		Manifest:         manifest,
	}); err != nil {
		_, _ = w.Client.MarkCheckpointFailed(ctx, api.WorkerCheckpointFailedRequest{
			Lease:           request.Lease,
			RunSuspensionID: opened.RunSuspensionID,
			WaitpointID:     opened.WaitpointID,
			CheckpointID:    opened.CheckpointID,
			Error:           err.Error(),
		})
		return fmt.Errorf("mark checkpoint ready: %w", err)
	}
	return ErrDetached
}

func (w ControlWaitpoints) AddWaitpoint(ctx context.Context, request WaitRequest) (api.WorkerCreateWaitpointResponse, error) {
	if w.Client == nil {
		return api.WorkerCreateWaitpointResponse{}, errors.New("waitpoint control client is required")
	}
	return w.Client.CreateWaitpoint(ctx, api.WorkerCreateWaitpointRequest{
		Lease:          request.Lease,
		CorrelationID:  request.CorrelationID,
		Kind:           request.Kind,
		Params:         request.Params,
		Metadata:       request.Metadata,
		Tags:           request.Tags,
		TimeoutSeconds: request.TimeoutSeconds,
		Ordinal:        request.Ordinal,
	})
}

func durationMilliseconds(value time.Duration) int64 {
	if value <= 0 {
		return 0
	}
	return value.Milliseconds()
}

var _ WaitHandler = ControlWaitpoints{}
