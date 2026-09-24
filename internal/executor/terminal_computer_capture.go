package executor

import (
	"context"
	"errors"
	"github.com/helmrdotdev/helmr/internal/computer"
	"github.com/helmrdotdev/helmr/internal/vm"
	"github.com/helmrdotdev/helmr/internal/workerapi"
	"time"
)

type terminalComputerCapturer struct {
	session     vm.ComputerCaptureSession
	publication func(workerapi.RunLeaseAssignment, string) computer.ContinuationPublication
}

func (c terminalComputerCapturer) capture(ctx context.Context, lease workerapi.RunLeaseAssignment, operationID string, register func(context.Context, workerapi.RegisterRunFinalizationRequest) error) (result workerapi.CheckpointComputer, retErr error) {
	defer func() {
		stopCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 30*time.Second)
		defer cancel()
		var err error
		if releaser, ok := c.session.(CheckpointSourceReleaser); ok {
			err = releaser.ReleaseCheckpointSource(stopCtx)
		} else if c.session != nil {
			err = c.session.Close(stopCtx)
		}
		if err != nil {
			retErr = errors.Join(retErr, &checkpointSourceReleaseError{err: err})
		}
	}()
	if c.session == nil || c.publication == nil || register == nil {
		return result, errors.New("Computer capture dependencies required")
	}
	shape, err := c.session.SnapshotLimits()
	if err != nil {
		return result, err
	}
	disk, err := c.session.PauseComputer(ctx)
	if err != nil {
		return result, err
	}
	if disk == nil || disk.ComputerID != lease.WorkspaceID || disk.Root.Validate(shape.ComputerBytes) != nil {
		return result, errors.New("paused Computer differs from finalization authority")
	}
	result = workerapi.CheckpointComputer{ComputerID: disk.ComputerID, LogicalBytes: disk.Root.LogicalBytes, Root: disk.Root}
	request := workerapi.RegisterRunFinalizationRequest{Lease: lease.Fence(), OperationID: operationID, Disk: result}
	if err = retryRunLeaseRequest(ctx, func(ctx context.Context) error { return register(ctx, request) }); err != nil {
		return result, err
	}
	if err = c.session.PublishComputer(ctx, disk.Root, c.publication(lease, operationID)); err != nil {
		return result, err
	}
	return result, nil
}
