package workerapi

import "time"

type CaptureAbortRequest struct {
	ComputerInstanceID string `json:"computer_instance_id"`
	WorkerEpoch        int64  `json:"worker_epoch"`
	DesiredVersion     int64  `json:"desired_version"`
	CheckpointID       string `json:"checkpoint_id"`
}

type CaptureAbortResponse struct {
	ComputerInstanceID  string               `json:"computer_instance_id"`
	ComputerID          string               `json:"computer_id"`
	WorkerHostID        string               `json:"worker_host_id"`
	WorkerEpoch         int64                `json:"worker_epoch"`
	DesiredVersion      int64                `json:"desired_version"`
	AbortDesiredVersion int64                `json:"abort_desired_version"`
	CheckpointID        string               `json:"checkpoint_id"`
	WriterGeneration    int64                `json:"writer_generation"`
	MembershipRevision  int64                `json:"membership_revision"`
	VMPlatformID        string               `json:"vm_platform_id"`
	Disposition         string               `json:"disposition"`
	WriteCapability     string               `json:"write_capability,omitempty"`
	Members             []CaptureAbortMember `json:"members"`
}

const (
	CaptureAborted           = "aborted"
	CaptureAbortAcknowledged = "acknowledged"
	CaptureAdopted           = "adopted"
)

type CaptureAbortMember struct {
	RunID                     string        `json:"run_id"`
	AttemptNumber             int32         `json:"attempt_number"`
	RunWaitID                 string        `json:"run_wait_id"`
	Lease                     RunLeaseFence `json:"lease"`
	BaseComputerDiskVersionID string        `json:"base_computer_disk_version_id"`
	ExpiresAt                 time.Time     `json:"expires_at"`
	Cancelled                 bool          `json:"cancelled"`
}

type CaptureAbortCompleteRequest struct {
	CancelledRunLeaseIDs []string `json:"cancelled_run_lease_ids"`
	ComputerInstanceID   string   `json:"computer_instance_id"`
	WorkerEpoch          int64    `json:"worker_epoch"`
	DesiredVersion       int64    `json:"desired_version"`
	CheckpointID         string   `json:"checkpoint_id"`
	AbortDesiredVersion  int64    `json:"abort_desired_version"`
}
