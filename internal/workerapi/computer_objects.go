package workerapi

import "github.com/helmrdotdev/helmr/internal/computer/blockformat"

// InitialComputerObjectRequest is a host-only attestation of inspected bytes.
// Worker identity, Computer scope and key authority come from authentication and
// the Runtime reservation, never from guest-supplied identifiers.
type InitialComputerObjectRequest struct {
	RuntimeInstanceID string                       `json:"runtime_instance_id"`
	DesiredVersion    int64                        `json:"desired_version"`
	Inspection        blockformat.ObjectInspection `json:"inspection"`
}
