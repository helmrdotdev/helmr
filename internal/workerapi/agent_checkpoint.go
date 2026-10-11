package workerapi

import "github.com/helmrdotdev/helmr/internal/computercheckpoint"

// Host identity comes exclusively from authenticated Worker transport.
type AgentCheckpointPublication struct {
	EnvironmentID string                      `json:"environment_id"`
	Manifest      computercheckpoint.Manifest `json:"manifest"`
}
