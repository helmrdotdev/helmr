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

// RunComputerObjectRequest binds ciphertext publication to an already registered
// checkpoint or finalization candidate, never to a guest-selected Computer.
type RunComputerObjectRequest struct {
	Lease       RunLeaseFence                  `json:"lease"`
	Checkpoint  *ComputerCheckpointPublication `json:"checkpoint,omitempty"`
	OperationID string                         `json:"operation_id,omitempty"`
	Inspection  blockformat.ObjectInspection   `json:"inspection"`
}
type ComputerCheckpointPublication struct {
	ID             string `json:"id"`
	RunWaitID      string `json:"run_wait_id"`
	RequestVersion int64  `json:"request_version"`
}

// ExecComputerObjectRequest attests bytes under the authenticated mount's
// current process finalization authority. The guest cannot publish objects.
type ExecComputerObjectRequest struct {
	OrgID            string                       `json:"org_id"`
	WorkspaceMountID string                       `json:"workspace_mount_id"`
	Inspection       blockformat.ObjectInspection `json:"inspection"`
}
