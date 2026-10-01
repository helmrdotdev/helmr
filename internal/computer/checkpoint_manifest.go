package computer

import (
	"encoding/json"

	"github.com/helmrdotdev/helmr/internal/disk"
)

// CheckpointManifest describes one capture checkpoint of an Instance: the
// recovery point of its resident members, the captured runtime state and the
// captured Computer disk. Its canonical JSON encoding is the manifest the
// checkpoint persists and the identity its readiness receipt fingerprints,
// so the field names, tags and order of this type family are persisted
// format: changing them changes stored manifests and receipt fingerprints.
type CheckpointManifest struct {
	RecoveryPoint CheckpointRecoveryPoint `json:"recovery_point"`
	RuntimeState  CheckpointRuntimeState  `json:"runtime_state"`
	ComputerState CheckpointComputerState `json:"computer_state"`
	Phases        []CheckpointPhase       `json:"phases,omitempty"`
}

// CheckpointRecoveryPoint identifies the captured Instance incarnation and
// its sealed member set.
type CheckpointRecoveryPoint struct {
	ID                  string            `json:"id"`
	ComputerID          string            `json:"computer_id"`
	ComputerInstanceID  string            `json:"computer_instance_id"`
	WriterGeneration    int64             `json:"writer_generation"`
	MembershipRevision  int64             `json:"membership_revision"`
	ComputerSpecID      string            `json:"computer_spec_id"`
	ProgramDeploymentID string            `json:"program_deployment_id,omitempty"`
	Runs                []CheckpointRun   `json:"runs"`
	Runtime             CheckpointRuntime `json:"runtime"`
}

// CheckpointRun is one sealed member of the recovery point.
type CheckpointRun struct {
	RunID                         string `json:"run_id"`
	AttemptNumber                 int32  `json:"attempt_number"`
	RunWaitID                     string `json:"run_wait_id"`
	RunLeaseID                    string `json:"run_lease_id"`
	ActorSpeculativeInputSequence *int64 `json:"actor_speculative_input_sequence,omitempty"`
	CorrelationID                 string `json:"correlation_id"`
}

// CheckpointRuntime identifies the VM platform and CPU shape the runtime
// state was captured on.
type CheckpointRuntime struct {
	Backend         string `json:"backend"`
	ID              string `json:"id"`
	Arch            string `json:"arch"`
	Contract        string `json:"contract"`
	KernelDigest    string `json:"kernel_digest"`
	InitramfsDigest string `json:"initramfs_digest"`
	RootfsDigest    string `json:"rootfs_digest"`
	ConfigDigest    string `json:"config_digest"`
	VMVCPUCount     int32  `json:"vm_vcpu_count"`
	CPUConfigDigest string `json:"cpu_config_digest"`
}

// CheckpointComputer binds the writable disk captured with the VM state and
// memory.
type CheckpointComputer struct {
	ComputerID   string           `json:"computer_id"`
	LogicalBytes int64            `json:"logical_bytes"`
	Root         disk.VersionRoot `json:"root"`
}

// CheckpointRuntimeState is the captured runtime state: the disk and the VM
// configuration, state, scratch disk and memory objects.
type CheckpointRuntimeState struct {
	Computer            *CheckpointComputer  `json:"computer,omitempty"`
	ConfigArtifact      CheckpointArtifact   `json:"config_artifact"`
	VMStateArtifact     CheckpointArtifact   `json:"vm_state_artifact"`
	ScratchDiskArtifact CheckpointArtifact   `json:"scratch_disk_artifact"`
	MemoryArtifacts     []CheckpointArtifact `json:"memory_artifacts,omitempty"`
	Config              json.RawMessage      `json:"config,omitempty"`
}

// CheckpointComputerState describes the captured Computer mount.
type CheckpointComputerState struct {
	Base CheckpointComputerBase `json:"base"`
}

// CheckpointComputerBase is the mount of the captured Computer disk.
type CheckpointComputerBase struct {
	MountPath string `json:"mount_path"`
}

// CheckpointArtifact describes one uploaded runtime object.
type CheckpointArtifact struct {
	Digest    string `json:"digest"`
	SizeBytes int64  `json:"size_bytes"`
	MediaType string `json:"media_type"`
}

// CheckpointPhase is a capture timing the worker host reports. Timings are
// not part of the manifest's identity.
type CheckpointPhase struct {
	Name       string                   `json:"name"`
	DurationMs int64                    `json:"duration_ms"`
	Role       string                   `json:"role,omitempty"`
	MediaType  string                   `json:"media_type,omitempty"`
	ErrorClass string                   `json:"error_class,omitempty"`
	Filepack   *CheckpointFilepackStats `json:"filepack,omitempty"`
}

// CheckpointFilepackStats are the file pack statistics of a capture phase.
type CheckpointFilepackStats struct {
	LogicalBytes       int64 `json:"logical_bytes,omitempty"`
	EncodedChunks      int64 `json:"encoded_chunks,omitempty"`
	UnpackWrittenBytes int64 `json:"unpack_written_bytes,omitempty"`
}
