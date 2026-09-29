package executor

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/helmrdotdev/helmr/internal/computer"
	"github.com/helmrdotdev/helmr/internal/vm"
	"github.com/helmrdotdev/helmr/internal/workerapi"
)

type ComputerCheckpointClient interface {
	CheckpointComputerPublicationClient
	RegisterCheckpoint(context.Context, workerapi.RegisterCheckpointRequest) (workerapi.ComputerCheckpointResponse, error)
	MarkCheckpointReady(context.Context, workerapi.CheckpointReadyRequest) (workerapi.ComputerCheckpointResponse, error)
	MarkCheckpointFailed(context.Context, workerapi.CheckpointFailedRequest) (workerapi.ComputerCheckpointResponse, error)
}

func (p *PreparedRuntimePool) captureRuntimeTarget(ctx context.Context, instances PreparedComputerInstanceClient, target workerapi.RuntimeReconcileTarget) error {
	if _, err := computerFreezeRequest(target); err != nil {
		return err
	}
	if p.ComputerCaptures == nil || p.Checkpoints == nil || p.CheckpointEncryptor == nil || p.ComputerObjects == nil || p.Capacity == nil || instances == nil {
		return errors.New("Computer capture dependencies are required")
	}
	ref := preparedRuntimeRef{id: target.ID, epoch: target.WorkerEpoch}
	p.mu.Lock()
	priorCleanup := p.captureCleanup[ref]
	p.mu.Unlock()
	if priorCleanup != nil {
		return p.ReclaimFailedRuntimeTarget(ctx, instances, target)
	}
	p.mu.Lock()
	entry, ok := p.checkedOutEntries[ref]
	if !ok {
		for key, entries := range p.entries {
			for i, e := range entries {
				if e.computerInstanceID == ref.id && e.runtimeEpoch == ref.epoch {
					entry, ok = e, true
					p.removeReadyEntryAtLocked(key, entries, i)
					p.markRuntimeCheckedOutLocked(ref.id, ref.epoch)
					if p.checkedOutEntries == nil {
						p.checkedOutEntries = map[preparedRuntimeRef]preparedRuntimeEntry{}
					}
					p.checkedOutEntries[ref] = entry
					break
				}
			}
			if ok {
				break
			}
		}
	}
	p.mu.Unlock()
	if !ok || entry.session == nil {
		return p.ReclaimFailedRuntimeTarget(ctx, instances, target)
	}
	if entry.target.Source.ComputerID != target.Source.ComputerID || entry.target.Source.WriterGeneration != target.Source.WriterGeneration {
		return errors.New("Computer capture source ownership changed")
	}
	session, ok := entry.session.(vm.CheckpointableSession)
	if !ok {
		return errors.New("Computer capture source cannot produce a checkpoint")
	}
	checkpointer := computerCheckpointer{session: session, capacity: p.Capacity, objects: p.ComputerObjects, encryptor: p.CheckpointEncryptor, tempDir: p.TempDir, computer: workerapi.CheckpointComputerBase{MountPath: "/workspace"}, publication: func(ComputerCheckpointRequest) computer.ContinuationPublication {
		return checkpointComputerPublisher{client: p.Checkpoints, objects: p.ComputerObjects, request: workerapi.CheckpointComputerObjectRequest{ComputerInstanceID: target.ID, WorkerEpoch: target.WorkerEpoch, DesiredVersion: target.DesiredVersion, CheckpointID: target.Capture.CheckpointID}}
	}}
	retainCleanup := func() {
		p.mu.Lock()
		defer p.mu.Unlock()
		if p.captureCleanup == nil {
			p.captureCleanup = map[preparedRuntimeRef]*computerCheckpointer{}
		}
		p.captureCleanup[ref] = &checkpointer
	}

	knownClose := false
	failureAttempted := false
	fail := func(parent context.Context, cause error) error {
		failureAttempted = true
		control, cancel := context.WithTimeout(context.WithoutCancel(parent), 15*time.Second)
		defer cancel()
		message := strings.TrimSpace(cause.Error())
		if len(message) > 1024 {
			message = message[:1024]
		}
		response, err := p.Checkpoints.MarkCheckpointFailed(control, workerapi.CheckpointFailedRequest{ComputerInstanceID: target.ID, WorkerEpoch: target.WorkerEpoch, DesiredVersion: target.DesiredVersion, CheckpointID: target.Capture.CheckpointID, Error: message})
		if err == nil {
			err = validateComputerCheckpointReceipt(target, response)
		}
		if err == nil {
			knownClose = true
		}
		return errors.Join(cause, err)
	}
	return p.ComputerCaptures.Capture(ctx, target, func(captureCtx context.Context) error {
		retainCleanup()
		result, err := checkpointer.CreateCheckpoint(captureCtx, ComputerCheckpointRequest{Target: target, Register: func(ctx context.Context, manifest workerapi.CheckpointManifest) error {
			return retryRunLeaseRequest(ctx, func(ctx context.Context) error {
				receipt, err := p.Checkpoints.RegisterCheckpoint(ctx, workerapi.RegisterCheckpointRequest{ComputerInstanceID: target.ID, WorkerEpoch: target.WorkerEpoch, DesiredVersion: target.DesiredVersion, CheckpointID: target.Capture.CheckpointID, Manifest: manifest})
				if err != nil {
					return err
				}
				return validateComputerCheckpointReceipt(target, receipt)
			})
		}})
		if err != nil {
			return fail(captureCtx, err)
		}
		err = retryRunLeaseRequest(captureCtx, func(ctx context.Context) error {
			receipt, err := p.Checkpoints.MarkCheckpointReady(ctx, workerapi.CheckpointReadyRequest{ComputerInstanceID: target.ID, WorkerEpoch: target.WorkerEpoch, DesiredVersion: target.DesiredVersion, CheckpointID: target.Capture.CheckpointID, Manifest: result.Manifest})
			if err != nil {
				return err
			}
			if err = validateComputerCheckpointReceipt(target, receipt); err != nil {
				return err
			}
			if receipt.ComputerDiskVersionID == "" {
				return fmt.Errorf("%w: checkpoint ready receipt omitted saved Computer version", errRunSourceOperationUnavailable)
			}
			return nil
		})
		if err != nil {
			return fail(captureCtx, err)
		}
		knownClose = true
		return nil
	}, func(excludeCtx context.Context) error {
		retainCleanup()
		if !knownClose && !failureAttempted {
			_ = fail(excludeCtx, errors.New("Computer capture failed before snapshot publication"))
		}
		if err := checkpointer.ReleaseCheckpointSource(excludeCtx); err != nil {
			return err
		}
		if err := p.releaseRuntimeAfterPhysicalCleanup(target.ID, target.WorkerEpoch); err != nil {
			return err
		}
		if !knownClose {
			return p.reportRuntimeTargetFailedWithProof(excludeCtx, instances, target, errors.New("checkpoint source excluded without a committed receipt"), workerapi.RuntimeCleanupSessionClosed)
		}
		closed := target
		closed.DesiredVersion++
		closed.Action = workerapi.RuntimeReconcileClose
		request := runtimeTargetStatusRequest(closed, nil)
		request.CleanupProof = &workerapi.RuntimeCleanupProof{Method: workerapi.RuntimeCleanupSessionClosed, CompletedAt: time.Now().UTC()}
		_, err := instances.MarkComputerInstanceClosed(excludeCtx, request)
		return err
	})
}

func validateComputerCheckpointReceipt(target workerapi.RuntimeReconcileTarget, response workerapi.ComputerCheckpointResponse) error {
	if response.ComputerInstanceID != target.ID || response.WorkerEpoch != target.WorkerEpoch || response.DesiredVersion != target.DesiredVersion || response.CheckpointID != target.Capture.CheckpointID {
		return fmt.Errorf("%w: checkpoint receipt does not match source operation", errRunSourceOperationUnavailable)
	}
	return nil
}
