package workerapi

import (
	"github.com/helmrdotdev/helmr/internal/disk"
	"github.com/helmrdotdev/helmr/internal/oci"
)

// InitialComputerVersionRequest publishes a certified disk version for one
// preparation. Computer and version identities are resolved by the Control Plane.
type InitialComputerVersionRequest struct {
	ComputerInstanceID string            `json:"computer_instance_id"`
	DesiredVersion     int64             `json:"desired_version"`
	Root               disk.VersionRoot  `json:"root"`
	Config             oci.RuntimeConfig `json:"config"`
}

type InitialComputerVersionResponse struct {
	ComputerID string `json:"computer_id"`
	VersionID  string `json:"version_id"`
}
