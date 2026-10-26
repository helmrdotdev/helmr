package computerhost

import (
	"context"
	"crypto/rand"
	"errors"
	"fmt"
	"github.com/helmrdotdev/helmr/internal/disk"
	"github.com/helmrdotdev/helmr/internal/networkpolicy"
	"github.com/helmrdotdev/helmr/internal/vm"
	"github.com/helmrdotdev/helmr/internal/workerapi"
	"os"
	"time"
	"uuid"
)

type preparationQualification struct {
	identity workerapi.AllocationIdentity
	machine  vm.CheckpointableMachine
}

// Qualification borrows the still-live preparation reservation after its source
// VM is absent. Its fresh physical identity is never a replay of that VM, and
// all clone writes are disposable. Only the captured immutable root is published.
func (o *PreparationAllocationOwner) qualifyPreparation(ctx context.Context, start workerapi.PreparationStart, shape workerapi.AllocationShape, root disk.VersionRoot, key workerapi.ComputerKeyMaterial) error {
	o.failureCode = "preparation_qualification_unavailable"
	// Release captured raw/staging bytes before allocating the clone; preserve
	// the already verified Runtime snapshot for the fixed probe.
	if err := o.capture.Close(); err != nil {
		return err
	}
	o.capture = nil
	raw := o.stage.disk.Name()
	if err := o.stage.disk.Close(); err != nil {
		return err
	}
	o.stage.disk = nil
	if err := os.Remove(raw); err != nil {
		return err
	}
	version, err := root.Digest()
	if err != nil {
		return err
	}
	// Already-reserved resources are reused, but every new physical launch
	// still observes current host health. Renew the original grant while waiting.
	waitCtx, stopWait := context.WithTimeout(ctx, time.Minute)
	defer stopWait()
	for {
		wake := o.workWake.snapshot()
		if waitCtx.Err() != nil {
			return context.Cause(waitCtx)
		}
		err := o.admitStart(waitCtx)
		if err == nil {
			break
		}
		if errors.Is(err, ErrCheckpointKeyUnavailable) {
			return err
		}
		if waitErr := waitAllocationWake(waitCtx, wake, 15*time.Second); waitErr != nil {
			return fmt.Errorf("qualification waited for host health: %w", errors.Join(err, waitErr))
		}
	}
	stopWait()
	ctx, cancel := context.WithTimeout(ctx, time.Minute)
	defer cancel()
	identity := o.identity
	identity.AllocationID = uuid.NewV7().String()
	o.qualification = &preparationQualification{identity: identity}
	source := workerapi.ComputerAllocationSource{Disk: workerapi.ComputerSourceMaterial{Root: root, VersionID: version, Keys: []workerapi.ComputerKeyMaterial{key}, WriteKeyID: key.ID}}
	device, err := o.attachQualification(ctx, identity, source)
	if err != nil {
		return err
	}
	var runtimeDrives []vm.ReadOnlyDrive
	for _, drive := range o.stage.drives {
		if drive.ID == vm.ProgramRuntimeDrive {
			runtimeDrives = append(runtimeDrives, drive)
		}
	}
	o.qualification.machine, err = o.machines.Backend.Materialize(ctx, vm.MaterializeRequest{NetworkPhase: networkpolicy.PhaseQualification, NetworkPolicy: networkpolicy.BlockedPolicy(), NetworkInstallation: start.NetworkInstallation,
		ID: identity.AllocationID, OwnerKind: vm.OwnerInstance, Purpose: vm.MaterializePreparationQualification,
		Binding:      vm.WorkloadBinding{WorkerEpoch: o.hostEpoch, OwnerID: identity.AllocationID, Generation: 1, ComputerInstanceID: identity.AllocationID, VMPlatformID: shape.VMPlatformID},
		RootfsDigest: start.RootfsDigest, ComputerMountPath: "/workspace", BaseComputerDiskVersionID: version,
		Resources:   vm.Resources{MilliCPU: shape.CPUMillis, MemoryMiB: shape.MemoryBytes / mebibyte, DiskMiB: shape.ScratchBytes / mebibyte, Slots: 1},
		VMVCPUCount: int32(shape.VCPUCount), CPUConfigDigest: shape.CPUConfigDigest, ReadOnlyDrives: runtimeDrives,
		Topology: vm.Topology{Computer: &vm.ComputerDisk{ComputerID: identity.OwnerID, Device: device, VersionID: version, SizeBytes: root.LogicalBytes}},
	})
	if err != nil {
		return err
	}
	if o.qualification.machine == nil {
		return errors.New("qualification backend returned no machine")
	}
	credential := make([]byte, 32)
	if _, err := rand.Read(credential); err != nil {
		return err
	}
	defer clear(credential)
	if err := o.runPreparationOn(ctx, o.qualification.machine, identity, credential, start, version, true); err != nil {
		return err
	}
	cleanupCtx, stopCleanup := context.WithTimeout(context.Background(), 30*time.Second)
	defer stopCleanup()
	return o.cleanupQualification(cleanupCtx)
}

// A failed physical/device cleanup retains the exact owner and the original
// reservation for cleanup-only retries. No prepared image is published first.
func (o *PreparationAllocationOwner) cleanupQualification(ctx context.Context) error {
	q := o.qualification
	if q == nil {
		return nil
	}
	var closeErr error
	if q.machine != nil {
		closeErr = q.machine.Close(ctx)
	}
	if err := o.machines.Backend.Cleanup(ctx, vm.Owner{Kind: vm.OwnerInstance, ID: q.identity.AllocationID}); err != nil {
		return errors.Join(closeErr, err)
	}
	if err := o.machines.releaseComputerDevice(q.identity.AllocationID, q.identity.Epoch); err != nil {
		return err
	}
	if err := os.RemoveAll(o.machines.computerPreparationDirectory(q.identity.AllocationID, q.identity.Epoch)); err != nil {
		return err
	}
	o.machines.mu.Lock()
	delete(o.machines.computerDevices, preparedMachineRef{id: q.identity.AllocationID, epoch: q.identity.Epoch})
	o.machines.mu.Unlock()
	o.qualification = nil
	return nil
}
