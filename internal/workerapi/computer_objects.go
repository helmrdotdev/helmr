package workerapi

import "github.com/helmrdotdev/helmr/internal/computer/blockformat"

// InitialComputerObjectRequest is a host-only attestation of inspected bytes.
// Worker identity, Computer scope and key authority come from authentication and
// the Runtime reservation, never from guest-supplied identifiers.
type InitialComputerObjectRequest struct {
	ComputerInstanceID string                       `json:"computer_instance_id"`
	DesiredVersion     int64                        `json:"desired_version"`
	Inspection         blockformat.ObjectInspection `json:"inspection"`
}

// CheckpointComputerObjectRequest binds disk publication to the physical capture,
// including a Computer with no resident Run.
type CheckpointComputerObjectRequest struct {
	ComputerInstanceID string                       `json:"computer_instance_id"`
	WorkerEpoch        int64                        `json:"worker_epoch"`
	DesiredVersion     int64                        `json:"desired_version"`
	CheckpointID       string                       `json:"checkpoint_id"`
	Inspection         blockformat.ObjectInspection `json:"inspection"`
}
