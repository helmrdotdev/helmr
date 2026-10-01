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
	AbortCapture(context.Context, workerapi.CaptureAbortRequest) (workerapi.CaptureAbortResponse, error)
	CompleteCaptureAbort(context.Context, workerapi.CaptureAbortCompleteRequest) (workerapi.ComputerCheckpointResponse, error)
	ListInstanceReconcileTargets(context.Context) (workerapi.InstanceReconcileResponse, error)
	CheckpointComputerPublicationClient
	RegisterCheckpoint(context.Context, workerapi.RegisterCheckpointRequest) (workerapi.ComputerCheckpointResponse, error)
	MarkCheckpointReady(context.Context, workerapi.CheckpointReadyRequest) (workerapi.ComputerCheckpointResponse, error)
}

func (p *PreparedMachines) captureInstanceTarget(ctx context.Context, instances PreparedComputerInstanceClient, target workerapi.InstanceReconcileTarget) error {
	if _, err := computerFreezeRequest(target); err != nil {
		return err
	}
	if p.ComputerCaptures == nil || p.Checkpoints == nil || p.CheckpointEncryptor == nil || p.ComputerObjects == nil || p.Reservations == nil || instances == nil {
		return errors.New("computer capture dependencies are required")
	}
	ref := preparedMachineRef{id: target.ID, epoch: target.WorkerEpoch}
	p.mu.Lock()
	claim := p.claims[ref]
	if claim != nil && claim.checkpointer != nil {
		p.mu.Unlock()
		return errors.New("computer capture source is retained by its current owner")
	}
	if claim != nil && claim.kind == orphanClaim {
		p.mu.Unlock()
		return p.reclaimFailedInstanceTarget(ctx, instances, target)
	}
	if claim != nil && (claim.teardown || claim.release != nil) {
		p.mu.Unlock()
		return errors.New("computer capture source is being closed by its owner")
	}
	claimedReady := false
	if claim == nil {
		for key, entries := range p.entries {
			for i, e := range entries {
				if e.computerInstanceID == ref.id && e.workerEpoch == ref.epoch {
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
	if claim == nil || entry.machine == nil {
		return p.reclaimFailedInstanceTarget(ctx, instances, target)
	}
	if claimedReady {
		// A capture that never began physical work returns the prepared
		// machine to the ready entries, where reconciliation still sees it.
		defer p.returnUnstartedCapture(ref, claim)
	}
	if entry.target.Source.ComputerID != target.Source.ComputerID || entry.target.Source.WriterGeneration != target.Source.WriterGeneration {
		return errors.New("computer capture source ownership changed")
	}
	machine := entry.machine
	checkpointer := computerCheckpointer{machine: machine, mount: mount, reservations: p.Reservations, objects: p.ComputerObjects, encryptor: p.CheckpointEncryptor, tempDir: p.TempDir, computer: workerapi.CheckpointComputerBase{MountPath: "/workspace"}, publication: func(computerCheckpointRequest) disk.ContinuationPublication {
		return checkpointComputerPublisher{client: p.Checkpoints, objects: p.ComputerObjects, request: workerapi.CheckpointComputerObjectRequest{ComputerInstanceID: target.ID, WorkerEpoch: target.WorkerEpoch, DesiredVersion: target.DesiredVersion, CheckpointID: target.Capture.CheckpointID}}
	}}
	// The original Server keeps its checkout generation during a reversible
	// capture. The hold blocks its teardown; only adopted capture or confirmed
	// source loss takes ownership away from it.
	hold := func() error {
		p.mu.Lock()
		defer p.mu.Unlock()
		if p.claims[ref] != claim || claim.teardown || claim.release != nil {
			return errors.New("computer capture source ownership changed")
		}
		claim.checkpointer = &checkpointer
		return nil
	}
	if err := hold(); err != nil {
		return err
	}

	knownClose := false
	finalTarget := target
	var progress captureAbortProgress
	sourceFailure := errors.New("checkpoint source stopped during worker shutdown or source exit")
	settle := func(excludeCtx context.Context) error {
		if !knownClose {
			for {
				p.mu.Lock()
				shuttingDown := p.closed
				ownerAvailable := p.claims[ref] == claim && !claim.ownerExited && !claim.teardown && claim.release == nil
				p.mu.Unlock()
				_, exited := entry.exit.finished()
				if ctx.Err() != nil || shuttingDown || exited || !ownerAvailable {
					break
				}
				control, cancel := context.WithTimeout(excludeCtx, 15*time.Second)
				adopted, err := p.resumeCaptureStep(control, target, &checkpointer, &progress)
				cancel()
				if err == nil {
					if adopted {
						knownClose = true
						finalTarget.DesiredVersion = target.DesiredVersion + 1
						break
					}
					p.mu.Lock()
					if p.closed || p.claims[ref] != claim || claim.ownerExited || claim.teardown || claim.release != nil {
						p.mu.Unlock()
						break
					}
					claim.checkpointer = nil
					claim.entry.target.DesiredVersion = target.DesiredVersion + 1
					p.mu.Unlock()
					p.ComputerCaptures.markCaptureResumed(target, progress.receipt.Members)
					return nil
				}
				if errors.Is(err, errCaptureAbortCleanupFailed) {
					sourceFailure = err
					break
				}
				// An uncertain reply retains every owner and reservation. Only a current
				// close/reclaim intent for this exact Instance permits physical exclusion.
				control, cancel = context.WithTimeout(excludeCtx, 15*time.Second)
				targets, readErr := p.Checkpoints.ListInstanceReconcileTargets(control)
				cancel()
				if readErr == nil {
					for _, current := range targets.Items {
						if current.ID == target.ID && current.WorkerEpoch == target.WorkerEpoch && current.Source.ComputerID == target.Source.ComputerID && current.Source.WriterGeneration == target.Source.WriterGeneration && current.DesiredVersion >= target.DesiredVersion && (current.Action == workerapi.InstanceReconcileClose || current.Action == workerapi.InstanceReconcileReclaim) {
							finalTarget = current
							knownClose = true
							break
						}
					}
				}
				if knownClose {
					break
				}
				if err := sleepWithContext(ctx, time.Second); err != nil {
					break
				}
			}
		} else {
			finalTarget.DesiredVersion = target.DesiredVersion + 1
		}
		p.mu.Lock()
		if p.claims[ref] != claim || claim.release != nil {
			p.mu.Unlock()
			return errors.New("capture source cleanup owner changed")
		}
		if claim.kind == serverClaim {
			p.claimGen++
			claim.gen = p.claimGen
			claim.kind = captureClaim
		}
		p.mu.Unlock()
		proofMethod, err := p.excludeCaptureSource(excludeCtx, target.ID, &checkpointer)
		if err != nil {
			return err
		}
		if err = p.releaseInstanceAfterPhysicalCleanup(excludeCtx, target.ID, target.WorkerEpoch); err != nil {
			return err
		}
		if !knownClose {
			if progress.receipt.AbortDesiredVersion > 0 {
				finalTarget.DesiredVersion = progress.receipt.AbortDesiredVersion
			}
			return p.reportInstanceTargetFailedWithProof(excludeCtx, instances, finalTarget, sourceFailure, proofMethod)
		}
		if finalTarget.Action == workerapi.InstanceReconcileReclaim {
			return p.reportInstanceTargetFailedWithProof(excludeCtx, instances, finalTarget, errors.New("checkpoint source reclaimed by Control Plane"), proofMethod)
		}
		finalTarget.Action = workerapi.InstanceReconcileClose
		request := instanceTargetStatusRequest(finalTarget, nil)
		request.CleanupProof = &workerapi.InstanceCleanupProof{Method: proofMethod, CompletedAt: time.Now().UTC()}
		_, err = instances.MarkComputerInstanceClosed(excludeCtx, request)
		return err
	}
	settled := false
	defer func() {
		// Validation may fail before any member pause is dispatched. Retain the
		// physical hold for the next attempt instead of closing a live source.
		if !settled {
			p.mu.Lock()
			if p.claims[ref] == claim {
				claim.checkpointer = nil
			}
			p.mu.Unlock()
		}
	}()
	return p.ComputerCaptures.capture(ctx, target, func(captureCtx context.Context) error {
		p.mu.Lock()
		ownerAvailable := !p.closed && p.claims[ref] == claim && !claim.ownerExited && !claim.teardown && claim.release == nil
		p.mu.Unlock()
		if !ownerAvailable {
			return errors.New("capture source holder exited before snapshot")
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
			return err
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
			return err
		}
		knownClose = true
		return nil
	}, func(excludeCtx context.Context) error {
		settled = true
		return settle(excludeCtx)

	})
}

// excludeCaptureSource stops a capture-owned source within the source release
// bound and returns the cleanup proof method. A served source whose save owner
// has not finished by then may be waiting on the VM itself, so the release
// escalates to physical cleanup of the instance; finalization then joins the
// save owner first (see releaseInstanceAfterPhysicalCleanup).
func (p *PreparedMachines) excludeCaptureSource(ctx context.Context, computerInstanceID string, capture *computerCheckpointer) (string, error) {
	releaseCtx, cancel := p.sourceReleaseContext(ctx)
	err := capture.ReleaseCheckpointSource(releaseCtx)
	cancel()
	if err == nil {
		return workerapi.InstanceCleanupMachineClosed, nil
	}
	if capture.mount == nil || capture.mount.saves.joined() {
		return "", err
	}
	if p.Backend == nil {
		return "", errors.Join(err, errors.New("VM backend does not support exact instance cleanup"))
	}
	cleanupCtx, cancel := preparedMachineControlContext(ctx)
	cleanupErr := p.Backend.Cleanup(cleanupCtx, vm.Owner{Kind: vm.OwnerInstance, ID: computerInstanceID})
	cancel()
	if cleanupErr != nil {
		return "", fmt.Errorf("stop capture source physically: %w", errors.Join(err, cleanupErr))
	}
	return workerapi.InstanceCleanupHostReconciled, nil
}

// sourceReleaseContext bounds one source release or owner join step. Detaching
// caller cancellation lets physical cleanup finish during worker shutdown.
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
		cause = errors.New("instance controller stopped")
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

func validateComputerCheckpointReceipt(target workerapi.InstanceReconcileTarget, response workerapi.ComputerCheckpointResponse) error {
	if response.ComputerInstanceID != target.ID || response.WorkerEpoch != target.WorkerEpoch || response.DesiredVersion != target.DesiredVersion || response.CheckpointID != target.Capture.CheckpointID {
		return fmt.Errorf("%w: checkpoint receipt does not match source operation", errForeignSourceReceipt)
	}
	return nil
}

// A resuming target can outlive a lost acknowledgment or a Worker restart. An
// existing local owner keeps the source; a restart without that owner is actual
// source loss, reported before the normal reclaim path performs exclusion.
func (p *PreparedMachines) reconcileCaptureAbortTarget(ctx context.Context, client PreparedComputerInstanceClient, target workerapi.InstanceReconcileTarget) error {
	ref := preparedMachineRef{id: target.ID, epoch: target.WorkerEpoch}
	p.mu.Lock()
	if p.claims[ref] != nil {
		p.mu.Unlock()
		return nil
	}
	for _, entries := range p.entries {
		for _, entry := range entries {
			if entry.computerInstanceID == ref.id && entry.workerEpoch == ref.epoch {
				p.mu.Unlock()
				return nil
			}
		}
	}
	p.mu.Unlock()
	return p.markInstanceTargetFailed(ctx, client, target, errors.New("capture source owner is absent after Worker restart"))
}
