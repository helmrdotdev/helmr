package guestd

// This is the physical half of convergence. The runtime must first attest the
// qualified adapter's protocol idle boundary for every supplied scope. A process
// count alone cannot show that a native root has stopped accepting work.
type nativeScopeEvidence struct {
	ScopeID string `json:"scopeId"`
	State   string `json:"state"`
}
