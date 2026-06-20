package api

import (
	"encoding/json"
	"time"

	"github.com/helmrdotdev/helmr/internal/compute"
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
	WorkerGroupID        string `json:"worker_group_id"`
	WorkerInstanceSecret string `json:"worker_instance_secret"`
}

type WorkerRunLeaseRequest struct {
	Capabilities WorkerCapabilities `json:"capabilities"`
}

type WorkerActivateRequest struct {
	Capabilities WorkerCapabilities `json:"capabilities"`
}

type WorkerCapabilities struct {
	ProtocolVersion         string                    `json:"protocol_version"`
	WorkerVersion           string                    `json:"worker_version,omitempty"`
	RuntimeID               string                    `json:"runtime_id"`
	RuntimeArch             string                    `json:"runtime_arch"`
	RuntimeABI              string                    `json:"runtime_abi"`
	KernelDigest            string                    `json:"kernel_digest"`
	InitramfsDigest         string                    `json:"initramfs_digest"`
	RootfsDigest            string                    `json:"rootfs_digest"`
	CNIProfile              string                    `json:"cni_profile"`
	Region                  string                    `json:"region,omitempty"`
	Labels                  map[string]string         `json:"labels,omitempty"`
	MaxVCPUs                int64                     `json:"max_vcpus"`
	MaxMemoryMiB            int64                     `json:"max_memory_mib"`
	MaxDiskMiB              int64                     `json:"max_disk_mib"`
	ExecutionSlotsAvailable int32                     `json:"execution_slots_available"`
	Network                 WorkerNetworkCapabilities `json:"network"`
}

type WorkerNetworkCapabilities struct {
	Internet      bool `json:"internet"`
	BlockInternet bool `json:"block_internet"`
	DenyCIDRs     bool `json:"deny_cidrs"`
	AllowCIDRs    bool `json:"allow_cidrs"`
	AllowDomains  bool `json:"allow_domains"`
}

type WorkerRunLeaseResponse struct {
	Lease *WorkerRunLease `json:"lease,omitempty"`
	Run   *WorkerRun      `json:"run,omitempty"`
}

type TraceContext struct {
	TraceID     string `json:"trace_id"`
	SpanID      string `json:"span_id,omitempty"`
	Traceparent string `json:"traceparent,omitempty"`
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
	WorkerGroupID    string       `json:"worker_group_id"`
	Status           WorkerStatus `json:"status"`
	ActiveExecutions int32        `json:"active_executions"`
}

type WorkerRunLease struct {
	ID                string       `json:"id"`
	OrgID             string       `json:"org_id"`
	RunID             string       `json:"run_id"`
	WorkerInstanceID  string       `json:"worker_instance_id"`
	ProtocolVersion   string       `json:"protocol_version"`
	AttemptNumber     int32        `json:"attempt_number"`
	DispatchMessageID string       `json:"dispatch_message_id,omitempty"`
	DispatchLeaseID   string       `json:"dispatch_lease_id,omitempty"`
	Trace             TraceContext `json:"trace"`
	ExpiresAt         time.Time    `json:"expires_at"`
}

type WorkerRunLeaseProvider interface {
	CurrentWorkerRunLease() WorkerRunLease
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
	ID                    string                   `json:"id"`
	Version               string                   `json:"version"`
	APIVersion            string                   `json:"api_version"`
	SDKVersion            string                   `json:"sdk_version,omitempty"`
	CLIVersion            string                   `json:"cli_version,omitempty"`
	BundleFormatVersion   int32                    `json:"bundle_format_version"`
	WorkerProtocolVersion string                   `json:"worker_protocol_version"`
	ProjectID             string                   `json:"project_id"`
	EnvironmentID         string                   `json:"environment_id"`
	DeploymentSource      DeploymentSourceArtifact `json:"deployment_source"`
}

type WorkerRun struct {
	ID                    string                         `json:"id"`
	Version               string                         `json:"version"`
	DeploymentVersion     string                         `json:"deployment_version"`
	APIVersion            string                         `json:"api_version"`
	SDKVersion            string                         `json:"sdk_version,omitempty"`
	CLIVersion            string                         `json:"cli_version,omitempty"`
	WorkerProtocolVersion string                         `json:"worker_protocol_version"`
	AttemptNumber         int32                          `json:"attempt_number"`
	AttemptID             string                         `json:"attempt_id"`
	RunLeaseID            string                         `json:"run_lease_id"`
	SnapshotVersion       int64                          `json:"snapshot_version"`
	TaskSessionID         string                         `json:"task_session_id"`
	TaskID                string                         `json:"task_id"`
	Payload               json.RawMessage                `json:"payload"`
	Secrets               ResolvedSecrets                `json:"secrets,omitempty"`
	DeploymentSource      DeploymentSourceArtifact       `json:"deployment_source"`
	DeploymentTask        WorkerDeploymentTask           `json:"deployment_task"`
	Workspace             WorkerWorkspace                `json:"workspace,omitempty"`
	Requirements          compute.RunRuntimeRequirements `json:"requirements"`
	Restore               *WorkerRestore                 `json:"restore,omitempty"`
	MaxDurationSeconds    int32                          `json:"max_duration_seconds"`
	ActiveDurationMs      int64                          `json:"active_duration_ms,omitempty"`
	Trace                 TraceContext                   `json:"trace"`
}

type WorkerDeploymentTask struct {
	ID                  string `json:"id"`
	FilePath            string `json:"file_path,omitempty"`
	ExportName          string `json:"export_name,omitempty"`
	HandlerEntrypoint   string `json:"handler_entrypoint,omitempty"`
	BundleDigest        string `json:"bundle_digest,omitempty"`
	BundleFormatVersion int32  `json:"bundle_format_version"`
}

type WorkerWorkspace struct {
	ID                string                   `json:"id,omitempty"`
	WriteLeaseID      string                   `json:"write_lease_id,omitempty"`
	WriteFencingToken string                   `json:"write_fencing_token,omitempty"`
	BaseVersionID     string                   `json:"base_version_id,omitempty"`
	MountPath         string                   `json:"mount_path,omitempty"`
	Artifact          *WorkerWorkspaceArtifact `json:"artifact,omitempty"`
}

type WorkerWorkspaceArtifact struct {
	Digest     string `json:"digest"`
	MediaType  string `json:"media_type"`
	Encoding   string `json:"encoding"`
	SizeBytes  int64  `json:"size_bytes"`
	EntryCount int32  `json:"entry_count"`
}

type ResolvedSecrets map[string][]byte

type WorkerRestore struct {
	CheckpointID string                   `json:"checkpoint_id"`
	Checkpoint   WorkerCheckpointManifest `json:"checkpoint"`
	Waitpoint    WorkerRestoreWaitpoint   `json:"waitpoint"`
}

type WorkerRestoreWaitpoint struct {
	ID                string          `json:"id"`
	RunSuspensionID   string          `json:"run_suspension_id"`
	Kind              string          `json:"kind"`
	ResumeKind        string          `json:"resume_kind"`
	ResumePayloadJSON json.RawMessage `json:"resume_payload_json"`
}

type WorkerStartRequest struct {
	Lease WorkerRunLease `json:"lease"`
}

type WorkerStartResponse struct {
	RunID  string `json:"run_id"`
	Status string `json:"status"`
}

type WorkerAcknowledgeRestoreRequest struct {
	Lease           WorkerRunLease `json:"lease"`
	RunSuspensionID string         `json:"run_suspension_id"`
	WaitpointID     string         `json:"waitpoint_id"`
	CheckpointID    string         `json:"checkpoint_id"`
}

type WorkerAcknowledgeRestoreResponse struct {
	RunID           string `json:"run_id"`
	RunSuspensionID string `json:"run_suspension_id"`
	WaitpointID     string `json:"waitpoint_id"`
	CheckpointID    string `json:"checkpoint_id"`
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
	Kind         string           `json:"kind"`
	ExitCode     *int32           `json:"exit_code,omitempty"`
	Output       json.RawMessage  `json:"output,omitempty"`
	Error        *string          `json:"error,omitempty"`
	FailureKind  *string          `json:"failure_kind,omitempty"`
	LimitSeconds *int32           `json:"limit_seconds,omitempty"`
	Usage        WorkerUsage      `json:"usage"`
	Workspace    *WorkerWorkspace `json:"workspace,omitempty"`
}

type WorkerUsage struct {
	ActiveDurationMs int64 `json:"active_duration_ms,omitempty"`
}

type WorkerReleaseResponse struct {
	RunID  string `json:"run_id"`
	Status string `json:"status"`
}

type WorkerDeploymentBuildTask struct {
	TaskID                     string                         `json:"task_id"`
	SandboxID                  string                         `json:"sandbox_id"`
	SandboxFingerprint         string                         `json:"sandbox_fingerprint"`
	SandboxImageArtifact       CASObject                      `json:"sandbox_image_artifact"`
	SandboxImageArtifactFormat string                         `json:"sandbox_image_artifact_format"`
	SandboxImageDigest         string                         `json:"sandbox_image_digest"`
	SandboxImageFormat         string                         `json:"sandbox_image_format"`
	WorkspaceMountPath         string                         `json:"workspace_mount_path"`
	FilesystemFormat           string                         `json:"filesystem_format"`
	FilePath                   string                         `json:"file_path"`
	ExportName                 string                         `json:"export_name"`
	HandlerEntrypoint          string                         `json:"handler_entrypoint"`
	BundleDigest               string                         `json:"bundle_digest"`
	BundleFormatVersion        int32                          `json:"bundle_format_version"`
	RequestedMilliCPU          int64                          `json:"requested_milli_cpu"`
	RequestedMemoryMiB         int64                          `json:"requested_memory_mib"`
	RequestedDiskMiB           int64                          `json:"requested_disk_mib"`
	Network                    compute.NetworkPolicy          `json:"network"`
	QueueName                  string                         `json:"queue_name"`
	ConcurrencyLimit           *int32                         `json:"concurrency_limit,omitempty"`
	TTL                        string                         `json:"ttl,omitempty"`
	MaxDurationSeconds         int32                          `json:"max_duration_seconds"`
	RetryPolicy                json.RawMessage                `json:"retry_policy,omitempty"`
	Secrets                    []SecretDeclaration            `json:"secrets,omitempty"`
	Schedules                  []WorkerDeploymentTaskSchedule `json:"schedules,omitempty"`
}

type SecretDeclaration struct {
	Name  string `json:"name"`
	Env   string `json:"env,omitempty"`
	File  string `json:"file,omitempty"`
	Dir   string `json:"dir,omitempty"`
	Mode  string `json:"mode,omitempty"`
	Owner string `json:"owner,omitempty"`
}

type WorkerDeploymentTaskSchedule struct {
	ID       string `json:"id,omitempty"`
	Cron     string `json:"cron"`
	Timezone string `json:"timezone,omitempty"`
	Active   *bool  `json:"active,omitempty"`
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

type WorkerWriteOutputRequest struct {
	Lease       WorkerRunLease  `json:"lease"`
	Channel     string          `json:"channel"`
	Payload     json.RawMessage `json:"payload"`
	ContentType string          `json:"content_type,omitempty"`
	ObjectRef   json.RawMessage `json:"object_ref,omitempty"`
}

type WorkerUpdateRunMetadataRequest struct {
	Lease     WorkerRunLease  `json:"lease"`
	Operation string          `json:"operation"`
	Key       string          `json:"key,omitempty"`
	Value     json.RawMessage `json:"value,omitempty"`
	Patch     json.RawMessage `json:"patch,omitempty"`
	Amount    float64         `json:"amount,omitempty"`
}

type WorkerEventResponse struct {
	RunID string `json:"run_id"`
}

type WorkerCreateWaitpointTokenRequest struct {
	Lease WorkerRunLease `json:"lease"`
	CreateWaitpointTokenRequest
}

type WorkerWaitpointKind string

const (
	WorkerWaitpointKindToken   WorkerWaitpointKind = "token"
	WorkerWaitpointKindTimer   WorkerWaitpointKind = "timer"
	WorkerWaitpointKindChannel WorkerWaitpointKind = "channel"
)

type WorkerCreateWaitpointRequest struct {
	Lease          WorkerRunLease      `json:"lease"`
	CorrelationID  string              `json:"correlation_id"`
	Kind           WorkerWaitpointKind `json:"kind"`
	Params         json.RawMessage     `json:"params,omitempty"`
	Metadata       json.RawMessage     `json:"metadata,omitempty"`
	Tags           []string            `json:"tags,omitempty"`
	TimeoutSeconds *int32              `json:"timeout_seconds,omitempty"`
	Ordinal        int32               `json:"ordinal,omitempty"`
}

type WorkerCreateWaitpointResponse struct {
	RunID           string          `json:"run_id"`
	RunSuspensionID string          `json:"run_suspension_id"`
	WaitpointID     string          `json:"waitpoint_id"`
	CheckpointID    string          `json:"checkpoint_id"`
	ResolutionKind  string          `json:"resolution_kind,omitempty"`
	Resolution      json.RawMessage `json:"resolution,omitempty"`
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
	Backend         string  `json:"backend"`
	ID              string  `json:"id"`
	Arch            string  `json:"arch"`
	ABI             string  `json:"abi"`
	KernelDigest    string  `json:"kernel_digest"`
	InitramfsDigest string  `json:"initramfs_digest"`
	RootfsDigest    string  `json:"rootfs_digest"`
	ConfigDigest    string  `json:"config_digest"`
	ImageKey        *string `json:"image_key,omitempty"`
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
	ArtifactDigest    string `json:"artifact_digest"`
	ArtifactSizeBytes int64  `json:"artifact_size_bytes"`
	ArtifactMediaType string `json:"artifact_media_type"`
	ArtifactEncoding  string `json:"artifact_encoding"`
	MountPath         string `json:"mount_path"`
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
	RunSuspensionID  string                   `json:"run_suspension_id"`
	WaitpointID      string                   `json:"waitpoint_id"`
	CheckpointID     string                   `json:"checkpoint_id"`
	ActiveDurationMs int64                    `json:"active_duration_ms"`
	Manifest         WorkerCheckpointManifest `json:"manifest"`
}

type WorkerCheckpointFailedRequest struct {
	Lease           WorkerRunLease `json:"lease"`
	RunSuspensionID string         `json:"run_suspension_id"`
	WaitpointID     string         `json:"waitpoint_id"`
	CheckpointID    string         `json:"checkpoint_id"`
	Error           string         `json:"error"`
}
