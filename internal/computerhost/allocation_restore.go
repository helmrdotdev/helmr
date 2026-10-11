package computerhost

import (
	"bytes"
	"context"
	"errors"
	"log/slog"
	"os"
	"path/filepath"

	"github.com/helmrdotdev/helmr/internal/filepack"
	"github.com/helmrdotdev/helmr/internal/vm"
	"github.com/helmrdotdev/helmr/internal/workerapi"
)

func (o *ComputerAllocationOwner) restore(ctx context.Context, delivery workerapi.ComputerAllocationDelivery, source workerapi.ComputerAllocationSource, device vm.ComputerDevice) (resultErr error) {
	m, err := o.client.ReadAgentCheckpoint(ctx, o.identity)
	if err != nil {
		return err
	}

	defer func() {
		if errors.Is(resultErr, errInvalidCheckpoint) {
			o.invalidCheckpointID = m.CheckpointID.String()
		}
	}()
	if _, err := m.Encode(); err != nil {
		return invalidCheckpoint(err)
	}

	expectedDisk, err := source.Disk.Root.Digest()
	if err != nil {
		return err
	}
	capturedDisk, err := m.Disk.Digest()
	if err != nil {
		return err
	}
	if m.ComputerID.String() != o.identity.OwnerID || m.LeaseEpoch >= o.identity.Epoch || expectedDisk != capturedDisk || m.Runtime.RuntimeID != delivery.Shape.VMPlatformID || int64(m.Runtime.VMVCPUCount) != delivery.Shape.VCPUCount || m.Runtime.CPUConfigDigest != delivery.Shape.CPUConfigDigest {
		return errors.New("checkpoint differs from admitted Computer allocation")
	}
	directory := filepath.Join(o.machines.computerPreparationDirectory(o.identity.InstanceID, o.identity.Epoch), "checkpoint")
	if err := os.MkdirAll(directory, 0700); err != nil {
		return err
	}
	limits, err := checkpointStagingSize(vm.SnapshotLimits{ComputerBytes: source.Disk.Root.LogicalBytes, MemoryBytes: delivery.Shape.MemoryBytes, ScratchBytes: delivery.Shape.ScratchBytes, StateBytes: vm.SnapshotStateLimit, ConfigBytes: vm.SnapshotConfigLimit}, o.machines.CheckpointCipher)
	if err != nil {
		return err
	}
	paths := make(map[string]string)
	for _, object := range m.Objects() {
		limit := map[string]int64{"vm_config": limits.config, "vm_state": limits.state, "memory": limits.memory, "scratch_disk": limits.scratch}[object.Role]
		path, err := downloadCheckpointObject(ctx, o.machines.ComputerObjects, o.machines.CheckpointCipher, directory, m.CheckpointID.String(), object, limit)
		if err != nil {
			if errors.Is(err, ErrCheckpointKeyUnavailable) {
				// Pause before cleanup can release this allocation for redispatch.
				o.checkpointKeyUnavailable()
				slog.Error("Worker checkpoint key differs from retained Computer", "checkpoint_id", m.CheckpointID, "computer_id", o.identity.OwnerID, "error", err)
			}
			return err
		}
		paths[object.Role] = path
	}
	config, err := os.ReadFile(paths["vm_config"])
	if err != nil {
		return err
	}
	if !bytes.Equal(config, m.Config) {
		return invalidCheckpoint(errors.New("encrypted checkpoint config differs from manifest"))
	}
	o.machine, err = o.machines.Backend.Restore(ctx, vm.RestoreRequest{ID: m.CheckpointID.String(), ComputerInstanceID: o.identity.InstanceID, OwnerKind: vm.OwnerInstance, Binding: vm.WorkloadBinding{WorkerEpoch: o.hostEpoch, OwnerID: o.identity.InstanceID, Generation: 1, ComputerInstanceID: o.identity.InstanceID, VMPlatformID: delivery.Shape.VMPlatformID}, Resources: vm.Resources{MilliCPU: delivery.Shape.CPUMillis, MemoryMiB: delivery.Shape.MemoryBytes / mebibyte, DiskMiB: delivery.Shape.ScratchBytes / mebibyte, Slots: 1}, VMState: paths["vm_state"], VMStateMediaType: m.VMState.MediaType, ScratchDisk: paths["scratch_disk"], ScratchDiskMediaType: m.ScratchDisk.MediaType, Memory: []string{paths["memory"]}, MemoryMediaTypes: []string{m.Memory.MediaType}, Manifest: config, Checkpoint: m.Runtime, Topology: vm.Topology{Computer: &vm.ComputerDisk{ComputerID: o.identity.OwnerID, VersionID: delivery.BaseVersion, SizeBytes: m.Disk.LogicalBytes, Device: device}}})
	if err != nil {
		if errors.Is(err, filepack.ErrInvalidContent) {
			return invalidCheckpoint(err)
		}
		return err
	}
	if o.machine == nil {
		return errors.New("computer restore returned no machine")
	}
	// Restore transferred raw memory/state into the backend's owned files.
	// Packed inputs and config are consumed; do not retain a second packed
	// RAM/scratch pair across the next capture's accounted staging lifetime.
	for _, role := range []string{"memory", "scratch_disk", "vm_config"} {
		if err := os.Remove(paths[role]); err != nil {
			return err
		}
	}

	o.checkpoint = &m
	return nil
}

func (o *ComputerAllocationOwner) activateRestore(ctx context.Context, delivery workerapi.ComputerAllocationDelivery) error {
	err := o.continueCheckpoint(ctx, delivery, nil, false)
	if errors.Is(err, errInvalidCheckpoint) {
		o.invalidCheckpointID = o.checkpoint.CheckpointID.String()
	}
	return err
}
