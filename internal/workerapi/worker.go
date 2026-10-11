package workerapi

import (
	"github.com/helmrdotdev/helmr/internal/vmplatform"
	"time"
)

const WorkerObservationInterval = 30 * time.Second

type Status string

const (
	StatusActive           Status = "active"
	StatusDraining         Status = "draining"
	StatusTerminationReady Status = "termination_ready"
)

type LogStream string

const (
	LogStreamStdout LogStream = "stdout"
	LogStreamStderr LogStream = "stderr"
)

type HostCredentialRequest struct {
	APIVersion       string `json:"api_version"`
	WorkerHostID     string `json:"worker_host_id"`
	WorkerHostSecret string `json:"worker_host_secret"`
	ServiceID        string `json:"service_id"`
}

type HostCredentialResponse struct {
	Credential       string `json:"credential"`
	ExpiresInSeconds int64  `json:"expires_in_seconds"`
	WorkerEpoch      int64  `json:"worker_epoch"`
}

type EnrollmentResponse struct {
	WorkerHostID     string `json:"worker_host_id"`
	WorkerGroupID    string `json:"worker_group_id"`
	WorkerPoolID     string `json:"worker_pool_id"`
	WorkerHostSecret string `json:"worker_host_secret"`
}

type EnrollmentRequest struct {
	APIVersion string `json:"api_version"`
	ResourceID string `json:"resource_id"`
	PoolName   string `json:"pool_name"`
}

type ActivateRequest struct {
	APIVersion   string       `json:"api_version"`
	Capabilities Capabilities `json:"capabilities"`
}

type ObserveRequest struct {
	Observation Observation `json:"observation"`
}

// Startup recovery requires complete guest, jailer, process and network ownership
// enumeration. Quarantined Instance IDs do not establish physical reclamation.
type StartupRecoveryRequest struct {
	Quarantined []string `json:"quarantined"`
}

type Observation struct {
	RunPausedReason string `json:"run_paused_reason,omitempty"`
	VMPausedReason  string `json:"vm_paused_reason,omitempty"`
}

type Capabilities struct {
	Runtime                   vmplatform.Profile    `json:"runtime"`
	CPUShapes                 []vmplatform.CPUShape `json:"cpu_shapes"`
	CPUEnvironment            CPUEnvironment        `json:"cpu_environment"`
	MaxVCPUs                  int64                 `json:"max_vcpus"`
	MaxMemoryMiB              int64                 `json:"max_memory_mib"`
	VMMilliCPU                int64                 `json:"vm_milli_cpu"`
	VMMemoryMiB               int64                 `json:"vm_memory_mib"`
	GuestEphemeralDiskBytes   int64                 `json:"guest_ephemeral_disk_bytes"`
	VMGuestEphemeralDiskBytes int64                 `json:"vm_guest_ephemeral_disk_bytes"`
	ExecutionSlotsAvailable   int32                 `json:"execution_slots_available"`
}

type CPUEnvironment struct {
	Digest             string `json:"digest"`
	FirecrackerVersion string `json:"firecracker_version"`
	HostKernelRelease  string `json:"host_kernel_release"`
	MicrocodeVersion   string `json:"microcode_version"`
	BIOSVersion        string `json:"bios_version"`
	BIOSRevision       string `json:"bios_revision"`
}

type StatusResponse struct {
	WorkerHostID    string    `json:"worker_host_id"`
	WorkerGroupID   string    `json:"worker_group_id"`
	Status          Status    `json:"status"`
	ActiveInstances int32     `json:"active_instances"`
	Readiness       Readiness `json:"readiness"`
}

type Readiness struct {
	Run      *RoleReadiness `json:"run,omitempty"`
	Instance *RoleReadiness `json:"instance,omitempty"`
}

type RoleReadiness struct {
	Ready        bool   `json:"ready"`
	PausedReason string `json:"paused_reason,omitempty"`
}

// FenceReasonProviderTermination reports provider termination already in progress.
const FenceReasonProviderTermination = "provider_termination"

type FenceRequest struct {
	ReasonCode string `json:"reason_code"`
}

type RuntimeProgram struct {
	DeploymentID string    `json:"deployment_id"`
	Runtime      CASObject `json:"runtime"`
	Artifact     CASObject `json:"artifact"`
	IndexDigest  string    `json:"index_digest"`
}

type SecretDelivery struct {
	Env   *SecretEnv  `json:"env,omitempty"`
	File  *SecretFile `json:"file,omitempty"`
	Value []byte      `json:"value"`
}

type SecretEnv struct {
	Name string `json:"name"`
}

type SecretFile struct {
	Path string `json:"path"`
}

type CASObject struct {
	Digest    string `json:"digest"`
	SizeBytes int64  `json:"size_bytes"`
	MediaType string `json:"media_type"`
}
