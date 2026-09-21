package workerapi

import "encoding/json"

// These requests belong to the trusted host Worker, never the guest runtime.
type ComputerInitializationRequest struct {
	RuntimeInstanceID string          `json:"runtime_instance_id"`
	DesiredVersion    int64           `json:"desired_version"`
	Disk              CASObject       `json:"disk"`
	LogicalBytes      int64           `json:"logical_bytes"`
	InitialConfig     json.RawMessage `json:"initial_config"`
}

type ComputerInitializationResponse struct {
	ID         string `json:"id"`
	ComputerID string `json:"computer_id"`
	VersionID  string `json:"version_id"`
	Status     string `json:"status"`
	ArtifactID string `json:"artifact_id,omitempty"`
}
