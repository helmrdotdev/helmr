package computerhost

import (
	"encoding/json"
	"testing"

	"github.com/helmrdotdev/helmr/internal/definition"
	"github.com/helmrdotdev/helmr/internal/disk"
	"github.com/helmrdotdev/helmr/internal/sha256sum"
	"github.com/helmrdotdev/helmr/internal/workerapi"
)

func instanceRestoreValidationFixture(t *testing.T, count int) (workerapi.InstanceReconcileTarget, workerapi.CheckpointManifest) {
	t.Helper()
	source := workerapi.InstanceSource{WriterGeneration: 5, ComputerID: "computer", ComputerSpecID: "spec", VMPlatformID: "platform", VMRuntimeContract: "contract", RootfsDigest: "rootfs", VMVCPUCount: 2, CPUConfigDigest: sha256sum.DigestBytes([]byte("cpu")), Computer: &workerapi.InstanceComputerSource{LogicalBytes: disk.SeedCapacity, Root: ptrVersionRoot(disk.SeedCapacity)}}
	point := workerapi.CheckpointRecoveryPoint{ID: "checkpoint", ComputerID: source.ComputerID, ComputerSpecID: source.ComputerSpecID, ComputerInstanceID: "captured", WriterGeneration: 4, MembershipRevision: 2, Runtime: workerapi.CheckpointRuntime{Backend: "firecracker", ID: source.VMPlatformID, Arch: string(definition.ArchitectureX8664), Contract: source.VMRuntimeContract, RootfsDigest: source.RootfsDigest, KernelDigest: "kernel", InitramfsDigest: "initramfs", ConfigDigest: "config", VMVCPUCount: 2, CPUConfigDigest: source.CPUConfigDigest}}
	if count > 0 {
		source.Program = &workerapi.RuntimeProgram{DeploymentID: "program"}
		point.ProgramDeploymentID = "program"
	}
	for i := range count {
		key := string(rune('a' + i))
		point.Runs = append(point.Runs, workerapi.CheckpointRun{RunID: "run-" + key, RunWaitID: "wait-" + key, RunLeaseID: "lease-" + key, AttemptNumber: 1, CorrelationID: "correlation-" + key})
	}
	a := workerapi.CheckpointArtifact{Digest: sha256sum.DigestBytes([]byte("object")), SizeBytes: 1, MediaType: "application/octet-stream"}
	manifest := workerapi.CheckpointManifest{RecoveryPoint: point, RuntimeState: workerapi.CheckpointRuntimeState{Computer: &workerapi.CheckpointComputer{ComputerID: source.ComputerID, LogicalBytes: disk.SeedCapacity, Root: *source.Computer.Root}, ConfigArtifact: a, VMStateArtifact: a, MemoryArtifacts: []workerapi.CheckpointArtifact{a}, ScratchDiskArtifact: a}}
	source.Restore = &workerapi.InstanceRestore{CheckpointID: point.ID}
	for _, role := range []string{"vm_config", "vm_state", "memory", "scratch_disk"} {
		source.Restore.Artifacts = append(source.Restore.Artifacts, workerapi.RunLeaseCheckpointArtifact{Role: role, Object: workerapi.CASObject(a)})
	}
	return workerapi.InstanceReconcileTarget{ID: "destination", Source: source}, manifest
}

func TestInstanceRestoreValidation(t *testing.T) {
	for _, count := range []int{0, 2} {
		target, m := instanceRestoreValidationFixture(t, count)
		encoded, err := json.Marshal(m)
		if err != nil {
			t.Fatal(err)
		}
		target.Source.Restore.Manifest = encoded
		got, err := validatePreparedMachineRestore(target, definition.ArchitectureX8664)
		if err != nil || len(got.RecoveryPoint.Runs) != count {
			t.Fatalf("count=%d: %v", count, err)
		}
	}
}

func TestInstanceRestoreValidationRejectsChangedIdentity(t *testing.T) {
	for _, test := range []struct {
		name   string
		change func(*workerapi.CheckpointManifest)
	}{
		{"computer", func(m *workerapi.CheckpointManifest) { m.RecoveryPoint.ComputerID = "other" }},
		{"source is destination", func(m *workerapi.CheckpointManifest) { m.RecoveryPoint.ComputerInstanceID = "destination" }},
		{"generation", func(m *workerapi.CheckpointManifest) { m.RecoveryPoint.WriterGeneration = 0 }},
		{"non advancing generation", func(m *workerapi.CheckpointManifest) { m.RecoveryPoint.WriterGeneration = 5 }},
		{"spec", func(m *workerapi.CheckpointManifest) { m.RecoveryPoint.ComputerSpecID = "other" }},
		{"program", func(m *workerapi.CheckpointManifest) { m.RecoveryPoint.ProgramDeploymentID = "other" }},
		{"platform", func(m *workerapi.CheckpointManifest) { m.RecoveryPoint.Runtime.ID = "other" }},
		{"cpu", func(m *workerapi.CheckpointManifest) {
			m.RecoveryPoint.Runtime.CPUConfigDigest = sha256sum.DigestBytes([]byte("other"))
		}},
		{"disk", func(m *workerapi.CheckpointManifest) { m.RuntimeState.Computer.ComputerID = "other" }},
		{"duplicate run", func(m *workerapi.CheckpointManifest) { m.RecoveryPoint.Runs[1].RunID = m.RecoveryPoint.Runs[0].RunID }},
		{"duplicate wait", func(m *workerapi.CheckpointManifest) {
			m.RecoveryPoint.Runs[1].RunWaitID = m.RecoveryPoint.Runs[0].RunWaitID
		}},
		{"duplicate lease", func(m *workerapi.CheckpointManifest) {
			m.RecoveryPoint.Runs[1].RunLeaseID = m.RecoveryPoint.Runs[0].RunLeaseID
		}},
		{"missing correlation", func(m *workerapi.CheckpointManifest) { m.RecoveryPoint.Runs[0].CorrelationID = "" }},
		{"negative cursor", func(m *workerapi.CheckpointManifest) {
			v := int64(-1)
			m.RecoveryPoint.Runs[0].ActorSpeculativeInputSequence = &v
		}},
	} {
		t.Run(test.name, func(t *testing.T) {
			target, m := instanceRestoreValidationFixture(t, 2)
			test.change(&m)
			encoded, err := json.Marshal(m)
			if err != nil {
				t.Fatal(err)
			}
			target.Source.Restore.Manifest = encoded
			if _, err := validatePreparedMachineRestore(target, definition.ArchitectureX8664); err == nil {
				t.Fatal("changed identity accepted")
			}
		})
	}
}
