package computer

import (
	"encoding/json"
	"testing"

	"github.com/helmrdotdev/helmr/internal/disk"
)

// goldenCheckpointManifest populates every field of the manifest family.
func goldenCheckpointManifest() CheckpointManifest {
	sequence := int64(7)
	return CheckpointManifest{
		RecoveryPoint: CheckpointRecoveryPoint{ID: "0192a000-0000-7000-8000-000000000001", ComputerID: "0192a000-0000-7000-8000-000000000002", ComputerInstanceID: "0192a000-0000-7000-8000-000000000003", WriterGeneration: 4, MembershipRevision: 5, ComputerSpecID: "0192a000-0000-7000-8000-000000000004", ProgramDeploymentID: "0192a000-0000-7000-8000-000000000005",
			Runs:    []CheckpointRun{{RunID: "0192a000-0000-7000-8000-000000000006", AttemptNumber: 2, RunWaitID: "0192a000-0000-7000-8000-000000000007", RunLeaseID: "0192a000-0000-7000-8000-000000000008", ActorSpeculativeInputSequence: &sequence, CorrelationID: "corr"}},
			Runtime: CheckpointRuntime{Backend: "firecracker", ID: "platform", Arch: "x86_64", Contract: "v1", KernelDigest: "sha256:k", InitramfsDigest: "sha256:i", RootfsDigest: "sha256:r", ConfigDigest: "sha256:c", VMVCPUCount: 2, CPUConfigDigest: "sha256:cpu"}},
		RuntimeState: CheckpointRuntimeState{Computer: &CheckpointComputer{ComputerID: "0192a000-0000-7000-8000-000000000002", LogicalBytes: 1024, Root: disk.GenerationRoot{FormatVersion: 1, LogicalBytes: 1024, Offset: 8, Pack: disk.GenerationPack{Digest: "sha256:p", SizeBytes: 512, Rank: 2}, Page: disk.GenerationPage{Digest: "sha256:g", Salt: "aa", KeyID: "key", Kind: 3, Count: 1, SizeBytes: 64}}},
			ConfigArtifact: CheckpointArtifact{Digest: "sha256:1", SizeBytes: 1, MediaType: "a"}, VMStateArtifact: CheckpointArtifact{Digest: "sha256:2", SizeBytes: 2, MediaType: "b"}, ScratchDiskArtifact: CheckpointArtifact{Digest: "sha256:3", SizeBytes: 3, MediaType: "c"}, MemoryArtifacts: []CheckpointArtifact{{Digest: "sha256:4", SizeBytes: 4, MediaType: "d"}}, Config: json.RawMessage(`{"runtime":{}}`)},
		ComputerState: CheckpointComputerState{Base: CheckpointComputerBase{MountPath: "/workspace"}},
		Phases:        []CheckpointPhase{{Name: "upload", DurationMs: 5, Role: "memory", MediaType: "d", ErrorClass: "none", Filepack: &CheckpointFilepackStats{LogicalBytes: 1, EncodedChunks: 2, UnpackWrittenBytes: 3}}},
	}
}

// The manifest encoding is the persisted checkpoint manifest and the input
// of the readiness receipt fingerprint; it must stay byte-identical.
func TestCheckpointManifestEncodingIsStable(t *testing.T) {
	const golden = `{"recovery_point":{"id":"0192a000-0000-7000-8000-000000000001","computer_id":"0192a000-0000-7000-8000-000000000002","computer_instance_id":"0192a000-0000-7000-8000-000000000003","writer_generation":4,"membership_revision":5,"computer_spec_id":"0192a000-0000-7000-8000-000000000004","program_deployment_id":"0192a000-0000-7000-8000-000000000005","runs":[{"run_id":"0192a000-0000-7000-8000-000000000006","attempt_number":2,"run_wait_id":"0192a000-0000-7000-8000-000000000007","run_lease_id":"0192a000-0000-7000-8000-000000000008","actor_speculative_input_sequence":7,"correlation_id":"corr"}],"runtime":{"backend":"firecracker","id":"platform","arch":"x86_64","contract":"v1","kernel_digest":"sha256:k","initramfs_digest":"sha256:i","rootfs_digest":"sha256:r","config_digest":"sha256:c","vm_vcpu_count":2,"cpu_config_digest":"sha256:cpu"}},"runtime_state":{"computer":{"computer_id":"0192a000-0000-7000-8000-000000000002","logical_bytes":1024,"root":{"format_version":1,"logical_bytes":1024,"pack":{"digest":"sha256:p","size_bytes":512,"rank":2},"page":{"digest":"sha256:g","salt":"aa","key_id":"key","kind":3,"count":1,"size_bytes":64},"offset":8}},"config_artifact":{"digest":"sha256:1","size_bytes":1,"media_type":"a"},"vm_state_artifact":{"digest":"sha256:2","size_bytes":2,"media_type":"b"},"scratch_disk_artifact":{"digest":"sha256:3","size_bytes":3,"media_type":"c"},"memory_artifacts":[{"digest":"sha256:4","size_bytes":4,"media_type":"d"}],"config":{"runtime":{}}},"computer_state":{"base":{"mount_path":"/workspace"}},"phases":[{"name":"upload","duration_ms":5,"role":"memory","media_type":"d","error_class":"none","filepack":{"logical_bytes":1,"encoded_chunks":2,"unpack_written_bytes":3}}]}`
	encoded, err := json.Marshal(goldenCheckpointManifest())
	if err != nil {
		t.Fatal(err)
	}
	if string(encoded) != golden {
		t.Fatalf("manifest encoding changed:\n%s\nwant\n%s", encoded, golden)
	}
	empty, err := json.Marshal(CheckpointManifest{RecoveryPoint: CheckpointRecoveryPoint{Runs: []CheckpointRun{}}})
	if err != nil {
		t.Fatal(err)
	}
	const emptyGolden = `{"recovery_point":{"id":"","computer_id":"","computer_instance_id":"","writer_generation":0,"membership_revision":0,"computer_spec_id":"","runs":[],"runtime":{"backend":"","id":"","arch":"","contract":"","kernel_digest":"","initramfs_digest":"","rootfs_digest":"","config_digest":"","vm_vcpu_count":0,"cpu_config_digest":""}},"runtime_state":{"config_artifact":{"digest":"","size_bytes":0,"media_type":""},"vm_state_artifact":{"digest":"","size_bytes":0,"media_type":""},"scratch_disk_artifact":{"digest":"","size_bytes":0,"media_type":""}},"computer_state":{"base":{"mount_path":""}}}`
	if string(empty) != emptyGolden {
		t.Fatalf("omitted manifest fields changed:\n%s\nwant\n%s", empty, emptyGolden)
	}
}
