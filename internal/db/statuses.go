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

type WorkerHostStatus = string

const (
	WorkerHostStatusRegistering      WorkerHostStatus = "registering"
	WorkerHostStatusActive           WorkerHostStatus = "active"
	WorkerHostStatusDraining         WorkerHostStatus = "draining"
	WorkerHostStatusTerminationReady WorkerHostStatus = "termination_ready"
	WorkerHostStatusLost             WorkerHostStatus = "lost"
)

type PublicAccessTokenStatus = string

const (
	PublicAccessTokenStatusActive  PublicAccessTokenStatus = "active"
	PublicAccessTokenStatusExpired PublicAccessTokenStatus = "expired"
)

type InstanceDesiredState = string

const (
	InstanceDesiredStateReady  InstanceDesiredState = "ready"
	InstanceDesiredStateClosed InstanceDesiredState = "closed"
)

type InstanceObservedState = string

const (
	InstanceObservedStateAllocated InstanceObservedState = "allocated"
	InstanceObservedStateReady     InstanceObservedState = "ready"
	InstanceObservedStateClosed    InstanceObservedState = "closed"
	InstanceObservedStateFailed    InstanceObservedState = "failed"
	InstanceObservedStateLost      InstanceObservedState = "lost"
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

type ComputerCheckpointStatus = string

const (
	ComputerCheckpointStatusCreating ComputerCheckpointStatus = "creating"
	ComputerCheckpointStatusReady    ComputerCheckpointStatus = "ready"
	ComputerCheckpointStatusInvalid  ComputerCheckpointStatus = "invalid"
	ComputerCheckpointStatusDeleted  ComputerCheckpointStatus = "deleted"
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

type ComputerStatus = string

const (
	ComputerStatusActive           ComputerStatus = "active"
	ComputerStatusDeleting         ComputerStatus = "deleting"
	ComputerStatusRecoveryRequired ComputerStatus = "recovery_required"
	ComputerStatusDeleted          ComputerStatus = "deleted"
)

type ComputerDesiredState = string

const (
	ComputerDesiredStateActive  ComputerDesiredState = "active"
	ComputerDesiredStateStopped ComputerDesiredState = "stopped"
	ComputerDesiredStateDeleted ComputerDesiredState = "deleted"
)

type ComputerDirtyState = string

const (
	ComputerDirtyStateClean          ComputerDirtyState = "clean"
	ComputerDirtyStateDirty          ComputerDirtyState = "dirty"
	ComputerDirtyStateCapturing      ComputerDirtyState = "capturing"
	ComputerDirtyStateDirtyStateLost ComputerDirtyState = "dirty_state_lost"
)

type ComputerDiskVersionStatus = string

const (
	ComputerDiskVersionStatusInitializing ComputerDiskVersionStatus = "initializing"
	ComputerDiskVersionStatusPrivate      ComputerDiskVersionStatus = "private"
	ComputerDiskVersionStatusCommitted    ComputerDiskVersionStatus = "committed"
	ComputerDiskVersionStatusDiscarded    ComputerDiskVersionStatus = "discarded"
)

type ComputerMountStatus = string

const (
	ComputerMountStatusMounting   ComputerMountStatus = "mounting"
	ComputerMountStatusMounted    ComputerMountStatus = "mounted"
	ComputerMountStatusUnmounting ComputerMountStatus = "unmounting"
	ComputerMountStatusUnmounted  ComputerMountStatus = "unmounted"
	ComputerMountStatusLost       ComputerMountStatus = "lost"
	ComputerMountStatusFailed     ComputerMountStatus = "failed"
)

type ComputerLeaseStatus = string

const (
	ComputerLeaseStatusActive    ComputerLeaseStatus = "active"
	ComputerLeaseStatusReleasing ComputerLeaseStatus = "releasing"
	ComputerLeaseStatusReleased  ComputerLeaseStatus = "released"
	ComputerLeaseStatusExpired   ComputerLeaseStatus = "expired"
	ComputerLeaseStatusFenced    ComputerLeaseStatus = "fenced"
)

type ComputerCommandStatus = string

const (
	ComputerCommandStatusPending   ComputerCommandStatus = "pending"
	ComputerCommandStatusStarting  ComputerCommandStatus = "starting"
	ComputerCommandStatusRunning   ComputerCommandStatus = "running"
	ComputerCommandStatusStopping  ComputerCommandStatus = "stopping"
	ComputerCommandStatusCancelled ComputerCommandStatus = "cancelled"
	ComputerCommandStatusTimedOut  ComputerCommandStatus = "timed_out"
	ComputerCommandStatusLost      ComputerCommandStatus = "lost"
	ComputerCommandStatusExited    ComputerCommandStatus = "exited"
	ComputerCommandStatusFailed    ComputerCommandStatus = "failed"
)
