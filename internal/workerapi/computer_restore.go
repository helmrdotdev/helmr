package workerapi

import "time"

// ComputerRestoreAckRequest proves installation of the complete captured set on
// the committed destination. Individual wait outcomes have separate receipts.
// An idle restore requires Grants to be an explicit empty slice (JSON []), not nil.
type ComputerRestoreAckRequest struct {
	ComputerInstanceID string                 `json:"computer_instance_id"`
	CheckpointID       string                 `json:"checkpoint_id"`
	DesiredVersion     int64                  `json:"desired_version"`
	WriterGeneration   int64                  `json:"writer_generation"`
	Grants             []ComputerRestoreGrant `json:"grants"`
}

type ComputerRestoreGrant struct {
	RunID string        `json:"run_id"`
	Lease RunLeaseFence `json:"lease"`
}

type ComputerRestoreAckResponse struct {
	ComputerInstanceID string `json:"computer_instance_id"`
	CheckpointID       string `json:"checkpoint_id"`
	DesiredVersion     int64  `json:"desired_version"`
	WriterGeneration   int64  `json:"writer_generation"`
}

func (request *ComputerRestoreAckRequest) UnmarshalJSON(raw []byte) error {
	type plain ComputerRestoreAckRequest
	var decoded plain
	if err := decodeClosedWorkerJSON(raw, &decoded); err != nil {
		return err
	}
	*request = ComputerRestoreAckRequest(decoded)
	return nil
}

type ComputerRestorePlanRequest struct {
	EnvironmentID      string `json:"environment_id"`
	ComputerInstanceID string `json:"computer_instance_id"`
	WriterGeneration   int64  `json:"writer_generation"`
}
type ComputerRestorePlanResponse struct {
	Plan *ComputerRestorePlan `json:"plan,omitempty"`
}
type ComputerRestorePlan struct {
	ComputerInstanceID string                  `json:"computer_instance_id"`
	ComputerID         string                  `json:"computer_id"`
	CheckpointID       string                  `json:"checkpoint_id"`
	DesiredVersion     int64                   `json:"desired_version"`
	WriterGeneration   int64                   `json:"writer_generation"`
	WorkerHostID       string                  `json:"worker_host_id"`
	WorkerEpoch        int64                   `json:"worker_epoch"`
	VMPlatformID       string                  `json:"vm_platform_id"`
	WriteCapability    string                  `json:"write_capability"`
	Members            []ComputerRestoreMember `json:"members"`
}
type ComputerRestoreMember struct {
	RunID                     string        `json:"run_id"`
	AttemptNumber             int32         `json:"attempt_number"`
	Lease                     RunLeaseFence `json:"lease"`
	BaseComputerDiskVersionID string        `json:"base_computer_disk_version_id"`
	ExpiresAt                 time.Time     `json:"expires_at"`
}

func (r *ComputerRestorePlanRequest) UnmarshalJSON(raw []byte) error {
	type plain ComputerRestorePlanRequest
	var value plain
	if err := decodeClosedWorkerJSON(raw, &value); err != nil {
		return err
	}
	*r = ComputerRestorePlanRequest(value)
	return nil
}
