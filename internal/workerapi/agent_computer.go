package workerapi

import "time"

// AgentComputerCaptureRequest carries the retained lease identity. The HTTP
// worker credential supplies the host and worker epoch, never this payload.
type AgentComputerCaptureRequest struct {
	EnvironmentID     string `json:"environment_id"`
	ComputerID        string `json:"computer_id"`
	CheckpointID      string `json:"checkpoint_id"`
	LeaseEpoch        int64  `json:"lease_epoch"`
	ChannelCredential string `json:"channel_credential"`
}
type AgentComputerCaptureResponse struct {
	Capture []byte `json:"capture"`
	SaveID  string `json:"save_id"`
}
type AgentComputerReceiptRequest struct {
	EnvironmentID string `json:"environment_id"`
	CheckpointID  string `json:"checkpoint_id"`
	Receipt       []byte `json:"receipt"`
}
type AgentComputerRestoreRequest struct {
	EnvironmentID     string `json:"environment_id"`
	CheckpointID      string `json:"checkpoint_id"`
	LeaseEpoch        int64  `json:"lease_epoch"`
	ChannelCredential string `json:"channel_credential"`
}
type AgentComputerInstallationResponse struct {
	Installation []byte `json:"installation"`
}

// Installation and Receipt are the exact protobuf messages exchanged with the
// owned guest. JSON carries bytes to avoid reconstructing a retained request.
type AgentComputerInstallationRequest struct {
	EnvironmentID string `json:"environment_id"`
	Installation  []byte `json:"installation"`
	Receipt       []byte `json:"receipt"`
}
type AgentComputerUnsealedRequest struct {
	EnvironmentID    string    `json:"environment_id"`
	CheckpointID     string    `json:"checkpoint_id"`
	Rejected         bool      `json:"rejected"`
	AbsentObservedAt time.Time `json:"absent_observed_at"`
}
type AgentComputerSaveAbsenceRequest struct {
	EnvironmentID string `json:"environment_id"`
	CheckpointID  string `json:"checkpoint_id"`
	Evidence      string `json:"evidence"`
}

// Continuation failures preserve reconciliation distinctions. None authorizes
// replaying an uncertain physical operation; the owner must inspect its attempt.
const (
	AgentComputerAuthorityUnavailable = "computer_authority_unavailable"
	AgentComputerNotReady             = "computer_not_ready"
	AgentComputerEvidenceConflict     = "computer_evidence_conflict"
)

type AgentComputerControlsResponse struct {
	Controls []byte `json:"controls"`
}

// Source preparation requires the current guest observation; an installed
// request cannot be regenerated from later state.
type AgentComputerSourceAbortRequest struct {
	EnvironmentID     string `json:"environment_id"`
	CheckpointID      string `json:"checkpoint_id"`
	LeaseEpoch        int64  `json:"lease_epoch"`
	ChannelCredential string `json:"channel_credential"`
	Receipt           []byte `json:"receipt"`
}

// The worker's authenticated identity supplies host and worker epoch. The
// physical instance prevents applying a receipt to another allocation.
type AgentComputerLeaseRequest struct {
	EnvironmentID string `json:"environment_id"`
	ComputerID    string `json:"computer_id"`
	InstanceID    string `json:"instance_id"`
	LeaseEpoch    int64  `json:"lease_epoch"`
}

// InvalidCheckpointID is set only for a positively invalid retained checkpoint,
// after the exact allocation's physical cleanup has completed.
type AgentComputerStoppedRequest struct {
	AgentComputerLeaseRequest
	InvalidCheckpointID string `json:"invalid_checkpoint_id,omitempty"`
}

type AgentComputerLeaseResponse struct {
	ExpiresAt time.Time `json:"expires_at"`
}
