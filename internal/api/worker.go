package api

import (
	"encoding/json"
	"time"
)

type WorkerTokenRequest struct {
	WorkerInstanceID     string `json:"worker_instance_id"`
	WorkerInstanceSecret string `json:"worker_instance_secret"`
}

type WorkerTokenResponse struct {
	Token            string `json:"token"`
	ExpiresInSeconds int64  `json:"expires_in_seconds"`
}

type WorkerRegisterRequest struct {
	BootstrapToken string `json:"bootstrap_token"`
	ResourceID     string `json:"resource_id,omitempty"`
}

type WorkerRegisterResponse struct {
	WorkerInstanceID     string `json:"worker_instance_id"`
	WorkerInstanceSecret string `json:"worker_instance_secret"`
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

type WorkerDeploymentBuildLeaseRequest struct {
	Capabilities WorkerCapabilities `json:"capabilities"`
}

type WorkerDeploymentBuildLeaseResponse struct {
	Lease      *WorkerDeploymentBuildLease `json:"lease,omitempty"`
	Deployment *WorkerDeploymentBuild      `json:"deployment,omitempty"`
}

type WorkerStatus string

const (
	WorkerStatusActive   WorkerStatus = "active"
	WorkerStatusDraining WorkerStatus = "draining"
)

type WorkerStatusResponse struct {
	WorkerInstanceID string       `json:"worker_instance_id"`
	Status           WorkerStatus `json:"status"`
	ActiveExecutions int32        `json:"active_executions"`
}

type WorkerRunLease struct {
	ID                string    `json:"id"`
	OrgID             string    `json:"org_id"`
	RunID             string    `json:"run_id"`
	WorkerInstanceID  string    `json:"worker_instance_id"`
	DispatchMessageID string    `json:"dispatch_message_id,omitempty"`
	DispatchLeaseID   string    `json:"dispatch_lease_id,omitempty"`
	ExpiresAt         time.Time `json:"expires_at"`
}

type WorkerDeploymentBuildLease struct {
	ID               string    `json:"id"`
	OrgID            string    `json:"org_id"`
	ProjectID        string    `json:"project_id"`
	EnvironmentID    string    `json:"environment_id"`
	DeploymentID     string    `json:"deployment_id"`
	WorkerInstanceID string    `json:"worker_instance_id"`
	ExpiresAt        time.Time `json:"expires_at"`
}

type WorkerDeploymentBuild struct {
	ID               string                   `json:"id"`
	ProjectID        string                   `json:"project_id"`
	EnvironmentID    string                   `json:"environment_id"`
	DeploymentSource DeploymentSourceArtifact `json:"deployment_source"`
}

type WorkerRun struct {
	ID                     string                   `json:"id"`
	TaskID                 string                   `json:"task_id"`
	Payload                json.RawMessage          `json:"payload"`
	Secrets                ResolvedSecrets          `json:"secrets,omitempty"`
	DeploymentSource       DeploymentSourceArtifact `json:"deployment_source"`
	Workspace              GitHubSource             `json:"workspace"`
	DeploymentTask         WorkerDeploymentTask     `json:"deployment_task"`
	WorkspaceCheckoutToken *WorkerCheckoutToken     `json:"workspace_checkout_token,omitempty"`
	Restore                *WorkerRestore           `json:"restore,omitempty"`
	MaxDurationSeconds     int32                    `json:"max_duration_seconds"`
	ActiveDurationMs       int64                    `json:"active_duration_ms,omitempty"`
}

type WorkerDeploymentTask struct {
	ID                string `json:"id"`
	FilePath          string `json:"file_path,omitempty"`
	ExportName        string `json:"export_name,omitempty"`
	HandlerEntrypoint string `json:"handler_entrypoint,omitempty"`
	BundleDigest      string `json:"bundle_digest,omitempty"`
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

type WorkerAcknowledgeRestoreRequest struct {
	Lease        WorkerRunLease `json:"lease"`
	WaitpointID  string         `json:"waitpoint_id"`
	CheckpointID string         `json:"checkpoint_id"`
}

type WorkerAcknowledgeRestoreResponse struct {
	RunID        string `json:"run_id"`
	WaitpointID  string `json:"waitpoint_id"`
	CheckpointID string `json:"checkpoint_id"`
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

type WorkerDeploymentBuildTask struct {
	TaskID             string `json:"task_id"`
	FilePath           string `json:"file_path"`
	ExportName         string `json:"export_name"`
	HandlerEntrypoint  string `json:"handler_entrypoint"`
	BundleDigest       string `json:"bundle_digest"`
	RequestedMilliCPU  int64  `json:"requested_milli_cpu"`
	RequestedMemoryMiB int64  `json:"requested_memory_mib"`
	MaxDurationSeconds int32  `json:"max_duration_seconds"`
}

type WorkerDeploymentBuildResult struct {
	BuildManifestDigest      string                      `json:"build_manifest_digest"`
	DeploymentManifestDigest string                      `json:"deployment_manifest_digest"`
	Tasks                    []WorkerDeploymentBuildTask `json:"tasks"`
	CASObjects               []CASObject                 `json:"cas_objects,omitempty"`
	Error                    *string                     `json:"error,omitempty"`
}

type WorkerCompleteDeploymentBuildRequest struct {
	Lease  WorkerDeploymentBuildLease  `json:"lease"`
	Result WorkerDeploymentBuildResult `json:"result"`
}

type WorkerDeploymentBuildResponse struct {
	DeploymentID string `json:"deployment_id"`
	Status       string `json:"status"`
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
	WorkerWaitpointKindToken WorkerWaitpointKind = "token"
	WorkerWaitpointKindDelay WorkerWaitpointKind = "delay"
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
	RecoveryPoint  WorkerCheckpointRecoveryPoint  `json:"recovery_point"`
	RuntimeState   WorkerCheckpointRuntimeState   `json:"runtime_state"`
	WorkspaceState WorkerCheckpointWorkspaceState `json:"workspace_state"`
	Phases         []WorkerCheckpointPhase        `json:"phases,omitempty"`
}

type WorkerCheckpointRecoveryPoint struct {
	ID          string                  `json:"id,omitempty"`
	RunID       string                  `json:"run_id,omitempty"`
	WaitpointID string                  `json:"waitpoint_id,omitempty"`
	Runtime     WorkerCheckpointRuntime `json:"runtime"`
}

type WorkerCheckpointRuntime struct {
	Backend      string  `json:"backend"`
	Arch         string  `json:"arch"`
	ABI          string  `json:"abi"`
	KernelDigest string  `json:"kernel_digest"`
	RootfsDigest string  `json:"rootfs_digest"`
	ConfigDigest string  `json:"config_digest"`
	ImageKey     *string `json:"image_key,omitempty"`
}

type WorkerCheckpointRuntimeState struct {
	ConfigArtifact      WorkerCheckpointArtifact   `json:"config_artifact"`
	VMStateArtifact     WorkerCheckpointArtifact   `json:"vm_state_artifact"`
	ScratchDiskArtifact WorkerCheckpointArtifact   `json:"scratch_disk_artifact"`
	MemoryArtifacts     []WorkerCheckpointArtifact `json:"memory_artifacts,omitempty"`
	Config              json.RawMessage            `json:"config,omitempty"`
}

type WorkerCheckpointWorkspaceState struct {
	Base WorkerCheckpointWorkspaceBase `json:"base"`
}

type WorkerCheckpointWorkspaceBase struct {
	Kind              string        `json:"kind"`
	Repository        string        `json:"repository,omitempty"`
	Ref               string        `json:"ref,omitempty"`
	SHA               string        `json:"sha,omitempty"`
	Subpath           string        `json:"subpath,omitempty"`
	RefKind           GitHubRefKind `json:"ref_kind,omitempty"`
	RefName           string        `json:"ref_name,omitempty"`
	FullRef           string        `json:"full_ref,omitempty"`
	DefaultBranch     string        `json:"default_branch,omitempty"`
	ArtifactDigest    string        `json:"artifact_digest"`
	ArtifactMediaType string        `json:"artifact_media_type"`
	ArtifactEncoding  string        `json:"artifact_encoding"`
	MountPath         string        `json:"mount_path"`
	VolumeKind        string        `json:"volume_kind"`
}

type WorkerCheckpointArtifact struct {
	Digest            string `json:"digest"`
	SizeBytes         int64  `json:"size_bytes"`
	MediaType         string `json:"media_type"`
	EncryptDurationMs int64  `json:"encrypt_duration_ms,omitempty"`
	StoreDurationMs   int64  `json:"store_duration_ms,omitempty"`
}

type WorkerCheckpointPhase struct {
	Name       string `json:"name"`
	DurationMs int64  `json:"duration_ms"`
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
