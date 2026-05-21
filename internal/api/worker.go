package api

import (
	"encoding/json"
	"time"
)

type WorkerTokenRequest struct {
	WorkerHostID string `json:"worker_host_id"`
	WorkerSecret string `json:"worker_secret"`
}

type WorkerTokenResponse struct {
	Token            string `json:"token"`
	ExpiresInSeconds int64  `json:"expires_in_seconds"`
}

type WorkerRegisterRequest struct {
	RegistrationToken string             `json:"registration_token"`
	ExternalID        string             `json:"external_id,omitempty"`
	Capabilities      WorkerCapabilities `json:"capabilities,omitempty"`
}

type WorkerRegisterResponse struct {
	WorkerHostID string `json:"worker_host_id"`
	WorkerSecret string `json:"worker_secret"`
}

type RevokeWorkerCredentialsResponse struct {
	Revoked int64 `json:"revoked"`
}

type WorkerRunLeaseRequest struct {
	Capabilities WorkerCapabilities `json:"capabilities"`
}

type WorkerActivateRequest struct {
	Capabilities WorkerCapabilities `json:"capabilities"`
}

type WorkerCapabilities struct {
	RuntimeArch             string            `json:"runtime_arch"`
	RuntimeABI              string            `json:"runtime_abi"`
	KernelDigest            string            `json:"kernel_digest"`
	RootfsDigest            string            `json:"rootfs_digest"`
	CNIProfile              string            `json:"cni_profile"`
	Region                  string            `json:"region,omitempty"`
	Labels                  map[string]string `json:"labels,omitempty"`
	MaxVCPUs                int64             `json:"max_vcpus"`
	MaxMemoryMiB            int64             `json:"max_memory_mib"`
	MaxDiskMiB              int64             `json:"max_disk_mib"`
	ExecutionSlotsAvailable int32             `json:"execution_slots_available"`
}

type WorkerRunLeaseResponse struct {
	Lease *WorkerRunLease `json:"lease,omitempty"`
	Run   *WorkerRun      `json:"run,omitempty"`
}

type WorkerStatus string

const (
	WorkerStatusActive   WorkerStatus = "active"
	WorkerStatusDraining WorkerStatus = "draining"
)

type WorkerStatusResponse struct {
	WorkerHostID     string       `json:"worker_host_id"`
	Status           WorkerStatus `json:"status"`
	ActiveExecutions int32        `json:"active_executions"`
}

type WorkerRunLease struct {
	ID             string    `json:"id"`
	RunID          string    `json:"run_id"`
	WorkerHostID   string    `json:"worker_host_id"`
	QueueMessageID string    `json:"queue_message_id,omitempty"`
	QueueLeaseID   string    `json:"queue_lease_id,omitempty"`
	ExpiresAt      time.Time `json:"expires_at"`
}

type WorkerRun struct {
	ID                     string               `json:"id"`
	TaskID                 string               `json:"task_id"`
	Payload                json.RawMessage      `json:"payload"`
	Secrets                ResolvedSecrets      `json:"secrets,omitempty"`
	TaskSource             TaskSourceArtifact   `json:"task_source"`
	Workspace              GitHubSource         `json:"workspace"`
	DeploymentTask         WorkerDeploymentTask `json:"deployment_task"`
	WorkspaceCheckoutToken *WorkerCheckoutToken `json:"workspace_checkout_token,omitempty"`
	Restore                *WorkerRestore       `json:"restore,omitempty"`
	MaxDurationSeconds     int32                `json:"max_duration_seconds"`
	ActiveDurationMs       int64                `json:"active_duration_ms,omitempty"`
}

type WorkerDeploymentTask struct {
	ID         string `json:"id"`
	ModulePath string `json:"module_path,omitempty"`
	ExportName string `json:"export_name,omitempty"`
}

type ResolvedSecrets map[string][]byte

type WorkerRestore struct {
	CheckpointID string                   `json:"checkpoint_id"`
	Checkpoint   WorkerCheckpointManifest `json:"checkpoint"`
	Waitpoint    WorkerRestoreWaitpoint   `json:"waitpoint"`
}

type WorkerRestoreWaitpoint struct {
	ID                    string          `json:"id"`
	Kind                  string          `json:"kind"`
	ResolutionKind        string          `json:"resolution_kind"`
	ResolutionPayloadJSON json.RawMessage `json:"resolution_payload_json"`
}

type WorkerCheckoutToken struct {
	Token     string    `json:"token"`
	ExpiresAt time.Time `json:"expires_at"`
}

type WorkerStartRequest struct {
	Lease WorkerRunLease `json:"lease"`
}

type WorkerStartResponse struct {
	RunID  string `json:"run_id"`
	Status string `json:"status"`
}

type WorkerRenewRequest struct {
	Lease WorkerRunLease `json:"lease"`
}

type WorkerRenewResponse struct {
	Lease WorkerRunLease `json:"lease"`
}

type WorkerReleaseRequest struct {
	Lease  WorkerRunLease      `json:"lease"`
	Result WorkerReleaseResult `json:"result"`
}

type WorkerReleaseResult struct {
	Kind         string          `json:"kind"`
	ExitCode     *int32          `json:"exit_code,omitempty"`
	Output       json.RawMessage `json:"output,omitempty"`
	Error        *string         `json:"error,omitempty"`
	FailureKind  *string         `json:"failure_kind,omitempty"`
	LimitSeconds *int32          `json:"limit_seconds,omitempty"`
}

type WorkerReleaseResponse struct {
	RunID  string `json:"run_id"`
	Status string `json:"status"`
}

type WorkerLogStream string

const (
	WorkerLogStreamStdout WorkerLogStream = "stdout"
	WorkerLogStreamStderr WorkerLogStream = "stderr"
)

type WorkerAppendLogRequest struct {
	Lease         WorkerRunLease  `json:"lease"`
	Stream        WorkerLogStream `json:"stream"`
	ObservedSeq   uint64          `json:"observed_seq"`
	ContentBase64 string          `json:"content_base64"`
}

type WorkerRecordLogEntryRequest struct {
	Lease WorkerRunLease `json:"lease"`
	Entry string         `json:"entry"`
}

type WorkerEmitEventRequest struct {
	Lease     WorkerRunLease  `json:"lease"`
	EventType string          `json:"event_type"`
	Content   json.RawMessage `json:"content"`
}

type WorkerEventResponse struct {
	RunID string `json:"run_id"`
}

type WorkerWaitpointKind string

const (
	WorkerWaitpointKindApproval WorkerWaitpointKind = "approval"
	WorkerWaitpointKindMessage  WorkerWaitpointKind = "message"
)

type WorkerCreateWaitpointRequest struct {
	Lease          WorkerRunLease      `json:"lease"`
	CorrelationID  string              `json:"correlation_id"`
	Kind           WorkerWaitpointKind `json:"kind"`
	Request        json.RawMessage     `json:"request"`
	DisplayText    string              `json:"display_text"`
	TimeoutSeconds *int32              `json:"timeout_seconds,omitempty"`
	Policy         string              `json:"policy,omitempty"`
}

type WorkerCreateWaitpointResponse struct {
	RunID        string `json:"run_id"`
	WaitpointID  string `json:"waitpoint_id"`
	CheckpointID string `json:"checkpoint_id"`
}

type WorkerCheckpointManifest struct {
	RuntimeBackend       string          `json:"runtime_backend"`
	RuntimeArch          string          `json:"runtime_arch"`
	RuntimeABI           string          `json:"runtime_abi"`
	KernelDigest         *string         `json:"kernel_digest,omitempty"`
	RootfsDigest         *string         `json:"rootfs_digest,omitempty"`
	ImageKey             *string         `json:"image_key,omitempty"`
	RuntimeConfigDigest  *string         `json:"runtime_config_digest,omitempty"`
	ManifestDigest       *string         `json:"manifest_digest,omitempty"`
	VMStateDigest        *string         `json:"vm_state_digest,omitempty"`
	WorkspaceUpperDigest *string         `json:"workspace_upper_digest,omitempty"`
	MemoryDigests        []string        `json:"memory_digests,omitempty"`
	CASObjects           []CASObject     `json:"cas_objects,omitempty"`
	Manifest             json.RawMessage `json:"manifest,omitempty"`
}

type CASObject struct {
	Digest    string `json:"digest"`
	SizeBytes int64  `json:"size_bytes"`
	MediaType string `json:"media_type"`
}

type WorkerCheckpointReadyRequest struct {
	Lease            WorkerRunLease           `json:"lease"`
	WaitpointID      string                   `json:"waitpoint_id"`
	CheckpointID     string                   `json:"checkpoint_id"`
	ActiveDurationMs int64                    `json:"active_duration_ms"`
	Manifest         WorkerCheckpointManifest `json:"manifest"`
}

type WorkerCheckpointFailedRequest struct {
	Lease        WorkerRunLease `json:"lease"`
	WaitpointID  string         `json:"waitpoint_id"`
	CheckpointID string         `json:"checkpoint_id"`
	Error        string         `json:"error"`
}
