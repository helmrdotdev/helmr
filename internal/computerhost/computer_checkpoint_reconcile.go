package computerhost

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/helmrdotdev/helmr/internal/disk"
	"github.com/helmrdotdev/helmr/internal/vm"
	"github.com/helmrdotdev/helmr/internal/workerapi"
)

type ComputerCheckpointClient interface {
	CheckpointComputerPublicationClient
	RegisterCheckpoint(context.Context, workerapi.RegisterCheckpointRequest) (workerapi.ComputerCheckpointResponse, error)
	MarkCheckpointReady(context.Context, workerapi.CheckpointReadyRequest) (workerapi.ComputerCheckpointResponse, error)
	MarkCheckpointFailed(context.Context, workerapi.CheckpointFailedRequest) (workerapi.ComputerCheckpointResponse, error)
}

func (p *PreparedMachines) captureRuntimeTarget(ctx context.Context, instances PreparedComputerInstanceClient, target workerapi.RuntimeReconcileTarget) error {
	if _, err := computerFreezeRequest(target); err != nil {
		return err
	}
	if p.ComputerCaptures == nil || p.Checkpoints == nil || p.CheckpointEncryptor == nil || p.ComputerObjects == nil || p.Reservations == nil || instances == nil {
		return errors.New("computer capture dependencies are required")
	}
	ref := preparedMachineRef{id: target.ID, epoch: target.WorkerEpoch}
	p.mu.Lock()
	claim := p.claims[ref]
	if claim != nil && (claim.checkpointer != nil || claim.kind == orphanClaim) {
		// An earlier capture began but could not prove source exclusion, or
		// the source has no holder left; physical cleanup ends it.
		p.mu.Unlock()
		return p.reclaimFailedRuntimeTarget(ctx, instances, target)
	}
	if claim != nil && (claim.teardown || claim.release != nil) {
		p.mu.Unlock()
		return errors.New("computer capture source is being closed by its owner")
	}
	claimedReady := false
	if claim == nil {
		for key, entries := range p.entries {
			for i, e := range entries {
				if e.computerInstanceID == ref.id && e.runtimeEpoch == ref.epoch {
					p.removeReadyEntryAtLocked(key, entries, i)
					claim = p.claimLocked(ref, captureClaim, e)
					claimedReady = true
					break
				}
			}
			if claim != nil {
				break
			}
		}
	}
	var entry preparedMachineEntry
	var mount *instanceMount
	if claim != nil {
		entry, mount = claim.entry, claim.mount
	}
	p.mu.Unlock()
	if claim == nil || entry.session == nil {
		return p.reclaimFailedRuntimeTarget(ctx, instances, target)
	}
	if claimedReady {
		// A capture that never began physical work returns the prepared
		// machine to the ready entries, where reconciliation still sees it.
		defer p.returnUnstartedCapture(ref, claim)
	}
	if entry.target.Source.ComputerID != target.Source.ComputerID || entry.target.Source.WriterGeneration != target.Source.WriterGeneration {
		return errors.New("computer capture source ownership changed")
	}
	session, ok := entry.session.(vm.CheckpointableMachine)
	if !ok {
		return errors.New("computer capture source cannot produce a checkpoint")
	}
	checkpointer := computerCheckpointer{session: session, mount: mount, reservations: p.Reservations, objects: p.ComputerObjects, encryptor: p.CheckpointEncryptor, tempDir: p.TempDir, computer: workerapi.CheckpointComputerBase{MountPath: "/workspace"}, publication: func(computerCheckpointRequest) disk.ContinuationPublication {
		return checkpointComputerPublisher{client: p.Checkpoints, objects: p.ComputerObjects, request: workerapi.CheckpointComputerObjectRequest{ComputerInstanceID: target.ID, WorkerEpoch: target.WorkerEpoch, DesiredVersion: target.DesiredVersion, CheckpointID: target.Capture.CheckpointID}}
	}}
	// Once members are paused, capture owns the source: a Server's claim is
	// taken over under a fresh generation, which makes the Server's handle
	// stale, and the checkpointer is retained until exclusion is proven. A
	// Server that has begun its own teardown keeps its claim.
	takeOver := func() error {
		p.mu.Lock()
		defer p.mu.Unlock()
		if p.claims[ref] != claim || claim.teardown || claim.release != nil {
			return errors.New("computer capture source ownership changed")
		}
		if claim.kind == serverClaim {
			p.claimGen++
			claim.gen = p.claimGen
			claim.kind = captureClaim
		}
		claim.checkpointer = &checkpointer
		return nil
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
	return p.ComputerCaptures.capture(ctx, target, func(captureCtx context.Context) error {
		if err := takeOver(); err != nil {
			return fail(captureCtx, err)
		}
		result, err := checkpointer.CreateCheckpoint(captureCtx, computerCheckpointRequest{Target: target, Register: func(ctx context.Context, manifest workerapi.CheckpointManifest) error {
			return retryControlRequest(ctx, func(ctx context.Context) error {
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
		err = retryControlRequest(captureCtx, func(ctx context.Context) error {
			receipt, err := p.Checkpoints.MarkCheckpointReady(ctx, workerapi.CheckpointReadyRequest{ComputerInstanceID: target.ID, WorkerEpoch: target.WorkerEpoch, DesiredVersion: target.DesiredVersion, CheckpointID: target.Capture.CheckpointID, Manifest: result.Manifest})
			if err != nil {
				return err
			}
			if err = validateComputerCheckpointReceipt(target, receipt); err != nil {
				return err
			}
			if receipt.ComputerDiskVersionID == "" {
				return fmt.Errorf("%w: checkpoint ready receipt omitted saved Computer version", errForeignSourceReceipt)
			}
			return nil
		})
		if err != nil {
			return fail(captureCtx, err)
		}
		knownClose = true
		return nil
	}, func(excludeCtx context.Context) error {
		takeOverErr := takeOver()
		if !knownClose && !failureAttempted {
			_ = fail(excludeCtx, errors.New("computer capture failed before snapshot publication"))
		}
		if takeOverErr != nil {
			return takeOverErr
		}
		proofMethod, err := p.excludeCaptureSource(excludeCtx, target.ID, &checkpointer)
		if err != nil {
			return err
		}
		if err := p.releaseRuntimeAfterPhysicalCleanup(excludeCtx, target.ID, target.WorkerEpoch); err != nil {
			return err
		}
		if !knownClose {
			return p.reportRuntimeTargetFailedWithProof(excludeCtx, instances, target, errors.New("checkpoint source excluded without a committed receipt"), proofMethod)
		}
		closed := target
		closed.DesiredVersion++
		closed.Action = workerapi.RuntimeReconcileClose
		request := runtimeTargetStatusRequest(closed, nil)
		request.CleanupProof = &workerapi.RuntimeCleanupProof{Method: proofMethod, CompletedAt: time.Now().UTC()}
		_, err = instances.MarkComputerInstanceClosed(excludeCtx, request)
		return err
	})
}

// excludeCaptureSource stops a capture-owned source within the source release
// bound and returns the cleanup proof method. A served source whose save owner
// has not finished by then may be waiting on the VM itself, so the release
// escalates to physical cleanup of the runtime; finalization then joins the
// save owner first (see releaseRuntimeAfterPhysicalCleanup).
func (p *PreparedMachines) excludeCaptureSource(ctx context.Context, computerInstanceID string, capture *computerCheckpointer) (string, error) {
	releaseCtx, cancel := p.sourceReleaseContext(ctx)
	err := capture.ReleaseCheckpointSource(releaseCtx)
	cancel()
	if err == nil {
		return workerapi.RuntimeCleanupSessionClosed, nil
	}
	if capture.mount == nil || capture.mount.saves.joined() {
		return "", err
	}
	if p.Backend == nil {
		return "", errors.Join(err, errors.New("runtime connector does not support exact runtime cleanup"))
	}
	cleanupCtx, cancel := preparedMachineControlContext(ctx)
	cleanupErr := p.Backend.Cleanup(cleanupCtx, vm.Owner{Kind: vm.OwnerRuntime, ID: computerInstanceID})
	cancel()
	if cleanupErr != nil {
		return "", fmt.Errorf("stop capture source physically: %w", errors.Join(err, cleanupErr))
	}
	return workerapi.RuntimeCleanupHostReconciled, nil
}

// sourceReleaseContext bounds one step of releasing a source: a release
// attempt, or joining its save owner after physical cleanup. It is detached
// from the caller's cancellation so worker shutdown or capture's own cleanup
// deadline cannot abandon a step midway; the bound keeps any caller from
// waiting indefinitely. An escalated exclusion takes at most the release
// bound, the physical cleanup bound and the join bound (about 45s with the
// defaults). That exceeds capture's 30s cleanup context, so its closure report
// can then fail; the claim has been finalized by then, and the next Close
// target reports the closure from unclaimed reconciliation. Worker shutdown is
// delayed by at most the same bound.
func (p *PreparedMachines) sourceReleaseContext(parent context.Context) (context.Context, context.CancelFunc) {
	timeout := p.sourceReleaseTimeout
	if timeout <= 0 {
		timeout = defaultPreparedMachineControlTimeout
	}
	return context.WithTimeout(context.WithoutCancel(parent), timeout)
}

// returnUnstartedCapture hands a ready machine that capture claimed back to the
// ready entries when capture ended before taking over its source. A machine
// that has exited meanwhile, or whose prepared machines have closed, is cleaned
// up and reported failed instead.
func (p *PreparedMachines) returnUnstartedCapture(ref preparedMachineRef, claim *machineClaim) {
	p.mu.Lock()
	if p.claims[ref] != claim || claim.checkpointer != nil || claim.release != nil {
		p.mu.Unlock()
		return
	}
	delete(p.claims, ref)
	entry := claim.entry
	var cause error
	if exitErr, exited := entry.exit.finished(); exited {
		cause = preparedMachineExitCause(exitErr)
	} else if p.closed {
		cause = errors.New("runtime controller stopped")
	}
	if cause == nil {
		key := entry.machineKey
		if strings.TrimSpace(key) == "" {
			key = entry.computerInstanceID
		}
		if p.entries == nil {
			p.entries = map[string][]preparedMachineEntry{}
		}
		p.entries[key] = append(p.entries[key], entry)
		p.mu.Unlock()
		return
	}
	p.mu.Unlock()
	p.cleanupClaimedEntryAsync(entry, cause)
}

func validateComputerCheckpointReceipt(target workerapi.RuntimeReconcileTarget, response workerapi.ComputerCheckpointResponse) error {
	if response.ComputerInstanceID != target.ID || response.WorkerEpoch != target.WorkerEpoch || response.DesiredVersion != target.DesiredVersion || response.CheckpointID != target.Capture.CheckpointID {
		return fmt.Errorf("%w: checkpoint receipt does not match source operation", errForeignSourceReceipt)
	}
	return nil
}
