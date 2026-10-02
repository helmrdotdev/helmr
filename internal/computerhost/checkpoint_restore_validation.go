package computerhost

import (
	"encoding/json"
	"errors"
	"fmt"
	"strings"

	"github.com/helmrdotdev/helmr/internal/artifact"
	"github.com/helmrdotdev/helmr/internal/definition"
	"github.com/helmrdotdev/helmr/internal/sha256sum"
	"github.com/helmrdotdev/helmr/internal/workerapi"
)

func validateRestoreIdentity(
	checkpoint workerapi.CheckpointManifest,
	workerArchitecture definition.RuntimeArchitecture,
) error {
	runtimeInfo := checkpoint.RecoveryPoint.Runtime
	if runtimeInfo.Backend != "firecracker" {
		return fmt.Errorf("restore checkpoint recovery_point.runtime.backend %q is not supported", runtimeInfo.Backend)
	}
	if err := artifact.ValidateRuntimeArchitecture(workerArchitecture); err != nil {
		return fmt.Errorf("validate worker runtime architecture: %w", err)
	}
	if runtimeInfo.Arch != string(workerArchitecture) {
		return fmt.Errorf("restore checkpoint recovery_point.runtime.arch %q does not match worker arch %q", runtimeInfo.Arch, workerArchitecture)
	}
	if strings.TrimSpace(runtimeInfo.Contract) == "" {
		return errors.New("restore checkpoint recovery_point.runtime.contract is required")
	}
	if err := requireCheckpointDigest("recovery_point.runtime.id", runtimeInfo.ID); err != nil {
		return err
	}
	if err := requireCheckpointDigest("recovery_point.runtime.kernel_digest", runtimeInfo.KernelDigest); err != nil {
		return err
	}
	if err := requireCheckpointDigest("recovery_point.runtime.initramfs_digest", runtimeInfo.InitramfsDigest); err != nil {
		return err
	}
	if err := requireCheckpointDigest("recovery_point.runtime.rootfs_digest", runtimeInfo.RootfsDigest); err != nil {
		return err
	}
	if err := requireCheckpointDigest("recovery_point.runtime.config_digest", runtimeInfo.ConfigDigest); err != nil {
		return err
	}
	if runtimeInfo.VMVCPUCount <= 0 {
		return errors.New("restore checkpoint recovery_point.runtime.vm_vcpu_count must be positive")
	}
	if !sha256sum.ValidDigest(runtimeInfo.CPUConfigDigest) {
		return errors.New("restore checkpoint recovery_point.runtime.cpu_config_digest must be canonical")
	}
	return requireCheckpointArtifact(checkpoint.RuntimeState.ConfigArtifact, "runtime_state.config_artifact")
}

func requireCheckpointDigest(field string, value string) error {
	if strings.TrimSpace(value) == "" {
		return fmt.Errorf("restore checkpoint %s is required", field)
	}
	return nil
}

func requireCheckpointArtifact(artifact workerapi.CheckpointArtifact, field string) error {
	if strings.TrimSpace(artifact.Digest) == "" {
		return fmt.Errorf("restore checkpoint %s.digest is required", field)
	}
	if strings.TrimSpace(artifact.MediaType) == "" {
		return fmt.Errorf("restore checkpoint %s.media_type is required", field)
	}
	return nil
}

func validatePreparedMachineRestore(
	target workerapi.InstanceReconcileTarget,
	workerArchitecture definition.RuntimeArchitecture,
) (workerapi.CheckpointManifest, error) {
	restore := target.Source.Restore
	if restore == nil || strings.TrimSpace(restore.CheckpointID) == "" ||
		len(restore.Manifest) == 0 {
		return workerapi.CheckpointManifest{}, errors.New("prepared machine restore authority is incomplete")
	}
	var checkpoint workerapi.CheckpointManifest
	if err := json.Unmarshal(restore.Manifest, &checkpoint); err != nil {
		return workerapi.CheckpointManifest{}, fmt.Errorf("decode prepared machine restore manifest: %w", err)
	}
	if err := validateRestoreIdentity(checkpoint, workerArchitecture); err != nil {
		return workerapi.CheckpointManifest{}, err
	}
	if checkpoint.RecoveryPoint.Runtime.VMVCPUCount != target.Source.VMVCPUCount ||
		checkpoint.RecoveryPoint.Runtime.CPUConfigDigest != target.Source.CPUConfigDigest {
		return workerapi.CheckpointManifest{}, errors.New("prepared machine restore manifest CPU shape does not match its reservation")
	}

	point := checkpoint.RecoveryPoint
	programID := ""
	if target.Source.Program != nil {
		programID = target.Source.Program.DeploymentID
	}
	if point.ID != restore.CheckpointID || point.ComputerID != target.Source.ComputerID ||
		point.ComputerSpecID != target.Source.ComputerSpecID || point.ProgramDeploymentID != programID ||
		strings.TrimSpace(point.ComputerInstanceID) == "" || point.ComputerInstanceID == target.ID ||
		point.WriterGeneration <= 0 || target.Source.WriterGeneration <= point.WriterGeneration || point.MembershipRevision < 0 ||
		point.Runtime.ID != target.Source.VMPlatformID || point.Runtime.Contract != target.Source.VMRuntimeContract ||
		point.Runtime.RootfsDigest != target.Source.RootfsDigest {
		return workerapi.CheckpointManifest{}, errors.New("computer restore manifest identity is inconsistent")
	}
	if len(point.Runs) > 0 && programID == "" {
		return workerapi.CheckpointManifest{}, errors.New("captured Runs require a Program")
	}
	runs, waits, leases := map[string]bool{}, map[string]bool{}, map[string]bool{}
	for _, member := range point.Runs {
		if strings.TrimSpace(member.RunID) == "" || strings.TrimSpace(member.RunWaitID) == "" ||
			strings.TrimSpace(member.RunLeaseID) == "" || strings.TrimSpace(member.CorrelationID) == "" ||
			member.AttemptNumber <= 0 || (member.SessionSpeculativeInputSequence != nil && *member.SessionSpeculativeInputSequence < 0) ||
			runs[member.RunID] || waits[member.RunWaitID] || leases[member.RunLeaseID] {
			return workerapi.CheckpointManifest{}, errors.New("computer restore member identity is invalid")
		}
		runs[member.RunID], waits[member.RunWaitID], leases[member.RunLeaseID] = true, true, true
	}
	// A reserved disk is not interchangeable with the one paired with this RAM.
	// Check the tuple before any disk expansion, downloads or VM materialization.
	if err := validateCheckpointComputerSource(target.Source, checkpoint.RuntimeState.Computer); err != nil {
		return workerapi.CheckpointManifest{}, err
	}
	if len(checkpoint.RuntimeState.MemoryArtifacts) != 1 {
		return workerapi.CheckpointManifest{}, errors.New("prepared machine restore requires exactly one memory artifact")
	}
	expected := []struct {
		role    string
		ordinal int32
		value   workerapi.CheckpointArtifact
	}{
		{"vm_config", 0, checkpoint.RuntimeState.ConfigArtifact},
		{"vm_state", 0, checkpoint.RuntimeState.VMStateArtifact},
		{"memory", 0, checkpoint.RuntimeState.MemoryArtifacts[0]},
		{"scratch_disk", 0, checkpoint.RuntimeState.ScratchDiskArtifact},
	}
	if len(restore.Artifacts) != len(expected) {
		return workerapi.CheckpointManifest{}, errors.New("prepared machine restore artifact membership is incomplete")
	}
	for index, want := range expected {
		got := restore.Artifacts[index]
		if got.Role != want.role || got.Ordinal != want.ordinal ||
			got.Object.Digest != want.value.Digest || got.Object.SizeBytes != want.value.SizeBytes ||
			got.Object.MediaType != want.value.MediaType {
			return workerapi.CheckpointManifest{}, errors.New("prepared machine restore artifact membership does not match its manifest")
		}
	}
	return checkpoint, nil
}

func validateCheckpointComputerSource(source workerapi.InstanceSource, captured *workerapi.CheckpointComputer) error {
	reserved := source.Computer
	if reserved == nil || reserved.Seed != nil || reserved.Root == nil || captured == nil {
		return errors.New("checkpoint restore requires a paired Computer disk version")
	}
	if captured.ComputerID != source.ComputerID || captured.LogicalBytes != reserved.LogicalBytes || captured.Root != *reserved.Root {
		return errors.New("checkpoint disk version differs from retained Computer source")
	}
	return captured.Root.Validate(reserved.LogicalBytes)
}
