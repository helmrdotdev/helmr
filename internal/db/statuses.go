package db

type WorkerGroupStatus = string

const (
	WorkerGroupStatusActive   WorkerGroupStatus = "active"
	WorkerGroupStatusPaused   WorkerGroupStatus = "paused"
	WorkerGroupStatusDraining WorkerGroupStatus = "draining"
	WorkerGroupStatusDisabled WorkerGroupStatus = "disabled"
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
