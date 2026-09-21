package executor

import (
	"context"
	"errors"
	"os"
	"time"

	"github.com/helmrdotdev/helmr/internal/capacity"
	"github.com/helmrdotdev/helmr/internal/cas"
	"github.com/helmrdotdev/helmr/internal/checkpoint"
	"github.com/helmrdotdev/helmr/internal/computer"
	"github.com/helmrdotdev/helmr/internal/vm"
	"github.com/helmrdotdev/helmr/internal/workerapi"
)

type terminalComputerCapturer struct {
	session   vm.ComputerCaptureSession
	objects   cas.ImmutableStore
	capacity  *capacity.Ledger
	encryptor *checkpoint.Encryptor
	tempDir   string
}

func (c terminalComputerCapturer) capture(ctx context.Context, lease workerapi.RunLeaseAssignment, operationID string, register func(context.Context, workerapi.RegisterRunFinalizationRequest) error) (result workerapi.CheckpointComputer, retErr error) {
	key := capacity.Key{Kind: "finalization-staging", ID: operationID, Epoch: lease.LeaseSequence}
	var reserved bool
	var directory string
	var candidate *computer.DiskCandidate
	defer func() {
		stopCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 30*time.Second)
		defer cancel()
		var stopErr error
		if releaser, ok := c.session.(CheckpointSourceReleaser); ok {
			stopErr = releaser.ReleaseCheckpointSource(stopCtx)
		} else if c.session != nil {
			stopErr = c.session.Close(stopCtx)
		}
		var cleanupErr error
		if candidate != nil {
			cleanupErr = candidate.Close()
		}
		if directory != "" {
			cleanupErr = errors.Join(cleanupErr, os.RemoveAll(directory))
		}
		if reserved && cleanupErr == nil {
			cleanupErr = c.capacity.Release(key)
		}
		if err := errors.Join(stopErr, cleanupErr); err != nil {
			retErr = errors.Join(retErr, &checkpointSourceReleaseError{err: err})
		}
	}()
	if c.session == nil || c.objects == nil || c.capacity == nil || c.encryptor == nil || register == nil {
		return result, errors.New("Computer capture dependencies are required")
	}
	shape, err := c.session.SnapshotLimits()
	if err != nil {
		return result, err
	}
	limit, err := (computer.DiskStore{Cipher: c.encryptor}).CaptureSizeLimit(shape.ComputerBytes)
	if err != nil {
		return result, err
	}
	reserved, err = c.capacity.Reserve(key, capacity.Vector{GuestEphemeralDiskBytes: limit})
	if err != nil {
		return result, err
	}
	if !reserved {
		return result, errors.New("Computer finalization staging is already owned")
	}
	if err := os.MkdirAll(c.tempDir, 0700); err != nil {
		return result, err
	}
	directory, err = os.MkdirTemp(c.tempDir, "finalization-")
	if err != nil {
		return result, err
	}
	disk, err := c.session.PauseComputer(ctx)
	if err != nil {
		return result, err
	}
	if disk == nil || disk.ComputerID != lease.WorkspaceID || disk.SizeBytes != shape.ComputerBytes {
		return result, errors.New("paused Computer differs from finalization authority")
	}
	candidate, err = (computer.DiskStore{Cipher: c.encryptor}).Capture(ctx, disk.ComputerID, disk.Path, directory)
	if err != nil {
		return result, err
	}
	artifact := candidate.Artifact()
	if err := artifact.Validate(shape.ComputerBytes); err != nil {
		return result, err
	}
	result = workerapi.CheckpointComputer{ComputerID: disk.ComputerID, LogicalBytes: artifact.LogicalBytes, Artifact: checkpointDescriptor(artifact.Object)}
	request := workerapi.RegisterRunFinalizationRequest{Lease: lease.Fence(), OperationID: operationID, Disk: result}
	if err := retryRunLeaseRequest(ctx, func(callCtx context.Context) error { return register(callCtx, request) }); err != nil {
		return result, err
	}
	if err := retryCheckpointUpload(ctx, func() error { return candidate.Upload(ctx, c.objects) }); err != nil {
		return result, err
	}
	return result, nil
}
