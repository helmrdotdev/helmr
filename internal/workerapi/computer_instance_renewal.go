package workerapi

import "time"

type ComputerInstanceRenewRequest struct {
	EnvironmentID    string `json:"environment_id"`
	AllocationID     string `json:"allocation_id"`
	WriterGeneration int64  `json:"writer_generation"`
}
type ComputerInstanceRenewResponse struct {
	AllocationID     string    `json:"allocation_id"`
	WriterGeneration int64     `json:"writer_generation"`
	DesiredState     string    `json:"desired_state"`
	DesiredVersion   int64     `json:"desired_version"`
	ObservedVersion  int64     `json:"observed_version"`
	WriterExpiresAt  time.Time `json:"writer_expires_at"`
}
