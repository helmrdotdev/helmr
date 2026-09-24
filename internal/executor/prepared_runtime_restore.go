package executor

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

	"github.com/helmrdotdev/helmr/internal/capacity"
	"github.com/helmrdotdev/helmr/internal/cas"
	"github.com/helmrdotdev/helmr/internal/compute"
	"github.com/helmrdotdev/helmr/internal/deployment"
	workspacev0 "github.com/helmrdotdev/helmr/internal/proto/workspace/v0"
	"github.com/helmrdotdev/helmr/internal/vm"
	"github.com/helmrdotdev/helmr/internal/workerapi"
	"golang.org/x/sync/errgroup"
)

func (p *PreparedRuntimePool) restorePreparedRuntime(
	ctx context.Context,
	target workerapi.RuntimeReconcileTarget,
	topology vm.RuntimeTopology,
	readOnlyDrives []vm.ReadOnlyDrive,
	record func(vm.RuntimePhase),
) (result vm.Session, retErr error) {
	restore := target.Source.Restore
	if restore == nil {
		return nil, errors.New("prepared runtime restore authority is required")
	}
	checkpoint, err := validatePreparedRuntimeRestore(target, p.RuntimeArchitecture)
	if err != nil {
		return nil, err
	}
	restoring, ok := p.Connector.(vm.RestoringConnector)
	if !ok {
		return nil, errors.New("connector does not support checkpoint restore")
	}
	if p.CAS == nil || p.CheckpointEncryptor == nil {
		return nil, errors.New("prepared runtime restore CAS and encryption are required")
	}
	if p.Capacity == nil {
		return nil, errors.New("restore capacity ledger is required")
	}
	_, staging, err := p.checkpointRestoreCapacity(target)
	if err != nil {
		return nil, err
	}
	key := restoreStagingKey(target.ID, target.WorkerEpoch)
	if p.Capacity.Snapshot().Reservations[key].GuestEphemeralDiskBytes != staging {
		return nil, errors.New("checkpoint restore staging was not reserved")
	}
	directory := p.restorePreparationDirectory(target.ID, target.WorkerEpoch)
	if err := os.Mkdir(directory, 0700); err != nil {
		return nil, err
	}
	defer func() {
		cleanupErr := os.RemoveAll(directory)
		if cleanupErr == nil {
			cleanupErr = p.Capacity.Release(key)
		}
		if cleanupErr != nil {
			if result != nil {
				cleanupErr = errors.Join(cleanupErr, p.closeSession(ctx, result))
				result = nil
			}
			retErr = errors.Join(retErr, cleanupErr)
		}
	}()
	runner := ProgramRunner{CAS: p.CAS, CheckpointEncryptor: p.CheckpointEncryptor, TempDir: p.TempDir}
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
			path, err := runner.materializeCheckpointObject(groupCtx, artifact.value, artifact.suffix, directory)
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
	session, err := restoring.Restore(ctx, vm.RestoreRequest{
		ID: restore.CheckpointID, RuntimeInstanceID: target.ID, OwnerKind: vm.OwnerRuntime,
		Resources: compute.ResourceVector{MilliCPU: int64(target.Source.ReservedCPUMillis), MemoryMiB: int64(target.Source.ReservedMemoryMiB), DiskMiB: target.Source.ReservedDiskMiB, Slots: target.Source.ReservedExecutionSlots},
		Binding:   runtimeTargetWorkloadBinding(target),
		VMState:   paths[1], VMStateMediaType: runtimeState.VMStateArtifact.MediaType,
		Memory: []string{paths[2]}, MemoryMediaTypes: []string{runtimeState.MemoryArtifacts[0].MediaType},
		ScratchDisk: paths[3], ScratchDiskMediaType: runtimeState.ScratchDiskArtifact.MediaType,
		Manifest: manifest,
		Checkpoint: vm.CheckpointIdentity{
			RuntimeBackend: runtimeInfo.Backend, RuntimeID: runtimeInfo.ID,
			RuntimeArch: runtimeInfo.Arch, VMRuntimeContract: runtimeInfo.Contract,
			KernelDigest: runtimeInfo.KernelDigest, InitramfsDigest: runtimeInfo.InitramfsDigest,
			RootfsDigest: runtimeInfo.RootfsDigest, RuntimeConfigDigest: runtimeInfo.ConfigDigest,
			VMVCPUCount: runtimeInfo.VMVCPUCount, CPUConfigDigest: runtimeInfo.CPUConfigDigest,
		},
		Topology: topology, ReadOnlyDrives: readOnlyDrives,
		RecordPhase: record,
	})
	if err != nil {
		return nil, err
	}
	verify := &workspacev0.VerifyProgramRestoreRequest{
		RunId: restore.RunID, AttemptNumber: uint32(restore.AttemptNumber), RunWaitId: restore.RunWaitID,
		CheckpointId: restore.CheckpointID, CorrelationId: checkpoint.RecoveryPoint.CorrelationID,
	}
	if err := verifyRestoredProgramOnSession(ctx, session, verify); err != nil {
		return nil, errors.Join(fmt.Errorf("verify restored frozen program: %w", err), p.closeSession(ctx, session))
	}
	return session, nil
}

func validatePreparedRuntimeRestore(
	target workerapi.RuntimeReconcileTarget,
	workerArchitecture deployment.RuntimeArchitecture,
) (workerapi.CheckpointManifest, error) {
	restore := target.Source.Restore
	if restore == nil || strings.TrimSpace(restore.CheckpointID) == "" ||
		strings.TrimSpace(restore.RunID) == "" || restore.AttemptNumber <= 0 ||
		strings.TrimSpace(restore.RunWaitID) == "" || len(restore.Manifest) == 0 {
		return workerapi.CheckpointManifest{}, errors.New("prepared runtime restore authority is incomplete")
	}
	var checkpoint workerapi.CheckpointManifest
	if err := json.Unmarshal(restore.Manifest, &checkpoint); err != nil {
		return workerapi.CheckpointManifest{}, fmt.Errorf("decode prepared runtime restore manifest: %w", err)
	}
	if err := validateRestoreIdentity(checkpoint, workerArchitecture); err != nil {
		return workerapi.CheckpointManifest{}, err
	}
	if checkpoint.RecoveryPoint.Runtime.VMVCPUCount != target.Source.VMVCPUCount ||
		checkpoint.RecoveryPoint.Runtime.CPUConfigDigest != target.Source.CPUConfigDigest {
		return workerapi.CheckpointManifest{}, errors.New("prepared runtime restore manifest CPU shape does not match its reservation")
	}
	if checkpoint.RecoveryPoint.ID != restore.CheckpointID || checkpoint.RecoveryPoint.RunID != restore.RunID ||
		checkpoint.RecoveryPoint.AttemptNumber != restore.AttemptNumber ||
		checkpoint.RecoveryPoint.RunWaitID != restore.RunWaitID ||
		strings.TrimSpace(checkpoint.RecoveryPoint.CorrelationID) == "" {
		return workerapi.CheckpointManifest{}, errors.New("prepared runtime restore manifest identity is inconsistent")
	}
	// A reserved disk is not interchangeable with the one paired with this RAM.
	// Check the tuple before any disk expansion, downloads or VM materialization.
	if err := validateCheckpointComputerSource(target.Source, checkpoint.RuntimeState.Computer); err != nil {
		return workerapi.CheckpointManifest{}, err
	}
	if len(checkpoint.RuntimeState.MemoryArtifacts) != 1 {
		return workerapi.CheckpointManifest{}, errors.New("prepared runtime restore requires exactly one memory artifact")
	}
	expected := []struct {
		role    string
		ordinal int32
		value   workerapi.CheckpointArtifact
	}{
		{"runtime_config", 0, checkpoint.RuntimeState.ConfigArtifact},
		{"vm_state", 0, checkpoint.RuntimeState.VMStateArtifact},
		{"memory", 0, checkpoint.RuntimeState.MemoryArtifacts[0]},
		{"scratch_disk", 0, checkpoint.RuntimeState.ScratchDiskArtifact},
	}
	if len(restore.Artifacts) != len(expected) {
		return workerapi.CheckpointManifest{}, errors.New("prepared runtime restore artifact membership is incomplete")
	}
	for index, want := range expected {
		got := restore.Artifacts[index]
		if got.Role != want.role || got.Ordinal != want.ordinal ||
			got.Object.Digest != want.value.Digest || got.Object.SizeBytes != want.value.SizeBytes ||
			got.Object.MediaType != want.value.MediaType {
			return workerapi.CheckpointManifest{}, errors.New("prepared runtime restore artifact membership does not match its manifest")
		}
	}
	return checkpoint, nil
}

func validateCheckpointComputerSource(source workerapi.RuntimeSource, captured *workerapi.CheckpointComputer) error {
	reserved := source.Computer
	if reserved == nil || reserved.Seed != nil || reserved.Root == nil || captured == nil {
		return errors.New("checkpoint restore requires a paired Computer generation")
	}
	if captured.ComputerID != source.WorkspaceID || captured.LogicalBytes != reserved.LogicalBytes || captured.Root != *reserved.Root {
		return errors.New("checkpoint generation differs from retained Computer source")
	}
	return captured.Root.Validate(reserved.LogicalBytes)
}

func restoreStagingKey(id string, epoch int64) capacity.Key {
	return capacity.Key{Kind: "checkpoint-restore", ID: id, Epoch: epoch}
}
func (p *PreparedRuntimePool) restorePreparationDirectory(id string, epoch int64) string {
	root := strings.TrimSpace(p.TempDir)
	if root == "" {
		root = os.TempDir()
	}
	return filepath.Join(root, "restore-"+id+"-"+strconv.FormatInt(epoch, 10))
}

// Retain raw RAM and state with the runtime; decrypted packed inputs live only
// through materialization. Ciphertext sizes safely bound their plaintext files.
func (p *PreparedRuntimePool) checkpointRestoreCapacity(target workerapi.RuntimeReconcileTarget) (retained, staging int64, err error) {
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
			return 0, 0, capacity.ErrOverflow
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
		return 0, 0, capacity.ErrOverflow
	}
	retained += state
	return retained, staging, nil
}
