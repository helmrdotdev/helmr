package api

import (
	"encoding/json"
	"time"

	"github.com/helmrdotdev/helmr/internal/compute"
)

type WorkerTokenRequest struct {
	WorkerInstanceID     string `json:"worker_instance_id"`
	WorkerInstanceSecret string `json:"worker_instance_secret"`
	ServiceID            string `json:"service_id"`
	ProtocolVersion      string `json:"protocol_version"`
	SupportsRun          bool   `json:"supports_run"`
	SupportsBuild        bool   `json:"supports_build"`
}

type WorkerTokenResponse struct {
	Token            string   `json:"token"`
	ExpiresInSeconds int64    `json:"expires_in_seconds"`
	WorkerEpoch      int64    `json:"worker_epoch"`
	Roles            []string `json:"roles"`
	ProtocolVersion  string   `json:"protocol_version"`
}

type WorkerEnrollmentResponse struct {
	WorkerInstanceID     string `json:"worker_instance_id"`
	WorkerGroupID        string `json:"worker_group_id"`
	WorkerInstanceSecret string `json:"worker_instance_secret"`
}

type WorkerEnrollmentChallengeRequest struct {
	WorkerGroupID string `json:"worker_group_id"`
}

type WorkerEnrollmentChallengeResponse struct {
	Nonce           string    `json:"nonce"`
	WorkerGroupID   string    `json:"worker_group_id"`
	ExpiresAt       time.Time `json:"expires_at"`
	ProtocolVersion string    `json:"protocol_version"`
}

type WorkerEnrollmentRequest struct {
	WorkerGroupID            string            `json:"worker_group_id"`
	Nonce                    string            `json:"nonce,omitempty"`
	InstanceIdentityDocument json.RawMessage   `json:"instance_identity_document,omitempty"`
	SignedSTSRequest         SignedHTTPRequest `json:"signed_sts_request"`
	SupportsRun              bool              `json:"supports_run"`
	SupportsBuild            bool              `json:"supports_build"`
	ProtocolVersion          string            `json:"protocol_version"`
}

type SignedHTTPRequest struct {
	Method  string              `json:"method"`
	URL     string              `json:"url"`
	Headers map[string][]string `json:"headers"`
	Body    string              `json:"body"`
}

type WorkerRunLeaseDiscoveryRequest struct{}

const (
	WorkerRunLeaseMinTTL              = 30 * time.Second
	WorkerRunFinalizationMinTTL       = 20 * time.Minute
	WorkerRunFinalizationTerminalTail = 10 * time.Minute
	WorkerRunFinalizationReplayTail   = 30 * time.Second
)

type WorkerRunLeaseWork struct {
	LeaseID       string `json:"lease_id"`
	LeaseSequence int64  `json:"lease_sequence"`
}

type WorkerRunLeaseDiscoveryResponse struct {
	Items []WorkerRunLeaseWork `json:"items"`
}

type WorkerActivateRequest struct {
	Capabilities             WorkerCapabilities `json:"capabilities"`
	CertificationProfile     string             `json:"certification_profile"`
	CertificationFingerprint string             `json:"certification_fingerprint"`
}

type WorkerObserveRequest struct {
	Observation WorkerObservation `json:"observation"`
}

type WorkerCertificationRenewRequest struct {
	Capabilities WorkerCapabilities `json:"capabilities"`
}

type WorkerStartupRecoveryRequest struct {
	InventoryComplete bool      `json:"inventory_complete"`
	InventoryScope    string    `json:"inventory_scope"`
	ObservedAt        time.Time `json:"observed_at"`
	Inventory         []string  `json:"inventory"`
	Reclaimed         []string  `json:"reclaimed,omitempty"`
	Quarantined       []string  `json:"quarantined,omitempty"`
	Errors            []string  `json:"errors,omitempty"`
}

// WorkerDrainCompletionRequest is the worker's proof that a server-directed
// drain has removed both durable execution authority and local runtime state.
// The control plane must treat an identical proof as idempotent.
type WorkerDrainCompletionRequest struct {
	InventoryComplete bool      `json:"inventory_complete"`
	InventoryScope    string    `json:"inventory_scope"`
	ObservedAt        time.Time `json:"observed_at"`
	Inventory         []string  `json:"inventory"`
	Reclaimed         []string  `json:"reclaimed,omitempty"`
	Quarantined       []string  `json:"quarantined,omitempty"`
	Errors            []string  `json:"errors,omitempty"`
}

type WorkerNetworkFacts struct {
	HostInterfaceName string `json:"host_interface_name"`
	GuestAddress      string `json:"guest_address"`
	GatewayAddress    string `json:"gateway_address"`
	Subnet            string `json:"subnet"`
	TapName           string `json:"tap_name"`
	NetNSName         string `json:"netns_name"`
	GuestMAC          string `json:"guest_mac"`
}

type WorkerObservation struct {
	CPUPressureBPS           int32           `json:"cpu_pressure_bps"`
	MemoryPressureBPS        int32           `json:"memory_pressure_bps"`
	WorkloadDiskPressureBPS  int32           `json:"workload_disk_pressure_bps"`
	ScratchPressureBPS       int32           `json:"scratch_pressure_bps"`
	BuildCachePressureBPS    int32           `json:"build_cache_pressure_bps"`
	ArtifactCachePressureBPS int32           `json:"artifact_cache_pressure_bps"`
	CheckpointPressureBPS    int32           `json:"checkpoint_pressure_bps"`
	LeakedSlotCount          int32           `json:"leaked_slot_count"`
	RunQueueDepth            int32           `json:"run_queue_depth"`
	BuildQueueDepth          int32           `json:"build_queue_depth"`
	RuntimeStartQueueDepth   int32           `json:"runtime_start_queue_depth"`
	RunPausedReason          string          `json:"run_paused_reason,omitempty"`
	BuildPausedReason        string          `json:"build_paused_reason,omitempty"`
	RuntimePausedReason      string          `json:"runtime_paused_reason,omitempty"`
	HealthDetails            json.RawMessage `json:"health_details,omitempty"`
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
	SubstrateFormat         string                    `json:"substrate_format,omitempty"`
	SubstrateBuilderABI     string                    `json:"substrate_builder_abi,omitempty"`
	SubstrateLayoutABI      string                    `json:"substrate_layout_abi,omitempty"`
	Region                  string                    `json:"region,omitempty"`
	Labels                  map[string]string         `json:"labels,omitempty"`
	MaxVCPUs                int64                     `json:"max_vcpus"`
	MaxMemoryMiB            int64                     `json:"max_memory_mib"`
	VMMilliCPU              int64                     `json:"vm_milli_cpu"`
	VMMemoryMiB             int64                     `json:"vm_memory_mib"`
	MaxDiskMiB              int64                     `json:"max_disk_mib"`
	VMMaxDiskMiB            int64                     `json:"vm_max_disk_mib"`
	ExecutionSlotsAvailable int32                     `json:"execution_slots_available"`
	SupportsRun             bool                      `json:"supports_run"`
	SupportsBuild           bool                      `json:"supports_build"`
	ToolchainCatalogDigest  string                    `json:"toolchain_catalog_digest,omitempty"`
	MaxBuildExecutors       int32                     `json:"max_build_executors"`
	MaxRuntimeStarts        int32                     `json:"max_runtime_starts"`
	ScratchBytes            int64                     `json:"scratch_bytes"`
	VMMaxScratchBytes       int64                     `json:"vm_max_scratch_bytes"`
	BuildCacheBytes         int64                     `json:"build_cache_bytes"`
	ArtifactCacheBytes      int64                     `json:"artifact_cache_bytes"`
	HugepagesBytes          int64                     `json:"hugepages_bytes"`
	CheckpointBytes         int64                     `json:"checkpoint_bytes"`
	Observation             WorkerObservation         `json:"observation"`
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

type WorkerDeploymentBuildLeaseRequest struct{}

type WorkerDeploymentBuildLeaseResponse struct {
	Lease      *WorkerDeploymentBuildLease `json:"lease,omitempty"`
	Deployment *WorkerDeploymentBuild      `json:"deployment,omitempty"`
}

type WorkerDeploymentBuildStartRequest struct {
	Lease WorkerDeploymentBuildLease `json:"lease"`
}

type WorkerDeploymentBuildStartResponse struct {
	Lease WorkerDeploymentBuildLease `json:"lease"`
}

type WorkerDeploymentBuildRenewRequest struct {
	Lease WorkerDeploymentBuildLease `json:"lease"`
}

type WorkerDeploymentBuildRenewResponse struct {
	Lease WorkerDeploymentBuildLease `json:"lease"`
}

type WorkerDeploymentBuildRejectRequest struct {
	Lease      WorkerDeploymentBuildLease `json:"lease"`
	ReasonCode string                     `json:"reason_code"`
	Error      json.RawMessage            `json:"error,omitempty"`
}

type WorkerDeploymentBuildDeliveryFailureReason string

const (
	WorkerDeploymentBuildDeliveryBuildGuestFailed      WorkerDeploymentBuildDeliveryFailureReason = "build_guest_failed"
	WorkerDeploymentBuildDeliveryProgramVerifierFailed WorkerDeploymentBuildDeliveryFailureReason = "program_verifier_failed"
)

type WorkerDeploymentBuildDeliveryFailureRequest struct {
	Lease      WorkerDeploymentBuildLease                 `json:"lease"`
	ReasonCode WorkerDeploymentBuildDeliveryFailureReason `json:"reasonCode"`
}

type WorkerStatus string

const (
	WorkerStatusActive   WorkerStatus = "active"
	WorkerStatusDraining WorkerStatus = "draining"
	WorkerStatusDisabled WorkerStatus = "disabled"
)

type WorkerStatusResponse struct {
	WorkerInstanceID string       `json:"worker_instance_id"`
	WorkerGroupID    string       `json:"worker_group_id"`
	Status           WorkerStatus `json:"status"`
	ActiveExecutions int32        `json:"active_executions"`
}

type WorkerFenceRequest struct {
	ReasonCode string `json:"reason_code"`
}

type WorkerRuntimeInstance struct {
	ID                     string     `json:"id"`
	OrgID                  string     `json:"org_id"`
	ProjectID              string     `json:"project_id"`
	EnvironmentID          string     `json:"environment_id"`
	WorkerInstanceID       string     `json:"worker_instance_id"`
	RuntimeEpoch           int64      `json:"runtime_epoch"`
	RuntimeID              string     `json:"runtime_id"`
	DeploymentDefinitionID string     `json:"deployment_definition_id"`
	State                  string     `json:"state"`
	ReservedCpuMillis      int32      `json:"reserved_cpu_millis"`
	ReservedMemoryMiB      int32      `json:"reserved_memory_mib"`
	ReservedDiskMiB        int64      `json:"reserved_disk_mib"`
	ReservedExecutionSlots int32      `json:"reserved_execution_slots"`
	WorkspaceMountID       string     `json:"workspace_mount_id,omitempty"`
	ExpiresAt              *time.Time `json:"expires_at,omitempty"`
}

type WorkerRuntimeSource struct {
	DeploymentDefinitionID string                  `json:"deployment_definition_id"`
	WorkspaceID            string                  `json:"workspace_id"`
	RuntimeID              string                  `json:"runtime_id"`
	WorkspaceImage         CASObject               `json:"workspace_image"`
	WorkspaceArchitecture  string                  `json:"workspace_architecture"`
	BaseVersionID          string                  `json:"base_version_id"`
	WorkspaceArtifact      WorkerWorkspaceArtifact `json:"workspace_artifact"`
	RootfsDigest           string                  `json:"rootfs_digest"`
	ReservedCpuMillis      int32                   `json:"reserved_cpu_millis"`
	ReservedMemoryMiB      int32                   `json:"reserved_memory_mib"`
	ReservedDiskMiB        int64                   `json:"reserved_disk_mib"`
	ReservedExecutionSlots int32                   `json:"reserved_execution_slots"`
	RuntimeABI             string                  `json:"runtime_abi"`
	Network                compute.NetworkPolicy   `json:"network"`
	RuntimeSubstrate       *WorkerRuntimeSubstrate `json:"runtime_substrate,omitempty"`
	Program                *WorkerRuntimeProgram   `json:"program,omitempty"`
	Restore                *WorkerRuntimeRestore   `json:"restore,omitempty"`
}

type WorkerRuntimeRestore struct {
	CheckpointID  string                             `json:"checkpoint_id"`
	RunID         string                             `json:"run_id"`
	AttemptNumber int32                              `json:"attempt_number"`
	RunWaitID     string                             `json:"run_wait_id"`
	Kind          string                             `json:"kind"`
	Manifest      json.RawMessage                    `json:"manifest"`
	Artifacts     []WorkerRunLeaseCheckpointArtifact `json:"artifacts"`
}

type WorkerRuntimeProgram struct {
	DeploymentID         string                  `json:"deployment_id"`
	Runtime              WorkerRuntimeDescriptor `json:"runtime"`
	Code                 CASObject               `json:"code"`
	Dependencies         CASObject               `json:"dependencies"`
	BuildContractVersion string                  `json:"build_contract_version"`
}

type WorkerRuntimeInstanceStateRequest struct {
	ID                      string                     `json:"id"`
	WorkerEpoch             int64                      `json:"worker_epoch"`
	NetworkSlotID           string                     `json:"network_slot_id"`
	NetworkSlotGeneration   int64                      `json:"network_slot_generation"`
	DesiredVersion          int64                      `json:"desired_version"`
	ExpectedObservedVersion int64                      `json:"expected_observed_version"`
	RuntimeSubstrateID      string                     `json:"runtime_substrate_id,omitempty"`
	NetworkFacts            *WorkerNetworkFacts        `json:"network_facts,omitempty"`
	ReasonCode              string                     `json:"reason_code,omitempty"`
	Error                   json.RawMessage            `json:"error,omitempty"`
	CleanupProof            *WorkerRuntimeCleanupProof `json:"cleanup_proof,omitempty"`
}

type WorkerRuntimeCleanupProof struct {
	Method      string    `json:"method"`
	CompletedAt time.Time `json:"completed_at"`
}

const (
	WorkerRuntimeCleanupSessionClosed   = "session_closed"
	WorkerRuntimeCleanupHostReconciled  = "host_reconciled"
	WorkerRuntimeCleanupNotMaterialized = "not_materialized"
)

type WorkerRuntimeReconcileRequest struct{}

type WorkerRuntimeReconcileResponse struct {
	Target *WorkerRuntimeReconcileTarget `json:"target,omitempty"`
}

type WorkerRuntimeReconcileTarget struct {
	ID                     string              `json:"id"`
	WorkerEpoch            int64               `json:"worker_epoch"`
	NetworkSlotID          string              `json:"network_slot_id"`
	NetworkSlotGeneration  int64               `json:"network_slot_generation"`
	DesiredState           string              `json:"desired_state"`
	DesiredVersion         int64               `json:"desired_version"`
	ObservedState          string              `json:"observed_state"`
	ObservedVersion        int64               `json:"observed_version"`
	ObservedDesiredVersion int64               `json:"observed_desired_version"`
	Action                 string              `json:"action"`
	Source                 WorkerRuntimeSource `json:"source"`
}

const (
	WorkerRuntimeReconcilePrepare = "prepare"
	WorkerRuntimeReconcileClose   = "close"
	WorkerRuntimeReconcileReclaim = "reclaim"
)

type WorkerRunLeaseClaimRequest struct {
	LeaseID       string `json:"lease_id"`
	LeaseSequence int64  `json:"lease_sequence"`
}

type WorkerRunLeaseClaimResponse struct {
	Lease     WorkerRunLeaseReceipt     `json:"lease"`
	Program   WorkerRuntimeProgram      `json:"program"`
	Workspace WorkerWorkspaceAttachment `json:"workspace"`
	Secrets   []WorkerSecretDelivery    `json:"secrets"`
	Execution WorkerRunLeaseExecution   `json:"execution"`
}

type WorkerRunStartRequest struct {
	Lease   WorkerRunLeaseReceipt  `json:"lease"`
	Fresh   *WorkerRunStartFresh   `json:"fresh,omitempty"`
	Restore *WorkerRunStartRestore `json:"restore,omitempty"`
	Attach  *WorkerRunStartAttach  `json:"attach,omitempty"`
}

type WorkerRunStartFresh struct{}

type WorkerRunStartRestore struct {
	RunWaitID            string `json:"run_wait_id"`
	CheckpointID         string `json:"checkpoint_id"`
	ResumeAttachID       string `json:"resume_attach_id"`
	ResumeRequestVersion int64  `json:"resume_request_version"`
}

type WorkerRunStartAttach struct {
	Child  *WorkerRunStartChildAttach  `json:"child,omitempty"`
	Parent *WorkerRunStartParentAttach `json:"parent,omitempty"`
}

type WorkerRunStartChildAttach struct {
	RunWaitID      string `json:"run_wait_id"`
	CheckpointID   string `json:"checkpoint_id"`
	ResumeAttachID string `json:"resume_attach_id"`
}

type WorkerRunStartParentAttach struct {
	RunWaitID            string `json:"run_wait_id"`
	CheckpointID         string `json:"checkpoint_id"`
	ResumeAttachID       string `json:"resume_attach_id"`
	ResumeRequestVersion int64  `json:"resume_request_version"`
}

type WorkerRunStartResponse struct {
	Lease WorkerRunLeaseReceipt `json:"lease"`
}

type WorkerRunResumeReleaseRequest struct {
	Lease                WorkerRunLeaseReceipt `json:"lease"`
	RunWaitID            string                `json:"run_wait_id"`
	CheckpointID         string                `json:"checkpoint_id"`
	ResumeAttachID       string                `json:"resume_attach_id"`
	ResumeRequestVersion int64                 `json:"resume_request_version"`
	RunLeaseID           string                `json:"run_lease_id"`
}

type WorkerRunResumeReleaseResponse struct {
	Lease                WorkerRunLeaseReceipt `json:"lease"`
	RunWaitID            string                `json:"run_wait_id"`
	CheckpointID         string                `json:"checkpoint_id"`
	ResumeAttachID       string                `json:"resume_attach_id"`
	ResumeRequestVersion int64                 `json:"resume_request_version"`
}

type WorkerRunLeaseRenewRequest struct {
	Lease WorkerRunLeaseReceipt `json:"lease"`
}

type WorkerRunLeaseRenewResponse struct {
	Lease WorkerRunLeaseReceipt `json:"lease"`
}

type WorkerRunFinalizationKind string

const (
	WorkerRunFinalizationCapture WorkerRunFinalizationKind = "capture"
	WorkerRunFinalizationReset   WorkerRunFinalizationKind = "reset"
)

type WorkerRunQuiescenceProof struct {
	RunID         string `json:"run_id"`
	AttemptNumber int32  `json:"attempt_number"`
	RunLeaseID    string `json:"run_lease_id"`
}

type WorkerBeginRunFinalizationRequest struct {
	Lease           WorkerRunLeaseReceipt     `json:"lease"`
	ProgramQuiesced WorkerRunQuiescenceProof  `json:"program_quiesced"`
	OperationID     string                    `json:"operation_id"`
	Kind            WorkerRunFinalizationKind `json:"kind"`
}

type WorkerBeginRunFinalizationResponse struct {
	Lease       WorkerRunLeaseReceipt     `json:"lease"`
	OperationID string                    `json:"operation_id"`
	Kind        WorkerRunFinalizationKind `json:"kind"`
	StartedAt   time.Time                 `json:"started_at"`
}

type WorkerRunEntrypointRequest struct {
	Lease                WorkerRunLeaseReceipt `json:"lease"`
	EntrypointKind       string                `json:"entrypoint_kind"`
	EntrypointDeclaredID string                `json:"entrypoint_declared_id"`
}

type WorkerCompleteTaskRequest struct {
	Lease     WorkerRunLeaseReceipt    `json:"lease"`
	Outcome   WorkerTaskOutcome        `json:"outcome"`
	Workspace WorkerTaskWorkspaceProof `json:"workspace"`
}

type WorkerCompleteActorRequest struct {
	Lease     WorkerRunLeaseReceipt    `json:"lease"`
	Outcome   WorkerActorOutcome       `json:"outcome"`
	Workspace WorkerTaskWorkspaceProof `json:"workspace"`
}

type WorkerCommitActorTurnRequest struct {
	Lease                  WorkerRunLeaseReceipt       `json:"lease"`
	CorrelationID          string                      `json:"correlation_id"`
	TargetInputSequence    int64                       `json:"target_input_sequence"`
	BaseWorkspaceVersionID string                      `json:"base_workspace_version_id"`
	Tree                   WorkerWorkspaceTreeIdentity `json:"tree"`
	Artifact               *WorkerWorkspaceArtifact    `json:"artifact,omitempty"`
}

type WorkerCommitActorTurnResponse struct {
	Lease                  WorkerRunLeaseReceipt       `json:"lease"`
	RunID                  string                      `json:"run_id"`
	AttemptNumber          int32                       `json:"attempt_number"`
	RunLeaseID             string                      `json:"run_lease_id"`
	CorrelationID          string                      `json:"correlation_id"`
	CommittedInputSequence int64                       `json:"committed_input_sequence"`
	WorkspaceVersionID     string                      `json:"workspace_version_id"`
	Tree                   WorkerWorkspaceTreeIdentity `json:"tree"`
}

type WorkerActorOutcome struct {
	TerminalInputSequence int64                 `json:"terminal_input_sequence"`
	Succeeded             *WorkerActorSucceeded `json:"succeeded,omitempty"`
	Failed                *WorkerTaskFailure    `json:"failed,omitempty"`
}

type WorkerActorSucceeded struct{}

type WorkerTaskOutcome struct {
	Succeeded      *WorkerTaskSucceeded `json:"succeeded,omitempty"`
	Failed         *WorkerTaskFailure   `json:"failed,omitempty"`
	PayloadInvalid *WorkerTaskFailure   `json:"payload_invalid,omitempty"`
}

type WorkerTaskSucceeded struct {
	Output json.RawMessage `json:"output"`
}

type WorkerTaskFailure struct {
	Message string          `json:"message"`
	Details json.RawMessage `json:"details,omitempty"`
}

type WorkerTaskWorkspaceProof struct {
	Captured   *WorkerTaskWorkspaceCapture  `json:"captured,omitempty"`
	RolledBack *WorkerTaskWorkspaceRollback `json:"rolled_back,omitempty"`
}

type WorkerTaskWorkspaceCapture struct {
	Receipt  WorkerWorkspaceFinalizationReceipt `json:"receipt"`
	Tree     WorkerWorkspaceTreeIdentity        `json:"tree"`
	Artifact WorkerWorkspaceArtifact            `json:"artifact"`
}

type WorkerTaskWorkspaceRollback struct {
	Receipt WorkerWorkspaceFinalizationReceipt `json:"receipt"`
	Target  WorkerWorkspaceResetTarget         `json:"target"`
}

type WorkerWorkspaceFinalizationReceipt struct {
	OperationID        string                           `json:"operation_id"`
	RequestFingerprint string                           `json:"request_fingerprint"`
	Fence              WorkerWorkspaceFinalizationFence `json:"fence"`
}

type WorkerWorkspaceFinalizationFence struct {
	WorkerInstanceID       string    `json:"worker_instance_id"`
	WorkerEpoch            int64     `json:"worker_epoch"`
	RuntimeInstanceID      string    `json:"runtime_instance_id"`
	RuntimeIdentityID      string    `json:"runtime_identity_id"`
	WorkspaceID            string    `json:"workspace_id"`
	WorkspaceMountID       string    `json:"workspace_mount_id"`
	RunID                  string    `json:"run_id"`
	AttemptNumber          int32     `json:"attempt_number"`
	RunLeaseID             string    `json:"run_lease_id"`
	LeaseSequence          int64     `json:"lease_sequence"`
	WorkspaceLeaseID       string    `json:"workspace_lease_id"`
	OwnershipGeneration    int64     `json:"ownership_generation"`
	WriterGeneration       int64     `json:"writer_generation"`
	MountFencingGeneration int64     `json:"mount_fencing_generation"`
	ExpiresAt              time.Time `json:"expires_at"`
	BaseWorkspaceVersionID string    `json:"base_workspace_version_id"`
}

type WorkerWorkspaceTreeIdentity struct {
	Digest     string `json:"digest"`
	SizeBytes  int64  `json:"size_bytes"`
	EntryCount int32  `json:"entry_count"`
}

type WorkerWorkspaceResetTarget struct {
	BaseWorkspaceVersionID string                      `json:"base_workspace_version_id"`
	Tree                   WorkerWorkspaceTreeIdentity `json:"tree"`
	Empty                  *WorkerEmptyWorkspace       `json:"empty,omitempty"`
	Artifact               *WorkerWorkspaceArtifact    `json:"artifact,omitempty"`
}

type WorkerEmptyWorkspace struct {
}

type WorkerRunLeaseReceipt struct {
	ID                         string       `json:"id"`
	RunID                      string       `json:"run_id"`
	AttemptNumber              int32        `json:"attempt_number"`
	LeaseSequence              int64        `json:"lease_sequence"`
	WorkerGroupID              string       `json:"worker_group_id"`
	WorkerInstanceID           string       `json:"worker_instance_id"`
	WorkerEpoch                int64        `json:"worker_epoch"`
	WorkerProtocolVersion      string       `json:"worker_protocol_version"`
	RuntimeInstanceID          string       `json:"runtime_instance_id"`
	RuntimeIdentityID          string       `json:"runtime_identity_id"`
	NetworkSlotID              string       `json:"network_slot_id"`
	NetworkSlotGeneration      int64        `json:"network_slot_generation"`
	WorkspaceID                string       `json:"workspace_id"`
	WorkspaceMountID           string       `json:"workspace_mount_id"`
	WorkspaceLeaseID           string       `json:"workspace_lease_id"`
	BaseWorkspaceVersionID     string       `json:"base_workspace_version_id"`
	OwnershipGeneration        int64        `json:"ownership_generation"`
	WriterGeneration           int64        `json:"writer_generation"`
	MountFencingGeneration     int64        `json:"mount_fencing_generation"`
	RequestedCPUMillis         int64        `json:"requested_cpu_millis"`
	RequestedMemoryBytes       int64        `json:"requested_memory_bytes"`
	RequestedWorkloadDiskBytes int64        `json:"requested_workload_disk_bytes"`
	RequestedScratchBytes      int64        `json:"requested_scratch_bytes"`
	RequestedExecutionSlots    int32        `json:"requested_execution_slots"`
	MaxActiveDurationMs        int64        `json:"max_active_duration_ms"`
	ActiveElapsedMs            int64        `json:"active_elapsed_ms"`
	Trace                      TraceContext `json:"trace"`
	StartDeadlineAt            time.Time    `json:"start_deadline_at"`
	ExpiresAt                  time.Time    `json:"expires_at"`
}

type WorkerWorkspaceAttachment struct {
	WriteCapability string                     `json:"write_capability"`
	ResetTarget     WorkerWorkspaceResetTarget `json:"reset_target"`
}

type WorkerSecretDelivery struct {
	Env   *WorkerSecretEnv  `json:"env,omitempty"`
	File  *WorkerSecretFile `json:"file,omitempty"`
	Value []byte            `json:"value"`
}

type WorkerSecretEnv struct {
	Name string `json:"name"`
}

type WorkerSecretFile struct {
	Path string `json:"path"`
}

type WorkerRunLeaseExecution struct {
	Fresh   *WorkerRunLeaseFresh   `json:"fresh,omitempty"`
	Restore *WorkerRunLeaseRestore `json:"restore,omitempty"`
	Attach  *WorkerRunLeaseAttach  `json:"attach,omitempty"`
}

type WorkerRunLeaseFresh struct {
	ProgramStart []byte `json:"program_start"`
}

type WorkerRunLeaseRestore struct {
	RunWaitID            string                          `json:"run_wait_id"`
	CheckpointID         string                          `json:"checkpoint_id"`
	ResumeAttachID       string                          `json:"resume_attach_id"`
	ResumeRequestVersion int64                           `json:"resume_request_version"`
	Recreated            *WorkerRunLeaseRecreatedRestore `json:"recreated,omitempty"`
	Retained             *WorkerRunLeaseRetainedRestore  `json:"retained,omitempty"`
	Decision             WorkerRunLeaseDecision          `json:"decision"`
}

type WorkerRunLeaseRecreatedRestore struct {
	Kind      string                             `json:"kind"`
	Manifest  json.RawMessage                    `json:"manifest"`
	Artifacts []WorkerRunLeaseCheckpointArtifact `json:"artifacts"`
}

type WorkerRunLeaseRetainedRestore struct {
	EnclosingRunWaitID string `json:"enclosing_run_wait_id"`
}

type WorkerRunLeaseAttach struct {
	Child  *WorkerRunLeaseChildAttach  `json:"child,omitempty"`
	Parent *WorkerRunLeaseParentAttach `json:"parent,omitempty"`
}

type WorkerRunLeaseChildAttach struct {
	RunWaitID      string `json:"run_wait_id"`
	CheckpointID   string `json:"checkpoint_id"`
	ResumeAttachID string `json:"resume_attach_id"`
	ProgramStart   []byte `json:"program_start"`
}

type WorkerRunLeaseParentAttach struct {
	RunWaitID            string                 `json:"run_wait_id"`
	CheckpointID         string                 `json:"checkpoint_id"`
	ResumeAttachID       string                 `json:"resume_attach_id"`
	ResumeRequestVersion int64                  `json:"resume_request_version"`
	Decision             WorkerRunLeaseDecision `json:"decision"`
}

type WorkerRunLeaseCheckpointArtifact struct {
	Role    string    `json:"role"`
	Ordinal int32     `json:"ordinal"`
	Object  CASObject `json:"object"`
}

type WorkerRunLeaseDecision struct {
	Completed *WorkerRunLeaseCompleted `json:"completed,omitempty"`
	Failed    *WorkerRunLeaseFailed    `json:"failed,omitempty"`
	Cancelled *WorkerRunLeaseCancelled `json:"cancelled,omitempty"`
}

type WorkerRunLeaseCompleted struct {
	NoResult   *struct{}       `json:"no_result,omitempty"`
	ResultJSON json.RawMessage `json:"result_json,omitempty"`
}

type WorkerRunLeaseFailed struct {
	ReasonCode string          `json:"reason_code"`
	Error      json.RawMessage `json:"error,omitempty"`
}

type WorkerRunLeaseCancelled struct {
	ReasonCode string          `json:"reason_code"`
	Error      json.RawMessage `json:"error,omitempty"`
}

type WorkerRunLease struct {
	ID                    string       `json:"id"`
	OrgID                 string       `json:"org_id"`
	RunID                 string       `json:"run_id"`
	WorkerGroupID         string       `json:"worker_group_id"`
	WorkerInstanceID      string       `json:"worker_instance_id"`
	WorkerEpoch           int64        `json:"worker_epoch"`
	LeaseSequence         int64        `json:"lease_sequence"`
	SnapshotVersion       int64        `json:"snapshot_version"`
	RuntimeInstanceID     string       `json:"runtime_instance_id"`
	NetworkSlotID         string       `json:"network_slot_id"`
	NetworkSlotGeneration int64        `json:"network_slot_generation"`
	ProtocolVersion       string       `json:"protocol_version"`
	AttemptNumber         int32        `json:"attempt_number"`
	Trace                 TraceContext `json:"trace"`
	ExpiresAt             time.Time    `json:"expires_at"`
}

type WorkerRunLeaseProvider interface {
	CurrentWorkerRunLease() WorkerRunLease
}

type WorkerRunLeaseReceiptProvider interface {
	CurrentWorkerRunLeaseReceipt() WorkerRunLeaseReceipt
}

type WorkerDeploymentBuildLease struct {
	ID                         string    `json:"id"`
	OrgID                      string    `json:"org_id"`
	ProjectID                  string    `json:"project_id"`
	EnvironmentID              string    `json:"environment_id"`
	DeploymentID               string    `json:"deployment_id"`
	WorkerGroupID              string    `json:"worker_group_id"`
	WorkerInstanceID           string    `json:"worker_instance_id"`
	WorkerEpoch                int64     `json:"worker_epoch"`
	LeaseSequence              int64     `json:"lease_sequence"`
	WorkerProtocolVersion      string    `json:"worker_protocol_version"`
	ExpiresAt                  time.Time `json:"expires_at"`
	RequestedWorkloadDiskBytes int64     `json:"requested_workload_disk_bytes"`
	RequestedScratchBytes      int64     `json:"requested_scratch_bytes"`
	RequestedCPUMillis         int64     `json:"requested_cpu_millis"`
	RequestedMemoryBytes       int64     `json:"requested_memory_bytes"`
	RequestedBuildExecutors    int32     `json:"requested_build_executors"`
}

type WorkerDeploymentBuild struct {
	ID                      string                   `json:"id"`
	Version                 string                   `json:"version"`
	APIVersion              string                   `json:"api_version"`
	SDKVersion              string                   `json:"sdk_version,omitempty"`
	CLIVersion              string                   `json:"cli_version,omitempty"`
	BundleFormatVersion     int32                    `json:"bundle_format_version"`
	WorkerProtocolVersion   string                   `json:"worker_protocol_version"`
	ProjectID               string                   `json:"project_id"`
	EnvironmentID           string                   `json:"environment_id"`
	DeploymentSource        DeploymentSourceArtifact `json:"deployment_source"`
	Runtime                 WorkerRuntimeDescriptor  `json:"runtime"`
	StandardToolchainDigest string                   `json:"standard_toolchain_digest"`
	BuildContractVersion    string                   `json:"build_contract_version"`
}

type WorkerRuntimeDescriptor struct {
	Architecture      string `json:"architecture"`
	Digest            string `json:"digest"`
	FormatVersion     int    `json:"formatVersion"`
	MediaType         string `json:"mediaType"`
	RuntimeAPIVersion string `json:"runtimeApiVersion"`
	SizeBytes         int64  `json:"sizeBytes"`
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
	RunLeaseID            string                         `json:"run_lease_id"`
	SnapshotVersion       int64                          `json:"snapshot_version"`
	SessionID             string                         `json:"session_id"`
	TaskID                string                         `json:"task_id"`
	Payload               json.RawMessage                `json:"payload"`
	Secrets               ResolvedSecrets                `json:"secrets,omitempty"`
	DeploymentSource      DeploymentSourceArtifact       `json:"deployment_source"`
	DeploymentTask        WorkerDeploymentTask           `json:"deployment_task"`
	Workspace             WorkerWorkspace                `json:"workspace"`
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
	ID                string                        `json:"id,omitempty"`
	WorkspaceMountID  string                        `json:"workspace_mount_id,omitempty"`
	FencingGeneration int64                         `json:"fencing_generation,omitempty"`
	WriteLeaseID      string                        `json:"write_lease_id,omitempty"`
	WriteFencingToken string                        `json:"write_fencing_token,omitempty"`
	BaseVersionID     string                        `json:"base_version_id,omitempty"`
	MountPath         string                        `json:"mount_path,omitempty"`
	Artifact          *WorkerWorkspaceArtifact      `json:"artifact,omitempty"`
	SubstrateSource   *WorkerRuntimeSubstrateSource `json:"substrate_source,omitempty"`
}

type WorkerRuntimeSubstrateSource struct {
	DeploymentDefinitionID string                  `json:"deployment_definition_id"`
	WorkspaceImage         CASObject               `json:"workspace_image"`
	RuntimeSubstrate       *WorkerRuntimeSubstrate `json:"runtime_substrate,omitempty"`
}

type WorkerRuntimeSubstrate struct {
	ID                     string    `json:"id,omitempty"`
	DeploymentDefinitionID string    `json:"deployment_definition_id"`
	Artifact               CASObject `json:"artifact"`
	SubstrateDigest        string    `json:"substrate_digest"`
	Format                 string    `json:"format"`
	BuilderABI             string    `json:"builder_abi"`
	LayoutABI              string    `json:"layout_abi"`
	SizeBytes              int64     `json:"size_bytes"`
	Retired                bool      `json:"retired"`
}

type WorkerRuntimeSubstrateRegisterRequest struct {
	ID                     string          `json:"id,omitempty"`
	DeploymentDefinitionID string          `json:"deployment_definition_id"`
	Artifact               CASObject       `json:"artifact"`
	SubstrateDigest        string          `json:"substrate_digest"`
	Format                 string          `json:"format"`
	BuilderABI             string          `json:"builder_abi"`
	LayoutABI              string          `json:"layout_abi"`
	SizeBytes              int64           `json:"size_bytes"`
	Source                 json.RawMessage `json:"source,omitempty"`
}

type WorkerRuntimeSubstrateRegisterResponse struct {
	RuntimeSubstrate WorkerRuntimeSubstrate `json:"runtime_substrate"`
}

type WorkerRuntimeSubstrateLookupRequest struct {
	DeploymentDefinitionID string `json:"deployment_definition_id"`
	SubstrateDigest        string `json:"substrate_digest"`
	Format                 string `json:"format"`
	BuilderABI             string `json:"builder_abi"`
	LayoutABI              string `json:"layout_abi"`
}

type WorkerRuntimeSubstrateLookupResponse struct {
	RuntimeSubstrate WorkerRuntimeSubstrate `json:"runtime_substrate"`
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
	RunWait      WorkerRestoreRunWait     `json:"run_wait"`
}

type WorkerRestoreRunWait struct {
	ID                   string          `json:"id"`
	CorrelationID        string          `json:"correlation_id"`
	ResumeAttachID       string          `json:"resume_attach_id"`
	ResumeRequestVersion int64           `json:"resume_request_version"`
	Kind                 string          `json:"kind"`
	ResumeKind           string          `json:"resume_kind"`
	ResumePayloadJSON    json.RawMessage `json:"resume_payload_json"`
}

type WorkerStartRequest struct {
	Lease WorkerRunLease `json:"lease"`
}

type WorkerRejectRunRequest struct {
	Lease      WorkerRunLease  `json:"lease"`
	ReasonCode string          `json:"reason_code"`
	Error      json.RawMessage `json:"error,omitempty"`
}

type WorkerStartResponse struct {
	RunID  string         `json:"run_id"`
	Status string         `json:"status"`
	Lease  WorkerRunLease `json:"lease"`
}

type WorkerAcknowledgeRestoreRequest struct {
	Lease                WorkerRunLease          `json:"lease"`
	RunWaitID            string                  `json:"run_wait_id"`
	CheckpointID         string                  `json:"checkpoint_id"`
	ResumeRequestVersion int64                   `json:"resume_request_version"`
	Phases               []WorkerCheckpointPhase `json:"phases,omitempty"`
}

type WorkerAcknowledgeRestoreResponse struct {
	RunID        string `json:"run_id"`
	RunWaitID    string `json:"run_wait_id"`
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
	Kind             string           `json:"kind"`
	ActiveDurationMs int64            `json:"active_duration_ms,omitempty"`
	ExitCode         *int32           `json:"exit_code,omitempty"`
	Output           json.RawMessage  `json:"output,omitempty"`
	Error            *string          `json:"error,omitempty"`
	FailureKind      *string          `json:"failure_kind,omitempty"`
	LimitSeconds     *int32           `json:"limit_seconds,omitempty"`
	Workspace        *WorkerWorkspace `json:"workspace,omitempty"`
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

type WorkerDeploymentStream struct {
	Name              string          `json:"name"`
	Direction         string          `json:"direction"`
	SchemaFingerprint string          `json:"schema_fingerprint,omitempty"`
	SchemaJSON        json.RawMessage `json:"schema_json,omitempty"`
}

type WorkerDeploymentQueue struct {
	Name             string `json:"name"`
	ConcurrencyLimit *int32 `json:"concurrency_limit,omitempty"`
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
	ProgramReceipt           json.RawMessage             `json:"program_receipt,omitempty"`
	Tasks                    []WorkerDeploymentBuildTask `json:"tasks"`
	Queues                   []WorkerDeploymentQueue     `json:"queues"`
	Streams                  []WorkerDeploymentStream    `json:"streams,omitempty"`
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

type WorkerRunLogAppendRequest struct {
	Lease         WorkerRunLeaseReceipt `json:"lease"`
	Stream        WorkerLogStream       `json:"stream"`
	ObservedSeq   uint64                `json:"observed_seq"`
	ContentBase64 string                `json:"content_base64"`
}

type WorkerRecordLogEntryRequest struct {
	Lease WorkerRunLease `json:"lease"`
	Entry string         `json:"entry"`
}

type WorkerOutputStreamAppendRequest struct {
	Lease          WorkerRunLease  `json:"lease"`
	Stream         string          `json:"stream"`
	Data           json.RawMessage `json:"data"`
	ContentType    string          `json:"content_type,omitempty"`
	CorrelationID  string          `json:"correlation_id,omitempty"`
	IdempotencyKey string          `json:"idempotency_key,omitempty"`
}

type WorkerActiveStreamReadRequest struct {
	Lease          WorkerRunLease `json:"lease"`
	Stream         string         `json:"stream"`
	AfterSequence  int64          `json:"after_sequence,omitempty"`
	CorrelationID  string         `json:"correlation_id,omitempty"`
	TimeoutSeconds *int32         `json:"timeout_seconds,omitempty"`
	Block          bool           `json:"block"`
}

type WorkerActiveStreamReadResponse struct {
	Record   *StreamRecordResponse `json:"record,omitempty"`
	TimedOut bool                  `json:"timed_out,omitempty"`
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

type WorkerCreateTokenRequest struct {
	Lease            WorkerRunLease  `json:"lease"`
	TimeoutAt        *time.Time      `json:"timeout_at,omitempty"`
	TimeoutInSeconds *int32          `json:"timeout_in_seconds,omitempty"`
	Tags             []string        `json:"tags,omitempty"`
	Metadata         json.RawMessage `json:"metadata,omitempty"`
}

type WorkerRunWaitKind string

const (
	WorkerRunWaitKindToken  WorkerRunWaitKind = "token"
	WorkerRunWaitKindTimer  WorkerRunWaitKind = "timer"
	WorkerRunWaitKindStream WorkerRunWaitKind = "stream"
)

type WorkerCreateRunWaitRequest struct {
	Lease              WorkerRunLeaseReceipt `json:"lease"`
	CorrelationID      string                `json:"correlation_id"`
	Kind               WorkerRunWaitKind     `json:"kind"`
	Params             json.RawMessage       `json:"params,omitempty"`
	Metadata           json.RawMessage       `json:"metadata,omitempty"`
	Tags               []string              `json:"tags,omitempty"`
	TimeoutSeconds     *int32                `json:"timeout_seconds,omitempty"`
	IdleTimeoutSeconds *int32                `json:"idle_timeout_seconds,omitempty"`
}

type WorkerCreateRunWaitResponse struct {
	RunID              string          `json:"run_id"`
	RunWaitID          string          `json:"run_wait_id"`
	ResumeAttachID     string          `json:"resume_attach_id,omitempty"`
	RuntimeInstanceID  string          `json:"runtime_instance_id,omitempty"`
	RuntimeEpoch       int64           `json:"runtime_epoch,omitempty"`
	CheckpointDelayMs  int64           `json:"checkpoint_delay_ms,omitempty"`
	WorkspaceVersionID string          `json:"workspace_version_id,omitempty"`
	ResolutionKind     string          `json:"resolution_kind,omitempty"`
	Resolution         json.RawMessage `json:"resolution,omitempty"`
}

type WorkerRunWaitPollRequest struct {
	Lease     WorkerRunLeaseReceipt `json:"lease"`
	RunWaitID string                `json:"run_wait_id"`
}

type WorkerRunWaitPollStatus string

const (
	WorkerRunWaitPollStatusWaiting             WorkerRunWaitPollStatus = "waiting"
	WorkerRunWaitPollStatusCheckpointRequested WorkerRunWaitPollStatus = "checkpoint_requested"
	WorkerRunWaitPollStatusResumeRequested     WorkerRunWaitPollStatus = "resume_requested"
	WorkerRunWaitPollStatusTerminal            WorkerRunWaitPollStatus = "terminal"
)

type WorkerRunWaitPollResponse struct {
	RunID            string                  `json:"run_id"`
	RunWaitID        string                  `json:"run_wait_id"`
	Status           WorkerRunWaitPollStatus `json:"status"`
	RequestVersion   int64                   `json:"request_version,omitempty"`
	CheckpointID     string                  `json:"checkpoint_id,omitempty"`
	CaptureWorkspace bool                    `json:"capture_workspace,omitempty"`
	ResumeKind       string                  `json:"resume_kind,omitempty"`
	ResumePayload    json.RawMessage         `json:"resume_payload,omitempty"`
	RequireAck       bool                    `json:"require_ack,omitempty"`
}

type WorkerRunWaitResumeAckRequest struct {
	Lease                WorkerRunLeaseReceipt `json:"lease"`
	RunWaitID            string                `json:"run_wait_id"`
	ResumeRequestVersion int64                 `json:"resume_request_version"`
}

type WorkerRunWaitResumeAckResponse struct {
	RunID                string `json:"run_id"`
	RunWaitID            string `json:"run_wait_id"`
	ResumeRequestVersion int64  `json:"resume_request_version"`
}

type WorkerCheckpointResponse struct {
	RunID              string `json:"run_id"`
	RunWaitID          string `json:"run_wait_id"`
	CheckpointID       string `json:"checkpoint_id"`
	WorkspaceVersionID string `json:"workspace_version_id,omitempty"`
}

type WorkerCheckpointManifest struct {
	RecoveryPoint  WorkerCheckpointRecoveryPoint  `json:"recovery_point"`
	RuntimeState   WorkerCheckpointRuntimeState   `json:"runtime_state"`
	WorkspaceState WorkerCheckpointWorkspaceState `json:"workspace_state"`
	Phases         []WorkerCheckpointPhase        `json:"phases,omitempty"`
}

type WorkerCheckpointRecoveryPoint struct {
	ID            string                  `json:"id,omitempty"`
	RunID         string                  `json:"run_id,omitempty"`
	AttemptNumber int32                   `json:"attempt_number,omitempty"`
	RunWaitID     string                  `json:"run_wait_id,omitempty"`
	CorrelationID string                  `json:"correlation_id,omitempty"`
	Runtime       WorkerCheckpointRuntime `json:"runtime"`
}

type WorkerCheckpointRuntime struct {
	Backend         string                            `json:"backend"`
	ID              string                            `json:"id"`
	Arch            string                            `json:"arch"`
	ABI             string                            `json:"abi"`
	KernelDigest    string                            `json:"kernel_digest"`
	InitramfsDigest string                            `json:"initramfs_digest"`
	RootfsDigest    string                            `json:"rootfs_digest"`
	ConfigDigest    string                            `json:"config_digest"`
	Substrate       *WorkerCheckpointRuntimeSubstrate `json:"substrate,omitempty"`
}

type WorkerCheckpointRuntimeSubstrate struct {
	Digest     string `json:"digest"`
	Format     string `json:"format"`
	BuilderABI string `json:"builder_abi"`
	LayoutABI  string `json:"layout_abi"`
}

type WorkerCheckpointRuntimeState struct {
	ConfigArtifact      WorkerCheckpointArtifact   `json:"config_artifact"`
	VMStateArtifact     WorkerCheckpointArtifact   `json:"vm_state_artifact"`
	ScratchDiskArtifact WorkerCheckpointArtifact   `json:"scratch_disk_artifact"`
	RuntimeSubstrate    *WorkerRuntimeSubstrate    `json:"runtime_substrate,omitempty"`
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
	Name       string                         `json:"name"`
	DurationMs int64                          `json:"duration_ms"`
	Role       string                         `json:"role,omitempty"`
	MediaType  string                         `json:"media_type,omitempty"`
	ErrorClass string                         `json:"error_class,omitempty"`
	Filepack   *WorkerCheckpointFilepackStats `json:"filepack,omitempty"`
}

type WorkerCheckpointFilepackStats struct {
	LogicalBytes       int64 `json:"logical_bytes,omitempty"`
	AllocatedBytes     int64 `json:"allocated_bytes,omitempty"`
	SparseSupported    *bool `json:"sparse_supported,omitempty"`
	SparseDataRanges   int64 `json:"sparse_data_ranges,omitempty"`
	SparseDataBytes    int64 `json:"sparse_data_bytes,omitempty"`
	ZeroChunksSkipped  int64 `json:"zero_chunks_skipped,omitempty"`
	EncodedChunks      int64 `json:"encoded_chunks,omitempty"`
	CompressedBytes    int64 `json:"compressed_bytes,omitempty"`
	UnpackWrittenBytes int64 `json:"unpack_written_bytes,omitempty"`
}

type CASObject struct {
	Digest    string `json:"digest"`
	SizeBytes int64  `json:"size_bytes"`
	MediaType string `json:"media_type"`
}

type WorkerCheckpointReadyRequest struct {
	Lease            WorkerRunLeaseReceipt            `json:"lease"`
	RequestVersion   int64                            `json:"request_version"`
	RunWaitID        string                           `json:"run_wait_id"`
	CheckpointID     string                           `json:"checkpoint_id"`
	WorkspaceCapture WorkerCheckpointWorkspaceCapture `json:"workspace_capture"`
	Manifest         WorkerCheckpointManifest         `json:"manifest"`
}

type WorkerCheckpointWorkspaceCapture struct {
	Tree     WorkerWorkspaceTreeIdentity `json:"tree"`
	Artifact WorkerWorkspaceArtifact     `json:"artifact"`
}

type WorkerCheckpointFailedRequest struct {
	Lease          WorkerRunLeaseReceipt `json:"lease"`
	RequestVersion int64                 `json:"request_version"`
	RunWaitID      string                `json:"run_wait_id"`
	CheckpointID   string                `json:"checkpoint_id"`
	Error          string                `json:"error"`
}
