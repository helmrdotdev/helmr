package executor

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"strings"

	"github.com/helmrdotdev/helmr/internal/api"
	"github.com/helmrdotdev/helmr/internal/deployment"
	workspacev0 "github.com/helmrdotdev/helmr/internal/proto/workspace/v0"
	"github.com/helmrdotdev/helmr/internal/vm"
	"golang.org/x/sync/errgroup"
)

func (p *PreparedRuntimePool) restorePreparedRuntime(
	ctx context.Context,
	target api.WorkerRuntimeReconcileTarget,
	topology vm.RuntimeTopology,
	readOnlyDrives []vm.ReadOnlyDrive,
	record func(vm.RuntimePhase),
) (vm.Session, error) {
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
	runner := ProgramRunner{CAS: p.CAS, CheckpointEncryptor: p.CheckpointEncryptor, TempDir: p.TempDir}
	runtimeState := checkpoint.RuntimeState
	paths := make([]string, 4)
	group, groupCtx := errgroup.WithContext(ctx)
	artifacts := []struct {
		index  int
		value  api.WorkerCheckpointArtifact
		suffix string
	}{
		{0, runtimeState.ConfigArtifact, "manifest"},
		{1, runtimeState.VMStateArtifact, "vmstate"},
		{2, runtimeState.MemoryArtifacts[0], "memory"},
		{3, runtimeState.ScratchDiskArtifact, "scratch-disk"},
	}
	for _, artifact := range artifacts {
		group.Go(func() error {
			path, err := runner.materializeCheckpointObject(groupCtx, artifact.value.Digest, artifact.suffix)
			if err != nil {
				return err
			}
			paths[artifact.index] = path
			return nil
		})
	}
	if err := group.Wait(); err != nil {
		removeFiles(paths)
		return nil, err
	}
	defer removeFiles(paths)
	manifest, err := os.ReadFile(paths[0])
	if err != nil {
		return nil, fmt.Errorf("read restored runtime manifest: %w", err)
	}
	runtimeInfo := checkpoint.RecoveryPoint.Runtime
	session, err := restoring.Restore(ctx, vm.RestoreRequest{
		ID: restore.CheckpointID, RuntimeInstanceID: target.ID, OwnerKind: vm.OwnerRuntime,
		Binding: runtimeTargetWorkloadBinding(target),
		VMState: paths[1], VMStateMediaType: runtimeState.VMStateArtifact.MediaType,
		Memory: []string{paths[2]}, MemoryMediaTypes: []string{runtimeState.MemoryArtifacts[0].MediaType},
		ScratchDisk: paths[3], ScratchDiskMediaType: runtimeState.ScratchDiskArtifact.MediaType,
		Manifest: manifest,
		Checkpoint: vm.CheckpointIdentity{
			RuntimeBackend: runtimeInfo.Backend, RuntimeID: runtimeInfo.ID,
			RuntimeArch: runtimeInfo.Arch, RuntimeABI: runtimeInfo.ABI,
			KernelDigest: runtimeInfo.KernelDigest, InitramfsDigest: runtimeInfo.InitramfsDigest,
			RootfsDigest: runtimeInfo.RootfsDigest, RuntimeConfigDigest: runtimeInfo.ConfigDigest,
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
		return nil, errors.Join(fmt.Errorf("verify restored frozen Program: %w", err), session.Close(context.Background()))
	}
	return session, nil
}

func validatePreparedRuntimeRestore(
	target api.WorkerRuntimeReconcileTarget,
	workerArchitecture deployment.RuntimeArchitecture,
) (api.WorkerCheckpointManifest, error) {
	restore := target.Source.Restore
	if restore == nil || restore.Kind != "suspend" || strings.TrimSpace(restore.CheckpointID) == "" ||
		strings.TrimSpace(restore.RunID) == "" || restore.AttemptNumber <= 0 ||
		strings.TrimSpace(restore.RunWaitID) == "" || strings.TrimSpace(restore.Kind) == "" || len(restore.Manifest) == 0 {
		return api.WorkerCheckpointManifest{}, errors.New("prepared runtime restore authority is incomplete")
	}
	var checkpoint api.WorkerCheckpointManifest
	if err := json.Unmarshal(restore.Manifest, &checkpoint); err != nil {
		return api.WorkerCheckpointManifest{}, fmt.Errorf("decode prepared runtime restore manifest: %w", err)
	}
	if err := validateRestoreIdentity(checkpoint, workerArchitecture); err != nil {
		return api.WorkerCheckpointManifest{}, err
	}
	if checkpoint.RecoveryPoint.ID != restore.CheckpointID || checkpoint.RecoveryPoint.RunID != restore.RunID ||
		checkpoint.RecoveryPoint.AttemptNumber != restore.AttemptNumber ||
		checkpoint.RecoveryPoint.RunWaitID != restore.RunWaitID ||
		strings.TrimSpace(checkpoint.RecoveryPoint.CorrelationID) == "" {
		return api.WorkerCheckpointManifest{}, errors.New("prepared runtime restore manifest identity is inconsistent")
	}
	if len(checkpoint.RuntimeState.MemoryArtifacts) != 1 {
		return api.WorkerCheckpointManifest{}, errors.New("prepared runtime restore requires exactly one memory artifact")
	}
	expected := []struct {
		role    string
		ordinal int32
		value   api.WorkerCheckpointArtifact
	}{
		{"runtime_config", 0, checkpoint.RuntimeState.ConfigArtifact},
		{"vm_state", 0, checkpoint.RuntimeState.VMStateArtifact},
		{"memory", 0, checkpoint.RuntimeState.MemoryArtifacts[0]},
		{"scratch_disk", 0, checkpoint.RuntimeState.ScratchDiskArtifact},
	}
	if len(restore.Artifacts) != len(expected) {
		return api.WorkerCheckpointManifest{}, errors.New("prepared runtime restore Artifact membership is incomplete")
	}
	for index, want := range expected {
		got := restore.Artifacts[index]
		if got.Role != want.role || got.Ordinal != want.ordinal ||
			got.Object.Digest != want.value.Digest || got.Object.SizeBytes != want.value.SizeBytes ||
			got.Object.MediaType != want.value.MediaType {
			return api.WorkerCheckpointManifest{}, errors.New("prepared runtime restore Artifact membership does not match its manifest")
		}
	}
	base := checkpoint.WorkspaceState.Base
	workspace := target.Source.WorkspaceArtifact
	if base.ArtifactDigest != workspace.Digest || base.ArtifactSizeBytes != workspace.SizeBytes ||
		base.ArtifactMediaType != workspace.MediaType || base.ArtifactEncoding != workspace.Encoding ||
		strings.TrimSpace(base.MountPath) != "/workspace" {
		return api.WorkerCheckpointManifest{}, errors.New("prepared runtime restore Workspace frontier does not match its reservation")
	}
	return checkpoint, nil
}
