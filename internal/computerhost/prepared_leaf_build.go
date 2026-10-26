package computerhost

import (
	"context"
	"crypto/rand"
	"errors"
	"github.com/helmrdotdev/helmr/internal/networkpolicy"
	"math"
	"os"
	"path/filepath"
	"time"
	"uuid"

	"github.com/helmrdotdev/helmr/internal/vm"
	"github.com/helmrdotdev/helmr/internal/workerapi"
)

func (p *PreparedMachines) buildPreparedLeaf(leaf *preparedLeaf, envelope workerapi.PreparationSource) error {
	ctx := leaf.ctx
	defer envelope.Source.Disk.Clear()
	shape := envelope.Shape
	if shape.VCPUCount <= 0 || shape.VCPUCount > math.MaxInt32 || shape.CPUMillis != shape.VCPUCount*1000 || shape.MemoryBytes <= 0 || shape.MemoryBytes%mebibyte != 0 || shape.ScratchBytes <= 0 || shape.ScratchBytes%mebibyte != 0 {
		return errors.New("invalid prepared Computer shape")
	}
	program, err := bootProgram(envelope.Source.BootProgram)
	if err != nil {
		return err
	}
	if program == nil {
		return errors.New("prepared Computer requires its qualified Program")
	}
	required, err := p.computerReservation(shape, envelope.Source, program)
	if err != nil {
		return err
	}
	if required.CPUMillis > p.HostSlot.CPUMillis || required.MemoryBytes > p.HostSlot.MemoryBytes || required.HostDiskBytes > p.HostSlot.HostDiskBytes || required.VMSlots > p.HostSlot.VMSlots {
		return errors.New("prepared Computer exceeds its reserved full shape")
	}
	leaf.shape, leaf.manifest, leaf.source, leaf.program = shape, envelope.Manifest, envelope.Source, program
	if len(envelope.Source.Disk.Keys) == 0 {
		return errors.New("prepared Computer has no image keys")
	}
	source := envelope.Source
	key := workerapi.ComputerKeyMaterial{Scope: source.Disk.Keys[0].Scope, ID: uuid.NewV7().String(), Key: make([]byte, 32)}
	defer clear(key.Key)
	if _, err := rand.Read(key.Key); err != nil {
		return err
	}
	source.Disk.Keys = append(append([]workerapi.ComputerKeyMaterial(nil), source.Disk.Keys...), key)
	source.Disk.WriteKeyID = key.ID
	leaf.device, err = p.prepareAllocationDeviceFrom(ctx, leaf.identity, source, p.OptionalComputerRanges)
	if err != nil {
		return err
	}
	directory := p.computerBootProgramDirectory(leaf.identity.AllocationID, leaf.identity.Epoch)
	if err := os.MkdirAll(directory, 0700); err != nil {
		return err
	}
	leaf.images, err = p.prepareExecutableArtifacts(ctx, directory, leaf.identity.AllocationID, source.BootProgram, false, nil, true)
	if err != nil {
		return err
	}
	stage := filepath.Join(p.computerPreparationDirectory(leaf.identity.AllocationID, leaf.identity.Epoch), "boot-seed")
	if err := os.MkdirAll(stage, 0700); err != nil {
		return err
	}
	seed, err := p.openPreparedBootSeed(ctx, stage, envelope.Manifest, shape.MemoryBytes, shape.ScratchBytes)
	if err != nil {
		return err
	}
	request := vm.MaterializeRequest{NetworkPhase: networkpolicy.PhaseBootSeed, NetworkPolicy: networkpolicy.BlockedPolicy(), NetworkInstallation: source.NetworkInstallation, ID: leaf.identity.AllocationID, OwnerKind: vm.OwnerInstance, Binding: vm.WorkloadBinding{WorkerEpoch: leaf.identity.Epoch, OwnerID: leaf.identity.AllocationID, Generation: 1, ComputerInstanceID: leaf.identity.AllocationID, VMPlatformID: shape.VMPlatformID}, RootfsDigest: source.RootfsDigest, ComputerMountPath: "/workspace", BaseComputerDiskVersionID: source.Disk.VersionID, Resources: vm.Resources{MilliCPU: shape.CPUMillis, MemoryMiB: shape.MemoryBytes / mebibyte, DiskMiB: shape.ScratchBytes / mebibyte, Slots: 1}, VMVCPUCount: int32(shape.VCPUCount), CPUConfigDigest: shape.CPUConfigDigest, ReadOnlyDrives: bootProgramDrives(program, leaf.images), Topology: vm.Topology{Computer: &vm.ComputerDisk{ComputerID: leaf.identity.OwnerID, VersionID: source.Disk.VersionID, SizeBytes: source.Disk.Root.LogicalBytes, Device: leaf.device}}, BootSeed: seed}
	leaf.machine, err = p.Backend.(vm.PreparedComputerBackend).PrepareComputer(ctx, request)
	if err != nil {
		return err
	}
	if leaf.machine == nil {
		return errors.New("prepared backend returned no machine")
	}
	if err := leaf.images.close(); err != nil {
		return err
	}
	leaf.images = nil
	if err := os.RemoveAll(directory); err != nil {
		return err
	}
	return os.Remove(seed.ScratchDisk)
}

func (p *PreparedMachines) cleanupPreparedLeaf(leaf *preparedLeaf) error {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	var closeErr error
	if leaf.machine != nil {
		closeErr = leaf.machine.Close(ctx)
	}
	if err := p.Backend.Cleanup(ctx, vm.Owner{Kind: vm.OwnerInstance, ID: leaf.identity.AllocationID}); err != nil {
		return errors.Join(closeErr, err)
	}
	if err := leaf.images.close(); err != nil {
		return err
	}
	leaf.images = nil
	if err := p.releaseInstanceResources(leaf.identity.AllocationID, leaf.identity.Epoch); err != nil {
		return err
	}
	return p.Reservations.Release(leaf.reservationKey())
}
