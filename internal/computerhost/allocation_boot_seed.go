package computerhost

import (
	"context"
	"crypto/rand"
	"errors"
	"math"
	"os"
	"path/filepath"
	"sync"
	"sync/atomic"
	"time"
	"uuid"

	"github.com/helmrdotdev/helmr/internal/bootseed"
	"github.com/helmrdotdev/helmr/internal/cas"
	"github.com/helmrdotdev/helmr/internal/definition"
	"github.com/helmrdotdev/helmr/internal/httpclient"
	"github.com/helmrdotdev/helmr/internal/ids"
	computerv0 "github.com/helmrdotdev/helmr/internal/proto/computer/v0"
	"github.com/helmrdotdev/helmr/internal/reservation"
	"github.com/helmrdotdev/helmr/internal/vm"
	"github.com/helmrdotdev/helmr/internal/wire"
	"github.com/helmrdotdev/helmr/internal/workerapi"
)

type AllocatedBootSeedClient interface {
	DeliverBootSeedAllocation(context.Context, workerapi.AllocationIdentity) (workerapi.BootSeedAllocationDelivery, error)
	BootSeedStart(context.Context, workerapi.BootSeedExecutor) (workerapi.ComputerAllocationSource, error)
	RenewBootSeed(context.Context, workerapi.BootSeedExecutor) (workerapi.PreparationRenewal, error)
	PublishBootSeed(context.Context, workerapi.BootSeedPublication) error
	FailBootSeed(context.Context, workerapi.BootSeedFailure) error
	ObserveBootSeedStopped(context.Context, workerapi.AllocationIdentity) error
}

// BootSeedAllocationOwner owns one reservation and sequential private VMs. Every
// launch has a new physical identity; retries after startup only finish cleanup.
type BootSeedAllocationOwner struct {
	machines                     *PreparedMachines
	client                       AllocatedBootSeedClient
	identity                     workerapi.AllocationIdentity
	hostEpoch                    int64
	admitStart                   func(context.Context) error
	operation                    sync.Mutex
	finished                     atomic.Bool
	expires                      atomic.Int64
	workWake                     allocationWake
	closing, reserved, published bool
	executor                     workerapi.BootSeedExecutor
	active                       *preparationQualification
	builder                      vm.BootSeedCapture
	images                       *programSnapshots
}

func NewBootSeedAllocationOwner(machines *PreparedMachines, client AllocatedBootSeedClient, identity workerapi.AllocationIdentity, hostEpoch int64, admitStart func(context.Context) error) (*BootSeedAllocationOwner, error) {
	if machines == nil || machines.Backend == nil || machines.CAS == nil || machines.PlatformStore == nil || machines.ComputerObjects == nil || machines.ComputerRanges == nil || machines.CheckpointCipher == nil || machines.Reservations == nil || !filepath.IsAbs(machines.ComputerHelper) || len(machines.ComputerDevices) == 0 || !filepath.IsAbs(machines.TempDir) || machines.RuntimeArchitecture != definition.ArchitectureX8664 || machines.ComputerStagingBytes <= 0 || client == nil || admitStart == nil || identity.Kind != "seed" || identity.Epoch <= 0 || hostEpoch <= 0 {
		return nil, errors.New("boot seed requires physical backend, encrypted stores and capacity")
	}
	for _, id := range []string{identity.EnvironmentID, identity.OwnerID, identity.AllocationID} {
		if ids.Validate(id) != nil {
			return nil, errors.New("invalid boot seed allocation identity")
		}
	}
	if err := wire.ValidatePreparationLogLimits(machines.PreparationLogLimits); err != nil {
		return nil, err
	}
	return &BootSeedAllocationOwner{machines: machines, client: client, identity: identity, hostEpoch: hostEpoch, admitStart: admitStart}, nil
}
func (o *BootSeedAllocationOwner) Finished() bool { return o.finished.Load() }
func (o *BootSeedAllocationOwner) Run(ctx context.Context) (resultErr error) {
	if !o.operation.TryLock() {
		return errors.New("boot seed allocation already running")
	}
	defer o.operation.Unlock()
	if o.finished.Load() {
		return nil
	}
	if o.closing {
		return o.cleanup()
	}
	delivery, err := o.client.DeliverBootSeedAllocation(ctx, o.identity)
	if err != nil {
		var closed *httpclient.Error
		if errors.As(err, &closed) && closed.Code == workerapi.AllocationClosed {
			o.closing = true
			return o.cleanup()
		}
		return err
	}
	if delivery.Identity != o.identity || len(delivery.ChannelCredential) != 32 || !delivery.ExpiresAt.After(time.Now()) {
		clear(delivery.ChannelCredential)
		return errors.New("boot seed delivery differs or expired")
	}
	o.executor = workerapi.BootSeedExecutor{Identity: o.identity, ChannelCredential: delivery.ChannelCredential}
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
	jobs.Go(func() { o.renew(ctx, cancel) })
	source, err := o.client.BootSeedStart(ctx, o.executor)
	if err != nil {
		return err
	}
	defer source.Disk.Clear()
	shape := delivery.Shape
	if shape.CPUMillis <= 0 || shape.VCPUCount <= 0 || shape.VCPUCount > math.MaxInt32 || shape.CPUMillis != shape.VCPUCount*1000 || shape.MemoryBytes <= 0 || shape.MemoryBytes%mebibyte != 0 || shape.ScratchBytes <= 0 || shape.ScratchBytes%mebibyte != 0 {
		return errors.New("invalid boot seed resource shape")
	}
	program, err := bootProgram(source.BootProgram)
	if err != nil {
		return err
	}
	if program == nil {
		return errors.New("boot seed requires immutable Program")
	}
	capacity, err := instanceReservationVectorWithProjection(shape.CPUMillis, shape.MemoryBytes/mebibyte, shape.ScratchBytes/mebibyte, source.Disk.Root.LogicalBytes)
	if err != nil {
		return err
	}
	limits, err := checkpointStagingSize(vm.SnapshotLimits{ComputerBytes: source.Disk.Root.LogicalBytes, MemoryBytes: shape.MemoryBytes, ScratchBytes: shape.ScratchBytes, StateBytes: vm.SnapshotStateLimit, ConfigBytes: vm.SnapshotConfigLimit}, o.machines.CheckpointCipher)
	if err != nil {
		return err
	}
	// Ciphertext, retained raw RAM/scratch and the decrypted qualification source
	// coexist. Only one VM/device consumes the base reservation at a time.
	for _, extra := range []int64{o.machines.ComputerStagingBytes, limits.total, shape.MemoryBytes, shape.ScratchBytes, vm.SnapshotStateLimit, program.Size()} {
		if extra < 0 || capacity.HostDiskBytes > math.MaxInt64-extra {
			return reservation.ErrOverflow
		}
		capacity.HostDiskBytes += extra
	}
	if err := waitAllocationCapacity(ctx, o.admitStart, o.machines.Reservations, instanceReservationKey(o.identity.AllocationID, o.identity.Epoch), capacity, &o.workWake); err != nil {
		if errors.Is(err, ErrCheckpointKeyUnavailable) {
			o.closing = true
		}
		return err
	}
	o.reserved = true
	o.closing = true
	directory := o.machines.computerBootProgramDirectory(o.identity.AllocationID, o.identity.Epoch)
	if err := os.MkdirAll(directory, 0700); err != nil {
		return err
	}
	o.images, err = o.machines.prepareProgramArtifacts(ctx, directory, o.identity.AllocationID, source.BootProgram)
	if err != nil {
		return err
	}
	o.machines.seedProgramArtifacts(ctx, o.images, cas.Descriptor(program.Artifact))
	request, err := o.prepareVM(ctx, source, shape, bootProgramDrives(program, o.images))
	if err != nil {
		return err
	}
	o.builder, err = o.machines.Backend.PrepareBootSeed(ctx, request)
	if err != nil {
		return err
	}
	if o.builder == nil {
		return errors.New("boot seed backend returned no builder")
	}
	artifact, err := o.builder.Capture(ctx)
	if err != nil {
		return err
	}
	retained := filepath.Join(directory, "seed")
	if err := os.Mkdir(retained, 0700); err != nil {
		return err
	}
	for role, file := range map[string]*vm.SnapshotFile{"state": &artifact.VMState, "memory": &artifact.Memory, "scratch": &artifact.ScratchDisk} {
		path := filepath.Join(retained, role)
		if err := os.Link(file.Path, path); err != nil {
			return err
		}
		file.Path = path
	}
	if err := o.stopVM(ctx); err != nil {
		return err
	}
	env, _ := uuid.Parse(o.identity.EnvironmentID)
	build, _ := uuid.Parse(o.identity.OwnerID)
	manifest, err := o.machines.publishBootSeed(ctx, retained, env, build, artifact, shape.ScratchBytes)
	if err != nil {
		return err
	}
	// Publication has joined all file readers. Qualification uses remote encrypted
	// objects and independent device overlays, never the builder's local files.
	if err := os.RemoveAll(retained); err != nil {
		return err
	}
	var qualification bootseed.Qualification
	for i := range qualification.Clones {
		if err := o.admitStart(ctx); err != nil {
			return err
		}
		request, err = o.prepareVM(ctx, source, shape, bootProgramDrives(program, o.images))
		if err != nil {
			return err
		}
		request.Purpose = vm.MaterializePreparationQualification
		stage := filepath.Join(o.machines.computerPreparationDirectory(o.active.identity.AllocationID, o.active.identity.Epoch), "seed")
		if err := os.Mkdir(stage, 0700); err != nil {
			return err
		}
		request.BootSeed, err = o.machines.openBootSeed(ctx, stage, manifest, shape.MemoryBytes, shape.ScratchBytes)
		if err != nil {
			return err
		}
		o.active.machine, err = o.machines.Backend.Materialize(ctx, request)
		if err != nil {
			return err
		}
		if o.active.machine == nil {
			return errors.New("boot seed qualification returned no machine")
		}
		clone, ok := o.active.machine.(vm.BootSeedClone)
		if !ok {
			return errors.New("boot seed clone has no freshness evidence")
		}
		qualification.Clones[i] = clone.BootSeedIdentity()
		credential := make([]byte, 32)
		if _, err = rand.Read(credential); err != nil {
			return err
		}
		probeCtx, stop := context.WithTimeout(ctx, time.Minute)
		err = runPrivatePreparation(probeCtx, o.active.machine, o.active.identity, credential, source.ImageConfig, source.Disk.VersionID, &o.expires, &o.workWake, func(context.Context) (*computerv0.PreparationStart, error) {
			return &computerv0.PreparationStart{QualificationOnly: true, LogLimits: o.machines.PreparationLogLimits}, nil
		}, func(string) {})
		stop()
		clear(credential)
		if err != nil {
			return err
		}
		if err := o.stopVM(ctx); err != nil {
			return err
		}
	}
	if err := qualification.Validate(); err != nil {
		return err
	}
	if err := retryPreparation(ctx, func(ctx context.Context) error {
		return o.client.PublishBootSeed(ctx, workerapi.BootSeedPublication{Executor: o.executor, Manifest: manifest, Qualification: qualification})
	}); err != nil {
		return err
	}
	o.published = true
	return nil
}

func (o *BootSeedAllocationOwner) prepareVM(ctx context.Context, source workerapi.ComputerAllocationSource, shape workerapi.AllocationShape, drives []vm.ReadOnlyDrive) (vm.MaterializeRequest, error) {
	identity := o.identity
	identity.AllocationID = uuid.NewV7().String()
	o.active = &preparationQualification{identity: identity}
	if len(source.Disk.Keys) == 0 {
		return vm.MaterializeRequest{}, errors.New("boot seed has no read keys")
	}
	key := workerapi.ComputerKeyMaterial{Scope: source.Disk.Keys[0].Scope, ID: uuid.NewV7().String(), Key: make([]byte, 32)}
	defer clear(key.Key)
	if _, err := rand.Read(key.Key); err != nil {
		return vm.MaterializeRequest{}, err
	}
	source.Disk.Keys = append(append([]workerapi.ComputerKeyMaterial(nil), source.Disk.Keys...), key)
	source.Disk.WriteKeyID = key.ID
	device, err := o.machines.prepareAllocationDevice(ctx, identity, source)
	if err != nil {
		return vm.MaterializeRequest{}, err
	}
	return vm.MaterializeRequest{ID: identity.AllocationID, OwnerKind: vm.OwnerInstance, Binding: vm.WorkloadBinding{WorkerEpoch: o.hostEpoch, OwnerID: identity.AllocationID, Generation: 1, ComputerInstanceID: identity.AllocationID, VMPlatformID: shape.VMPlatformID}, RootfsDigest: source.RootfsDigest, ComputerMountPath: "/workspace", BaseComputerDiskVersionID: source.Disk.VersionID, Resources: vm.Resources{MilliCPU: shape.CPUMillis, MemoryMiB: shape.MemoryBytes / mebibyte, DiskMiB: shape.ScratchBytes / mebibyte, Slots: 1}, VMVCPUCount: int32(shape.VCPUCount), CPUConfigDigest: shape.CPUConfigDigest, ReadOnlyDrives: drives, Topology: vm.Topology{Computer: &vm.ComputerDisk{ComputerID: identity.OwnerID, Device: device, VersionID: source.Disk.VersionID, SizeBytes: source.Disk.Root.LogicalBytes}}}, nil
}
func (o *BootSeedAllocationOwner) stopVM(ctx context.Context) error {
	if o.active == nil {
		return nil
	}
	var closeErr error
	if o.builder != nil {
		closeErr = o.builder.Close(ctx)
	}
	if o.active.machine != nil {
		closeErr = errors.Join(closeErr, o.active.machine.Close(ctx))
	}
	if err := o.machines.Backend.Cleanup(ctx, vm.Owner{Kind: vm.OwnerInstance, ID: o.active.identity.AllocationID}); err != nil {
		return errors.Join(closeErr, err)
	}
	if err := o.machines.releaseComputerDevice(o.active.identity.AllocationID, o.active.identity.Epoch); err != nil {
		return err
	}
	if err := os.RemoveAll(o.machines.computerPreparationDirectory(o.active.identity.AllocationID, o.active.identity.Epoch)); err != nil {
		return err
	}
	o.machines.mu.Lock()
	delete(o.machines.computerDevices, preparedMachineRef{id: o.active.identity.AllocationID, epoch: o.active.identity.Epoch})
	o.machines.mu.Unlock()
	o.builder = nil
	o.active = nil
	return nil
}
func (o *BootSeedAllocationOwner) cleanup() error {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	if err := o.stopVM(ctx); err != nil {
		return err
	}
	if !o.published && len(o.executor.ChannelCredential) == 32 {
		if err := o.client.FailBootSeed(ctx, workerapi.BootSeedFailure{Executor: o.executor, Code: "boot_seed_build_failed"}); err != nil {
			return err
		}
	}
	if err := o.images.close(); err != nil {
		return err
	}
	o.images = nil
	if err := os.RemoveAll(o.machines.computerBootProgramDirectory(o.identity.AllocationID, o.identity.Epoch)); err != nil {
		return err
	}
	if o.reserved {
		if err := o.machines.Reservations.Release(instanceReservationKey(o.identity.AllocationID, o.identity.Epoch)); err != nil {
			return err
		}
		o.reserved = false
	}
	if err := o.client.ObserveBootSeedStopped(ctx, o.identity); err != nil {
		return err
	}
	o.finished.Store(true)
	return nil
}
func (o *BootSeedAllocationOwner) renew(ctx context.Context, cancel context.CancelCauseFunc) {
	for {
		expires := time.Unix(0, o.expires.Load())
		remaining := time.Until(expires)
		if remaining <= 0 {
			cancel(errors.New("boot seed allocation expired"))
			return
		}
		if sleepWithContext(ctx, min(10*time.Second, max(100*time.Millisecond, remaining/2), remaining)) != nil {
			return
		}
		callCtx, stop := context.WithDeadline(ctx, minTime(expires, time.Now().Add(5*time.Second)))
		result, err := o.client.RenewBootSeed(callCtx, o.executor)
		stop()
		if err == nil {
			if result.ExpiresAt.Before(expires) || !result.ExpiresAt.After(time.Now()) {
				cancel(errors.New("boot seed renewal regressed or expired"))
				return
			}
			o.expires.Store(result.ExpiresAt.UnixNano())
		} else if preparationAuthorityRejected(err) {
			cancel(err)
			return
		}
	}
}
