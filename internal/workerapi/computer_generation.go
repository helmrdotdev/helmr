package workerapi

import (
	"github.com/helmrdotdev/helmr/internal/computer"
	"github.com/helmrdotdev/helmr/internal/oci"
)

// InitialComputerGenerationRequest publishes a certified generation for one
// preparation. Computer and version identities are resolved by the Control Plane.
type InitialComputerGenerationRequest struct {
	RuntimeInstanceID string                  `json:"runtime_instance_id"`
	DesiredVersion    int64                   `json:"desired_version"`
	Root              computer.GenerationRoot `json:"root"`
	Config            oci.RuntimeConfig       `json:"config"`
}

type InitialComputerGenerationResponse struct {
	ComputerID string `json:"computer_id"`
	VersionID  string `json:"version_id"`
}
