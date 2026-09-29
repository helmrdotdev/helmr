package executor

import (
	"encoding/json"
	"github.com/helmrdotdev/helmr/internal/cas"
	"github.com/helmrdotdev/helmr/internal/computer"
	"github.com/helmrdotdev/helmr/internal/deployment"
	"github.com/helmrdotdev/helmr/internal/sha256sum"
	"github.com/helmrdotdev/helmr/internal/workerapi"
	"testing"
)

func TestValidatePreparedRuntimeRestoreExactTupleAndMembership(t *testing.T) {
	cpuConfigDigest := sha256sum.DigestBytes([]byte("cpu-config"))
	artifact := func(digest, mediaType string, size int64) workerapi.CheckpointArtifact {
		return workerapi.CheckpointArtifact{Digest: digest, MediaType: mediaType, SizeBytes: size}
	}
	checkpoint := workerapi.CheckpointManifest{
		RecoveryPoint: workerapi.CheckpointRecoveryPoint{
			ID: "checkpoint-1", ComputerID: "01950000-0000-7000-8000-000000000001", ComputerInstanceID: "source-instance", ComputerSpecID: "spec", ProgramDeploymentID: "program", WriterGeneration: 1, MembershipRevision: 2, Runs: []workerapi.CheckpointRun{{RunID: "run-1", AttemptNumber: 2, RunWaitID: "wait-1", RunLeaseID: "lease-1", CorrelationID: "correlation-1"}},
			Runtime: workerapi.CheckpointRuntime{Backend: "firecracker", ID: "runtime-shape", Arch: testCheckpointRuntimeArchitecture(),
				Contract: "abi-1", KernelDigest: "kernel", InitramfsDigest: "initramfs", RootfsDigest: "rootfs", ConfigDigest: "config",
				VMVCPUCount: 2, CPUConfigDigest: cpuConfigDigest},
		},
		RuntimeState: workerapi.CheckpointRuntimeState{
			Computer:            &workerapi.CheckpointComputer{ComputerID: "01950000-0000-7000-8000-000000000001", LogicalBytes: computer.SeedCapacity, Root: testGenerationRoot(computer.SeedCapacity)},
			ConfigArtifact:      artifact("config-object", cas.CheckpointVMConfigMediaType, 10),
			VMStateArtifact:     artifact("state-object", cas.CheckpointVMStateMediaType, 20),
			MemoryArtifacts:     []workerapi.CheckpointArtifact{artifact("memory-object", cas.CheckpointMemoryMediaType, 30)},
			ScratchDiskArtifact: artifact("scratch-object", cas.CheckpointScratchDiskMediaType, 40),
		},
		ComputerState: workerapi.CheckpointComputerState{Base: workerapi.CheckpointComputerBase{

			MountPath: "/workspace",
		}},
	}
	manifest, err := json.Marshal(checkpoint)
	if err != nil {
		t.Fatal(err)
	}
	object := func(value workerapi.CheckpointArtifact) workerapi.CASObject {
		return workerapi.CASObject(value)
	}
	target := workerapi.RuntimeReconcileTarget{Source: workerapi.RuntimeSource{
		VMVCPUCount: 2, CPUConfigDigest: cpuConfigDigest,
		WriterGeneration: 2, ComputerSpecID: "spec", VMPlatformID: "runtime-shape", VMRuntimeContract: "abi-1", RootfsDigest: "rootfs", Program: &workerapi.RuntimeProgram{DeploymentID: "program"},
		ComputerID: checkpoint.RuntimeState.Computer.ComputerID,
		Computer:   &workerapi.RuntimeComputerSource{VersionID: "01950000-0000-7000-8000-000000000002", LogicalBytes: computer.SeedCapacity, Root: ptrGenerationRoot(computer.SeedCapacity)},
		Restore: &workerapi.RuntimeRestore{
			CheckpointID: "checkpoint-1",
			Manifest:     manifest,
			Artifacts: []workerapi.RunLeaseCheckpointArtifact{
				{Role: "vm_config", Object: object(checkpoint.RuntimeState.ConfigArtifact)},
				{Role: "vm_state", Object: object(checkpoint.RuntimeState.VMStateArtifact)},
				{Role: "memory", Object: object(checkpoint.RuntimeState.MemoryArtifacts[0])},
				{Role: "scratch_disk", Object: object(checkpoint.RuntimeState.ScratchDiskArtifact)},
			},
		},
	}}
	if _, err := validatePreparedRuntimeRestore(target, deployment.ArchitectureX8664); err != nil {
		t.Fatal(err)
	}
	target.Source.CPUConfigDigest = sha256sum.DigestBytes([]byte("other-cpu-config"))
	if _, err := validatePreparedRuntimeRestore(target, deployment.ArchitectureX8664); err == nil {
		t.Fatal("mismatched runtime reservation CPU shape was accepted")
	}
	target.Source.CPUConfigDigest = cpuConfigDigest
	target.Source.Restore.Artifacts[2].Role = "vm_state"
	if _, err := validatePreparedRuntimeRestore(target, deployment.ArchitectureX8664); err == nil {
		t.Fatal("mismatched Checkpoint Artifact membership was accepted")
	}
	target.Source.Restore.Artifacts[2].Role = "memory"

}
