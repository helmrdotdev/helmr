package computerhost

import (
	"context"
	"errors"
	"fmt"
	"math"
	"math/rand/v2"
	"net/http"
	"path/filepath"
	"sync"
	"sync/atomic"
	"time"

	"github.com/helmrdotdev/helmr/internal/artifact"
	"github.com/helmrdotdev/helmr/internal/definition"
	"github.com/helmrdotdev/helmr/internal/disk"
	"github.com/helmrdotdev/helmr/internal/disk/blockformat"
	"github.com/helmrdotdev/helmr/internal/httpclient"
	"github.com/helmrdotdev/helmr/internal/ids"
	"github.com/helmrdotdev/helmr/internal/reservation"
	"github.com/helmrdotdev/helmr/internal/vm"
	"github.com/helmrdotdev/helmr/internal/wire"
	"github.com/helmrdotdev/helmr/internal/workerapi"
)

type AllocatedPreparationClient interface {
	AppendPreparationLog(context.Context, workerapi.PreparationLogRequest) (workerapi.DiagnosticLogReceipt, error)
	DeliverPreparationAllocation(context.Context, workerapi.AllocationIdentity) (workerapi.PreparationAllocationDelivery, error)
	PreparationStart(context.Context, workerapi.PreparationExecutor) (workerapi.PreparationStart, error)
	PreparationSecrets(context.Context, workerapi.PreparationExecutor) (workerapi.PreparationSecrets, error)
	PreparationWriteKey(context.Context, workerapi.PreparationExecutor) (workerapi.ComputerKeyMaterial, error)
	RenewPreparation(context.Context, workerapi.PreparationExecutor) (workerapi.PreparationRenewal, error)
	FailPreparation(context.Context, workerapi.PreparationFailure) error
	BeginPreparationCapture(context.Context, workerapi.PreparationCaptureBegin) error
	RegisterPreparationObject(context.Context, workerapi.PreparationObject) error
	CertifyPreparationObject(context.Context, workerapi.PreparationObject) error
	RecordPreparationCapture(context.Context, workerapi.PreparationPublication) error
	PublishPreparation(context.Context, workerapi.PreparationPublication) error
	ObservePreparationStopped(context.Context, workerapi.AllocationIdentity) error
}

// PreparationAllocationOwner is the single owner of a private preparation VM.
// Run may retry cleanup but never reruns authored code after physical startup.
type PreparationAllocationOwner struct {
	machines   *PreparedMachines
	client     AllocatedPreparationClient
	identity   workerapi.AllocationIdentity
	admitStart func(context.Context) error
	hostEpoch  int64
	stopLogs   context.CancelFunc
	operation  sync.Mutex
	finished   atomic.Bool
	expires    atomic.Int64
	closing    bool
	reserved   bool
	absent     bool
	published  bool
	stopping   atomic.Bool
	executor   workerapi.PreparationExecutor
	machine    vm.CheckpointableMachine
	stage      *preparationStage
	capture    *disk.InitialVersion
	stageImage func(context.Context, *preparationStage, workerapi.AllocationIdentity, workerapi.PreparationStart) error
}

func NewPreparationAllocationOwner(machines *PreparedMachines, client AllocatedPreparationClient, identity workerapi.AllocationIdentity, hostEpoch int64, admitStart func(context.Context) error) (*PreparationAllocationOwner, error) {
	if machines == nil || machines.Backend == nil || machines.CAS == nil || machines.PlatformStore == nil || machines.ComputerObjects == nil || machines.Reservations == nil || !filepath.IsAbs(machines.TempDir) || machines.RuntimeArchitecture != definition.ArchitectureX8664 || machines.ComputerStagingBytes <= 0 || client == nil || admitStart == nil || identity.Kind != "preparation" || identity.Epoch <= 0 || hostEpoch <= 0 {
		return nil, errors.New("preparation requires its physical backend, artifact stores, capacity and diagnostic bounds")
	}
	if err := wire.ValidatePreparationLogLimits(machines.PreparationLogLimits); err != nil {
		return nil, err
	}
	for _, id := range []string{identity.EnvironmentID, identity.OwnerID, identity.InstanceID} {
		if ids.Validate(id) != nil {
			return nil, errors.New("invalid preparation allocation identity")
		}
	}
	return &PreparationAllocationOwner{machines: machines, client: client, identity: identity, hostEpoch: hostEpoch, admitStart: admitStart, stageImage: machines.stagePreparation}, nil
}
func (o *PreparationAllocationOwner) Finished() bool { return o.finished.Load() }
func (o *PreparationAllocationOwner) Run(ctx context.Context) (resultErr error) {
	if !o.operation.TryLock() {
		return errors.New("preparation allocation is already running")
	}
	defer o.operation.Unlock()
	if o.finished.Load() {
		return nil
	}
	if o.closing {
		return o.cleanup()
	}
	delivery, err := o.client.DeliverPreparationAllocation(ctx, o.identity)
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
	if delivery.Identity != o.identity || len(delivery.ChannelCredential) != 32 || !delivery.ExpiresAt.After(time.Now()) {
		clear(delivery.ChannelCredential)
		return errors.New("preparation delivery differs or expired")
	}
	o.executor = workerapi.PreparationExecutor{Identity: o.identity, ChannelCredential: delivery.ChannelCredential}
	defer func() { clear(o.executor.ChannelCredential); o.executor.ChannelCredential = nil }()
	ctx, cancel := context.WithCancelCause(ctx)
	var jobs sync.WaitGroup
	defer func() {
		cancel(context.Canceled)
		jobs.Wait()
		if o.closing {
			resultErr = errors.Join(resultErr, o.cleanup())
		}
	}()
	o.expires.Store(delivery.ExpiresAt.UnixNano())
	jobs.Go(func() { o.renewPreparation(ctx, cancel) })
	start, err := o.client.PreparationStart(ctx, o.executor)
	if err != nil {
		return err
	}
	if start.SeedObject.Digest != start.Seed.ArtifactDigest || start.Seed.Profile != definition.ComputerSeedProfile || start.SeedObject.MediaType != definition.ComputerSeedMediaType || !definition.ValidDeclaredID(start.ComputerDefinitionID) {
		return errors.New("preparation seed or definition is invalid")
	}
	if err := (disk.SeedArtifact{Object: computerObject(start.SeedObject), LogicalBytes: disk.SeedCapacity}).Validate(disk.SeedCapacity); err != nil {
		return err
	}
	shape := delivery.Shape
	if shape.CPUMillis <= 0 || shape.VCPUCount <= 0 || shape.VCPUCount > math.MaxInt32 || shape.CPUMillis != shape.VCPUCount*1000 || shape.MemoryBytes <= 0 || shape.MemoryBytes%mebibyte != 0 || shape.ScratchBytes <= 0 || shape.ScratchBytes%mebibyte != 0 {
		return errors.New("invalid preparation resource shape")
	}
	if start.Program.Runtime.SizeBytes < 1 || start.Program.Runtime.SizeBytes > artifact.MaxRuntimePhysicalBytes || start.Program.Artifact.SizeBytes < 1 || start.Program.Artifact.SizeBytes > artifact.MaxProgramPhysicalBytes {
		return errors.New("invalid preparation Program staging size")
	}
	capacity, err := instanceReservationVectorWithProjection(shape.CPUMillis, shape.MemoryBytes/mebibyte, shape.ScratchBytes/mebibyte, disk.SeedCapacity)
	if err != nil {
		return err
	}
	for _, bytes := range []int64{o.machines.ComputerStagingBytes, start.Program.Runtime.SizeBytes, start.Program.Artifact.SizeBytes} {
		if capacity.HostDiskBytes > math.MaxInt64-bytes {
			return reservation.ErrOverflow
		}
		capacity.HostDiskBytes += bytes
	}
	if err := waitAllocationCapacity(ctx, o.admitStart, o.machines.Reservations, instanceReservationKey(o.identity.InstanceID, o.identity.Epoch), capacity); err != nil {
		if errors.Is(err, ErrCheckpointKeyUnavailable) {
			o.closing = true
		}
		return err
	}
	o.reserved = true
	o.closing = true
	o.stage, err = o.machines.preparationDirectory(o.identity)
	if err != nil {
		return err
	}
	if err := o.stageImage(ctx, o.stage, o.identity, start); err != nil {
		return err
	}
	o.machine, err = o.machines.Backend.Materialize(ctx, vm.MaterializeRequest{ID: o.identity.InstanceID, OwnerKind: vm.OwnerInstance, Binding: vm.WorkloadBinding{WorkerEpoch: o.hostEpoch, OwnerID: o.identity.InstanceID, Generation: 1, ComputerInstanceID: o.identity.InstanceID, VMPlatformID: shape.VMPlatformID}, RootfsDigest: start.RootfsDigest, ComputerMountPath: "/workspace", BaseComputerDiskVersionID: start.Seed.ArtifactDigest, Resources: vm.Resources{MilliCPU: shape.CPUMillis, MemoryMiB: shape.MemoryBytes / mebibyte, DiskMiB: shape.ScratchBytes / mebibyte, Slots: 1}, VMVCPUCount: int32(shape.VCPUCount), CPUConfigDigest: shape.CPUConfigDigest, ReadOnlyDrives: o.stage.drives, Topology: vm.Topology{Computer: &vm.ComputerDisk{ComputerID: o.identity.OwnerID, File: o.stage.disk, VersionID: start.Seed.ArtifactDigest, SizeBytes: disk.SeedCapacity}}})
	if err != nil {
		return err
	}
	if o.machine == nil {
		return errors.New("preparation backend returned no machine")
	}
	jobs.Go(func() {
		err := o.machine.Wait(ctx)
		if !o.stopping.Load() {
			if err == nil {
				err = errors.New("preparation VM exited")
			}
			cancel(err)
		}
	})
	logCtx, stopLogs := context.WithCancel(ctx)
	o.stopLogs = stopLogs
	for _, stream := range []string{"stdout", "stderr"} {
		jobs.Go(func() { o.deliverPreparationLogs(logCtx, stream) })
	}
	if err := o.runGuestPreparation(ctx, start); err != nil {
		return err
	}
	var key workerapi.ComputerKeyMaterial
	err = retryPreparation(ctx, func(ctx context.Context) error {
		clear(key.Key)
		var err error
		key, err = o.client.PreparationWriteKey(ctx, o.executor)
		return err
	})
	defer clear(key.Key)
	if err != nil {
		return err
	}
	if err := retryPreparation(ctx, func(ctx context.Context) error {
		return o.client.BeginPreparationCapture(ctx, workerapi.PreparationCaptureBegin{Executor: o.executor, LogicalBytes: disk.SeedCapacity})
	}); err != nil {
		return err
	}
	if err := flushPreparation(ctx, o.machine, o.identity.OwnerID); err != nil {
		return err
	}
	// No guest writer may survive the source cut. Keep the allocation lease and
	// custody until publication settles, even though the private VM is now absent.
	if err := o.stopMachine(ctx); err != nil {
		return err
	}
	o.capture, err = disk.CaptureInitialVersion(ctx, disk.VersionCapture{Disk: o.stage.disk, Capacity: disk.SeedCapacity, StagingParent: o.stage.directory, Scope: key.Scope, KeyID: key.ID, Key: key.Key, Fanout: 64, PackLimit: blockformat.MinPackLimit, MaxStagedBytes: o.machines.ComputerStagingBytes, MaxObjects: 1 << 20})
	if err != nil {
		return err
	}
	publisher := preparationPublisher{client: o.client, executor: o.executor, objects: o.machines.ComputerObjects}
	rootLocator, err := o.capture.Publish(ctx, publisher)
	if err != nil {
		return err
	}
	root, err := disk.NewVersionRoot(rootLocator, disk.SeedCapacity)
	if err != nil {
		return err
	}
	receipt := workerapi.PreparationPublication{Executor: o.executor, Root: root, Evidence: "private preparation exited, filesystem flushed and VM absent before full disk capture"}
	if err := retryPreparation(ctx, func(ctx context.Context) error { return o.client.RecordPreparationCapture(ctx, receipt) }); err != nil {
		return err
	}
	if err := retryPreparation(ctx, func(ctx context.Context) error { return o.client.PublishPreparation(ctx, receipt) }); err != nil {
		return err
	}
	o.published = true
	return nil
}

func (o *PreparationAllocationOwner) renewPreparation(ctx context.Context, cancel context.CancelCauseFunc) {
	for {
		expires := time.Unix(0, o.expires.Load())
		remaining := time.Until(expires)
		if remaining <= 0 {
			cancel(errors.New("preparation allocation authority expired"))
			return
		}
		if sleepWithContext(ctx, min(10*time.Second, max(100*time.Millisecond, remaining/2), remaining)) != nil {
			return
		}
		requestCtx, stop := context.WithDeadline(ctx, minTime(expires, time.Now().Add(5*time.Second)))
		result, err := o.client.RenewPreparation(requestCtx, o.executor)
		stop()
		if err == nil {
			if result.ExpiresAt.Before(expires) || !result.ExpiresAt.After(time.Now()) {
				cancel(errors.New("preparation renewal regressed or expired"))
				return
			}
			o.expires.Store(result.ExpiresAt.UnixNano())
		} else if preparationAuthorityRejected(err) {
			cancel(err)
			return
		}
	}
}
func preparationAuthorityRejected(err error) bool {
	var typed interface{ WorkerAuthorityRejected() bool }
	return httpclient.IsStatus(err, http.StatusBadRequest) || httpclient.IsStatus(err, http.StatusForbidden) || httpclient.IsStatus(err, http.StatusConflict) || (errors.As(err, &typed) && typed.WorkerAuthorityRejected())
}
func retryPreparation(ctx context.Context, operation func(context.Context) error) error {
	delay := time.Second
	for {
		callCtx, stop := context.WithTimeout(ctx, 10*time.Second)
		err := operation(callCtx)
		stop()
		if err == nil || preparationAuthorityRejected(err) {
			return err
		}
		if sleepWithContext(ctx, delay/2+time.Duration(rand.Int64N(int64(delay/2)+1))) != nil {
			return errors.Join(ctx.Err(), err)
		}
		delay = min(10*time.Second, delay*2)
	}
}
func (o *PreparationAllocationOwner) stopMachine(ctx context.Context) error {
	if o.stopLogs != nil {
		o.stopLogs()
	}
	if o.absent {
		return nil
	}
	o.stopping.Store(true)
	var closeErr error
	if o.machine != nil {
		closeErr = o.machine.Close(ctx)
	}
	if err := o.machines.Backend.Cleanup(ctx, vm.Owner{Kind: vm.OwnerInstance, ID: o.identity.InstanceID}); err != nil {
		return fmt.Errorf("prove preparation VM absent: %w", errors.Join(closeErr, err))
	}
	o.absent = true
	return nil
}
func (o *PreparationAllocationOwner) cleanup() error {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	if err := o.stopMachine(ctx); err != nil {
		return err
	}
	if !o.published && len(o.executor.ChannelCredential) == 32 {
		if err := o.client.FailPreparation(ctx, workerapi.PreparationFailure{Executor: o.executor, Code: "preparation_execution_failed"}); err != nil {
			return err
		}
	}
	if o.capture != nil {
		if err := o.capture.Close(); err != nil {
			return err
		}
		o.capture = nil
	}
	if err := o.stage.close(); err != nil {
		return err
	}
	if o.reserved {
		if err := o.machines.Reservations.Release(instanceReservationKey(o.identity.InstanceID, o.identity.Epoch)); err != nil {
			return err
		}
		o.reserved = false
	}
	if err := o.client.ObservePreparationStopped(ctx, o.identity); err != nil {
		return err
	}
	o.finished.Store(true)
	return nil
}
