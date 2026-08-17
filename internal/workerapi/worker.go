package workerapi

import (
	"encoding/json"
	"time"

	"github.com/helmrdotdev/helmr/capacityapi"
	"github.com/helmrdotdev/helmr/internal/api"
)

type TokenRequest struct {
	WorkerInstanceID     string `json:"worker_instance_id"`
	WorkerInstanceSecret string `json:"worker_instance_secret"`
	ServiceID            string `json:"service_id"`
}

type TokenResponse struct {
	Token            string `json:"token"`
	ExpiresInSeconds int64  `json:"expires_in_seconds"`
	WorkerEpoch      int64  `json:"worker_epoch"`
}

type EnrollmentResponse struct {
	WorkerInstanceID     string `json:"worker_instance_id"`
	WorkerGroupID        string `json:"worker_group_id"`
	WorkerPoolID         string `json:"worker_pool_id"`
	WorkerInstanceSecret string `json:"worker_instance_secret"`
}

type EnrollmentRequest struct {
	ResourceID string `json:"resource_id"`
	PoolName   string `json:"pool_name"`
}

type RunLeaseDiscoveryRequest struct{}

const (
	WorkerObservationInterval         = 30 * time.Second
	WorkerObservationFreshnessSeconds = int64(120)
	RunFinalizationTerminalTail       = 10 * time.Minute
	RunFinalizationReplayTail         = 30 * time.Second
)

type RunLeaseWork struct {
	LeaseID       string `json:"lease_id"`
	LeaseSequence int64  `json:"lease_sequence"`
}

type RunLeaseDiscoveryResponse struct {
	Items []RunLeaseWork `json:"items"`
}

type ActivateRequest struct {
	Capabilities Capabilities `json:"capabilities"`
}

type ObserveRequest struct {
	Observation Observation `json:"observation"`
}

type StartupRecoveryRequest struct {
	InventoryComplete bool      `json:"inventory_complete"`
	InventoryScope    string    `json:"inventory_scope"`
	ObservedAt        time.Time `json:"observed_at"`
	Inventory         []string  `json:"inventory"`
	Reclaimed         []string  `json:"reclaimed,omitempty"`
	Quarantined       []string  `json:"quarantined,omitempty"`
	Errors            []string  `json:"errors,omitempty"`
}

// DrainCompletionRequest is the worker's proof that a server-directed
// drain has removed both durable execution authority and local runtime state.
// The control plane must treat an identical proof as idempotent.
type DrainCompletionRequest struct {
	InventoryComplete bool      `json:"inventory_complete"`
	InventoryScope    string    `json:"inventory_scope"`
	ObservedAt        time.Time `json:"observed_at"`
	Inventory         []string  `json:"inventory"`
	Reclaimed         []string  `json:"reclaimed,omitempty"`
	Quarantined       []string  `json:"quarantined,omitempty"`
	Errors            []string  `json:"errors,omitempty"`
}

type Observation struct {
	RunPausedReason     string `json:"run_paused_reason,omitempty"`
	RuntimePausedReason string `json:"runtime_paused_reason,omitempty"`
}

type Capabilities struct {
	Runtime                   capacityapi.RuntimeProfile `json:"runtime"`
	CPUShapes                 []capacityapi.CPUShape     `json:"cpu_shapes"`
	CPUEnvironment            CPUEnvironment             `json:"cpu_environment"`
	SubstrateFormat           string                     `json:"substrate_format,omitempty"`
	SubstrateContract         string                     `json:"substrate_contract,omitempty"`
	MaxVCPUs                  int64                      `json:"max_vcpus"`
	MaxMemoryMiB              int64                      `json:"max_memory_mib"`
	VMMilliCPU                int64                      `json:"vm_milli_cpu"`
	VMMemoryMiB               int64                      `json:"vm_memory_mib"`
	GuestEphemeralDiskBytes   int64                      `json:"guest_ephemeral_disk_bytes"`
	VMGuestEphemeralDiskBytes int64                      `json:"vm_guest_ephemeral_disk_bytes"`
	ExecutionSlotsAvailable   int32                      `json:"execution_slots_available"`
}

type CPUEnvironment struct {
	Digest             string `json:"digest"`
	FirecrackerVersion string `json:"firecracker_version"`
	HostKernelRelease  string `json:"host_kernel_release"`
	MicrocodeVersion   string `json:"microcode_version"`
	BIOSVersion        string `json:"bios_version"`
	BIOSRevision       string `json:"bios_revision"`
}

type Status string

const (
	StatusActive           Status = "active"
	StatusDraining         Status = "draining"
	StatusTerminationReady Status = "termination_ready"
)

type StatusResponse struct {
	WorkerInstanceID string    `json:"worker_instance_id"`
	WorkerGroupID    string    `json:"worker_group_id"`
	Status           Status    `json:"status"`
	ActiveExecutions int32     `json:"active_executions"`
	Readiness        Readiness `json:"readiness"`
}

type Readiness struct {
	Run     *RoleReadiness `json:"run,omitempty"`
	Runtime *RoleReadiness `json:"runtime,omitempty"`
}

type RoleReadiness struct {
	Ready        bool   `json:"ready"`
	PausedReason string `json:"paused_reason,omitempty"`
}

type FenceRequest struct {
	ReasonCode string `json:"reason_code"`
}

type RuntimeInstance struct {
	ID                     string     `json:"id"`
	OrgID                  string     `json:"org_id"`
	ProjectID              string     `json:"project_id"`
	EnvironmentID          string     `json:"environment_id"`
	WorkerInstanceID       string     `json:"worker_instance_id"`
	RuntimeEpoch           int64      `json:"runtime_epoch"`
	RuntimeID              string     `json:"runtime_id"`
	VMVCPUCount            int32      `json:"vm_vcpu_count"`
	CPUConfigDigest        string     `json:"cpu_config_digest"`
	DeploymentDefinitionID string     `json:"deployment_definition_id"`
	State                  string     `json:"state"`
	ReservedCPUMillis      int32      `json:"reserved_cpu_millis"`
	ReservedMemoryMiB      int32      `json:"reserved_memory_mib"`
	ReservedDiskMiB        int64      `json:"reserved_disk_mib"`
	ReservedExecutionSlots int32      `json:"reserved_execution_slots"`
	WorkspaceMountID       string     `json:"workspace_mount_id,omitempty"`
	ExpiresAt              *time.Time `json:"expires_at,omitempty"`
}

type RuntimeSource struct {
	DeploymentDefinitionID string            `json:"deployment_definition_id"`
	WorkspaceID            string            `json:"workspace_id"`
	RuntimeIdentityID      string            `json:"runtime_identity_id"`
	VMVCPUCount            int32             `json:"vm_vcpu_count"`
	CPUConfigDigest        string            `json:"cpu_config_digest"`
	WorkspaceImage         CASObject         `json:"workspace_image"`
	WorkspaceArchitecture  string            `json:"workspace_architecture"`
	BaseVersionID          string            `json:"base_version_id"`
	WorkspaceArtifact      WorkspaceArtifact `json:"workspace_artifact"`
	RootfsDigest           string            `json:"rootfs_digest"`
	ReservedCPUMillis      int32             `json:"reserved_cpu_millis"`
	ReservedMemoryMiB      int32             `json:"reserved_memory_mib"`
	ReservedDiskMiB        int64             `json:"reserved_disk_mib"`
	ReservedExecutionSlots int32             `json:"reserved_execution_slots"`
	VMRuntimeContract      string            `json:"vm_runtime_contract"`
	Program                *RuntimeProgram   `json:"program,omitempty"`
	Restore                *RuntimeRestore   `json:"restore,omitempty"`
}

type RuntimeRestore struct {
	CheckpointID        string                       `json:"checkpoint_id"`
	RunID               string                       `json:"run_id"`
	AttemptNumber       int32                        `json:"attempt_number"`
	RunWaitID           string                       `json:"run_wait_id"`
	Kind                string                       `json:"kind"`
	Manifest            json.RawMessage              `json:"manifest"`
	Artifacts           []RunLeaseCheckpointArtifact `json:"artifacts"`
	SourceWorkspaceBase *RuntimeRestoreWorkspaceBase `json:"source_workspace_base,omitempty"`
}

type RuntimeRestoreWorkspaceBase struct {
	VersionID string                  `json:"version_id"`
	Base      CheckpointWorkspaceBase `json:"base"`
}

type RuntimeProgram struct {
	DeploymentID  string    `json:"deployment_id"`
	Runtime       CASObject `json:"runtime"`
	Artifact      CASObject `json:"artifact"`
	BuildContract string    `json:"build_contract"`
	IndexDigest   string    `json:"index_digest"`
}

type RuntimeInstanceStateRequest struct {
	ID                      string               `json:"id"`
	WorkerEpoch             int64                `json:"worker_epoch"`
	DesiredVersion          int64                `json:"desired_version"`
	ExpectedObservedVersion int64                `json:"expected_observed_version"`
	VMVCPUCount             int32                `json:"vm_vcpu_count,omitempty"`
	CPUConfigDigest         string               `json:"cpu_config_digest,omitempty"`
	RuntimeSubstrateID      string               `json:"runtime_substrate_id,omitempty"`
	ReasonCode              string               `json:"reason_code,omitempty"`
	Error                   json.RawMessage      `json:"error,omitempty"`
	CleanupProof            *RuntimeCleanupProof `json:"cleanup_proof,omitempty"`
}

const (
	RuntimeFailureReconcile     = "runtime_reconcile_failed"
	RuntimeFailureWorkerInvalid = "worker_runtime_invalid"
)

type RuntimeCleanupProof struct {
	Method      string    `json:"method"`
	CompletedAt time.Time `json:"completed_at"`
}

const (
	RuntimeCleanupSessionClosed   = "session_closed"
	RuntimeCleanupHostReconciled  = "host_reconciled"
	RuntimeCleanupNotMaterialized = "not_materialized"
)

type RuntimeReconcileRequest struct{}

type RuntimeReconcileResponse struct {
	Target *RuntimeReconcileTarget `json:"target,omitempty"`
}

type RuntimeReconcileTarget struct {
	ID                     string        `json:"id"`
	WorkerEpoch            int64         `json:"worker_epoch"`
	DesiredState           string        `json:"desired_state"`
	DesiredVersion         int64         `json:"desired_version"`
	ObservedState          string        `json:"observed_state"`
	ObservedVersion        int64         `json:"observed_version"`
	ObservedDesiredVersion int64         `json:"observed_desired_version"`
	Action                 string        `json:"action"`
	Source                 RuntimeSource `json:"source"`
}

const (
	RuntimeReconcilePrepare = "prepare"
	RuntimeReconcileClose   = "close"
	RuntimeReconcileReclaim = "reclaim"
)

type RunLeaseClaimRequest struct {
	LeaseID       string `json:"lease_id"`
	LeaseSequence int64  `json:"lease_sequence"`
}

type RunLeaseClaimResponse struct {
	Lease     RunLeaseAssignment  `json:"lease"`
	Program   RuntimeProgram      `json:"program"`
	Workspace WorkspaceAttachment `json:"workspace"`
	Secrets   []SecretDelivery    `json:"secrets"`
	Execution RunLeaseExecution   `json:"execution"`
}

type RunStartRequest struct {
	Lease   RunLeaseFence    `json:"lease"`
	Fresh   *RunStartFresh   `json:"fresh,omitempty"`
	Restore *RunStartRestore `json:"restore,omitempty"`
	Attach  *RunStartAttach  `json:"attach,omitempty"`
}

type RunStartFresh struct{}

type RunStartRestore struct {
	RunWaitID            string `json:"run_wait_id"`
	CheckpointID         string `json:"checkpoint_id"`
	ResumeAttachID       string `json:"resume_attach_id"`
	ResumeRequestVersion int64  `json:"resume_request_version"`
}

type RunStartAttach struct {
	Child  *RunStartChildAttach  `json:"child,omitempty"`
	Parent *RunStartParentAttach `json:"parent,omitempty"`
}

type RunStartChildAttach struct {
	RunWaitID      string `json:"run_wait_id"`
	CheckpointID   string `json:"checkpoint_id"`
	ResumeAttachID string `json:"resume_attach_id"`
}

type RunStartParentAttach struct {
	RunWaitID            string `json:"run_wait_id"`
	CheckpointID         string `json:"checkpoint_id"`
	ResumeAttachID       string `json:"resume_attach_id"`
	ResumeRequestVersion int64  `json:"resume_request_version"`
}

type RunStartResponse struct {
	Lease RunLeaseFence `json:"lease"`
}

type RunResumeReleaseRequest struct {
	Lease                RunLeaseFence `json:"lease"`
	RunWaitID            string        `json:"run_wait_id"`
	CheckpointID         string        `json:"checkpoint_id"`
	ResumeAttachID       string        `json:"resume_attach_id"`
	ResumeRequestVersion int64         `json:"resume_request_version"`
}

type RunResumeReleaseResponse struct {
	Lease                RunLeaseFence `json:"lease"`
	RunWaitID            string        `json:"run_wait_id"`
	CheckpointID         string        `json:"checkpoint_id"`
	ResumeAttachID       string        `json:"resume_attach_id"`
	ResumeRequestVersion int64         `json:"resume_request_version"`
}

type RunLeaseRenewRequest struct {
	Lease             RunLeaseFence `json:"lease"`
	ExpectedExpiresAt time.Time     `json:"expected_expires_at"`
}

type RunLeaseRenewResponse struct {
	Lease                  RunLeaseFence `json:"lease"`
	ExpiresAt              time.Time     `json:"expires_at"`
	BaseWorkspaceVersionID string        `json:"base_workspace_version_id"`
}

type RunFinalizationKind string

const (
	RunFinalizationCapture RunFinalizationKind = "capture"
	RunFinalizationReset   RunFinalizationKind = "reset"
)

type RunQuiescenceProof struct {
	RunID         string `json:"run_id"`
	AttemptNumber int32  `json:"attempt_number"`
	RunLeaseID    string `json:"run_lease_id"`
}

type BeginRunFinalizationRequest struct {
	Lease           RunLeaseFence       `json:"lease"`
	ProgramQuiesced RunQuiescenceProof  `json:"program_quiesced"`
	OperationID     string              `json:"operation_id"`
	Kind            RunFinalizationKind `json:"kind"`
}

type BeginRunFinalizationResponse struct {
	Lease                  RunLeaseFence           `json:"lease"`
	BaseWorkspaceVersionID string                  `json:"base_workspace_version_id"`
	ExpiresAt              time.Time               `json:"expires_at"`
	OperationID            string                  `json:"operation_id"`
	Kind                   RunFinalizationKind     `json:"kind"`
	StartedAt              time.Time               `json:"started_at"`
	Handoff                *RunFinalizationHandoff `json:"handoff,omitempty"`
}

type RunFinalizationHandoff struct {
	ParentRunID         string `json:"parent_run_id"`
	ParentAttemptNumber int32  `json:"parent_attempt_number"`
	RunWaitID           string `json:"run_wait_id"`
	SuspendCheckpointID string `json:"suspend_checkpoint_id"`
	ResumeAttachID      string `json:"resume_attach_id"`
	CorrelationID       string `json:"correlation_id"`
}

type RunEntrypointRequest struct {
	Lease                RunLeaseFence `json:"lease"`
	EntrypointKind       string        `json:"entrypoint_kind"`
	EntrypointDeclaredID string        `json:"entrypoint_declared_id"`
}

type CompleteTaskRequest struct {
	Lease     RunLeaseFence          `json:"lease"`
	Outcome   TaskOutcome            `json:"outcome"`
	Workspace TaskWorkspaceProof     `json:"workspace"`
	Handoff   *TaskHandoffCheckpoint `json:"handoff,omitempty"`
}

type TaskHandoffCheckpoint struct {
	CheckpointID string             `json:"checkpoint_id"`
	Manifest     CheckpointManifest `json:"manifest"`
}

type CompleteActorRequest struct {
	Lease     RunLeaseFence      `json:"lease"`
	Outcome   ActorOutcome       `json:"outcome"`
	Workspace TaskWorkspaceProof `json:"workspace"`
}

type CommitActorTurnRequest struct {
	Lease                  RunLeaseFence         `json:"lease"`
	CorrelationID          string                `json:"correlation_id"`
	TargetInputSequence    int64                 `json:"target_input_sequence"`
	BaseWorkspaceVersionID string                `json:"base_workspace_version_id"`
	Tree                   WorkspaceTreeIdentity `json:"tree"`
	Artifact               *WorkspaceArtifact    `json:"artifact,omitempty"`
}

type CommitActorTurnResponse struct {
	Lease                  RunLeaseFence         `json:"lease"`
	CorrelationID          string                `json:"correlation_id"`
	CommittedInputSequence int64                 `json:"committed_input_sequence"`
	WorkspaceVersionID     string                `json:"workspace_version_id"`
	Tree                   WorkspaceTreeIdentity `json:"tree"`
}

type SendActorInputRequest struct {
	Lease          RunLeaseFence   `json:"lease"`
	CorrelationID  string          `json:"correlation_id"`
	SessionID      string          `json:"session_id"`
	Input          json.RawMessage `json:"input"`
	IdempotencyKey string          `json:"idempotency_key,omitempty"`
}

type SendActorInputResponse struct {
	CorrelationID string                   `json:"correlation_id"`
	Completed     *api.SessionInput        `json:"completed,omitempty"`
	Failed        *RuntimeOperationFailure `json:"failed,omitempty"`
}

type StartActorRequest struct {
	Lease           RunLeaseFence             `json:"lease"`
	CorrelationID   string                    `json:"correlation_id"`
	ActorDeclaredID string                    `json:"actor_declared_id"`
	Key             *string                   `json:"key,omitempty"`
	InputPresent    bool                      `json:"input_present"`
	Input           json.RawMessage           `json:"input,omitempty"`
	IdempotencyKey  string                    `json:"idempotency_key,omitempty"`
	Workspace       api.WorkspaceIDTarget     `json:"workspace"`
	Run             *api.StartActorRunOptions `json:"run,omitempty"`
}

type StartActorResponse struct {
	CorrelationID string                   `json:"correlation_id"`
	Completed     *api.StartActorResponse  `json:"completed,omitempty"`
	Failed        *RuntimeOperationFailure `json:"failed,omitempty"`
}

type SessionReferenceRequest struct {
	Lease         RunLeaseFence `json:"lease"`
	CorrelationID string        `json:"correlation_id"`
	SessionID     string        `json:"session_id"`
}

type SessionStatusResponse struct {
	CorrelationID string                   `json:"correlation_id"`
	Completed     *api.Session             `json:"completed,omitempty"`
	Failed        *RuntimeOperationFailure `json:"failed,omitempty"`
}

type CloseSessionRequest struct {
	SessionReferenceRequest
	IdempotencyKey string `json:"idempotency_key,omitempty"`
}

type CloseSessionResponse struct {
	CorrelationID string                   `json:"correlation_id"`
	Completed     *api.SessionCloseReceipt `json:"completed,omitempty"`
	Failed        *RuntimeOperationFailure `json:"failed,omitempty"`
}

type ReadSessionOutputPageRequest struct {
	SessionReferenceRequest
	After *int64 `json:"after,omitempty"`
	Limit int32  `json:"limit"`
}

type ReadSessionOutputPageResponse struct {
	CorrelationID string                   `json:"correlation_id"`
	Completed     *api.SessionOutputPage   `json:"completed,omitempty"`
	Failed        *RuntimeOperationFailure `json:"failed,omitempty"`
}

type WorkspaceAddress struct {
	WorkspaceID string `json:"workspace_id"`
}

type CreateWorkspaceRequest struct {
	Lease             RunLeaseFence         `json:"lease"`
	CorrelationID     string                `json:"correlation_id"`
	SandboxDeclaredID string                `json:"sandbox_declared_id"`
	Key               *string               `json:"key,omitempty"`
	Secrets           []api.WorkspaceSecret `json:"secrets,omitempty"`
	IdempotencyKey    string                `json:"idempotency_key,omitempty"`
}

type CreateWorkspaceResponse struct {
	CorrelationID string                   `json:"correlation_id"`
	Completed     *CreateWorkspaceResult   `json:"completed,omitempty"`
	Failed        *RuntimeOperationFailure `json:"failed,omitempty"`
}

type CreateWorkspaceResult struct {
	WorkspaceID string `json:"workspace_id"`
}

type RetrieveWorkspaceRequest struct {
	Lease         RunLeaseFence    `json:"lease"`
	CorrelationID string           `json:"correlation_id"`
	Workspace     WorkspaceAddress `json:"workspace"`
}

type RetrieveWorkspaceResponse struct {
	CorrelationID string                   `json:"correlation_id"`
	Completed     *api.WorkspaceSnapshot   `json:"completed,omitempty"`
	Failed        *RuntimeOperationFailure `json:"failed,omitempty"`
}

type ReadWorkspaceFileRequest struct {
	RetrieveWorkspaceRequest
	Path string `json:"path"`
}

type ReadWorkspaceFileResponse struct {
	CorrelationID string                    `json:"correlation_id"`
	Completed     *api.WorkspaceFileContent `json:"completed,omitempty"`
	Failed        *RuntimeOperationFailure  `json:"failed,omitempty"`
}

type StatWorkspaceFileResponse struct {
	CorrelationID string                   `json:"correlation_id"`
	Completed     *api.WorkspaceFileEntry  `json:"completed,omitempty"`
	Failed        *RuntimeOperationFailure `json:"failed,omitempty"`
}

type ListWorkspaceFilesRequest struct {
	ReadWorkspaceFileRequest
	Cursor string `json:"cursor,omitempty"`
	Limit  int32  `json:"limit"`
}

type ListWorkspaceFilesResponse struct {
	CorrelationID string                   `json:"correlation_id"`
	Completed     *api.WorkspaceFilePage   `json:"completed,omitempty"`
	Failed        *RuntimeOperationFailure `json:"failed,omitempty"`
}

type ExecuteWorkspaceRequest struct {
	RetrieveWorkspaceRequest
	Command        []string          `json:"command"`
	Cwd            string            `json:"cwd,omitempty"`
	Env            map[string]string `json:"env,omitempty"`
	Stdin          []byte            `json:"stdin,omitempty"`
	TimeoutMS      *int64            `json:"timeout_ms,omitempty"`
	IdempotencyKey string            `json:"idempotency_key"`
}

type WorkspaceExecPending struct {
	ProcessID string `json:"process_id"`
}

type ExecuteWorkspaceResponse struct {
	CorrelationID string                      `json:"correlation_id"`
	Completed     *api.ExecuteWorkspaceResult `json:"completed,omitempty"`
	Pending       *WorkspaceExecPending       `json:"pending,omitempty"`
	Failed        *RuntimeOperationFailure    `json:"failed,omitempty"`
}

type PollWorkspaceExecRequest struct {
	RetrieveWorkspaceRequest
	ProcessID string `json:"process_id"`
}

type DeleteWorkspaceRequest struct {
	RetrieveWorkspaceRequest
	IdempotencyKey string `json:"idempotency_key,omitempty"`
}

type DeleteWorkspaceResponse struct {
	CorrelationID string                      `json:"correlation_id"`
	Completed     *api.DeleteWorkspaceReceipt `json:"completed,omitempty"`
	Failed        *RuntimeOperationFailure    `json:"failed,omitempty"`
}

type InvokeChildTaskRequest struct {
	Lease                         RunLeaseFence   `json:"lease"`
	CorrelationID                 string          `json:"correlation_id"`
	RunWaitID                     string          `json:"run_wait_id,omitempty"`
	ResumeAttachID                string          `json:"resume_attach_id,omitempty"`
	TaskDeclaredID                string          `json:"task_declared_id"`
	Method                        string          `json:"method"`
	PayloadPresent                bool            `json:"payload_present"`
	Payload                       json.RawMessage `json:"payload,omitempty"`
	Workspace                     json.RawMessage `json:"workspace"`
	Options                       json.RawMessage `json:"options"`
	IdempotencyKey                string          `json:"idempotency_key,omitempty"`
	ActorSpeculativeInputSequence *int64          `json:"actor_speculative_input_sequence,omitempty"`
}

type ChildTaskStartResult struct {
	RunID string `json:"run_id"`
}

type InvokeChildTaskResponse struct {
	CorrelationID string                   `json:"correlation_id"`
	Completed     *ChildTaskStartResult    `json:"completed,omitempty"`
	OpenedWait    *CreateRunWaitResponse   `json:"opened_wait,omitempty"`
	Failed        *RuntimeOperationFailure `json:"failed,omitempty"`
}

type AppendActorOutputRequest struct {
	Lease          RunLeaseFence   `json:"lease"`
	CorrelationID  string          `json:"correlation_id"`
	Data           json.RawMessage `json:"data"`
	ContentType    string          `json:"content_type"`
	IdempotencyKey string          `json:"idempotency_key,omitempty"`
}

type AppendActorOutputResponse struct {
	CorrelationID string                   `json:"correlation_id"`
	Completed     *api.SessionOutput       `json:"completed,omitempty"`
	Failed        *RuntimeOperationFailure `json:"failed,omitempty"`
}

type RuntimeOperationFailure struct {
	Code      string `json:"code"`
	Message   string `json:"message"`
	Retryable bool   `json:"retryable"`
}

type ActorOutcome struct {
	TerminalInputSequence int64           `json:"terminal_input_sequence"`
	Succeeded             *ActorSucceeded `json:"succeeded,omitempty"`
	Failed                *TaskFailure    `json:"failed,omitempty"`
}

type ActorSucceeded struct{}

type TaskOutcome struct {
	Succeeded      *TaskSucceeded `json:"succeeded,omitempty"`
	Failed         *TaskFailure   `json:"failed,omitempty"`
	PayloadInvalid *TaskFailure   `json:"payload_invalid,omitempty"`
}

type TaskSucceeded struct {
	Output json.RawMessage `json:"output"`
}

type TaskFailure struct {
	Message string          `json:"message"`
	Details json.RawMessage `json:"details,omitempty"`
}

type TaskWorkspaceProof struct {
	Captured   *TaskWorkspaceCapture  `json:"captured,omitempty"`
	RolledBack *TaskWorkspaceRollback `json:"rolled_back,omitempty"`
}

type TaskWorkspaceCapture struct {
	Receipt  WorkspaceFinalizationReceipt `json:"receipt"`
	Tree     WorkspaceTreeIdentity        `json:"tree"`
	Artifact WorkspaceArtifact            `json:"artifact"`
}

type TaskWorkspaceRollback struct {
	Receipt WorkspaceFinalizationReceipt `json:"receipt"`
	Target  WorkspaceResetTarget         `json:"target"`
}

type WorkspaceFinalizationReceipt struct {
	OperationID        string                     `json:"operation_id"`
	RequestFingerprint string                     `json:"request_fingerprint"`
	Fence              WorkspaceFinalizationFence `json:"fence"`
}

type WorkspaceFinalizationFence struct {
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

type WorkspaceTreeIdentity struct {
	Digest     string `json:"digest"`
	SizeBytes  int64  `json:"size_bytes"`
	EntryCount int32  `json:"entry_count"`
}

type WorkspaceResetTarget struct {
	BaseWorkspaceVersionID string                `json:"base_workspace_version_id"`
	Tree                   WorkspaceTreeIdentity `json:"tree"`
	Empty                  *EmptyWorkspace       `json:"empty,omitempty"`
	Artifact               *WorkspaceArtifact    `json:"artifact,omitempty"`
}

type EmptyWorkspace struct {
}

type RunLeaseFence struct {
	ID            string `json:"id"`
	LeaseSequence int64  `json:"lease_sequence"`
}

type RunLeaseAssignment struct {
	ID                               string           `json:"id"`
	RunID                            string           `json:"run_id"`
	AttemptNumber                    int32            `json:"attempt_number"`
	LeaseSequence                    int64            `json:"lease_sequence"`
	WorkerGroupID                    string           `json:"worker_group_id"`
	WorkerInstanceID                 string           `json:"worker_instance_id"`
	WorkerEpoch                      int64            `json:"worker_epoch"`
	RuntimeInstanceID                string           `json:"runtime_instance_id"`
	RuntimeIdentityID                string           `json:"runtime_identity_id"`
	WorkspaceID                      string           `json:"workspace_id"`
	WorkspaceMountID                 string           `json:"workspace_mount_id"`
	WorkspaceLeaseID                 string           `json:"workspace_lease_id"`
	BaseWorkspaceVersionID           string           `json:"base_workspace_version_id"`
	OwnershipGeneration              int64            `json:"ownership_generation"`
	WriterGeneration                 int64            `json:"writer_generation"`
	MountFencingGeneration           int64            `json:"mount_fencing_generation"`
	RequestedCPUMillis               int64            `json:"requested_cpu_millis"`
	RequestedMemoryBytes             int64            `json:"requested_memory_bytes"`
	RequestedGuestEphemeralDiskBytes int64            `json:"requested_guest_ephemeral_disk_bytes"`
	RequestedExecutionSlots          int32            `json:"requested_execution_slots"`
	MaxActiveDurationMs              int64            `json:"max_active_duration_ms"`
	ActiveElapsedMs                  int64            `json:"active_elapsed_ms"`
	Trace                            api.TraceContext `json:"trace"`
	StartDeadlineAt                  time.Time        `json:"start_deadline_at"`
	ExpiresAt                        time.Time        `json:"expires_at"`
}

func (assignment RunLeaseAssignment) Fence() RunLeaseFence {
	return RunLeaseFence{
		ID:            assignment.ID,
		LeaseSequence: assignment.LeaseSequence,
	}
}

type WorkspaceAttachment struct {
	WriteCapability string               `json:"write_capability"`
	ResetTarget     WorkspaceResetTarget `json:"reset_target"`
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

type RunLeaseExecution struct {
	Fresh   *RunLeaseFresh   `json:"fresh,omitempty"`
	Restore *RunLeaseRestore `json:"restore,omitempty"`
	Attach  *RunLeaseAttach  `json:"attach,omitempty"`
}

type RunLeaseFresh struct {
	ProgramStart []byte `json:"program_start"`
}

type RunLeaseRestore struct {
	RunWaitID            string                    `json:"run_wait_id"`
	CheckpointID         string                    `json:"checkpoint_id"`
	ResumeAttachID       string                    `json:"resume_attach_id"`
	ResumeRequestVersion int64                     `json:"resume_request_version"`
	CorrelationID        string                    `json:"correlation_id"`
	EntrypointKind       string                    `json:"entrypoint_kind"`
	EntrypointDeclaredID string                    `json:"entrypoint_declared_id"`
	Recreated            *RunLeaseRecreatedRestore `json:"recreated,omitempty"`
	Retained             *RunLeaseRetainedRestore  `json:"retained,omitempty"`
	Decision             RunLeaseDecision          `json:"decision"`
}

type RunLeaseRecreatedRestore struct {
	Kind      string                       `json:"kind"`
	Manifest  json.RawMessage              `json:"manifest"`
	Artifacts []RunLeaseCheckpointArtifact `json:"artifacts"`
}

type RunLeaseRetainedRestore struct {
	EnclosingRunWaitID string `json:"enclosing_run_wait_id"`
}

type RunLeaseAttach struct {
	Child  *RunLeaseChildAttach  `json:"child,omitempty"`
	Parent *RunLeaseParentAttach `json:"parent,omitempty"`
}

type RunLeaseChildAttach struct {
	ParentRunID         string `json:"parent_run_id"`
	ParentAttemptNumber int32  `json:"parent_attempt_number"`
	RunWaitID           string `json:"run_wait_id"`
	CheckpointID        string `json:"checkpoint_id"`
	ResumeAttachID      string `json:"resume_attach_id"`
	CorrelationID       string `json:"correlation_id"`
	ProgramStart        []byte `json:"program_start"`
}

type RunLeaseParentAttach struct {
	RunWaitID            string           `json:"run_wait_id"`
	CheckpointID         string           `json:"checkpoint_id"`
	ResumeAttachID       string           `json:"resume_attach_id"`
	ResumeRequestVersion int64            `json:"resume_request_version"`
	CorrelationID        string           `json:"correlation_id"`
	EntrypointKind       string           `json:"entrypoint_kind"`
	EntrypointDeclaredID string           `json:"entrypoint_declared_id"`
	Decision             RunLeaseDecision `json:"decision"`
}

type RunLeaseCheckpointArtifact struct {
	Role    string    `json:"role"`
	Ordinal int32     `json:"ordinal"`
	Object  CASObject `json:"object"`
}

type RunLeaseDecision struct {
	Completed *RunLeaseCompleted `json:"completed,omitempty"`
	Failed    *RunLeaseFailed    `json:"failed,omitempty"`
	Cancelled *RunLeaseCancelled `json:"cancelled,omitempty"`
}

type RunLeaseCompleted struct {
	NoResult   *struct{}       `json:"no_result,omitempty"`
	ResultJSON json.RawMessage `json:"result_json,omitempty"`
}

type RunLeaseFailed struct {
	ReasonCode string          `json:"reason_code"`
	Error      json.RawMessage `json:"error,omitempty"`
}

type RunLeaseCancelled struct {
	ReasonCode string          `json:"reason_code"`
	Error      json.RawMessage `json:"error,omitempty"`
}

type RunLease struct {
	ID                string           `json:"id"`
	OrgID             string           `json:"org_id"`
	RunID             string           `json:"run_id"`
	WorkerGroupID     string           `json:"worker_group_id"`
	WorkerInstanceID  string           `json:"worker_instance_id"`
	WorkerEpoch       int64            `json:"worker_epoch"`
	LeaseSequence     int64            `json:"lease_sequence"`
	SnapshotVersion   int64            `json:"snapshot_version"`
	RuntimeInstanceID string           `json:"runtime_instance_id"`
	AttemptNumber     int32            `json:"attempt_number"`
	Trace             api.TraceContext `json:"trace"`
	ExpiresAt         time.Time        `json:"expires_at"`
}

type RunLeaseProvider interface {
	CurrentWorkerRunLease() RunLease
}

type RunLeaseAssignmentProvider interface {
	CurrentWorkerRunLeaseAssignment() RunLeaseAssignment
}

type Workspace struct {
	ID                string             `json:"id,omitempty"`
	WorkspaceMountID  string             `json:"workspace_mount_id,omitempty"`
	FencingGeneration int64              `json:"fencing_generation,omitempty"`
	WriteLeaseID      string             `json:"write_lease_id,omitempty"`
	WriteFencingToken string             `json:"write_fencing_token,omitempty"`
	BaseVersionID     string             `json:"base_version_id,omitempty"`
	MountPath         string             `json:"mount_path,omitempty"`
	Artifact          *WorkspaceArtifact `json:"artifact,omitempty"`
}

type RuntimeSubstrate struct {
	ID                     string `json:"id,omitempty"`
	DeploymentDefinitionID string `json:"deployment_definition_id"`
	SubstrateDigest        string `json:"substrate_digest"`
	Format                 string `json:"format"`
	Contract               string `json:"contract"`
	SizeBytes              int64  `json:"size_bytes"`
}

type RuntimeSubstrateRegisterRequest struct {
	DeploymentDefinitionID string `json:"deployment_definition_id"`
	SubstrateDigest        string `json:"substrate_digest"`
	Format                 string `json:"format"`
	Contract               string `json:"contract"`
	SizeBytes              int64  `json:"size_bytes"`
}

type RuntimeSubstrateRegisterResponse struct {
	RuntimeSubstrate RuntimeSubstrate `json:"runtime_substrate"`
}

type WorkspaceArtifact struct {
	Digest     string `json:"digest"`
	MediaType  string `json:"media_type"`
	Encoding   string `json:"encoding"`
	SizeBytes  int64  `json:"size_bytes"`
	EntryCount int32  `json:"entry_count"`
}

type Restore struct {
	CheckpointID string             `json:"checkpoint_id"`
	Checkpoint   CheckpointManifest `json:"checkpoint"`
	RunWait      RestoreRunWait     `json:"run_wait"`
}

type RestoreRunWait struct {
	ID                   string          `json:"id"`
	CorrelationID        string          `json:"correlation_id"`
	ResumeAttachID       string          `json:"resume_attach_id"`
	ResumeRequestVersion int64           `json:"resume_request_version"`
	Kind                 string          `json:"kind"`
	ResumeKind           string          `json:"resume_kind"`
	ResumePayloadJSON    json.RawMessage `json:"resume_payload_json"`
}

type SecretDeclaration struct {
	Name  string `json:"name"`
	Env   string `json:"env,omitempty"`
	File  string `json:"file,omitempty"`
	Dir   string `json:"dir,omitempty"`
	Mode  string `json:"mode,omitempty"`
	Owner string `json:"owner,omitempty"`
}

type LogStream string

const (
	LogStreamStdout     LogStream = "stdout"
	LogStreamStderr     LogStream = "stderr"
	LogStreamStructured LogStream = "structured"
)

type RunLogAppendRequest struct {
	Lease         RunLeaseFence `json:"lease"`
	Stream        LogStream     `json:"stream"`
	ObservedSeq   uint64        `json:"observed_seq"`
	ContentBase64 string        `json:"content_base64"`
}

type UpdateRunMetadataRequest struct {
	Lease       RunLeaseFence   `json:"lease"`
	OperationID string          `json:"operation_id"`
	Operation   string          `json:"operation"`
	Key         string          `json:"key,omitempty"`
	Value       json.RawMessage `json:"value,omitempty"`
	Patch       json.RawMessage `json:"patch,omitempty"`
	Amount      *float64        `json:"amount,omitempty"`
}

type StructuredLogRequest struct {
	Lease       RunLeaseFence   `json:"lease"`
	ObservedSeq uint64          `json:"observed_seq"`
	Level       string          `json:"level"`
	Message     string          `json:"message"`
	Attributes  json.RawMessage `json:"attributes"`
}

type CreateTokenRequest struct {
	Lease          RunLeaseFence   `json:"lease"`
	CorrelationID  string          `json:"correlation_id"`
	TimeoutMS      *int64          `json:"timeout_ms,omitempty"`
	Tags           []string        `json:"tags,omitempty"`
	Metadata       json.RawMessage `json:"metadata,omitempty"`
	IdempotencyKey string          `json:"idempotency_key,omitempty"`
}

type RunWaitKind string

const (
	RunWaitKindToken      RunWaitKind = "token"
	RunWaitKindTimer      RunWaitKind = "timer"
	RunWaitKindActorInput RunWaitKind = "actor_input"
	RunWaitKindChild      RunWaitKind = "child"
)

type CreateRunWaitRequest struct {
	Lease                         RunLeaseFence   `json:"lease"`
	CorrelationID                 string          `json:"correlation_id"`
	RunWaitID                     string          `json:"run_wait_id"`
	ResumeAttachID                string          `json:"resume_attach_id"`
	Kind                          RunWaitKind     `json:"kind"`
	Params                        json.RawMessage `json:"params,omitempty"`
	Metadata                      json.RawMessage `json:"metadata,omitempty"`
	Tags                          []string        `json:"tags,omitempty"`
	TimeoutMS                     *int64          `json:"timeout_ms,omitempty"`
	IdleTimeoutMS                 *int64          `json:"idle_timeout_ms,omitempty"`
	ActorSpeculativeInputSequence *int64          `json:"actor_speculative_input_sequence,omitempty"`
}

type CreateRunWaitResponse struct {
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

type RunWaitPollRequest struct {
	Lease     RunLeaseFence `json:"lease"`
	RunWaitID string        `json:"run_wait_id"`
}

type RunWaitPollStatus string

const (
	RunWaitPollStatusWaiting             RunWaitPollStatus = "waiting"
	RunWaitPollStatusCheckpointRequested RunWaitPollStatus = "checkpoint_requested"
	RunWaitPollStatusResumeRequested     RunWaitPollStatus = "resume_requested"
	RunWaitPollStatusTerminal            RunWaitPollStatus = "terminal"
)

type RunWaitPollResponse struct {
	RunID            string            `json:"run_id"`
	RunWaitID        string            `json:"run_wait_id"`
	Status           RunWaitPollStatus `json:"status"`
	RequestVersion   int64             `json:"request_version,omitempty"`
	CheckpointID     string            `json:"checkpoint_id,omitempty"`
	CaptureWorkspace bool              `json:"capture_workspace,omitempty"`
	RetainSource     bool              `json:"retain_source,omitempty"`
	ResumeKind       string            `json:"resume_kind,omitempty"`
	ResumePayload    json.RawMessage   `json:"resume_payload,omitempty"`
	RequireAck       bool              `json:"require_ack,omitempty"`
}

type RunWaitResumeAckRequest struct {
	Lease                RunLeaseFence `json:"lease"`
	RunWaitID            string        `json:"run_wait_id"`
	ResumeRequestVersion int64         `json:"resume_request_version"`
}

type RunWaitResumeAckResponse struct {
	RunID                string `json:"run_id"`
	RunWaitID            string `json:"run_wait_id"`
	ResumeRequestVersion int64  `json:"resume_request_version"`
}

type CheckpointResponse struct {
	RunID              string `json:"run_id"`
	RunWaitID          string `json:"run_wait_id"`
	CheckpointID       string `json:"checkpoint_id"`
	WorkspaceVersionID string `json:"workspace_version_id,omitempty"`
}

type CheckpointManifest struct {
	RecoveryPoint  CheckpointRecoveryPoint  `json:"recovery_point"`
	RuntimeState   CheckpointRuntimeState   `json:"runtime_state"`
	WorkspaceState CheckpointWorkspaceState `json:"workspace_state"`
	Phases         []CheckpointPhase        `json:"phases,omitempty"`
}

type CheckpointRecoveryPoint struct {
	ID            string            `json:"id,omitempty"`
	RunID         string            `json:"run_id,omitempty"`
	AttemptNumber int32             `json:"attempt_number,omitempty"`
	RunWaitID     string            `json:"run_wait_id,omitempty"`
	CorrelationID string            `json:"correlation_id,omitempty"`
	Runtime       CheckpointRuntime `json:"runtime"`
}

type CheckpointRuntime struct {
	Backend         string                      `json:"backend"`
	ID              string                      `json:"id"`
	Arch            string                      `json:"arch"`
	Contract        string                      `json:"contract"`
	KernelDigest    string                      `json:"kernel_digest"`
	InitramfsDigest string                      `json:"initramfs_digest"`
	RootfsDigest    string                      `json:"rootfs_digest"`
	ConfigDigest    string                      `json:"config_digest"`
	VMVCPUCount     int32                       `json:"vm_vcpu_count"`
	CPUConfigDigest string                      `json:"cpu_config_digest"`
	Substrate       *CheckpointRuntimeSubstrate `json:"substrate,omitempty"`
}

type CheckpointRuntimeSubstrate struct {
	Digest    string `json:"digest"`
	Format    string `json:"format"`
	Contract  string `json:"contract"`
	SizeBytes int64  `json:"size_bytes"`
}

type CheckpointRuntimeState struct {
	ConfigArtifact      CheckpointArtifact   `json:"config_artifact"`
	VMStateArtifact     CheckpointArtifact   `json:"vm_state_artifact"`
	ScratchDiskArtifact CheckpointArtifact   `json:"scratch_disk_artifact"`
	MemoryArtifacts     []CheckpointArtifact `json:"memory_artifacts,omitempty"`
	Config              json.RawMessage      `json:"config,omitempty"`
}

type CheckpointWorkspaceState struct {
	Base CheckpointWorkspaceBase `json:"base"`
}

type CheckpointWorkspaceBase struct {
	ArtifactDigest    string `json:"artifact_digest"`
	ArtifactSizeBytes int64  `json:"artifact_size_bytes"`
	ArtifactMediaType string `json:"artifact_media_type"`
	ArtifactEncoding  string `json:"artifact_encoding"`
	MountPath         string `json:"mount_path"`
}

func CheckpointWorkspaceBaseEqual(left, right CheckpointWorkspaceBase) bool {
	return left.ArtifactDigest == right.ArtifactDigest &&
		left.ArtifactSizeBytes == right.ArtifactSizeBytes &&
		left.ArtifactMediaType == right.ArtifactMediaType &&
		left.ArtifactEncoding == right.ArtifactEncoding &&
		left.MountPath == right.MountPath
}

type CheckpointArtifact struct {
	Digest            string `json:"digest"`
	SizeBytes         int64  `json:"size_bytes"`
	MediaType         string `json:"media_type"`
	EncryptDurationMs int64  `json:"encrypt_duration_ms,omitempty"`
	StoreDurationMs   int64  `json:"store_duration_ms,omitempty"`
}

type CheckpointPhase struct {
	Name       string                   `json:"name"`
	DurationMs int64                    `json:"duration_ms"`
	Role       string                   `json:"role,omitempty"`
	MediaType  string                   `json:"media_type,omitempty"`
	ErrorClass string                   `json:"error_class,omitempty"`
	Filepack   *CheckpointFilepackStats `json:"filepack,omitempty"`
}

type CheckpointFilepackStats struct {
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

type CheckpointReadyRequest struct {
	Lease            RunLeaseFence              `json:"lease"`
	RequestVersion   int64                      `json:"request_version"`
	RunWaitID        string                     `json:"run_wait_id"`
	CheckpointID     string                     `json:"checkpoint_id"`
	SourceCleanup    *RuntimeCleanupProof       `json:"source_cleanup,omitempty"`
	WorkspaceCapture CheckpointWorkspaceCapture `json:"workspace_capture"`
	Manifest         CheckpointManifest         `json:"manifest"`
}

type CheckpointWorkspaceCapture struct {
	Tree     WorkspaceTreeIdentity `json:"tree"`
	Artifact WorkspaceArtifact     `json:"artifact"`
}

type CheckpointFailedRequest struct {
	Lease          RunLeaseFence `json:"lease"`
	RequestVersion int64         `json:"request_version"`
	RunWaitID      string        `json:"run_wait_id"`
	CheckpointID   string        `json:"checkpoint_id"`
	Error          string        `json:"error"`
}
