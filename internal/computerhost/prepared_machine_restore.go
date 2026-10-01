package computerhost

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"os"
	"path/filepath"
	"strconv"
	"strings"

	"github.com/helmrdotdev/helmr/internal/cas"
	computerv0 "github.com/helmrdotdev/helmr/internal/proto/computer/v0"
	"github.com/helmrdotdev/helmr/internal/reservation"
	"github.com/helmrdotdev/helmr/internal/vm"
	"github.com/helmrdotdev/helmr/internal/workerapi"
	"golang.org/x/sync/errgroup"
)

func (p *PreparedMachines) restorePreparedMachine(
	ctx context.Context,
	target workerapi.InstanceReconcileTarget,
	topology vm.Topology,
	readOnlyDrives []vm.ReadOnlyDrive,
	record func(vm.Phase),
) (result vm.Machine, retErr error) {
	restore := target.Source.Restore
	if restore == nil {
		return nil, errors.New("prepared machine restore authority is required")
	}
	checkpoint, err := validatePreparedMachineRestore(target, p.RuntimeArchitecture)
	if err != nil {
		return nil, err
	}
	if p.CAS == nil || p.CheckpointEncryptor == nil {
		return nil, errors.New("prepared machine restore CAS and encryption are required")
	}
	if p.Reservations == nil {
		return nil, errors.New("restore capacity ledger is required")
	}
	_, staging, err := p.checkpointRestoreCapacity(target)
	if err != nil {
		return nil, err
	}
	key := restoreStagingKey(target.ID, target.WorkerEpoch)
	if p.Reservations.Snapshot().Reservations[key].GuestEphemeralDiskBytes != staging {
		return nil, errors.New("checkpoint restore staging was not reserved")
	}
	directory := p.restorePreparationDirectory(target.ID, target.WorkerEpoch)
	if err := os.Mkdir(directory, 0700); err != nil {
		return nil, err
	}
	defer func() {
		cleanupErr := os.RemoveAll(directory)
		if cleanupErr == nil {
			cleanupErr = p.Reservations.Release(key)
		}
		if cleanupErr != nil {
			if result != nil {
				cleanupErr = errors.Join(cleanupErr, p.closeMachine(ctx, result))
				result = nil
			}
			retErr = errors.Join(retErr, cleanupErr)
		}
	}()
	runtimeState := checkpoint.RuntimeState
	paths := make([]string, 4)
	group, groupCtx := errgroup.WithContext(ctx)
	artifacts := []struct {
		index  int
		value  workerapi.CheckpointArtifact
		suffix string
	}{
		{0, runtimeState.ConfigArtifact, "manifest"},
		{1, runtimeState.VMStateArtifact, "vmstate"},
		{2, runtimeState.MemoryArtifacts[0], "memory"},
		{3, runtimeState.ScratchDiskArtifact, "scratch-disk"},
	}
	for _, artifact := range artifacts {
		group.Go(func() error {
			path, err := materializeCheckpointObject(groupCtx, p.CAS, p.CheckpointEncryptor, artifact.value, artifact.suffix, directory)
			if err != nil {
				return err
			}
			paths[artifact.index] = path
			return nil
		})
	}
	if err := group.Wait(); err != nil {
		return nil, err
	}
	manifest, err := os.ReadFile(paths[0])
	if err != nil {
		return nil, fmt.Errorf("read restored runtime manifest: %w", err)
	}
	runtimeInfo := checkpoint.RecoveryPoint.Runtime
	machine, err := p.Backend.Restore(ctx, vm.RestoreRequest{
		ID: restore.CheckpointID, ComputerInstanceID: target.ID, OwnerKind: vm.OwnerInstance,
		Resources: vm.Resources{MilliCPU: int64(target.Source.ReservedCPUMillis), MemoryMiB: int64(target.Source.ReservedMemoryMiB), DiskMiB: target.Source.ReservedDiskMiB, Slots: target.Source.ReservedExecutionSlots},
		Binding:   instanceTargetWorkloadBinding(target),
		VMState:   paths[1], VMStateMediaType: runtimeState.VMStateArtifact.MediaType,
		Memory: []string{paths[2]}, MemoryMediaTypes: []string{runtimeState.MemoryArtifacts[0].MediaType},
		ScratchDisk: paths[3], ScratchDiskMediaType: runtimeState.ScratchDiskArtifact.MediaType,
		Manifest: manifest,
		Checkpoint: vm.CheckpointIdentity{
			RuntimeBackend: runtimeInfo.Backend, RuntimeID: runtimeInfo.ID,
			RuntimeArch: runtimeInfo.Arch, VMRuntimeContract: runtimeInfo.Contract,
			KernelDigest: runtimeInfo.KernelDigest, InitramfsDigest: runtimeInfo.InitramfsDigest,
			RootfsDigest: runtimeInfo.RootfsDigest, VMConfigDigest: runtimeInfo.ConfigDigest,
			VMVCPUCount: runtimeInfo.VMVCPUCount, CPUConfigDigest: runtimeInfo.CPUConfigDigest,
		},
		Topology: topology, ReadOnlyDrives: readOnlyDrives,
		RecordPhase: record,
	})
	if err != nil {
		return nil, err
	}
	point := checkpoint.RecoveryPoint
	identity := &computerv0.ComputerRestoreIdentity{ComputerId: point.ComputerID, SourceComputerInstanceId: point.ComputerInstanceID, WriterGeneration: point.WriterGeneration, CheckpointId: point.ID}
	for _, member := range point.Runs {
		identity.Runs = append(identity.Runs, &computerv0.CapturedRun{RunId: member.RunID, AttemptNumber: uint32(member.AttemptNumber), RunWaitId: member.RunWaitID, RunLeaseId: member.RunLeaseID, CorrelationId: member.CorrelationID})
	}
	verify := &computerv0.VerifyComputerRestoreRequest{Identity: identity}
	err = guestControl{machine: machine}.verifyRestore(ctx, verify)
	if err != nil {
		return nil, errors.Join(fmt.Errorf("verify restored frozen Computer: %w", err), p.closeMachine(ctx, machine))
	}

	return machine, nil
}

func restoreStagingKey(id string, epoch int64) reservation.Key {
	return reservation.Key{Kind: "checkpoint-restore", ID: id, Epoch: epoch}
}
func (p *PreparedMachines) restorePreparationDirectory(id string, epoch int64) string {
	root := strings.TrimSpace(p.TempDir)
	if root == "" {
		root = os.TempDir()
	}
	return filepath.Join(root, "restore-"+id+"-"+strconv.FormatInt(epoch, 10))
}

// Retain raw RAM and state with the instance; decrypted packed inputs live only
// through materialization. Ciphertext sizes safely bound their plaintext files.
func (p *PreparedMachines) checkpointRestoreCapacity(target workerapi.InstanceReconcileTarget) (retained, staging int64, err error) {
	restore := target.Source.Restore
	if restore == nil {
		return 0, 0, nil
	}
	if target.Source.ReservedMemoryMiB <= 0 {
		return 0, 0, errors.New("restore memory reservation is required")
	}
	retained = int64(target.Source.ReservedMemoryMiB) * mebibyte
	var checkpoint workerapi.CheckpointManifest
	if err = json.Unmarshal(restore.Manifest, &checkpoint); err != nil {
		return 0, 0, err
	}
	if len(checkpoint.RuntimeState.MemoryArtifacts) != 1 {
		return 0, 0, errors.New("restore requires one memory artifact")
	}
	artifacts := []workerapi.CheckpointArtifact{checkpoint.RuntimeState.ConfigArtifact, checkpoint.RuntimeState.VMStateArtifact, checkpoint.RuntimeState.ScratchDiskArtifact, checkpoint.RuntimeState.MemoryArtifacts[0]}
	for _, artifact := range artifacts {
		if err = cas.ValidateDescriptor(cas.Descriptor{Digest: artifact.Digest, SizeBytes: artifact.SizeBytes, MediaType: artifact.MediaType}); err != nil {
			return 0, 0, err
		}
		if artifact.SizeBytes >= math.MaxInt64-staging {
			return 0, 0, reservation.ErrOverflow
		}
		staging += artifact.SizeBytes
	}
	configLimit, err := p.CheckpointEncryptor.EncryptedSize(64 << 10)
	if err != nil {
		return 0, 0, err
	}
	if checkpoint.RuntimeState.ConfigArtifact.SizeBytes > configLimit {
		return 0, 0, errors.New("restore config artifact exceeds supported size")
	}
	state := checkpoint.RuntimeState.VMStateArtifact.SizeBytes
	if state > math.MaxInt64-retained {
		return 0, 0, reservation.ErrOverflow
	}
	retained += state
	return retained, staging, nil
}
