package workerapi

// InitialComputerKeyRequest selects authority, never a caller-chosen key or scope.
type InitialComputerKeyRequest struct {
	RuntimeInstanceID string `json:"runtime_instance_id"`
	DesiredVersion    int64  `json:"desired_version"`
}

// ComputerKeyMaterial is host-only. Its owner must clear Key after use and must
// not log this response or forward it to the guest.
type ComputerKeyMaterial struct {
	Scope string `json:"scope"`
	ID    string `json:"id"`
	Key   []byte `json:"key"`
}
