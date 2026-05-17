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
	MarkCheckpointReady(context.Context, api.WorkerCheckpointReadyRequest) (api.WorkerCreateWaitpointResponse, error)
	MarkCheckpointFailed(context.Context, api.WorkerCheckpointFailedRequest) (api.WorkerCreateWaitpointResponse, error)
}

type ControlWaitpoints struct {
	Client WaitpointClient
}

func (w ControlWaitpoints) Wait(ctx context.Context, request WaitRequest) error {
	if w.Client == nil {
		return errors.New("waitpoint control client is required")
	}
	opened, err := w.Client.CreateWaitpoint(ctx, api.WorkerCreateWaitpointRequest{
		Claim:          request.Claim,
		CorrelationID:  request.CorrelationID,
		Kind:           request.Kind,
		Request:        request.Request,
		DisplayText:    request.DisplayText,
		TimeoutSeconds: request.TimeoutSeconds,
	})
	if err != nil {
		return fmt.Errorf("create waitpoint: %w", err)
	}
	if request.Checkpointer == nil {
		err := errors.New("runtime checkpoint support is required")
		_, _ = w.Client.MarkCheckpointFailed(ctx, api.WorkerCheckpointFailedRequest{
			Claim:        request.Claim,
			WaitpointID:  opened.WaitpointID,
			CheckpointID: opened.CheckpointID,
			Error:        err.Error(),
		})
		return err
	}
	manifest, err := request.Checkpointer.CreateCheckpoint(ctx, CheckpointRequest{
		RunID:        request.Claim.RunID,
		WaitpointID:  opened.WaitpointID,
		CheckpointID: opened.CheckpointID,
	})
	if err != nil {
		_, _ = w.Client.MarkCheckpointFailed(ctx, api.WorkerCheckpointFailedRequest{
			Claim:        request.Claim,
			WaitpointID:  opened.WaitpointID,
			CheckpointID: opened.CheckpointID,
			Error:        err.Error(),
		})
		return fmt.Errorf("create checkpoint: %w", err)
	}
	if _, err := w.Client.MarkCheckpointReady(ctx, api.WorkerCheckpointReadyRequest{
		Claim:            request.Claim,
		WaitpointID:      opened.WaitpointID,
		CheckpointID:     opened.CheckpointID,
		ActiveDurationMs: durationMilliseconds(request.ActiveDuration),
		Manifest:         manifest,
	}); err != nil {
		_, _ = w.Client.MarkCheckpointFailed(ctx, api.WorkerCheckpointFailedRequest{
			Claim:        request.Claim,
			WaitpointID:  opened.WaitpointID,
			CheckpointID: opened.CheckpointID,
			Error:        err.Error(),
		})
		return fmt.Errorf("mark checkpoint ready: %w", err)
	}
	return ErrDetached
}

func durationMilliseconds(value time.Duration) int64 {
	if value <= 0 {
		return 0
	}
	return value.Milliseconds()
}

var _ WaitHandler = ControlWaitpoints{}
