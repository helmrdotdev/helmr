package computerhost

import (
	"context"
	"errors"
	"fmt"
	"math"
	"net/http"
	"path/filepath"
	"sync"
	"sync/atomic"
	"time"

	"github.com/helmrdotdev/helmr/internal/computercheckpoint"
	"github.com/helmrdotdev/helmr/internal/disk"
	"github.com/helmrdotdev/helmr/internal/httpclient"
	"github.com/helmrdotdev/helmr/internal/ids"
	agentv1 "github.com/helmrdotdev/helmr/internal/proto/agent/v1"
	"github.com/helmrdotdev/helmr/internal/reservation"
	"github.com/helmrdotdev/helmr/internal/vm"
	"github.com/helmrdotdev/helmr/internal/workerapi"
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
	saves                    *agentComputerSaves
	checkpoint               *computercheckpoint.Manifest
	checkpointCut            disk.CapturedVersion
	checkpointRetryAt        time.Time
	checkpointRetryDelay     time.Duration
	invalidCheckpointID      string
	closing                  bool
	finished                 atomic.Bool
	// This typed operation allows device attachment failures to be tested without
	// claiming that a simulated device proves Linux NBD or VMM behavior.
	attachDevice func(context.Context, workerapi.AllocationIdentity, workerapi.ComputerAllocationSource) (vm.ComputerDevice, error)
}

func NewComputerAllocationOwner(machines *PreparedMachines, client AllocatedComputerClient, identity workerapi.AllocationIdentity, hostID string, hostEpoch int64, admitStart func(context.Context) error, checkpointKeyUnavailable func(), observe func(context.Context, *agentv1.GuestSessionMessage) error) (*ComputerAllocationOwner, error) {
	if machines == nil || machines.Backend == nil || machines.Reservations == nil || client == nil || admitStart == nil || checkpointKeyUnavailable == nil || observe == nil || identity.Kind != "computer" || identity.Epoch <= 0 || hostEpoch <= 0 || machines.ComputerStagingBytes <= 0 || machines.ComputerSaveEvery <= 0 {
		return nil, errors.New("computer allocation requires its physical backend, capacity, client and observer")
	}
	if !filepath.IsAbs(machines.ComputerHelper) || len(machines.ComputerDevices) == 0 || machines.ComputerRanges == nil || machines.ComputerObjects == nil || machines.CheckpointCipher == nil {
		return nil, errors.New("computer allocation requires an explicit device helper and source")
	}
	for _, id := range []string{identity.EnvironmentID, identity.OwnerID, identity.InstanceID, hostID} {
		if ids.Validate(id) != nil {
			return nil, errors.New("invalid Computer allocation identity")
		}
	}
	return &ComputerAllocationOwner{machines: machines, client: client, identity: identity, hostID: hostID, hostEpoch: hostEpoch, admitStart: admitStart, checkpointKeyUnavailable: checkpointKeyUnavailable, observe: observe, attachDevice: machines.prepareAllocationDevice}, nil
}

func (o *ComputerAllocationOwner) leaseRequest() workerapi.AgentComputerLeaseRequest {
	return workerapi.AgentComputerLeaseRequest{EnvironmentID: o.identity.EnvironmentID, ComputerID: o.identity.OwnerID, InstanceID: o.identity.InstanceID, LeaseEpoch: o.identity.Epoch}
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
	defer func() {
		cancel(context.Canceled)
		jobs.Wait()
		if o.closing {
			resultErr = errors.Join(resultErr, o.cleanup())
		}
	}()
	jobs.Go(func() { o.renew(ctx, cancel, delivery.ExpiresAt) })
	source, err := o.client.ComputerAllocationSource(ctx, o.identity)
	if err != nil {
		return err
	}
	defer source.Disk.Clear()
	if source.Disk.VersionID != delivery.BaseVersion {
		return errors.New("computer source differs from admitted base version")
	}
	shape := delivery.Shape
	if shape.CPUMillis <= 0 || shape.VCPUCount <= 0 || shape.VCPUCount > math.MaxInt32 || shape.VCPUCount > math.MaxInt64/1000 || shape.CPUMillis != shape.VCPUCount*1000 || shape.MemoryBytes <= 0 || shape.MemoryBytes%mebibyte != 0 || shape.ScratchBytes <= 0 || shape.ScratchBytes%mebibyte != 0 {
		return errors.New("invalid physical Computer allocation shape")
	}
	request, err := instanceReservationVectorWithProjection(shape.CPUMillis, shape.MemoryBytes/mebibyte, shape.ScratchBytes/mebibyte, source.Disk.Root.LogicalBytes)
	if err != nil {
		return err
	}
	if request.HostDiskBytes > math.MaxInt64-o.machines.ComputerStagingBytes {
		return reservation.ErrOverflow
	}
	request.HostDiskBytes += o.machines.ComputerStagingBytes
	limits, err := checkpointStagingSize(vm.SnapshotLimits{ComputerBytes: source.Disk.Root.LogicalBytes, MemoryBytes: shape.MemoryBytes, ScratchBytes: shape.ScratchBytes, StateBytes: vm.SnapshotStateLimit, ConfigBytes: vm.SnapshotConfigLimit}, o.machines.CheckpointCipher)
	if err != nil {
		return err
	}
	state, err := o.machines.CheckpointCipher.EncryptedSize(vm.SnapshotStateLimit)
	if err != nil {
		return err
	}
	for _, extra := range []int64{limits.total, shape.MemoryBytes, state} {
		if request.HostDiskBytes > math.MaxInt64-extra {
			return reservation.ErrOverflow
		}
		request.HostDiskBytes += extra
	}

	if err := waitAllocationCapacity(ctx, o.admitStart, o.machines.Reservations, instanceReservationKey(o.identity.InstanceID, o.identity.Epoch), request); err != nil {
		if errors.Is(err, ErrCheckpointKeyUnavailable) {
			o.closing = true
		}
		return err
	}
	// From this point the owner can retain filesystem/device/VM effects. Every
	// later invocation only retries physical cleanup; this instance never boots twice.
	o.closing = true
	device, err := o.attachDevice(ctx, o.identity, source)
	// The local disk takes its own key copies. Do not retain the transport's
	// plaintext for the potentially long lifetime of this Computer.
	source.Disk.Clear()
	if err != nil {
		return err
	}
	if delivery.RestoredFrom != "" {
		err = o.restore(ctx, delivery, source, device)
	} else {
		o.machine, err = o.machines.Backend.Materialize(ctx, vm.MaterializeRequest{ID: o.identity.InstanceID, OwnerKind: vm.OwnerInstance, Binding: vm.WorkloadBinding{WorkerEpoch: o.hostEpoch, OwnerID: o.identity.InstanceID, Generation: 1, ComputerInstanceID: o.identity.InstanceID, VMPlatformID: shape.VMPlatformID}, RootfsDigest: source.RootfsDigest, ComputerMountPath: "/workspace", BaseComputerDiskVersionID: delivery.BaseVersion, Resources: vm.Resources{MilliCPU: shape.CPUMillis, MemoryMiB: shape.MemoryBytes / mebibyte, DiskMiB: shape.ScratchBytes / mebibyte, Slots: 1}, VMVCPUCount: int32(shape.VCPUCount), CPUConfigDigest: shape.CPUConfigDigest, Topology: vm.Topology{Computer: &vm.ComputerDisk{ComputerID: o.identity.OwnerID, VersionID: delivery.BaseVersion, SizeBytes: source.Disk.Root.LogicalBytes, Device: device}}})
	}
	if err != nil {
		return err
	}
	if o.machine == nil {
		return errors.New("computer backend returned no machine")
	}
	jobs.Go(func() {
		err := o.machine.Wait(ctx)
		if err == nil {
			err = errors.New("computer VM exited")
		}
		cancel(err)
	})
	if delivery.RestoredFrom != "" {
		if err := o.activateRestore(ctx, delivery); err != nil {
			return err
		}
	} else {
		if err := prepareAllocationImage(ctx, o.machine, delivery.Identity, delivery.ChannelCredential, delivery.BaseVersion, source.ImageConfig); err != nil {
			return err
		}
		for {
			requestCtx, requestCancel := context.WithTimeout(ctx, 10*time.Second)
			err = o.client.ObserveFreshComputerReady(requestCtx, workerapi.ComputerAllocationReady{Identity: o.identity, Shape: shape, BaseVersion: delivery.BaseVersion})
			requestCancel()
			if err == nil {
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
		authority := commandAuthority{EnvironmentID: o.identity.EnvironmentID, ComputerID: o.identity.OwnerID, ComputerInstanceID: o.identity.InstanceID, WriterGeneration: o.identity.Epoch, GuestChannelCredential: delivery.ChannelCredential}
		cancel((commandService{LogLimits: o.machines.CommandLogLimits}).Serve(ctx, o.machine, authority, o.client))
	})
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
			err := o.machines.ServeAllocatedSessions(sessionCtx, o.machine, delivery, o.hostID, o.client, o.observe)
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
	o.saves = &agentComputerSaves{client: o.client, objects: o.machines.ComputerObjects, identity: o.identity, machine: live, every: o.machines.ComputerSaveEvery,
		idle: func(ctx context.Context) error { return o.captureIdle(ctx, delivery, suspend) }}
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
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	var closeErr error
	if o.machine != nil {
		closeErr = o.machine.Close(ctx)
	}
	// A machine may cache a failed Close result. Exact backend reconciliation
	// retries physical absence independently before releasing device custody.
	if err := o.machines.Backend.Cleanup(ctx, vm.Owner{Kind: vm.OwnerInstance, ID: o.identity.InstanceID}); err != nil {
		return fmt.Errorf("prove allocated Computer absent: %w", errors.Join(closeErr, err))
	}
	// Stop device I/O before reporting physical absence, but retain its arena and
	// funded capacity until the CP acknowledges physical stop and save disposition.
	// The closed arena is retained custody, not a resumable publication owner.
	if err := o.machines.releaseComputerDevice(o.identity.InstanceID, o.identity.Epoch); err != nil {
		return err
	}
	if err := o.client.ObserveAgentComputerStopped(ctx, workerapi.AgentComputerStoppedRequest{AgentComputerLeaseRequest: o.leaseRequest(), InvalidCheckpointID: o.invalidCheckpointID}); err != nil {
		return err
	}
	if o.saves != nil {
		o.saves.release()
	}
	if o.checkpointCut != nil {
		o.checkpointCut.Release()
		o.checkpointCut = nil
	}
	if err := o.machines.releaseInstanceCapacity(o.identity.InstanceID, o.identity.Epoch); err != nil {
		return err
	}
	o.finished.Store(true)
	return nil
}
