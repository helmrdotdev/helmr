package workerapi

import "github.com/helmrdotdev/helmr/internal/networkpolicy"

const NetworkPolicyVersion = 1

// NetworkCapabilities reports the authenticated host's common installation
// restrictions. Local interface and peer denies remain additional host limits.
type NetworkCapabilities struct {
	Version      int                        `json:"version"`
	Installation networkpolicy.Installation `json:"installation"`
}

// AllocationRequest adds execution capability to a custody identity. Inventory
// and stop acknowledgements do not require execution capability.
type AllocationRequest struct {
	AllocationIdentity
	Network *NetworkCapabilities `json:"network"`
}
