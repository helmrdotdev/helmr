package db

type WorkerGroupState = string

const (
	WorkerGroupStateActive   WorkerGroupState = "active"
	WorkerGroupStatePaused   WorkerGroupState = "paused"
	WorkerGroupStateDraining WorkerGroupState = "draining"
	WorkerGroupStateDisabled WorkerGroupState = "disabled"
)

type TelemetryOutboxState = string

const (
	TelemetryOutboxStatePending TelemetryOutboxState = "pending"
	TelemetryOutboxStateClaimed TelemetryOutboxState = "claimed"
	TelemetryOutboxStateWritten TelemetryOutboxState = "written"
	TelemetryOutboxStateFailed  TelemetryOutboxState = "failed"
)

type DeviceCodeStatus = string

const (
	DeviceCodeStatusPending  DeviceCodeStatus = "pending"
	DeviceCodeStatusApproved DeviceCodeStatus = "approved"
	DeviceCodeStatusDenied   DeviceCodeStatus = "denied"
	DeviceCodeStatusConsumed DeviceCodeStatus = "consumed"
)

type WorkerInstanceState = string

const (
	WorkerInstanceStateRegistering      WorkerInstanceState = "registering"
	WorkerInstanceStateActive           WorkerInstanceState = "active"
	WorkerInstanceStateDraining         WorkerInstanceState = "draining"
	WorkerInstanceStateTerminationReady WorkerInstanceState = "termination_ready"
	WorkerInstanceStateLost             WorkerInstanceState = "lost"
)

type PublicAccessTokenState = string

const (
	PublicAccessTokenStateActive  PublicAccessTokenState = "active"
	PublicAccessTokenStateRevoked PublicAccessTokenState = "revoked"
	PublicAccessTokenStateExpired PublicAccessTokenState = "expired"
)

type RuntimeDesiredState = string

const (
	RuntimeDesiredStateReady  RuntimeDesiredState = "ready"
	RuntimeDesiredStateClosed RuntimeDesiredState = "closed"
)

type RuntimeObservedState = string

const (
	RuntimeObservedStateAllocated RuntimeObservedState = "allocated"
	RuntimeObservedStateReady     RuntimeObservedState = "ready"
	RuntimeObservedStateClosed    RuntimeObservedState = "closed"
	RuntimeObservedStateFailed    RuntimeObservedState = "failed"
	RuntimeObservedStateLost      RuntimeObservedState = "lost"
)

type TokenState = string

const (
	TokenStatePending   TokenState = "pending"
	TokenStateCompleted TokenState = "completed"
	TokenStateExpired   TokenState = "expired"
	TokenStateCancelled TokenState = "cancelled"
)

type WaitState = string

const (
	WaitStatePending   WaitState = "pending"
	WaitStateCompleted WaitState = "completed"
	WaitStateFailed    WaitState = "failed"
	WaitStateCancelled WaitState = "cancelled"
)

type RunWaitState = string

const (
	RunWaitStateHot           RunWaitState = "hot"
	RunWaitStateCheckpointing RunWaitState = "checkpointing"
	RunWaitStateParked        RunWaitState = "parked"
	RunWaitStateResumePending RunWaitState = "resume_pending"
	RunWaitStateResuming      RunWaitState = "resuming"
	RunWaitStateReleased      RunWaitState = "released"
	RunWaitStateCancelled     RunWaitState = "cancelled"
	RunWaitStateFailed        RunWaitState = "failed"
)

type RunCheckpointState = string

const (
	RunCheckpointStateCreating RunCheckpointState = "creating"
	RunCheckpointStateReady    RunCheckpointState = "ready"
	RunCheckpointStateInvalid  RunCheckpointState = "invalid"
	RunCheckpointStateDeleted  RunCheckpointState = "deleted"
)

type RunStatus = string

const (
	RunStatusQueued          RunStatus = "queued"
	RunStatusRunning         RunStatus = "running"
	RunStatusWaiting         RunStatus = "waiting"
	RunStatusRetryDelayed    RunStatus = "retry_delayed"
	RunStatusCancelRequested RunStatus = "cancel_requested"
	RunStatusSucceeded       RunStatus = "succeeded"
	RunStatusFailed          RunStatus = "failed"
	RunStatusCancelled       RunStatus = "cancelled"
	RunStatusExpired         RunStatus = "expired"
	RunStatusSystemFailed    RunStatus = "system_failed"
)

type RunLeaseState = string

const (
	RunLeaseStateAssigned      RunLeaseState = "assigned"
	RunLeaseStateStarting      RunLeaseState = "starting"
	RunLeaseStateRunning       RunLeaseState = "running"
	RunLeaseStateCheckpointing RunLeaseState = "checkpointing"
	RunLeaseStateFinalizing    RunLeaseState = "finalizing"
	RunLeaseStateCheckpointed  RunLeaseState = "checkpointed"
	RunLeaseStateCompleted     RunLeaseState = "completed"
	RunLeaseStateFailed        RunLeaseState = "failed"
	RunLeaseStateCancelled     RunLeaseState = "cancelled"
	RunLeaseStateLost          RunLeaseState = "lost"
	RunLeaseStateRejected      RunLeaseState = "rejected"
	RunLeaseStateExpired       RunLeaseState = "expired"
)

type WorkspaceState = string

const (
	WorkspaceStateActive           WorkspaceState = "active"
	WorkspaceStateDeleting         WorkspaceState = "deleting"
	WorkspaceStateRecoveryRequired WorkspaceState = "recovery_required"
	WorkspaceStateDeleted          WorkspaceState = "deleted"
)

type WorkspaceDesiredState = string

const (
	WorkspaceDesiredStateActive  WorkspaceDesiredState = "active"
	WorkspaceDesiredStateStopped WorkspaceDesiredState = "stopped"
	WorkspaceDesiredStateDeleted WorkspaceDesiredState = "deleted"
)

type WorkspaceDirtyState = string

const (
	WorkspaceDirtyStateClean          WorkspaceDirtyState = "clean"
	WorkspaceDirtyStateDirty          WorkspaceDirtyState = "dirty"
	WorkspaceDirtyStateCapturing      WorkspaceDirtyState = "capturing"
	WorkspaceDirtyStateCaptureFailed  WorkspaceDirtyState = "capture_failed"
	WorkspaceDirtyStateDirtyStateLost WorkspaceDirtyState = "dirty_state_lost"
)

type WorkspaceVersionState = string

const (
	WorkspaceVersionStatePrivate   WorkspaceVersionState = "private"
	WorkspaceVersionStateCommitted WorkspaceVersionState = "committed"
	WorkspaceVersionStateDiscarded WorkspaceVersionState = "discarded"
)

type WorkspaceMountState = string

const (
	WorkspaceMountStateMounting   WorkspaceMountState = "mounting"
	WorkspaceMountStateMounted    WorkspaceMountState = "mounted"
	WorkspaceMountStateUnmounting WorkspaceMountState = "unmounting"
	WorkspaceMountStateUnmounted  WorkspaceMountState = "unmounted"
	WorkspaceMountStateLost       WorkspaceMountState = "lost"
	WorkspaceMountStateFailed     WorkspaceMountState = "failed"
)

type WorkspaceLeaseState = string

const (
	WorkspaceLeaseStateActive    WorkspaceLeaseState = "active"
	WorkspaceLeaseStateReleasing WorkspaceLeaseState = "releasing"
	WorkspaceLeaseStateReleased  WorkspaceLeaseState = "released"
	WorkspaceLeaseStateExpired   WorkspaceLeaseState = "expired"
	WorkspaceLeaseStateFenced    WorkspaceLeaseState = "fenced"
	WorkspaceLeaseStateLost      WorkspaceLeaseState = "lost"
)

type WorkspaceProcessState = string

const (
	WorkspaceProcessStatePending       WorkspaceProcessState = "pending"
	WorkspaceProcessStateStarting      WorkspaceProcessState = "starting"
	WorkspaceProcessStateRunning       WorkspaceProcessState = "running"
	WorkspaceProcessStateExitRequested WorkspaceProcessState = "exit_requested"
	WorkspaceProcessStateExited        WorkspaceProcessState = "exited"
	WorkspaceProcessStateFailed        WorkspaceProcessState = "failed"
)
