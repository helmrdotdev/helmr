package computerhost

import (
	"bytes"
	"context"
	"errors"
	"log/slog"
	"os"
	"path/filepath"
	"time"

	"github.com/helmrdotdev/helmr/internal/cas"
	"github.com/helmrdotdev/helmr/internal/computercheckpoint"
	"github.com/helmrdotdev/helmr/internal/filepack"
	"github.com/helmrdotdev/helmr/internal/vm"
	"github.com/helmrdotdev/helmr/internal/workerapi"
	"golang.org/x/sync/errgroup"
)

func (o *ComputerAllocationOwner) restore(ctx context.Context, delivery workerapi.ComputerAllocationDelivery, source workerapi.ComputerAllocationSource, device vm.ComputerDevice) (resultErr error) {
	var m computercheckpoint.Manifest
	var err error
	if o.checkpoint != nil {
		m = *o.checkpoint
	} else {
		m, err = o.client.ReadAgentCheckpoint(ctx, o.identity)
	}
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
	directory := filepath.Join(o.machines.computerPreparationDirectory(o.identity.AllocationID, o.identity.Epoch), "checkpoint")
	if err := os.MkdirAll(directory, 0700); err != nil {
		return err
	}
	limits, err := checkpointStagingSize(vm.SnapshotLimits{ComputerBytes: source.Disk.Root.LogicalBytes, MemoryBytes: delivery.Shape.MemoryBytes, ScratchBytes: delivery.Shape.ScratchBytes, StateBytes: vm.SnapshotStateLimit, ConfigBytes: vm.SnapshotConfigLimit}, o.machines.CheckpointCipher)
	if err != nil {
		return err
	}
	objects := m.Objects()
	stagedPaths := make([]string, len(objects))
	stageErrors := make([]error, len(objects))
	group, stageCtx := errgroup.WithContext(ctx)
	// The reservation already covers every ciphertext and plaintext together.
	// Overlap two bounded transfers; join them before VM creation or cleanup.
	group.SetLimit(2)
	var images *programSnapshots
	var memory *checkpointMemorySource
	defer func() { resultErr = errors.Join(resultErr, images.close()) }()
	if m.BootProgram != nil {
		group.Go(func() error {
			var err error
			images, err = o.machines.restoreBootProgram(stageCtx, directory, m.BootProgram)
			return err
		})
	}
	for i, object := range objects {
		queuedAt := time.Now()
		group.Go(func() error {
			slog.Info("Computer restore staging admitted", "started_at", queuedAt.UTC().Format(time.RFC3339Nano), "computer_id", o.identity.OwnerID, "allocation_id", o.identity.AllocationID, "lease_epoch", o.identity.Epoch, "role", object.Role, "queue_ms", float64(time.Since(queuedAt))/float64(time.Millisecond))
			if err := stageCtx.Err(); err != nil {
				stageErrors[i] = err
				return err
			}
			if object.Role == "memory" {
				started := time.Now()
				defer func() {
					slog.Info("Computer memory source opened", "allocation_id", o.identity.AllocationID, "lease_epoch", o.identity.Epoch, "started_at", started.UTC().Format(time.RFC3339Nano), "duration_ms", float64(time.Since(started))/float64(time.Millisecond))
				}()
				descriptor := cas.Descriptor{Digest: object.Digest, SizeBytes: object.SizeBytes, MediaType: object.MediaType}
				observed, err := o.machines.ComputerObjects.Stat(stageCtx, object.Digest)
				if err != nil {
					err = checkpointObjectReadError(err)
				} else if mismatch := cas.RequireExact(observed, descriptor); mismatch != nil {
					err = invalidCheckpoint(mismatch)
				}
				if err == nil {
					memory, err = openCheckpointMemory(stageCtx, o.machines.ComputerRanges, o.machines.CheckpointCipher, descriptor, m.MemoryHeaderDigest, o.identity.EnvironmentID+"/"+o.identity.OwnerID+"/"+m.CheckpointID.String()+"/memory", delivery.Shape.MemoryBytes)
				}
				stageErrors[i] = err
				return err
			}
			limit := map[string]int64{"vm_config": limits.config, "vm_state": limits.state, "memory": limits.memory, "scratch_disk": limits.scratch}[object.Role]
			started := time.Now()
			path, err := downloadCheckpointObject(stageCtx, o.machines.ComputerObjects, o.machines.CheckpointCipher, directory, m.CheckpointID.String(), object, limit)
			slog.Info("Computer checkpoint object staged", "started_at", started.UTC().Format(time.RFC3339Nano), "computer_id", o.identity.OwnerID, "allocation_id", o.identity.AllocationID, "lease_epoch", o.identity.Epoch, "role", object.Role, "duration_ms", time.Since(started).Milliseconds(), "error_class", vm.RuntimeErrorClass(err))
			stagedPaths[i], stageErrors[i] = path, err
			return err
		})
	}
	if stageErr := group.Wait(); stageErr != nil {
		// Preserve every observed failure identity, even if another transfer's
		// cancellation wins the group's first-error race.
		err := errors.Join(stageErr, errors.Join(stageErrors...))
		if errors.Is(err, ErrCheckpointKeyUnavailable) {
			// Pause before cleanup can release this allocation for redispatch.
			o.checkpointKeyUnavailable()
			slog.Error("Worker checkpoint key differs from retained Computer", "checkpoint_id", m.CheckpointID, "computer_id", o.identity.OwnerID, "error", err)
		}
		return err
	}
	paths := make(map[string]string, len(objects))
	for i, object := range objects {
		paths[object.Role] = stagedPaths[i]
	}
	config, err := os.ReadFile(paths["vm_config"])
	if err != nil {
		return err
	}
	if !bytes.Equal(config, m.Config) {
		return invalidCheckpoint(errors.New("encrypted checkpoint config differs from manifest"))
	}
	o.restoreMemory = memory
	o.machine, err = o.machines.Backend.Restore(ctx, vm.RestoreRequest{RecordPhase: func(phase vm.Phase) { o.recordVMPhase("restore", phase) }, ID: m.CheckpointID.String(), ComputerInstanceID: o.identity.AllocationID, OwnerKind: vm.OwnerInstance, Binding: vm.WorkloadBinding{WorkerEpoch: o.hostEpoch, OwnerID: o.identity.AllocationID, Generation: 1, ComputerInstanceID: o.identity.AllocationID, VMPlatformID: delivery.Shape.VMPlatformID}, Resources: vm.Resources{MilliCPU: delivery.Shape.CPUMillis, MemoryMiB: delivery.Shape.MemoryBytes / mebibyte, DiskMiB: delivery.Shape.ScratchBytes / mebibyte, Slots: 1}, VMState: paths["vm_state"], VMStateMediaType: m.VMState.MediaType, ScratchDisk: paths["scratch_disk"], ScratchDiskMediaType: m.ScratchDisk.MediaType, MemorySource: memory, Manifest: config, ReadOnlyDrives: bootProgramDrives(m.BootProgram, images), Checkpoint: m.Runtime, Topology: vm.Topology{Computer: &vm.ComputerDisk{ComputerID: o.identity.OwnerID, VersionID: delivery.BaseVersion, SizeBytes: m.Disk.LogicalBytes, Device: device}}})
	if err != nil {
		if errors.Is(err, filepack.ErrInvalidContent) || errors.Is(err, vm.ErrInvalidCheckpointContent) {
			return invalidCheckpoint(err)
		}
		return err
	}
	if o.machine == nil {
		return errors.New("computer restore returned no machine")
	}
	// Restore retained its authenticated memory source and owned state files.
	// Packed inputs and config are consumed; do not retain a second packed
	// scratch copy across the next capture's accounted staging lifetime.
	for _, role := range []string{"scratch_disk", "vm_config"} {
		if err := os.Remove(paths[role]); err != nil {
			return err
		}
	}

	o.checkpoint = &m
	o.bootProgram = m.BootProgram
	return nil
}

func (o *ComputerAllocationOwner) activateRestore(ctx context.Context, delivery workerapi.ComputerAllocationDelivery) error {
	err := o.continueCheckpoint(ctx, delivery, nil, false)
	if errors.Is(err, errInvalidCheckpoint) {
		o.invalidCheckpointID = o.checkpoint.CheckpointID.String()
	}
	return err
}
