package computerhost

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"math"
	"net/http"
	"os"
	"path/filepath"
	"sync"
	"sync/atomic"
	"time"

	"github.com/helmrdotdev/helmr/internal/computercheckpoint"
	"github.com/helmrdotdev/helmr/internal/disk"
	"github.com/helmrdotdev/helmr/internal/httpclient"
	"github.com/helmrdotdev/helmr/internal/ids"
	agentv1 "github.com/helmrdotdev/helmr/internal/proto/agent/v1"
	"github.com/helmrdotdev/helmr/internal/vm"
	"github.com/helmrdotdev/helmr/internal/workerapi"
	"golang.org/x/sync/errgroup"
)

type AllocatedComputerClient interface {
	AgentCheckpointClient
	AgentSaveClient
	AgentComputerClient
	ReadAgentCheckpoint(context.Context, workerapi.AllocationIdentity) (computercheckpoint.Manifest, error)
	PrepareAgentComputerRestore(context.Context, workerapi.AgentComputerRestoreRequest) (workerapi.AgentComputerInstallationResponse, error)
	AllocatedSessionClient
	workerapi.ComputerCommandClient
	DeliverComputerAllocation(context.Context, workerapi.AllocationIdentity) (workerapi.ComputerAllocationDelivery, error)
	ComputerAllocationSource(context.Context, workerapi.AllocationIdentity) (workerapi.ComputerAllocationSource, error)
	RenewAgentComputerLease(context.Context, workerapi.AgentComputerLeaseRequest) (workerapi.AgentComputerLeaseResponse, error)
	ObserveFreshComputerReady(context.Context, workerapi.ComputerAllocationReady) error
	ObserveAgentComputerStopped(context.Context, workerapi.AgentComputerStoppedRequest) error
}

// ComputerAllocationOwner owns one immutable physical instance through boot,
// execution and cleanup, including retries after uncertain cleanup/reporting.
// Its caller must retain this object until Finished, and never concurrently
// create another owner for the same identity. Run cancellation joins transports
// and physical cleanup; it is not a request to hibernate.
type ComputerAllocationOwner struct {
	machines                 *PreparedMachines
	client                   AllocatedComputerClient
	identity                 workerapi.AllocationIdentity
	hostID                   string
	admitStart               func(context.Context) error
	checkpointKeyUnavailable func()
	hostEpoch                int64
	observe                  func(context.Context, *agentv1.GuestSessionMessage) error
	operation                sync.Mutex
	machine                  vm.CheckpointableMachine
	prepared                 *preparedLeaf
	bootProgram              *computercheckpoint.Program
	saves                    *agentComputerSaves
	saveWake                 chan struct{}
	capturePermit            *capturePermit
	workWake                 allocationWake
	checkpoint               *computercheckpoint.Manifest
	restoreMemory            *checkpointMemorySource
	checkpointCut            disk.CapturedVersion
	checkpointRetryAt        time.Time
	checkpointRetryDelay     time.Duration
	invalidCheckpointID      string
	bootSeedID               string
	failedBootSeedID         string
	closing                  bool
	finished                 atomic.Bool
	// This typed operation allows device attachment failures to be tested without
	// claiming that a simulated device proves Linux NBD or VMM behavior.
	attachDevice func(context.Context, workerapi.AllocationIdentity, workerapi.ComputerAllocationSource) (vm.ComputerDevice, error)
}

func NewComputerAllocationOwner(machines *PreparedMachines, client AllocatedComputerClient, identity workerapi.AllocationIdentity, hostID string, hostEpoch int64, admitStart func(context.Context) error, checkpointKeyUnavailable func(), observe func(context.Context, *agentv1.GuestSessionMessage) error) (*ComputerAllocationOwner, error) {
	if machines == nil || machines.Backend == nil || machines.Reservations == nil || client == nil || admitStart == nil || checkpointKeyUnavailable == nil || observe == nil || identity.Kind != "computer" || identity.Epoch <= 0 || hostEpoch <= 0 || machines.ComputerStagingBytes <= 0 {
		return nil, errors.New("computer allocation requires its physical backend, capacity, client and observer")
	}
	if !filepath.IsAbs(machines.ComputerHelper) || len(machines.ComputerDevices) == 0 || machines.ComputerRanges == nil || machines.ComputerObjects == nil || machines.CheckpointCipher == nil {
		return nil, errors.New("computer allocation requires an explicit device helper and source")
	}
	for _, id := range []string{identity.EnvironmentID, identity.OwnerID, identity.AllocationID, hostID} {
		if ids.Validate(id) != nil {
			return nil, errors.New("invalid Computer allocation identity")
		}
	}
	return &ComputerAllocationOwner{machines: machines, client: client, identity: identity, hostID: hostID, hostEpoch: hostEpoch, admitStart: admitStart, checkpointKeyUnavailable: checkpointKeyUnavailable, observe: observe, attachDevice: machines.prepareAllocationDevice, saveWake: make(chan struct{}, 1)}, nil
}

func (o *ComputerAllocationOwner) leaseRequest() workerapi.AgentComputerLeaseRequest {
	return workerapi.AgentComputerLeaseRequest{EnvironmentID: o.identity.EnvironmentID, ComputerID: o.identity.OwnerID, AllocationID: o.identity.AllocationID, LeaseEpoch: o.identity.Epoch}
}

func (o *ComputerAllocationOwner) Finished() bool {
	return o.finished.Load()
}

func (o *ComputerAllocationOwner) Run(ctx context.Context) (resultErr error) {
	if !o.operation.TryLock() {
		return errors.New("computer allocation is already running")
	}
	defer o.operation.Unlock()
	if o.finished.Load() {
		return nil
	}
	if o.closing {
		return o.cleanup()
	}
	delivery, err := o.client.DeliverComputerAllocation(ctx, o.identity)
	if err != nil {
		var closed *httpclient.Error
		if errors.As(err, &closed) && closed.Code == workerapi.AllocationClosed {
			o.closing = true
			return o.cleanup()
		}
		// A permanent local fault releases undelivered assignments too. Closed
		// allocations above still clean up without consulting startup admission.
		if admissionErr := o.admitStart(ctx); errors.Is(admissionErr, ErrCheckpointKeyUnavailable) {
			o.closing = true
			return errors.Join(err, admissionErr, o.cleanup())
		}
		return err
	}
	if delivery.Identity != o.identity || !delivery.ExpiresAt.After(time.Now()) {
		return errors.New("computer delivery differs from its allocation or has expired")
	}
	ctx, cancel := context.WithCancelCause(ctx)
	var jobs sync.WaitGroup
	var machineWaitErr error
	defer func() {
		cancel(context.Canceled)
		jobs.Wait()
		if o.bootSeedID != "" && bootSeedArtifactFailure(errors.Join(resultErr, machineWaitErr, context.Cause(ctx))) {
			o.failedBootSeedID = o.bootSeedID
			resultErr = errors.Join(resultErr, machineWaitErr)
		}
		if o.checkpoint != nil && errors.Is(errors.Join(resultErr, machineWaitErr, context.Cause(ctx)), vm.ErrInvalidCheckpointContent) {
			o.invalidCheckpointID = o.checkpoint.CheckpointID.String()
			resultErr = errors.Join(resultErr, machineWaitErr)
		}
		if o.closing {
			resultErr = errors.Join(resultErr, o.cleanup())
		}
	}()
	jobs.Go(func() { o.renew(ctx, cancel, delivery.ExpiresAt) })
	phaseStarted := time.Now()
	source, err := o.client.ComputerAllocationSource(ctx, o.identity)
	if err != nil {
		return err
	}
	o.phaseCompleted("source_fetch", phaseStarted)
	defer source.Disk.Clear()
	if source.Disk.VersionID != delivery.BaseVersion {
		return errors.New("computer source differs from admitted base version")
	}
	shape := delivery.Shape
	if shape.CPUMillis <= 0 || shape.VCPUCount <= 0 || shape.VCPUCount > math.MaxInt32 || shape.VCPUCount > math.MaxInt64/1000 || shape.CPUMillis != shape.VCPUCount*1000 || shape.MemoryBytes <= 0 || shape.MemoryBytes%mebibyte != 0 || shape.ScratchBytes <= 0 || shape.ScratchBytes%mebibyte != 0 {
		return errors.New("invalid physical Computer allocation shape")
	}
	initialProgram, err := bootProgram(source.BootProgram)
	if err != nil {
		return err
	}
	if delivery.RestoredFrom != "" && !delivery.DiskOnlyRecovery {
		manifest, readErr := o.client.ReadAgentCheckpoint(ctx, o.identity)
		if readErr != nil {
			return readErr
		}
		if _, readErr = manifest.Encode(); readErr != nil {
			o.invalidCheckpointID = manifest.CheckpointID.String()
			o.closing = true
			return invalidCheckpoint(readErr)
		}
		o.checkpoint = &manifest
		initialProgram = manifest.BootProgram
	}
	request, err := o.machines.computerReservation(shape, source, initialProgram)
	if err != nil {
		return err
	}

	if admissionErr := o.admitStart(ctx); admissionErr == nil {
		o.prepared, err = o.machines.claimPreparedLeaf(o.identity, o.hostEpoch, delivery, source, request)
		if err != nil {
			return err
		}
	} else if errors.Is(admissionErr, ErrCheckpointKeyUnavailable) {
		o.closing = true
		return admissionErr
	}
	if o.prepared == nil {
		if err := o.machines.waitRealAllocationCapacity(ctx, o.admitStart, instanceReservationKey(o.identity.AllocationID, o.identity.Epoch), request, &o.workWake); err != nil {
			if errors.Is(err, ErrCheckpointKeyUnavailable) {
				o.closing = true
			}
			return err
		}
	}
	// From this point the owner can retain filesystem/device/VM effects. Every
	// later invocation only retries physical cleanup; this instance never boots twice.
	o.closing = true
	phaseStarted = time.Now()
	var device vm.ComputerDevice
	if o.prepared != nil {
		device = o.prepared.device
		o.bootProgram = o.prepared.program
		o.bootSeedID = o.prepared.manifest.BuildID.String()
		o.failedBootSeedID = o.bootSeedID
		o.machine, err = o.machines.adoptPreparedLeaf(ctx, o.prepared, o.identity, o.hostEpoch, source)
		source.Disk.Clear()
		if err != nil {
			return err
		}
		o.phaseCompleted("prepared_adoption", phaseStarted)
	} else {
		var images *programSnapshots
		defer func() { resultErr = errors.Join(resultErr, images.close()) }()
		group, stageCtx := errgroup.WithContext(ctx)
		group.Go(func() error {
			var attachErr error
			device, attachErr = o.attachDevice(stageCtx, o.identity, source)
			return attachErr
		})
		if source.BootProgram != nil && (delivery.RestoredFrom == "" || delivery.DiskOnlyRecovery) {
			group.Go(func() error {
				directory := o.machines.computerBootProgramDirectory(o.identity.AllocationID, o.identity.Epoch)
				if err := os.MkdirAll(directory, 0700); err != nil {
					return err
				}
				// A selected seed includes the exact read-only drive topology. Allow
				// its required content to finish staging within the allocation lifetime;
				// the optional cold-boot prefetch budget must not discard that topology.
				prefetchCtx := stageCtx
				prefetchCancel := func() {}
				if source.BootSeed == nil {
					prefetchCtx, prefetchCancel = context.WithTimeout(stageCtx, 5*time.Second)
				}
				defer prefetchCancel()
				var stageErr error
				images, stageErr = o.machines.prepareProgramArtifacts(prefetchCtx, directory, o.identity.AllocationID, source.BootProgram)
				if stageErr != nil {
					if stageCtx.Err() != nil {
						return stageCtx.Err()
					}
					o.machines.logInfo("Computer boot Program prefetch unavailable", "computer_id", o.identity.OwnerID, "error", stageErr.Error())
					if err := os.RemoveAll(directory); err != nil {
						return err
					}
					return o.machines.Reservations.ReleaseHostDisk(instanceReservationKey(o.identity.AllocationID, o.identity.Epoch), initialProgram.Size())
				}
				o.bootProgram = initialProgram
				return nil
			})
		}
		err = group.Wait()
		// The local disk takes its own key copies. Do not retain the transport's
		// plaintext for the potentially long lifetime of this Computer.
		source.Disk.Clear()
		if err != nil {
			return err
		}
		o.phaseCompleted("device_attach", phaseStarted)
		phaseStarted = time.Now()
		if delivery.RestoredFrom != "" && !delivery.DiskOnlyRecovery {
			err = o.restore(ctx, delivery, source, device)
		} else {
			materialize := vm.MaterializeRequest{RecordPhase: func(phase vm.Phase) { o.recordVMPhase("materialize", phase) }, ID: o.identity.AllocationID, OwnerKind: vm.OwnerInstance, Binding: vm.WorkloadBinding{WorkerEpoch: o.hostEpoch, OwnerID: o.identity.AllocationID, Generation: 1, ComputerInstanceID: o.identity.AllocationID, VMPlatformID: shape.VMPlatformID}, RootfsDigest: source.RootfsDigest, ComputerMountPath: "/workspace", BaseComputerDiskVersionID: delivery.BaseVersion, Resources: vm.Resources{MilliCPU: shape.CPUMillis, MemoryMiB: shape.MemoryBytes / mebibyte, DiskMiB: shape.ScratchBytes / mebibyte, Slots: 1}, VMVCPUCount: int32(shape.VCPUCount), CPUConfigDigest: shape.CPUConfigDigest, ReadOnlyDrives: bootProgramDrives(o.bootProgram, images), Topology: vm.Topology{Computer: &vm.ComputerDisk{ComputerID: o.identity.OwnerID, VersionID: delivery.BaseVersion, SizeBytes: source.Disk.Root.LogicalBytes, Device: device}}}
			if source.BootSeed != nil && delivery.RestoredFrom == "" && !delivery.DiskOnlyRecovery {
				matches, matchErr := o.machines.Backend.BootSeedMatches(materialize, source.BootSeed.Config)
				if matchErr != nil {
					o.failedBootSeedID = source.BootSeed.BuildID.String()
					return matchErr
				}
				if matches {
					o.bootSeedID = source.BootSeed.BuildID.String()
					directory := filepath.Join(o.machines.computerPreparationDirectory(o.identity.AllocationID, o.identity.Epoch), "boot-seed")
					if err := os.MkdirAll(directory, 0700); err != nil {
						return err
					}
					materialize.BootSeed, err = o.machines.openBootSeed(ctx, directory, *source.BootSeed, shape.MemoryBytes, shape.ScratchBytes)
					if err != nil {
						if errors.Is(err, ErrCheckpointKeyUnavailable) {
							o.checkpointKeyUnavailable()
						}
						if bootSeedArtifactFailure(err) {
							o.failedBootSeedID = o.bootSeedID
						}
						return err
					}
				}
			}
			if materialize.BootSeed != nil {
				// Any unsuccessful seed startup is excluded for this Computer only.
				// Keep the report pending until the control plane acknowledges readiness;
				// cleanup sends it only after proving physical closure.
				o.failedBootSeedID = o.bootSeedID
			}
			o.machine, err = o.machines.Backend.Materialize(ctx, materialize)
			if err == nil && o.machine != nil && materialize.BootSeed != nil {
				// The backend has unpacked scratch into its owned disk. Retain only the
				// live memory source; a later capture has its own accounted packed input.
				err = os.Remove(materialize.BootSeed.ScratchDisk)
			}
		}
		if err != nil {
			return err
		}
		if err := images.close(); err != nil {
			return err
		}
		images = nil
		if err := os.RemoveAll(o.machines.computerBootProgramDirectory(o.identity.AllocationID, o.identity.Epoch)); err != nil {
			return err
		}
	}
	if o.machine == nil {
		return errors.New("computer backend returned no machine")
	}
	o.phaseCompleted("vm_boot", phaseStarted)
	phaseStarted = time.Now()
	jobs.Go(func() {
		err := o.machine.Wait(ctx)
		machineWaitErr = err
		if err == nil {
			err = errors.New("computer VM exited")
		}
		cancel(err)
	})
	if delivery.RestoredFrom != "" && !delivery.DiskOnlyRecovery {
		if err := o.activateRestore(ctx, delivery); err != nil {
			return err
		}
	} else {
		if err := prepareAllocationImageWithProgram(ctx, o.machine, delivery.Identity, delivery.ChannelCredential, delivery.BaseVersion, source.ImageConfig, o.bootProgram); err != nil {
			return err
		}
		for {
			requestCtx, requestCancel := context.WithTimeout(ctx, 10*time.Second)
			err = o.client.ObserveFreshComputerReady(requestCtx, workerapi.ComputerAllocationReady{Identity: o.identity, Shape: shape, BaseVersion: delivery.BaseVersion})
			requestCancel()
			if err == nil {
				if o.bootSeedID != "" {
					o.machines.logInfo("Computer boot seed initialized", "computer_id", o.identity.OwnerID, "allocation_id", o.identity.AllocationID, "boot_seed_id", o.bootSeedID)
				}
				o.failedBootSeedID = ""
				break
			}
			var closed *httpclient.Error
			if errors.As(err, &closed) && closed.Code == workerapi.AllocationClosed {
				return err
			}
			var rejected interface{ WorkerAuthorityRejected() bool }
			if httpclient.IsStatus(err, http.StatusBadRequest) || (errors.As(err, &rejected) && rejected.WorkerAuthorityRejected()) {
				return err
			}
			if err := sleepWithContext(ctx, 250*time.Millisecond); err != nil {
				return context.Cause(ctx)
			}
		}
	}
	jobs.Go(func() {
		authority := commandAuthority{EnvironmentID: o.identity.EnvironmentID, ComputerID: o.identity.OwnerID, ComputerInstanceID: o.identity.AllocationID, WriterGeneration: o.identity.Epoch, GuestChannelCredential: delivery.ChannelCredential}
		cancel((commandService{LogLimits: o.machines.CommandLogLimits}).Serve(ctx, o.machine, authority, o.client))
	})
	o.phaseCompleted("agent_ready", phaseStarted)
	live, ok := o.machine.(liveCaptureMachine)
	if !ok {
		return errors.New("computer allocation requires live disk capture")
	}
	// The serial checkpoint owner temporarily joins ordinary attachments before
	// source continuation installs fresh grants. Their cancellation never stops
	// resident guest processes. The parent lifetime owns every restarted loop.
	var sessionCancel context.CancelFunc
	var sessionDone chan struct{}
	startSessions := func() {
		sessionCtx, stop := context.WithCancel(ctx)
		sessionCancel, sessionDone = stop, make(chan struct{})
		done := sessionDone
		jobs.Go(func() {
			defer close(done)
			err := o.machines.serveAllocatedSessions(sessionCtx, o.machine, delivery, o.hostID, o.client, o.observe, &o.workWake, o.bootProgram)
			if sessionCtx.Err() == nil {
				cancel(err)
			}
		})
	}
	startSessions()
	suspend := func() func() {
		sessionCancel()
		<-sessionDone
		return func() {
			if ctx.Err() == nil {
				startSessions()
			}
		}
	}
	o.saves = &agentComputerSaves{client: o.client, objects: o.machines.ComputerObjects, identity: o.identity, machine: live, changes: device.Changes, pinCommitted: device.PinCommitted, wake: o.saveWake,
		cancelIdle: o.cancelCaptureWait,
		idle:       func(ctx context.Context) error { return o.captureIdle(ctx, delivery, suspend) }}
	// This loop serializes disk and VM cuts; returning joins attachment and lease
	// jobs in the parent defer before physical cleanup releases capture retention.
	err = o.saves.run(ctx)
	if errors.Is(err, errComputerHibernated) {
		return nil
	}
	if ctx.Err() != nil {
		return errors.Join(context.Cause(ctx), err)
	}
	return err
}

func (o *ComputerAllocationOwner) renew(ctx context.Context, cancel context.CancelCauseFunc, expires time.Time) {
	for ctx.Err() == nil {
		remaining := time.Until(expires)
		if remaining <= 0 {
			cancel(errors.New("computer allocation authority expired"))
			return
		}
		// Keep a renewal opportunity before expiry, including redelivery with a
		// short remaining lease. Bound near-expiry retries to avoid a tight loop.
		delay := min(10*time.Second, max(100*time.Millisecond, remaining/2), remaining)
		if sleepWithContext(ctx, delay) != nil {
			return
		}
		if !time.Now().Before(expires) {
			cancel(errors.New("computer allocation authority expired"))
			return
		}
		requestCtx, requestCancel := context.WithDeadline(ctx, minTime(expires, time.Now().Add(5*time.Second)))
		result, err := o.client.RenewAgentComputerLease(requestCtx, o.leaseRequest())
		requestCancel()
		if err == nil {
			if result.ExpiresAt.Before(expires) || !result.ExpiresAt.After(time.Now()) {
				cancel(errors.New("computer lease renewal regressed or expired"))
				return
			}
			expires = result.ExpiresAt
		} else if httpclient.IsStatus(err, http.StatusBadRequest) || httpclient.IsStatus(err, http.StatusForbidden) || httpclient.IsStatus(err, http.StatusConflict) {
			cancel(err)
			return
		}
	}
}

func minTime(a, b time.Time) time.Time {
	if a.Before(b) {
		return a
	}
	return b
}

func (o *ComputerAllocationOwner) cleanup() error {
	o.cancelCaptureWait()
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	var closeErr error
	physical := preparedMachineRef{id: o.identity.AllocationID, epoch: o.identity.Epoch}
	if o.prepared != nil {
		select {
		case <-o.prepared.done:
		case <-ctx.Done():
			return ctx.Err()
		}
		physical = preparedMachineRef{id: o.prepared.identity.AllocationID, epoch: o.prepared.identity.Epoch}
		if o.machine == nil && o.prepared.machine != nil {
			closeErr = o.prepared.machine.Close(ctx)
		}
	}
	if o.machine != nil {
		closeErr = o.machine.Close(ctx)
	}
	// A machine may cache a failed Close result. Exact backend reconciliation
	// retries physical absence independently before releasing device custody.
	if err := o.machines.Backend.Cleanup(ctx, vm.Owner{Kind: vm.OwnerInstance, ID: physical.id}); err != nil {
		return fmt.Errorf("prove allocated Computer absent: %w", errors.Join(closeErr, err))
	}
	// Stop device I/O before reporting physical absence, but retain its arena and
	// funded capacity until the CP acknowledges physical stop and save disposition.
	// The closed arena is retained custody, not a resumable publication owner.
	if err := o.machines.releaseComputerDevice(physical.id, physical.epoch); err != nil {
		return err
	}
	if err := o.client.ObserveAgentComputerStopped(ctx, workerapi.AgentComputerStoppedRequest{AgentComputerLeaseRequest: o.leaseRequest(), InvalidCheckpointID: o.invalidCheckpointID, FailedBootSeedID: o.failedBootSeedID}); err != nil {
		return err
	}
	if o.saves != nil {
		o.saves.release()
	}
	if o.checkpointCut != nil {
		o.checkpointCut.Release()
		o.checkpointCut = nil
	}
	if err := o.machines.releaseInstanceResources(physical.id, physical.epoch); err != nil {
		return err
	}
	if physical.id != o.identity.AllocationID || physical.epoch != o.identity.Epoch {
		// Checkpoint publication remains owned by the real allocation, while
		// the adopted disk/VM retain their original physical staging directory.
		if err := o.machines.releaseInstanceResources(o.identity.AllocationID, o.identity.Epoch); err != nil {
			return err
		}
	}
	if o.machines.Reservations != nil {
		if err := o.machines.Reservations.Release(instanceReservationKey(o.identity.AllocationID, o.identity.Epoch)); err != nil {
			return err
		}
	}
	o.machines.forgetCheckpointSource(instanceReservationKey(o.identity.AllocationID, o.identity.Epoch))
	o.finished.Store(true)
	return nil
}

// WakeSaves coalesces authenticated control-plane notifications for this exact
// allocation. The save owner rereads durable requests after the edge.
func (o *ComputerAllocationOwner) WakeSaves() {
	select {
	case o.saveWake <- struct{}{}:
	default:
	}
}

func (o *ComputerAllocationOwner) phaseCompleted(phase string, started time.Time) {
	attrs := []any{"computer_id", o.identity.OwnerID, "allocation_id", o.identity.AllocationID, "lease_epoch", o.identity.Epoch, "phase", phase, "duration_ms", float64(time.Since(started)) / float64(time.Millisecond)}
	if phase == "agent_ready" {
		if diagnostic, ok := o.machine.(interface{ MemoryRestoreDiagnostics() *vm.MemoryRestoreStats }); ok {
			if stats := diagnostic.MemoryRestoreDiagnostics(); stats != nil {
				o.recordVMPhase("restore", vm.Phase{Name: "restore_memory_agent_ready", Memory: stats})
			}
		}
	}
	if phase == "agent_ready" && o.restoreMemory != nil {
		stats := o.restoreMemory.MemoryReadStats()
		attrs = append(attrs, memoryReadDiagnosticAttrs(stats)...)
		attrs = append(attrs, "memory_range_requests", stats.RangeRequests, "memory_ciphertext_bytes", stats.CiphertextBytes, "memory_decode_ms", stats.DecodeDurationMs)
	}
	slog.Info("Computer allocation phase completed", attrs...)
}

func (o *ComputerAllocationOwner) recordVMPhase(launch string, phase vm.Phase) {
	attrs := []any{"launch", launch, "computer_id", o.identity.OwnerID, "allocation_id", o.identity.AllocationID, "lease_epoch", o.identity.Epoch, "phase", phase.Name, "duration_ms", phase.DurationMs, "role", phase.Role, "error_class", phase.ErrorClass}
	if !phase.StartedAt.IsZero() {
		attrs = append(attrs, "phase_started_at", phase.StartedAt.UTC().Format(time.RFC3339Nano))
	}
	if phase.Memory != nil {
		attrs = append(attrs, memoryReadDiagnosticAttrs(phase.Memory.MemoryReadStats)...)
		m := phase.Memory
		attrs = append(attrs, "memory_demand_duration_buckets_us", m.DemandDurationBuckets, "memory_join_duration_buckets_us", m.JoinDurationBuckets)
		var observations []byte
		if phase.Name == "restore_memory_hydration" {
			observations, _ = json.Marshal(m.FaultObservations)
		}
		attrs = append(attrs, "memory_observed_demand_pages", m.ObservedDemandPages, "memory_observed_demand_windows", m.ObservedDemandWindows, "memory_demand_load_attempts", m.DemandLoads, "memory_background_load_attempts", m.BackgroundLoads, "memory_background_discarded_windows", m.BackgroundDiscardedWindows, "memory_background_demand_wait_ms", m.BackgroundDemandWaitMs, "memory_demand_load_errors", m.DemandLoadErrors, "memory_background_load_errors", m.BackgroundLoadErrors, "memory_join_background_attempts", m.JoinBackground, "memory_join_demand_attempts", m.JoinDemand, "memory_join_background_ms", m.JoinBackgroundMs, "memory_join_demand_ms", m.JoinDemandMs, "memory_fault_queue_ms", m.FaultQueueMs, "memory_fault_service_ms", m.FaultServiceMs, "memory_demand_read_ms", m.DemandSourceReadMs, "memory_background_read_ms", m.BackgroundSourceReadMs, "memory_demand_populate_ms", m.DemandPopulateMs, "memory_background_populate_ms", m.BackgroundPopulateMs, "memory_observation_started_at", m.ObservationStartedAt.UTC().Format(time.RFC3339Nano), "memory_fault_observations_json", string(observations), "memory_dropped_fault_observations", m.DroppedFaultObservations)
		attrs = append(attrs, "memory_demand_faults", phase.Memory.DemandFaults, "memory_loaded_chunks", phase.Memory.LoadedChunks, "memory_total_chunks", phase.Memory.TotalChunks, "memory_range_requests", phase.Memory.RangeRequests, "memory_ciphertext_bytes", phase.Memory.CiphertextBytes, "memory_decode_ms", phase.Memory.DecodeDurationMs)
	}
	if phase.Filepack != nil {
		attrs = append(attrs, "logical_bytes", phase.Filepack.LogicalBytes, "encoded_chunks", phase.Filepack.EncodedChunks, "unpack_written_bytes", phase.Filepack.UnpackWrittenBytes)
	}
	slog.Info("Computer VM phase completed", attrs...)
}

func memoryReadDiagnosticAttrs(stats vm.MemoryReadStats) []any {
	return []any{"memory_background_range_requests", stats.BackgroundRangeRequests, "memory_background_ciphertext_bytes", stats.BackgroundCiphertextBytes, "memory_background_range_header_ms", stats.BackgroundRangeHeaderMs, "memory_background_range_body_ms", stats.BackgroundRangeBodyMs, "memory_range_header_ms", stats.RangeHeaderMs, "memory_range_body_ms", stats.RangeBodyMs, "memory_range_active", stats.RangeActive, "memory_range_peak", stats.RangePeak}
}
