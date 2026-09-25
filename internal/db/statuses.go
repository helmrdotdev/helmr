package db

type WorkerGroupStatus = string

const (
	WorkerGroupStatusActive   WorkerGroupStatus = "active"
	WorkerGroupStatusPaused   WorkerGroupStatus = "paused"
	WorkerGroupStatusDraining WorkerGroupStatus = "draining"
	WorkerGroupStatusDisabled WorkerGroupStatus = "disabled"
)

type TelemetryOutboxStatus = string

const (
	TelemetryOutboxStatusPending TelemetryOutboxStatus = "pending"
	TelemetryOutboxStatusClaimed TelemetryOutboxStatus = "claimed"
	TelemetryOutboxStatusWritten TelemetryOutboxStatus = "written"
	TelemetryOutboxStatusFailed  TelemetryOutboxStatus = "failed"
)

type DeviceCodeStatus = string

const (
	DeviceCodeStatusPending  DeviceCodeStatus = "pending"
	DeviceCodeStatusApproved DeviceCodeStatus = "approved"
	DeviceCodeStatusDenied   DeviceCodeStatus = "denied"
	DeviceCodeStatusConsumed DeviceCodeStatus = "consumed"
)

type WorkerInstanceStatus = string

const (
	WorkerInstanceStatusRegistering      WorkerInstanceStatus = "registering"
	WorkerInstanceStatusActive           WorkerInstanceStatus = "active"
	WorkerInstanceStatusDraining         WorkerInstanceStatus = "draining"
	WorkerInstanceStatusTerminationReady WorkerInstanceStatus = "termination_ready"
	WorkerInstanceStatusLost             WorkerInstanceStatus = "lost"
)

type PublicAccessTokenStatus = string

const (
	PublicAccessTokenStatusActive  PublicAccessTokenStatus = "active"
	PublicAccessTokenStatusExpired PublicAccessTokenStatus = "expired"
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

type TokenStatus = string

const (
	TokenStatusPending   TokenStatus = "pending"
	TokenStatusCompleted TokenStatus = "completed"
	TokenStatusExpired   TokenStatus = "expired"
	TokenStatusCancelled TokenStatus = "cancelled"
)

type WaitStatus = string

const (
	WaitStatusPending   WaitStatus = "pending"
	WaitStatusCompleted WaitStatus = "completed"
	WaitStatusFailed    WaitStatus = "failed"
	WaitStatusCancelled WaitStatus = "cancelled"
)

type RunWaitStatus = string

const (
	RunWaitStatusHot           RunWaitStatus = "hot"
	RunWaitStatusCheckpointing RunWaitStatus = "checkpointing"
	RunWaitStatusParked        RunWaitStatus = "parked"
	RunWaitStatusResumePending RunWaitStatus = "resume_pending"
	RunWaitStatusResuming      RunWaitStatus = "resuming"
	RunWaitStatusReleased      RunWaitStatus = "released"
	RunWaitStatusCancelled     RunWaitStatus = "cancelled"
	RunWaitStatusFailed        RunWaitStatus = "failed"
)

type RunCheckpointStatus = string

const (
	RunCheckpointStatusCreating RunCheckpointStatus = "creating"
	RunCheckpointStatusReady    RunCheckpointStatus = "ready"
	RunCheckpointStatusInvalid  RunCheckpointStatus = "invalid"
	RunCheckpointStatusDeleted  RunCheckpointStatus = "deleted"
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

type RunLeaseStatus = string

const (
	RunLeaseStatusAssigned      RunLeaseStatus = "assigned"
	RunLeaseStatusStarting      RunLeaseStatus = "starting"
	RunLeaseStatusRunning       RunLeaseStatus = "running"
	RunLeaseStatusCheckpointing RunLeaseStatus = "checkpointing"
	RunLeaseStatusFinalizing    RunLeaseStatus = "finalizing"
	RunLeaseStatusCheckpointed  RunLeaseStatus = "checkpointed"
	RunLeaseStatusCompleted     RunLeaseStatus = "completed"
	RunLeaseStatusFailed        RunLeaseStatus = "failed"
	RunLeaseStatusCancelled     RunLeaseStatus = "cancelled"
	RunLeaseStatusLost          RunLeaseStatus = "lost"
	RunLeaseStatusRejected      RunLeaseStatus = "rejected"
	RunLeaseStatusExpired       RunLeaseStatus = "expired"
)

type WorkspaceStatus = string

const (
	WorkspaceStatusActive           WorkspaceStatus = "active"
	WorkspaceStatusDeleting         WorkspaceStatus = "deleting"
	WorkspaceStatusRecoveryRequired WorkspaceStatus = "recovery_required"
	WorkspaceStatusDeleted          WorkspaceStatus = "deleted"
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

type WorkspaceVersionStatus = string

const (
	WorkspaceVersionStatusInitializing WorkspaceVersionStatus = "initializing"
	WorkspaceVersionStatusPrivate      WorkspaceVersionStatus = "private"
	WorkspaceVersionStatusCommitted    WorkspaceVersionStatus = "committed"
	WorkspaceVersionStatusDiscarded    WorkspaceVersionStatus = "discarded"
)

type WorkspaceMountStatus = string

const (
	WorkspaceMountStatusMounting   WorkspaceMountStatus = "mounting"
	WorkspaceMountStatusMounted    WorkspaceMountStatus = "mounted"
	WorkspaceMountStatusUnmounting WorkspaceMountStatus = "unmounting"
	WorkspaceMountStatusUnmounted  WorkspaceMountStatus = "unmounted"
	WorkspaceMountStatusLost       WorkspaceMountStatus = "lost"
	WorkspaceMountStatusFailed     WorkspaceMountStatus = "failed"
)

type WorkspaceLeaseStatus = string

const (
	WorkspaceLeaseStatusActive    WorkspaceLeaseStatus = "active"
	WorkspaceLeaseStatusReleasing WorkspaceLeaseStatus = "releasing"
	WorkspaceLeaseStatusReleased  WorkspaceLeaseStatus = "released"
	WorkspaceLeaseStatusExpired   WorkspaceLeaseStatus = "expired"
	WorkspaceLeaseStatusFenced    WorkspaceLeaseStatus = "fenced"
)

type WorkspaceProcessStatus = string

const (
	WorkspaceProcessStatusPending       WorkspaceProcessStatus = "pending"
	WorkspaceProcessStatusStarting      WorkspaceProcessStatus = "starting"
	WorkspaceProcessStatusRunning       WorkspaceProcessStatus = "running"
	WorkspaceProcessStatusExitRequested WorkspaceProcessStatus = "exit_requested"
	WorkspaceProcessStatusExited        WorkspaceProcessStatus = "exited"
	WorkspaceProcessStatusFailed        WorkspaceProcessStatus = "failed"
)
