package api

import (
	"encoding/json"
	"time"
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
	CPUPressureBPS                int32           `json:"cpu_pressure_bps"`
	MemoryPressureBPS             int32           `json:"memory_pressure_bps"`
	GuestEphemeralDiskPressureBPS int32           `json:"guest_ephemeral_disk_pressure_bps"`
	BuildCachePressureBPS         int32           `json:"build_cache_pressure_bps"`
	ArtifactCachePressureBPS      int32           `json:"artifact_cache_pressure_bps"`
	CheckpointPressureBPS         int32           `json:"checkpoint_pressure_bps"`
	LeakedSlotCount               int32           `json:"leaked_slot_count"`
	RunQueueDepth                 int32           `json:"run_queue_depth"`
	BuildQueueDepth               int32           `json:"build_queue_depth"`
	RuntimeStartQueueDepth        int32           `json:"runtime_start_queue_depth"`
	RunPausedReason               string          `json:"run_paused_reason,omitempty"`
	BuildPausedReason             string          `json:"build_paused_reason,omitempty"`
	RuntimePausedReason           string          `json:"runtime_paused_reason,omitempty"`
	HealthDetails                 json.RawMessage `json:"health_details,omitempty"`
}

type WorkerCapabilities struct {
	ProtocolVersion           string                    `json:"protocol_version"`
	WorkerVersion             string                    `json:"worker_version,omitempty"`
	RuntimeID                 string                    `json:"runtime_id"`
	RuntimeArch               string                    `json:"runtime_arch"`
	RuntimeABI                string                    `json:"runtime_abi"`
	KernelDigest              string                    `json:"kernel_digest"`
	InitramfsDigest           string                    `json:"initramfs_digest"`
	RootfsDigest              string                    `json:"rootfs_digest"`
	CNIProfile                string                    `json:"cni_profile"`
	SubstrateFormat           string                    `json:"substrate_format,omitempty"`
	SubstrateBuilderABI       string                    `json:"substrate_builder_abi,omitempty"`
	SubstrateLayoutABI        string                    `json:"substrate_layout_abi,omitempty"`
	Region                    string                    `json:"region,omitempty"`
	Labels                    map[string]string         `json:"labels,omitempty"`
	MaxVCPUs                  int64                     `json:"max_vcpus"`
	MaxMemoryMiB              int64                     `json:"max_memory_mib"`
	VMMilliCPU                int64                     `json:"vm_milli_cpu"`
	VMMemoryMiB               int64                     `json:"vm_memory_mib"`
	GuestEphemeralDiskBytes   int64                     `json:"guest_ephemeral_disk_bytes"`
	VMGuestEphemeralDiskBytes int64                     `json:"vm_guest_ephemeral_disk_bytes"`
	ExecutionSlotsAvailable   int32                     `json:"execution_slots_available"`
	SupportsRun               bool                      `json:"supports_run"`
	SupportsBuild             bool                      `json:"supports_build"`
	MaxBuildExecutors         int32                     `json:"max_build_executors"`
	MaxRuntimeStarts          int32                     `json:"max_runtime_starts"`
	BuildCacheBytes           int64                     `json:"build_cache_bytes"`
	ArtifactCacheBytes        int64                     `json:"artifact_cache_bytes"`
	HugepagesBytes            int64                     `json:"hugepages_bytes"`
	CheckpointBytes           int64                     `json:"checkpoint_bytes"`
	Observation               WorkerObservation         `json:"observation"`
	Network                   WorkerNetworkCapabilities `json:"network"`
}

type WorkerNetworkCapabilities struct {
	Internet      bool `json:"internet"`
	BlockInternet bool `json:"block_internet"`
	DenyCIDRs     bool `json:"deny_cidrs"`
	AllowCIDRs    bool `json:"allow_cidrs"`
	AllowDomains  bool `json:"allow_domains"`
}

type TraceContext struct {
	TraceID     string `json:"trace_id"`
	SpanID      string `json:"span_id,omitempty"`
	Traceparent string `json:"traceparent,omitempty"`
}

type WorkerDeploymentBuildLeaseRequest struct{}

type WorkerPlatformAcquisitionRequest struct{}

type WorkerPlatformAcquisitionResponse struct {
	Acquisition *WorkerPlatformAcquisition `json:"acquisition,omitempty"`
}

type WorkerPlatformAcquisition struct {
	DeploymentID      string `json:"deployment_id"`
	OrgID             string `json:"org_id"`
	ProjectID         string `json:"project_id"`
	EnvironmentID     string `json:"environment_id"`
	NodeVersion       string `json:"node_version"`
	ManagerName       string `json:"manager_name"`
	ManagerVersion    string `json:"manager_version"`
	ManagerIntegrity  string `json:"manager_integrity,omitempty"`
	BuildContract     string `json:"build_contract"`
	BuildPolicyDigest string `json:"build_policy_digest"`
}

type WorkerPlatformAcquisitionCandidates struct {
	Runtime   CASObject `json:"runtime"`
	Manager   CASObject `json:"manager"`
	Toolchain CASObject `json:"toolchain"`
}

type WorkerPlatformAcquisitionCompleteRequest struct {
	Acquisition WorkerPlatformAcquisition           `json:"acquisition"`
	Candidates  WorkerPlatformAcquisitionCandidates `json:"candidates"`
}

type WorkerPlatformAcquisitionFailureReason string

const (
	WorkerPlatformAcquisitionUnsupportedSelector WorkerPlatformAcquisitionFailureReason = "unsupported_selector"
	WorkerPlatformAcquisitionOriginRejected      WorkerPlatformAcquisitionFailureReason = "origin_rejected"
	WorkerPlatformAcquisitionIntegrityFailed     WorkerPlatformAcquisitionFailureReason = "integrity_failed"
	WorkerPlatformAcquisitionTopologyFailed      WorkerPlatformAcquisitionFailureReason = "topology_failed"
	WorkerPlatformAcquisitionConformanceFailed   WorkerPlatformAcquisitionFailureReason = "conformance_failed"
	WorkerPlatformAcquisitionDenied              WorkerPlatformAcquisitionFailureReason = "denied"
)

type WorkerPlatformAcquisitionFailRequest struct {
	Acquisition WorkerPlatformAcquisition              `json:"acquisition"`
	Reason      WorkerPlatformAcquisitionFailureReason `json:"reason"`
	Error       json.RawMessage                        `json:"error"`
}

type WorkerPlatformAcquisitionResult struct {
	DeploymentID string `json:"deployment_id"`
	Status       string `json:"status"`
}

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
	DeploymentID         string    `json:"deployment_id"`
	Runtime              CASObject `json:"runtime"`
	Artifact             CASObject `json:"artifact"`
	BuildContractVersion string    `json:"build_contract_version"`
	IndexDigest          string    `json:"index_digest"`
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
	Lease     WorkerRunLeaseAssignment  `json:"lease"`
	Program   WorkerRuntimeProgram      `json:"program"`
	Workspace WorkerWorkspaceAttachment `json:"workspace"`
	Secrets   []WorkerSecretDelivery    `json:"secrets"`
	Execution WorkerRunLeaseExecution   `json:"execution"`
}

type WorkerRunStartRequest struct {
	Lease   WorkerRunLeaseFence    `json:"lease"`
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
	Lease WorkerRunLeaseFence `json:"lease"`
}

type WorkerRunResumeReleaseRequest struct {
	Lease                WorkerRunLeaseFence `json:"lease"`
	RunWaitID            string              `json:"run_wait_id"`
	CheckpointID         string              `json:"checkpoint_id"`
	ResumeAttachID       string              `json:"resume_attach_id"`
	ResumeRequestVersion int64               `json:"resume_request_version"`
}

type WorkerRunResumeReleaseResponse struct {
	Lease                WorkerRunLeaseFence `json:"lease"`
	RunWaitID            string              `json:"run_wait_id"`
	CheckpointID         string              `json:"checkpoint_id"`
	ResumeAttachID       string              `json:"resume_attach_id"`
	ResumeRequestVersion int64               `json:"resume_request_version"`
}

type WorkerRunLeaseRenewRequest struct {
	Lease             WorkerRunLeaseFence `json:"lease"`
	ExpectedExpiresAt time.Time           `json:"expected_expires_at"`
}

type WorkerRunLeaseRenewResponse struct {
	Lease                  WorkerRunLeaseFence `json:"lease"`
	ExpiresAt              time.Time           `json:"expires_at"`
	BaseWorkspaceVersionID string              `json:"base_workspace_version_id"`
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
	Lease           WorkerRunLeaseFence       `json:"lease"`
	ProgramQuiesced WorkerRunQuiescenceProof  `json:"program_quiesced"`
	OperationID     string                    `json:"operation_id"`
	Kind            WorkerRunFinalizationKind `json:"kind"`
}

type WorkerBeginRunFinalizationResponse struct {
	Lease                  WorkerRunLeaseFence           `json:"lease"`
	BaseWorkspaceVersionID string                        `json:"base_workspace_version_id"`
	ExpiresAt              time.Time                     `json:"expires_at"`
	OperationID            string                        `json:"operation_id"`
	Kind                   WorkerRunFinalizationKind     `json:"kind"`
	StartedAt              time.Time                     `json:"started_at"`
	Handoff                *WorkerRunFinalizationHandoff `json:"handoff,omitempty"`
}

type WorkerRunFinalizationHandoff struct {
	ParentRunID         string `json:"parent_run_id"`
	ParentAttemptNumber int32  `json:"parent_attempt_number"`
	RunWaitID           string `json:"run_wait_id"`
	SuspendCheckpointID string `json:"suspend_checkpoint_id"`
	ResumeAttachID      string `json:"resume_attach_id"`
	CorrelationID       string `json:"correlation_id"`
}

type WorkerRunEntrypointRequest struct {
	Lease                WorkerRunLeaseFence `json:"lease"`
	EntrypointKind       string              `json:"entrypoint_kind"`
	EntrypointDeclaredID string              `json:"entrypoint_declared_id"`
}

type WorkerCompleteTaskRequest struct {
	Lease     WorkerRunLeaseFence          `json:"lease"`
	Outcome   WorkerTaskOutcome            `json:"outcome"`
	Workspace WorkerTaskWorkspaceProof     `json:"workspace"`
	Handoff   *WorkerTaskHandoffCheckpoint `json:"handoff,omitempty"`
}

type WorkerTaskHandoffCheckpoint struct {
	CheckpointID string                   `json:"checkpoint_id"`
	Manifest     WorkerCheckpointManifest `json:"manifest"`
}

type WorkerCompleteActorRequest struct {
	Lease     WorkerRunLeaseFence      `json:"lease"`
	Outcome   WorkerActorOutcome       `json:"outcome"`
	Workspace WorkerTaskWorkspaceProof `json:"workspace"`
}

type WorkerCommitActorTurnRequest struct {
	Lease                  WorkerRunLeaseFence         `json:"lease"`
	CorrelationID          string                      `json:"correlation_id"`
	TargetInputSequence    int64                       `json:"target_input_sequence"`
	BaseWorkspaceVersionID string                      `json:"base_workspace_version_id"`
	Tree                   WorkerWorkspaceTreeIdentity `json:"tree"`
	Artifact               *WorkerWorkspaceArtifact    `json:"artifact,omitempty"`
}

type WorkerCommitActorTurnResponse struct {
	Lease                  WorkerRunLeaseFence         `json:"lease"`
	CorrelationID          string                      `json:"correlation_id"`
	CommittedInputSequence int64                       `json:"committed_input_sequence"`
	WorkspaceVersionID     string                      `json:"workspace_version_id"`
	Tree                   WorkerWorkspaceTreeIdentity `json:"tree"`
}

type WorkerSendActorInputRequest struct {
	Lease           WorkerRunLeaseFence `json:"lease"`
	CorrelationID   string              `json:"correlation_id"`
	ActorDeclaredID string              `json:"actor_declared_id"`
	ActorID         string              `json:"actor_id,omitempty"`
	ActorKey        string              `json:"actor_key,omitempty"`
	Input           json.RawMessage     `json:"input"`
	IdempotencyKey  string              `json:"idempotency_key,omitempty"`
}

type WorkerSendActorInputResponse struct {
	CorrelationID string                         `json:"correlation_id"`
	Completed     *SendActorInputResponse        `json:"completed,omitempty"`
	Failed        *WorkerRuntimeOperationFailure `json:"failed,omitempty"`
}

type WorkerStartActorRequest struct {
	Lease           WorkerRunLeaseFence   `json:"lease"`
	CorrelationID   string                `json:"correlation_id"`
	ActorDeclaredID string                `json:"actor_declared_id"`
	Key             *string               `json:"key,omitempty"`
	InputPresent    bool                  `json:"input_present"`
	Input           json.RawMessage       `json:"input,omitempty"`
	IdempotencyKey  string                `json:"idempotency_key,omitempty"`
	Workspace       WorkspaceTarget       `json:"workspace"`
	Run             *StartActorRunOptions `json:"run,omitempty"`
}

type WorkerStartActorResponse struct {
	CorrelationID string                         `json:"correlation_id"`
	Completed     *StartActorResponse            `json:"completed,omitempty"`
	Failed        *WorkerRuntimeOperationFailure `json:"failed,omitempty"`
}

type WorkerActorReferenceRequest struct {
	Lease           WorkerRunLeaseFence `json:"lease"`
	CorrelationID   string              `json:"correlation_id"`
	ActorDeclaredID string              `json:"actor_declared_id"`
	ActorID         string              `json:"actor_id,omitempty"`
	ActorKey        string              `json:"actor_key,omitempty"`
}

type WorkerActorStatusResponse struct {
	CorrelationID string                         `json:"correlation_id"`
	Completed     *ActorStatus                   `json:"completed,omitempty"`
	Failed        *WorkerRuntimeOperationFailure `json:"failed,omitempty"`
}

type WorkerCloseActorRequest struct {
	WorkerActorReferenceRequest
	IdempotencyKey string `json:"idempotency_key,omitempty"`
}

type WorkerCloseActorResponse struct {
	CorrelationID string                         `json:"correlation_id"`
	Completed     *ActorOperationReceipt         `json:"completed,omitempty"`
	Failed        *WorkerRuntimeOperationFailure `json:"failed,omitempty"`
}

type WorkerReadActorOutputPageRequest struct {
	WorkerActorReferenceRequest
	After *int64 `json:"after,omitempty"`
	Limit int32  `json:"limit"`
}

type WorkerReadActorOutputPageResponse struct {
	CorrelationID string                         `json:"correlation_id"`
	Completed     *ActorOutputPage               `json:"completed,omitempty"`
	Failed        *WorkerRuntimeOperationFailure `json:"failed,omitempty"`
}

type WorkerWorkspaceAddress struct {
	WorkspaceID  string `json:"workspace_id,omitempty"`
	WorkspaceKey string `json:"workspace_key,omitempty"`
}

type WorkerCreateWorkspaceRequest struct {
	Lease               WorkerRunLeaseFence `json:"lease"`
	CorrelationID       string              `json:"correlation_id"`
	WorkspaceDeclaredID string              `json:"workspace_declared_id"`
	Key                 *string             `json:"key,omitempty"`
	Secrets             []WorkspaceSecret   `json:"secrets,omitempty"`
	IdempotencyKey      string              `json:"idempotency_key,omitempty"`
}

type WorkerCreateWorkspaceResponse struct {
	CorrelationID string                         `json:"correlation_id"`
	Completed     *CreateWorkspaceResponse       `json:"completed,omitempty"`
	Failed        *WorkerRuntimeOperationFailure `json:"failed,omitempty"`
}

type WorkerRetrieveWorkspaceRequest struct {
	Lease         WorkerRunLeaseFence    `json:"lease"`
	CorrelationID string                 `json:"correlation_id"`
	Workspace     WorkerWorkspaceAddress `json:"workspace"`
}

type WorkerRetrieveWorkspaceResponse struct {
	CorrelationID string                         `json:"correlation_id"`
	Completed     *WorkspaceSnapshot             `json:"completed,omitempty"`
	Failed        *WorkerRuntimeOperationFailure `json:"failed,omitempty"`
}

type WorkerReadWorkspaceFileRequest struct {
	WorkerRetrieveWorkspaceRequest
	Path string `json:"path"`
}

type WorkerReadWorkspaceFileResponse struct {
	CorrelationID string                         `json:"correlation_id"`
	Completed     *WorkspaceFileContent          `json:"completed,omitempty"`
	Failed        *WorkerRuntimeOperationFailure `json:"failed,omitempty"`
}

type WorkerStatWorkspaceFileResponse struct {
	CorrelationID string                         `json:"correlation_id"`
	Completed     *WorkspaceFileEntry            `json:"completed,omitempty"`
	Failed        *WorkerRuntimeOperationFailure `json:"failed,omitempty"`
}

type WorkerListWorkspaceFilesRequest struct {
	WorkerReadWorkspaceFileRequest
	Cursor string `json:"cursor,omitempty"`
	Limit  int32  `json:"limit"`
}

type WorkerListWorkspaceFilesResponse struct {
	CorrelationID string                         `json:"correlation_id"`
	Completed     *WorkspaceFilePage             `json:"completed,omitempty"`
	Failed        *WorkerRuntimeOperationFailure `json:"failed,omitempty"`
}

type WorkerExecuteWorkspaceRequest struct {
	WorkerRetrieveWorkspaceRequest
	Command        []string          `json:"command"`
	Cwd            string            `json:"cwd,omitempty"`
	Env            map[string]string `json:"env,omitempty"`
	Stdin          []byte            `json:"stdin,omitempty"`
	TimeoutMS      *int64            `json:"timeout_ms,omitempty"`
	IdempotencyKey string            `json:"idempotency_key"`
}

type WorkerWorkspaceExecPending struct {
	ProcessID string `json:"process_id"`
}

type WorkerExecuteWorkspaceResponse struct {
	CorrelationID string                         `json:"correlation_id"`
	Completed     *ExecuteWorkspaceResult        `json:"completed,omitempty"`
	Pending       *WorkerWorkspaceExecPending    `json:"pending,omitempty"`
	Failed        *WorkerRuntimeOperationFailure `json:"failed,omitempty"`
}

type WorkerPollWorkspaceExecRequest struct {
	WorkerRetrieveWorkspaceRequest
	ProcessID string `json:"process_id"`
}

type WorkerDeleteWorkspaceRequest struct {
	WorkerRetrieveWorkspaceRequest
	IdempotencyKey string `json:"idempotency_key,omitempty"`
}

type WorkerDeleteWorkspaceResponse struct {
	CorrelationID string                         `json:"correlation_id"`
	Completed     *DeleteWorkspaceReceipt        `json:"completed,omitempty"`
	Failed        *WorkerRuntimeOperationFailure `json:"failed,omitempty"`
}

type WorkerInvokeChildTaskRequest struct {
	Lease                         WorkerRunLeaseFence `json:"lease"`
	CorrelationID                 string              `json:"correlation_id"`
	RunWaitID                     string              `json:"run_wait_id,omitempty"`
	ResumeAttachID                string              `json:"resume_attach_id,omitempty"`
	TaskDeclaredID                string              `json:"task_declared_id"`
	Method                        string              `json:"method"`
	PayloadPresent                bool                `json:"payload_present"`
	Payload                       json.RawMessage     `json:"payload,omitempty"`
	Workspace                     json.RawMessage     `json:"workspace"`
	Options                       json.RawMessage     `json:"options"`
	IdempotencyKey                string              `json:"idempotency_key,omitempty"`
	ActorSpeculativeInputSequence *int64              `json:"actor_speculative_input_sequence,omitempty"`
}

type WorkerChildTaskStartResult struct {
	RunID string `json:"run_id"`
}

type WorkerInvokeChildTaskResponse struct {
	CorrelationID string                         `json:"correlation_id"`
	Completed     *WorkerChildTaskStartResult    `json:"completed,omitempty"`
	OpenedWait    *WorkerCreateRunWaitResponse   `json:"opened_wait,omitempty"`
	Failed        *WorkerRuntimeOperationFailure `json:"failed,omitempty"`
}

type WorkerAppendActorOutputRequest struct {
	Lease          WorkerRunLeaseFence `json:"lease"`
	CorrelationID  string              `json:"correlation_id"`
	Data           json.RawMessage     `json:"data"`
	ContentType    string              `json:"content_type"`
	IdempotencyKey string              `json:"idempotency_key,omitempty"`
}

type WorkerAppendActorOutputResponse struct {
	CorrelationID string                         `json:"correlation_id"`
	Completed     *ActorOutputRecord             `json:"completed,omitempty"`
	Failed        *WorkerRuntimeOperationFailure `json:"failed,omitempty"`
}

type WorkerRuntimeOperationFailure struct {
	Code      string `json:"code"`
	Message   string `json:"message"`
	Retryable bool   `json:"retryable"`
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

type WorkerRunLeaseFence struct {
	ID            string `json:"id"`
	LeaseSequence int64  `json:"lease_sequence"`
}

type WorkerRunLeaseAssignment struct {
	ID                               string       `json:"id"`
	RunID                            string       `json:"run_id"`
	AttemptNumber                    int32        `json:"attempt_number"`
	LeaseSequence                    int64        `json:"lease_sequence"`
	WorkerGroupID                    string       `json:"worker_group_id"`
	WorkerInstanceID                 string       `json:"worker_instance_id"`
	WorkerEpoch                      int64        `json:"worker_epoch"`
	WorkerProtocolVersion            string       `json:"worker_protocol_version"`
	RuntimeInstanceID                string       `json:"runtime_instance_id"`
	RuntimeIdentityID                string       `json:"runtime_identity_id"`
	NetworkSlotID                    string       `json:"network_slot_id"`
	NetworkSlotGeneration            int64        `json:"network_slot_generation"`
	WorkspaceID                      string       `json:"workspace_id"`
	WorkspaceMountID                 string       `json:"workspace_mount_id"`
	WorkspaceLeaseID                 string       `json:"workspace_lease_id"`
	BaseWorkspaceVersionID           string       `json:"base_workspace_version_id"`
	OwnershipGeneration              int64        `json:"ownership_generation"`
	WriterGeneration                 int64        `json:"writer_generation"`
	MountFencingGeneration           int64        `json:"mount_fencing_generation"`
	RequestedCPUMillis               int64        `json:"requested_cpu_millis"`
	RequestedMemoryBytes             int64        `json:"requested_memory_bytes"`
	RequestedGuestEphemeralDiskBytes int64        `json:"requested_guest_ephemeral_disk_bytes"`
	RequestedExecutionSlots          int32        `json:"requested_execution_slots"`
	MaxActiveDurationMs              int64        `json:"max_active_duration_ms"`
	ActiveElapsedMs                  int64        `json:"active_elapsed_ms"`
	Trace                            TraceContext `json:"trace"`
	StartDeadlineAt                  time.Time    `json:"start_deadline_at"`
	ExpiresAt                        time.Time    `json:"expires_at"`
}

func (assignment WorkerRunLeaseAssignment) Fence() WorkerRunLeaseFence {
	return WorkerRunLeaseFence{
		ID:            assignment.ID,
		LeaseSequence: assignment.LeaseSequence,
	}
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
	CorrelationID        string                          `json:"correlation_id"`
	EntrypointKind       string                          `json:"entrypoint_kind"`
	EntrypointDeclaredID string                          `json:"entrypoint_declared_id"`
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
	ParentRunID         string `json:"parent_run_id"`
	ParentAttemptNumber int32  `json:"parent_attempt_number"`
	RunWaitID           string `json:"run_wait_id"`
	CheckpointID        string `json:"checkpoint_id"`
	ResumeAttachID      string `json:"resume_attach_id"`
	CorrelationID       string `json:"correlation_id"`
	ProgramStart        []byte `json:"program_start"`
}

type WorkerRunLeaseParentAttach struct {
	RunWaitID            string                 `json:"run_wait_id"`
	CheckpointID         string                 `json:"checkpoint_id"`
	ResumeAttachID       string                 `json:"resume_attach_id"`
	ResumeRequestVersion int64                  `json:"resume_request_version"`
	CorrelationID        string                 `json:"correlation_id"`
	EntrypointKind       string                 `json:"entrypoint_kind"`
	EntrypointDeclaredID string                 `json:"entrypoint_declared_id"`
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

type WorkerRunLeaseAssignmentProvider interface {
	CurrentWorkerRunLeaseAssignment() WorkerRunLeaseAssignment
}

type WorkerDeploymentBuildLease struct {
	ID                               string    `json:"id"`
	OrgID                            string    `json:"org_id"`
	ProjectID                        string    `json:"project_id"`
	EnvironmentID                    string    `json:"environment_id"`
	DeploymentID                     string    `json:"deployment_id"`
	WorkerGroupID                    string    `json:"worker_group_id"`
	WorkerInstanceID                 string    `json:"worker_instance_id"`
	WorkerEpoch                      int64     `json:"worker_epoch"`
	LeaseSequence                    int64     `json:"lease_sequence"`
	WorkerProtocolVersion            string    `json:"worker_protocol_version"`
	ExpiresAt                        time.Time `json:"expires_at"`
	RequestedGuestEphemeralDiskBytes int64     `json:"requested_guest_ephemeral_disk_bytes"`
	RequestedCPUMillis               int64     `json:"requested_cpu_millis"`
	RequestedMemoryBytes             int64     `json:"requested_memory_bytes"`
	RequestedBuildExecutors          int32     `json:"requested_build_executors"`
}

type WorkerDeploymentBuild struct {
	ID                    string                   `json:"id"`
	Version               string                   `json:"version"`
	APIVersion            string                   `json:"api_version"`
	WorkerProtocolVersion string                   `json:"worker_protocol_version"`
	ProjectID             string                   `json:"project_id"`
	EnvironmentID         string                   `json:"environment_id"`
	DeploymentSource      DeploymentSourceArtifact `json:"deployment_source"`
	Runtime               CASObject                `json:"runtime"`
	NodeVersion           string                   `json:"node_version"`
	Manager               WorkerManagerPin         `json:"manager"`
	Toolchain             CASObject                `json:"toolchain"`
	BuildContractVersion  string                   `json:"build_contract_version"`
}

type WorkerManagerPin struct {
	Artifact  CASObject `json:"artifact"`
	Integrity string    `json:"integrity,omitempty"`
	Name      string    `json:"name"`
	Version   string    `json:"version"`
}

type WorkerWorkspace struct {
	ID                string                   `json:"id,omitempty"`
	WorkspaceMountID  string                   `json:"workspace_mount_id,omitempty"`
	FencingGeneration int64                    `json:"fencing_generation,omitempty"`
	WriteLeaseID      string                   `json:"write_lease_id,omitempty"`
	WriteFencingToken string                   `json:"write_fencing_token,omitempty"`
	BaseVersionID     string                   `json:"base_version_id,omitempty"`
	MountPath         string                   `json:"mount_path,omitempty"`
	Artifact          *WorkerWorkspaceArtifact `json:"artifact,omitempty"`
}

type WorkerRuntimeSubstrate struct {
	ID                     string `json:"id,omitempty"`
	DeploymentDefinitionID string `json:"deployment_definition_id"`
	SubstrateDigest        string `json:"substrate_digest"`
	Format                 string `json:"format"`
	BuilderABI             string `json:"builder_abi"`
	LayoutABI              string `json:"layout_abi"`
	SizeBytes              int64  `json:"size_bytes"`
}

type WorkerRuntimeSubstrateRegisterRequest struct {
	DeploymentDefinitionID string `json:"deployment_definition_id"`
	SubstrateDigest        string `json:"substrate_digest"`
	Format                 string `json:"format"`
	BuilderABI             string `json:"builder_abi"`
	LayoutABI              string `json:"layout_abi"`
	SizeBytes              int64  `json:"size_bytes"`
}

type WorkerRuntimeSubstrateRegisterResponse struct {
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

type SecretDeclaration struct {
	Name  string `json:"name"`
	Env   string `json:"env,omitempty"`
	File  string `json:"file,omitempty"`
	Dir   string `json:"dir,omitempty"`
	Mode  string `json:"mode,omitempty"`
	Owner string `json:"owner,omitempty"`
}

type WorkerCompleteDeploymentBuildRequest struct {
	Lease  WorkerDeploymentBuildLease `json:"lease"`
	Result json.RawMessage            `json:"result"`
}

type WorkerDeploymentBuildResponse struct {
	DeploymentID string `json:"deployment_id"`
	Status       string `json:"status"`
}

type WorkerLogStream string

const (
	WorkerLogStreamStdout     WorkerLogStream = "stdout"
	WorkerLogStreamStderr     WorkerLogStream = "stderr"
	WorkerLogStreamStructured WorkerLogStream = "structured"
)

type WorkerRunLogAppendRequest struct {
	Lease         WorkerRunLeaseFence `json:"lease"`
	Stream        WorkerLogStream     `json:"stream"`
	ObservedSeq   uint64              `json:"observed_seq"`
	ContentBase64 string              `json:"content_base64"`
}

type WorkerUpdateRunMetadataRequest struct {
	Lease       WorkerRunLeaseFence `json:"lease"`
	OperationID string              `json:"operation_id"`
	Operation   string              `json:"operation"`
	Key         string              `json:"key,omitempty"`
	Value       json.RawMessage     `json:"value,omitempty"`
	Patch       json.RawMessage     `json:"patch,omitempty"`
	Amount      *float64            `json:"amount,omitempty"`
}

type WorkerStructuredLogRequest struct {
	Lease       WorkerRunLeaseFence `json:"lease"`
	ObservedSeq uint64              `json:"observed_seq"`
	Level       string              `json:"level"`
	Message     string              `json:"message"`
	Attributes  json.RawMessage     `json:"attributes"`
}

type WorkerCreateTokenRequest struct {
	Lease          WorkerRunLeaseFence `json:"lease"`
	CorrelationID  string              `json:"correlation_id"`
	TimeoutMS      *int64              `json:"timeout_ms,omitempty"`
	Tags           []string            `json:"tags,omitempty"`
	Metadata       json.RawMessage     `json:"metadata,omitempty"`
	IdempotencyKey string              `json:"idempotency_key,omitempty"`
}

type WorkerRunWaitKind string

const (
	WorkerRunWaitKindToken      WorkerRunWaitKind = "token"
	WorkerRunWaitKindTimer      WorkerRunWaitKind = "timer"
	WorkerRunWaitKindActorInput WorkerRunWaitKind = "actor_input"
	WorkerRunWaitKindChild      WorkerRunWaitKind = "child"
)

type WorkerCreateRunWaitRequest struct {
	Lease                         WorkerRunLeaseFence `json:"lease"`
	CorrelationID                 string              `json:"correlation_id"`
	RunWaitID                     string              `json:"run_wait_id"`
	ResumeAttachID                string              `json:"resume_attach_id"`
	Kind                          WorkerRunWaitKind   `json:"kind"`
	Params                        json.RawMessage     `json:"params,omitempty"`
	Metadata                      json.RawMessage     `json:"metadata,omitempty"`
	Tags                          []string            `json:"tags,omitempty"`
	TimeoutMS                     *int64              `json:"timeout_ms,omitempty"`
	IdleTimeoutMS                 *int64              `json:"idle_timeout_ms,omitempty"`
	ActorSpeculativeInputSequence *int64              `json:"actor_speculative_input_sequence,omitempty"`
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
	Lease     WorkerRunLeaseFence `json:"lease"`
	RunWaitID string              `json:"run_wait_id"`
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
	Lease                WorkerRunLeaseFence `json:"lease"`
	RunWaitID            string              `json:"run_wait_id"`
	ResumeRequestVersion int64               `json:"resume_request_version"`
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
	Lease            WorkerRunLeaseFence              `json:"lease"`
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
	Lease          WorkerRunLeaseFence `json:"lease"`
	RequestVersion int64               `json:"request_version"`
	RunWaitID      string              `json:"run_wait_id"`
	CheckpointID   string              `json:"checkpoint_id"`
	Error          string              `json:"error"`
}
