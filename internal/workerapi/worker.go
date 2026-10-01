package workerapi

import (
	"encoding/json"
	"time"

	"github.com/helmrdotdev/helmr/internal/api"
	"github.com/helmrdotdev/helmr/internal/disk"
	"github.com/helmrdotdev/helmr/internal/secretbinding"
	"github.com/helmrdotdev/helmr/internal/vmplatform"
)

type TokenRequest struct {
	APIVersion       string `json:"api_version"`
	WorkerHostID     string `json:"worker_host_id"`
	WorkerHostSecret string `json:"worker_host_secret"`
	ServiceID        string `json:"service_id"`
}

type TokenResponse struct {
	Token            string `json:"token"`
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

type RunLeaseDiscoveryRequest struct{}

const (
	WorkerObservationInterval   = 30 * time.Second
	RunFinalizationTerminalTail = 10 * time.Minute
	RunFinalizationReplayTail   = 30 * time.Second
)

type RunLeaseWork struct {
	LeaseID       string `json:"lease_id"`
	LeaseSequence int64  `json:"lease_sequence"`
}

type RunLeaseDiscoveryResponse struct {
	Items []RunLeaseWork `json:"items"`
}

type ActivateRequest struct {
	APIVersion   string       `json:"api_version"`
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

type Status string

const (
	StatusActive           Status = "active"
	StatusDraining         Status = "draining"
	StatusTerminationReady Status = "termination_ready"
)

type StatusResponse struct {
	WorkerHostID    string    `json:"worker_host_id"`
	WorkerGroupID   string    `json:"worker_group_id"`
	Status          Status    `json:"status"`
	ActiveInstances int32     `json:"active_instances"`
	Readiness       Readiness `json:"readiness"`
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

type ComputerInstance struct {
	ID                     string     `json:"id"`
	OrgID                  string     `json:"org_id"`
	ProjectID              string     `json:"project_id"`
	EnvironmentID          string     `json:"environment_id"`
	WorkerHostID           string     `json:"worker_host_id"`
	RuntimeEpoch           int64      `json:"runtime_epoch"`
	RuntimeID              string     `json:"runtime_id"`
	VMVCPUCount            int32      `json:"vm_vcpu_count"`
	CPUConfigDigest        string     `json:"cpu_config_digest"`
	ComputerSpecID         string     `json:"computer_spec_id"`
	Status                 string     `json:"status"`
	ReservedCPUMillis      int32      `json:"reserved_cpu_millis"`
	ReservedMemoryMiB      int32      `json:"reserved_memory_mib"`
	ReservedDiskMiB        int64      `json:"reserved_disk_mib"`
	ReservedExecutionSlots int32      `json:"reserved_execution_slots"`
	ComputerInstanceID     string     `json:"computer_instance_id,omitempty"`
	ExpiresAt              *time.Time `json:"expires_at,omitempty"`
}

type RuntimeSource struct {
	WriterGeneration       int64                  `json:"writer_generation"`
	Computer               *RuntimeComputerSource `json:"computer,omitempty"`
	ComputerSpecID         string                 `json:"computer_spec_id"`
	ComputerID             string                 `json:"computer_id"`
	VMPlatformID           string                 `json:"vm_platform_id"`
	VMVCPUCount            int32                  `json:"vm_vcpu_count"`
	CPUConfigDigest        string                 `json:"cpu_config_digest"`
	ComputerImage          CASObject              `json:"computer_image"`
	ComputerArchitecture   string                 `json:"computer_architecture"`
	RootfsDigest           string                 `json:"rootfs_digest"`
	ReservedCPUMillis      int32                  `json:"reserved_cpu_millis"`
	ReservedMemoryMiB      int32                  `json:"reserved_memory_mib"`
	ReservedDiskMiB        int64                  `json:"reserved_disk_mib"`
	ReservedExecutionSlots int32                  `json:"reserved_execution_slots"`
	VMRuntimeContract      string                 `json:"vm_runtime_contract"`
	Program                *RuntimeProgram        `json:"program,omitempty"`
	Restore                *RuntimeRestore        `json:"restore,omitempty"`
}

type RuntimeRestore struct {
	CheckpointID string                       `json:"checkpoint_id"`
	Manifest     json.RawMessage              `json:"manifest"`
	Artifacts    []RunLeaseCheckpointArtifact `json:"artifacts"`
}

type RuntimeProgram struct {
	DeploymentID string    `json:"deployment_id"`
	Runtime      CASObject `json:"runtime"`
	Artifact     CASObject `json:"artifact"`
	IndexDigest  string    `json:"index_digest"`
}

type ComputerInstanceStateRequest struct {
	ID                      string               `json:"id"`
	WorkerEpoch             int64                `json:"worker_epoch"`
	DesiredVersion          int64                `json:"desired_version"`
	ExpectedObservedVersion int64                `json:"expected_observed_version"`
	VMVCPUCount             int32                `json:"vm_vcpu_count,omitempty"`
	CPUConfigDigest         string               `json:"cpu_config_digest,omitempty"`
	ReasonCode              string               `json:"reason_code,omitempty"`
	Error                   json.RawMessage      `json:"error,omitempty"`
	CleanupProof            *RuntimeCleanupProof `json:"cleanup_proof,omitempty"`
}

const (
	RuntimeFailureComputerSource = "computer_source_unavailable"
	RuntimeFailureReconcile      = "runtime_reconcile_failed"
	RuntimeFailureWorkerInvalid  = "worker_runtime_invalid"
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
	Items []RuntimeReconcileTarget `json:"items"`
}

// RuntimeCapture is the complete durable capture intent. Correlation IDs are
// resolved from the guest's admitted waits, not invented by the Control Plane.
type RuntimeCapture struct {
	CheckpointID        string              `json:"checkpoint_id"`
	MembershipRevision  int64               `json:"membership_revision"`
	ProgramDeploymentID string              `json:"program_deployment_id,omitempty"`
	Runs                []RuntimeCaptureRun `json:"runs"`
}

type RuntimeCaptureRun struct {
	RunID                         string `json:"run_id"`
	AttemptNumber                 int32  `json:"attempt_number"`
	RunWaitID                     string `json:"run_wait_id"`
	RunLeaseID                    string `json:"run_lease_id"`
	ActorSpeculativeInputSequence *int64 `json:"actor_speculative_input_sequence,omitempty"`
}

type RuntimeReconcileTarget struct {
	Capture              *RuntimeCapture `json:"capture,omitempty"`
	ID                   string          `json:"id"`
	WorkerEpoch          int64           `json:"worker_epoch"`
	DesiredVersion       int64           `json:"desired_version"`
	ObservedVersion      int64           `json:"observed_version"`
	Action               string          `json:"action"`
	PreparationExpiresAt time.Time       `json:"preparation_expires_at"`
	Source               RuntimeSource   `json:"source"`
}

const (
	RuntimeReconcileCapture      = "capture"
	RuntimeReconcileAbortCapture = "abort_capture"
	RuntimeReconcilePrepare      = "prepare"
	RuntimeReconcileClose        = "close"
	RuntimeReconcileReclaim      = "reclaim"
)

type RunLeaseClaimRequest struct {
	LeaseID       string `json:"lease_id"`
	LeaseSequence int64  `json:"lease_sequence"`
}

type ProgramResume struct {
	CheckpointID   string `json:"checkpoint_id"`
	RunWaitID      string `json:"run_wait_id"`
	EntrypointKind string `json:"entrypoint_kind"`
}

type RunLeaseClaimResponse struct {
	ProgramResume *ProgramResume     `json:"program_resume,omitempty"`
	ProtectedEnv  *ProtectedEnv      `json:"protected_env,omitempty"`
	Lease         RunLeaseAssignment `json:"lease"`
	Program       RuntimeProgram     `json:"program"`
	Computer      ComputerAttachment `json:"computer"`
	Secrets       []SecretDelivery   `json:"secrets"`
	ProgramStart  []byte             `json:"program_start"`
}

type RunStartRequest struct {
	Lease RunLeaseFence `json:"lease"`
}

type RunStartResponse struct {
	Lease RunLeaseFence `json:"lease"`
}

type RunLeaseRenewRequest struct {
	Lease             RunLeaseFence `json:"lease"`
	ExpectedExpiresAt time.Time     `json:"expected_expires_at"`
}

type RunLeaseRenewResponse struct {
	Lease                     RunLeaseFence `json:"lease"`
	ExpiresAt                 time.Time     `json:"expires_at"`
	BaseComputerDiskVersionID string        `json:"base_computer_disk_version_id"`
}

type RunQuiescenceProof struct {
	RunID         string `json:"run_id"`
	AttemptNumber int32  `json:"attempt_number"`
	RunLeaseID    string `json:"run_lease_id"`
}

type BeginRunFinalizationRequest struct {
	Lease           RunLeaseFence      `json:"lease"`
	ProgramQuiesced RunQuiescenceProof `json:"program_quiesced"`
	OperationID     string             `json:"operation_id"`
}

type BeginRunFinalizationResponse struct {
	Lease       RunLeaseFence `json:"lease"`
	ExpiresAt   time.Time     `json:"expires_at"`
	OperationID string        `json:"operation_id"`
	StartedAt   time.Time     `json:"started_at"`
}

type RunEntrypointRequest struct {
	Lease                RunLeaseFence `json:"lease"`
	EntrypointKind       string        `json:"entrypoint_kind"`
	EntrypointDeclaredID string        `json:"entrypoint_declared_id"`
}

type CompleteTaskRequest struct {
	Lease       RunLeaseFence `json:"lease"`
	OperationID string        `json:"operation_id"`
	Outcome     TaskOutcome   `json:"outcome"`
}

type CompleteActorRequest struct {
	Lease       RunLeaseFence `json:"lease"`
	OperationID string        `json:"operation_id"`
	Outcome     ActorOutcome  `json:"outcome"`
}

type CommitActorTurnRequest struct {
	TurnID              string          `json:"turn_id"`
	RunGeneration       int64           `json:"run_generation"`
	Disposition         string          `json:"disposition"`
	Result              json.RawMessage `json:"result,omitempty"`
	Error               json.RawMessage `json:"error,omitempty"`
	Lease               RunLeaseFence   `json:"lease"`
	CorrelationID       string          `json:"correlation_id"`
	TargetInputSequence int64           `json:"target_input_sequence"`
}

type CommitActorTurnResponse struct {
	EventID                string        `json:"event_id"`
	Lease                  RunLeaseFence `json:"lease"`
	CorrelationID          string        `json:"correlation_id"`
	CommittedInputSequence int64         `json:"committed_input_sequence"`
}

type SubmitSessionDataRequest struct {
	Lease          RunLeaseFence   `json:"lease"`
	CorrelationID  string          `json:"correlation_id"`
	SessionID      string          `json:"session_id"`
	Data           json.RawMessage `json:"data"`
	TurnID         *string         `json:"turn_id,omitempty"`
	IdempotencyKey string          `json:"idempotency_key,omitempty"`
}

type SubmitSessionDataResponse struct {
	CorrelationID string                       `json:"correlation_id"`
	Completed     *api.SessionAdmissionReceipt `json:"completed,omitempty"`
	Failed        *RuntimeOperationFailure     `json:"failed,omitempty"`
}

type StartActorRequest struct {
	Lease           RunLeaseFence             `json:"lease"`
	CorrelationID   string                    `json:"correlation_id"`
	ActorDeclaredID string                    `json:"actor_declared_id"`
	Key             *string                   `json:"key,omitempty"`
	IdempotencyKey  string                    `json:"idempotency_key,omitempty"`
	Computer        api.ComputerIDTarget      `json:"computer"`
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

type TurnReferenceRequest struct {
	SessionReferenceRequest
	TurnID string `json:"turn_id"`
}

type SessionTurnResponse struct {
	CorrelationID string                   `json:"correlation_id"`
	Completed     *api.SessionTurn         `json:"completed,omitempty"`
	Failed        *RuntimeOperationFailure `json:"failed,omitempty"`
}

type InterruptSessionTurnRequest struct {
	TurnReferenceRequest
	IdempotencyKey string `json:"idempotency_key,omitempty"`
}

type InterruptSessionTurnResponse struct {
	CorrelationID string                    `json:"correlation_id"`
	Completed     *api.TurnInterruptReceipt `json:"completed,omitempty"`
	Failed        *RuntimeOperationFailure  `json:"failed,omitempty"`
}

type ResumeSessionRequest struct {
	SessionReferenceRequest
	HoldID         string `json:"hold_id"`
	IdempotencyKey string `json:"idempotency_key,omitempty"`
}

type ResumeSessionResponse struct {
	CorrelationID string                    `json:"correlation_id"`
	Completed     *api.SessionResumeReceipt `json:"completed,omitempty"`
	Failed        *RuntimeOperationFailure  `json:"failed,omitempty"`
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

type CancelSessionRequest struct {
	SessionReferenceRequest
	IdempotencyKey string `json:"idempotency_key,omitempty"`
}

type CancelSessionResponse struct {
	CorrelationID string                    `json:"correlation_id"`
	Completed     *api.SessionCancelReceipt `json:"completed,omitempty"`
	Failed        *RuntimeOperationFailure  `json:"failed,omitempty"`
}

type ReadSessionEventsRequest struct {
	SessionReferenceRequest
	After *int64 `json:"after,omitempty"`
	Limit int32  `json:"limit"`
}

type ReadSessionEventsResponse struct {
	CorrelationID string                   `json:"correlation_id"`
	Completed     *api.SessionEventPage    `json:"completed,omitempty"`
	Failed        *RuntimeOperationFailure `json:"failed,omitempty"`
}

type ComputerAddress struct {
	ComputerID string `json:"computer_id"`
}

type CreateComputerRequest struct {
	Lease             RunLeaseFence           `json:"lease"`
	CorrelationID     string                  `json:"correlation_id"`
	SandboxDeclaredID string                  `json:"sandbox_declared_id"`
	Key               *string                 `json:"key,omitempty"`
	Secrets           []secretbinding.Binding `json:"secrets,omitempty"`
	IdempotencyKey    string                  `json:"idempotency_key,omitempty"`
}

type CreateComputerResponse struct {
	CorrelationID string                   `json:"correlation_id"`
	Completed     *CreateComputerResult    `json:"completed,omitempty"`
	Failed        *RuntimeOperationFailure `json:"failed,omitempty"`
}

type CreateComputerResult struct {
	ComputerID string `json:"computer_id"`
}

type RetrieveComputerRequest struct {
	Lease         RunLeaseFence   `json:"lease"`
	CorrelationID string          `json:"correlation_id"`
	Computer      ComputerAddress `json:"computer"`
}

type RetrieveComputerResponse struct {
	CorrelationID string                   `json:"correlation_id"`
	Completed     *api.ComputerSnapshot    `json:"completed,omitempty"`
	Failed        *RuntimeOperationFailure `json:"failed,omitempty"`
}

type ComputerMembersRequest struct {
	RetrieveComputerRequest
	api.ComputerMembersQuery
}

type ComputerMembersResponse struct {
	CorrelationID string                           `json:"correlation_id"`
	Completed     *api.ListComputerMembersResponse `json:"completed,omitempty"`
	Failed        *RuntimeOperationFailure         `json:"failed,omitempty"`
}

type DeleteComputerRequest struct {
	RetrieveComputerRequest
	IdempotencyKey string `json:"idempotency_key,omitempty"`
}

type DeleteComputerResponse struct {
	CorrelationID string                     `json:"correlation_id"`
	Completed     *api.DeleteComputerReceipt `json:"completed,omitempty"`
	Failed        *RuntimeOperationFailure   `json:"failed,omitempty"`
}

type InvokeChildTaskRequest struct {
	TurnID                        *string         `json:"turn_id"`
	RunGeneration                 *int64          `json:"run_generation"`
	Lease                         RunLeaseFence   `json:"lease"`
	CorrelationID                 string          `json:"correlation_id"`
	RunWaitID                     string          `json:"run_wait_id,omitempty"`
	ResumeAttachID                string          `json:"resume_attach_id,omitempty"`
	TaskDeclaredID                string          `json:"task_declared_id"`
	Method                        string          `json:"method"`
	PayloadPresent                bool            `json:"payload_present"`
	Payload                       json.RawMessage `json:"payload,omitempty"`
	Computer                      json.RawMessage `json:"computer"`
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

type WriteTurnOutputRequest struct {
	MessageDeliveryID *string         `json:"message_delivery_id,omitempty"`
	TurnID            string          `json:"turn_id"`
	RunGeneration     int64           `json:"run_generation"`
	Lease             RunLeaseFence   `json:"lease"`
	CorrelationID     string          `json:"correlation_id"`
	Data              json.RawMessage `json:"data"`
	IdempotencyKey    string          `json:"idempotency_key,omitempty"`
}

type WriteOutputResponse struct {
	CorrelationID string                   `json:"correlation_id"`
	Completed     *api.SessionEvent        `json:"completed,omitempty"`
	Failed        *RuntimeOperationFailure `json:"failed,omitempty"`
}

type RuntimeOperationFailure struct {
	Code      string `json:"code"`
	Message   string `json:"message"`
	Retryable bool   `json:"retryable"`
}

type ActorOutcome struct {
	RunGeneration int64             `json:"run_generation"`
	Succeeded     *ActorSucceeded   `json:"succeeded,omitempty"`
	Failed        *TaskFailure      `json:"failed,omitempty"`
	Interrupted   *ActorInterrupted `json:"interrupted,omitempty"`
}

type ActorSucceeded struct{}

// ActorInterrupted records cooperative application convergence for the held Turn.
// Completion settles the member; process cleanup is acknowledged separately.
type ActorInterrupted struct {
	HoldID string  `json:"hold_id"`
	TurnID *string `json:"turn_id"`
}

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

type ComputerTreeIdentity struct {
	Digest     string `json:"digest"`
	SizeBytes  int64  `json:"size_bytes"`
	EntryCount int32  `json:"entry_count"`
}

// ComputerMountTarget binds guest authority to an already prepared Computer.
type ComputerMountTarget struct {
	BaseComputerDiskVersionID string `json:"base_computer_disk_version_id"`
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
	WorkerHostID                     string           `json:"worker_host_id"`
	WorkerEpoch                      int64            `json:"worker_epoch"`
	ComputerInstanceID               string           `json:"computer_instance_id"`
	VMPlatformID                     string           `json:"vm_platform_id"`
	ComputerID                       string           `json:"computer_id"`
	BaseComputerDiskVersionID        string           `json:"base_computer_disk_version_id"`
	WriterGeneration                 int64            `json:"writer_generation"`
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

type ComputerAttachment struct {
	WriteCapability string              `json:"write_capability"`
	Target          ComputerMountTarget `json:"target"`
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

type RunLeaseCheckpointArtifact struct {
	Role    string    `json:"role"`
	Ordinal int32     `json:"ordinal"`
	Object  CASObject `json:"object"`
}

type RunLease struct {
	ID                 string           `json:"id"`
	OrgID              string           `json:"org_id"`
	RunID              string           `json:"run_id"`
	WorkerGroupID      string           `json:"worker_group_id"`
	WorkerHostID       string           `json:"worker_host_id"`
	WorkerEpoch        int64            `json:"worker_epoch"`
	LeaseSequence      int64            `json:"lease_sequence"`
	SnapshotVersion    int64            `json:"snapshot_version"`
	ComputerInstanceID string           `json:"computer_instance_id"`
	AttemptNumber      int32            `json:"attempt_number"`
	Trace              api.TraceContext `json:"trace"`
	ExpiresAt          time.Time        `json:"expires_at"`
}

type RunLeaseProvider interface {
	CurrentWorkerRunLease() RunLease
}

type RunLeaseAssignmentProvider interface {
	CurrentWorkerRunLeaseAssignment() RunLeaseAssignment
}

type Computer struct {
	ID                        string            `json:"id,omitempty"`
	ComputerInstanceID        string            `json:"computer_instance_id,omitempty"`
	WriterGeneration          int64             `json:"writer_generation,omitempty"`
	BaseComputerDiskVersionID string            `json:"base_computer_disk_version_id,omitempty"`
	MountPath                 string            `json:"mount_path,omitempty"`
	Artifact                  *ComputerArtifact `json:"artifact,omitempty"`
}

type ComputerArtifact struct {
	Digest     string `json:"digest"`
	MediaType  string `json:"media_type"`
	Encoding   string `json:"encoding"`
	SizeBytes  int64  `json:"size_bytes"`
	EntryCount int32  `json:"entry_count"`
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
	TurnID                        *string         `json:"turn_id"`
	RunGeneration                 *int64          `json:"run_generation"`
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
	RunID                 string          `json:"run_id"`
	RunWaitID             string          `json:"run_wait_id"`
	ResumeAttachID        string          `json:"resume_attach_id,omitempty"`
	ComputerInstanceID    string          `json:"computer_instance_id,omitempty"`
	RuntimeEpoch          int64           `json:"runtime_epoch,omitempty"`
	ComputerDiskVersionID string          `json:"computer_disk_version_id,omitempty"`
	ResolutionKind        string          `json:"resolution_kind,omitempty"`
	Resolution            json.RawMessage `json:"resolution,omitempty"`
}

type RunWaitPollRequest struct {
	Lease     RunLeaseFence `json:"lease"`
	RunWaitID string        `json:"run_wait_id"`
}

type RunWaitPollStatus string

const (
	RunWaitPollStatusWaiting         RunWaitPollStatus = "waiting"
	RunWaitPollStatusResumeRequested RunWaitPollStatus = "resume_requested"
	RunWaitPollStatusTerminal        RunWaitPollStatus = "terminal"
)

type RunWaitPollResponse struct {
	RunID         string            `json:"run_id"`
	RunWaitID     string            `json:"run_wait_id"`
	Status        RunWaitPollStatus `json:"status"`
	ResumeKind    string            `json:"resume_kind,omitempty"`
	ResumePayload json.RawMessage   `json:"resume_payload,omitempty"`
}

type RunWaitResumeAckRequest struct {
	Lease        RunLeaseFence `json:"lease"`
	RunWaitID    string        `json:"run_wait_id"`
	CheckpointID string        `json:"checkpoint_id"`
}

type RunWaitResumeAckResponse struct {
	RunID        string `json:"run_id"`
	RunWaitID    string `json:"run_wait_id"`
	CheckpointID string `json:"checkpoint_id"`
}

type CheckpointResponse struct {
	RunID                 string `json:"run_id"`
	RunWaitID             string `json:"run_wait_id"`
	CheckpointID          string `json:"checkpoint_id"`
	ComputerDiskVersionID string `json:"computer_disk_version_id,omitempty"`
}

type CheckpointManifest struct {
	RecoveryPoint CheckpointRecoveryPoint `json:"recovery_point"`
	RuntimeState  CheckpointRuntimeState  `json:"runtime_state"`
	ComputerState CheckpointComputerState `json:"computer_state"`
	Phases        []CheckpointPhase       `json:"phases,omitempty"`
}

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

type CheckpointRun struct {
	RunID                         string `json:"run_id"`
	AttemptNumber                 int32  `json:"attempt_number"`
	RunWaitID                     string `json:"run_wait_id"`
	RunLeaseID                    string `json:"run_lease_id"`
	ActorSpeculativeInputSequence *int64 `json:"actor_speculative_input_sequence,omitempty"`
	CorrelationID                 string `json:"correlation_id"`
}

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

// CheckpointComputer binds the writable disk captured with the VM state and RAM.
// Its exact authenticated disk version also serves as a cold continuation.
type CheckpointComputer struct {
	ComputerID   string           `json:"computer_id"`
	LogicalBytes int64            `json:"logical_bytes"`
	Root         disk.VersionRoot `json:"root"`
}

type CheckpointRuntimeState struct {
	Computer            *CheckpointComputer  `json:"computer,omitempty"`
	ConfigArtifact      CheckpointArtifact   `json:"config_artifact"`
	VMStateArtifact     CheckpointArtifact   `json:"vm_state_artifact"`
	ScratchDiskArtifact CheckpointArtifact   `json:"scratch_disk_artifact"`
	MemoryArtifacts     []CheckpointArtifact `json:"memory_artifacts,omitempty"`
	Config              json.RawMessage      `json:"config,omitempty"`
}

type CheckpointComputerState struct {
	Base CheckpointComputerBase `json:"base"`
}

type CheckpointComputerBase struct {
	MountPath string `json:"mount_path"`
}

func CheckpointComputerBaseEqual(left, right CheckpointComputerBase) bool { return left == right }

type CheckpointArtifact struct {
	Digest    string `json:"digest"`
	SizeBytes int64  `json:"size_bytes"`
	MediaType string `json:"media_type"`
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
	EncodedChunks      int64 `json:"encoded_chunks,omitempty"`
	UnpackWrittenBytes int64 `json:"unpack_written_bytes,omitempty"`
}

type CASObject struct {
	Digest    string `json:"digest"`
	SizeBytes int64  `json:"size_bytes"`
	MediaType string `json:"media_type"`
}

// RegisterCheckpointRequest pins the exact encrypted snapshot before any upload.
// It does not claim that the objects exist or that the checkpoint is restorable.
type RegisterCheckpointRequest struct {
	ComputerInstanceID string             `json:"computer_instance_id"`
	WorkerEpoch        int64              `json:"worker_epoch"`
	DesiredVersion     int64              `json:"desired_version"`
	CheckpointID       string             `json:"checkpoint_id"`
	Manifest           CheckpointManifest `json:"manifest"`
}

type ComputerCheckpointResponse struct {
	ComputerInstanceID    string `json:"computer_instance_id"`
	WorkerEpoch           int64  `json:"worker_epoch"`
	DesiredVersion        int64  `json:"desired_version"`
	CheckpointID          string `json:"checkpoint_id"`
	ComputerDiskVersionID string `json:"computer_disk_version_id,omitempty"`
}

type CheckpointReadyRequest struct {
	ComputerInstanceID string             `json:"computer_instance_id"`
	WorkerEpoch        int64              `json:"worker_epoch"`
	DesiredVersion     int64              `json:"desired_version"`
	CheckpointID       string             `json:"checkpoint_id"`
	Manifest           CheckpointManifest `json:"manifest"`
}

// Session output is scoped to the current Actor execution outside a Turn.

// Session output and Turn output have distinct commands; an omitted Turn ID can
// never silently convert an old Turn writer into a Session writer.
type WriteSessionOutputRequest struct {
	Lease          RunLeaseFence   `json:"lease"`
	CorrelationID  string          `json:"correlation_id"`
	RunGeneration  int64           `json:"run_generation"`
	Data           json.RawMessage `json:"data"`
	IdempotencyKey string          `json:"idempotency_key,omitempty"`
}

type TurnExecutionRequest struct {
	Lease         RunLeaseFence `json:"lease"`
	CorrelationID string        `json:"correlation_id"`
	TurnID        string        `json:"turn_id"`
	RunGeneration int64         `json:"run_generation"`
}

type ClaimTurnMessageRequest struct {
	TurnExecutionRequest
	DeliveryID string `json:"delivery_id"`
}

type TurnMessageDelivery struct {
	MessageID  string          `json:"message_id"`
	TurnID     string          `json:"turn_id"`
	DeliveryID string          `json:"delivery_id"`
	Sequence   int64           `json:"sequence"`
	Data       json.RawMessage `json:"data"`
}

type ClaimTurnMessageResponse struct {
	CorrelationID string                   `json:"correlation_id"`
	Delivery      *TurnMessageDelivery     `json:"delivery"`
	Failed        *RuntimeOperationFailure `json:"failed,omitempty"`
}

type CompleteTurnMessageRequest struct {
	TurnExecutionRequest
	MessageID  string          `json:"message_id"`
	DeliveryID string          `json:"delivery_id"`
	Status     string          `json:"status"`
	Code       string          `json:"code,omitempty"`
	Details    json.RawMessage `json:"details,omitempty"`
}

type TurnCommandResponse struct {
	CorrelationID string                   `json:"correlation_id"`
	Accepted      bool                     `json:"accepted"`
	Failed        *RuntimeOperationFailure `json:"failed,omitempty"`
}

type SessionControlRequest struct {
	Lease         RunLeaseFence `json:"lease"`
	CorrelationID string        `json:"correlation_id"`
	RunGeneration int64         `json:"run_generation"`
}

type SessionControlResponse struct {
	CorrelationID string  `json:"correlation_id"`
	HoldID        *string `json:"hold_id"`
	TurnID        *string `json:"turn_id"`
	Reason        *string `json:"reason"`
}
