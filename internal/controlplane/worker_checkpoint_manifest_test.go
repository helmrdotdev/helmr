package controlplane

import (
	"bytes"
	"encoding/json"
	"testing"

	"github.com/helmrdotdev/helmr/internal/disk"
	"github.com/helmrdotdev/helmr/internal/workerapi"
)

// The Computer owner persists and fingerprints its manifest encoding, so the
// conversion of a worker's manifest must encode byte-identically, keeping
// absent lists absent and empty lists empty.
func TestComputerCheckpointManifestEncodesAsWorkerManifest(t *testing.T) {
	sequence := int64(7)
	full := workerapi.CheckpointManifest{
		RecoveryPoint: workerapi.CheckpointRecoveryPoint{ID: "0192a000-0000-7000-8000-000000000001", ComputerID: "0192a000-0000-7000-8000-000000000002", ComputerInstanceID: "0192a000-0000-7000-8000-000000000003", WriterGeneration: 4, MembershipRevision: 5, ComputerSpecID: "0192a000-0000-7000-8000-000000000004", ProgramDeploymentID: "0192a000-0000-7000-8000-000000000005",
			Runs: []workerapi.CheckpointRun{
				{RunID: "0192a000-0000-7000-8000-000000000006", AttemptNumber: 2, RunWaitID: "0192a000-0000-7000-8000-000000000007", RunLeaseID: "0192a000-0000-7000-8000-000000000008", ActorSpeculativeInputSequence: &sequence, CorrelationID: "corr"},
				{RunID: "0192a000-0000-7000-8000-000000000009", AttemptNumber: 1, RunWaitID: "0192a000-0000-7000-8000-00000000000a", RunLeaseID: "0192a000-0000-7000-8000-00000000000b", CorrelationID: "other"},
			},
			Runtime: workerapi.CheckpointRuntime{Backend: "firecracker", ID: "platform", Arch: "x86_64", Contract: "v1", KernelDigest: "sha256:k", InitramfsDigest: "sha256:i", RootfsDigest: "sha256:r", ConfigDigest: "sha256:c", VMVCPUCount: 2, CPUConfigDigest: "sha256:cpu"}},
		RuntimeState: workerapi.CheckpointRuntimeState{Computer: &workerapi.CheckpointComputer{ComputerID: "0192a000-0000-7000-8000-000000000002", LogicalBytes: 1024, Root: disk.GenerationRoot{FormatVersion: 1, LogicalBytes: 1024, Offset: 8, Pack: disk.GenerationPack{Digest: "sha256:p", SizeBytes: 512, Rank: 2}, Page: disk.GenerationPage{Digest: "sha256:g", Salt: "aa", KeyID: "key", Kind: 3, Count: 1, SizeBytes: 64}}},
			ConfigArtifact: workerapi.CheckpointArtifact{Digest: "sha256:1", SizeBytes: 1, MediaType: "a"}, VMStateArtifact: workerapi.CheckpointArtifact{Digest: "sha256:2", SizeBytes: 2, MediaType: "b"}, ScratchDiskArtifact: workerapi.CheckpointArtifact{Digest: "sha256:3", SizeBytes: 3, MediaType: "c"}, MemoryArtifacts: []workerapi.CheckpointArtifact{{Digest: "sha256:4", SizeBytes: 4, MediaType: "d"}, {Digest: "sha256:5", SizeBytes: 5, MediaType: "e"}}, Config: json.RawMessage(`{"runtime":{}}`)},
		ComputerState: workerapi.CheckpointComputerState{Base: workerapi.CheckpointComputerBase{MountPath: "/workspace"}},
		Phases: []workerapi.CheckpointPhase{
			{Name: "upload", DurationMs: 5, Role: "memory", MediaType: "d", ErrorClass: "none", Filepack: &workerapi.CheckpointFilepackStats{LogicalBytes: 1, EncodedChunks: 2, UnpackWrittenBytes: 3}},
			{Name: "freeze", DurationMs: 1},
		},
	}
	absent := workerapi.CheckpointManifest{}
	empty := workerapi.CheckpointManifest{RecoveryPoint: workerapi.CheckpointRecoveryPoint{Runs: []workerapi.CheckpointRun{}}, RuntimeState: workerapi.CheckpointRuntimeState{MemoryArtifacts: []workerapi.CheckpointArtifact{}, Config: json.RawMessage{}}, Phases: []workerapi.CheckpointPhase{}}
	for name, manifest := range map[string]workerapi.CheckpointManifest{"full": full, "absent": absent, "empty": empty} {
		want, err := json.Marshal(manifest)
		if err != nil {
			t.Fatal(err)
		}
		converted := computerCheckpointManifest(manifest)
		got, err := json.Marshal(converted)
		if err != nil {
			t.Fatal(err)
		}
		if !bytes.Equal(got, want) {
			t.Fatalf("%s manifest encodes as\n%s\nwant\n%s", name, got, want)
		}
		if (manifest.RecoveryPoint.Runs == nil) != (converted.RecoveryPoint.Runs == nil) || (manifest.Phases == nil) != (converted.Phases == nil) {
			t.Fatalf("%s manifest changed absent lists: runs=%v phases=%v", name, converted.RecoveryPoint.Runs == nil, converted.Phases == nil)
		}
	}
}
