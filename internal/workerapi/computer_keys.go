package workerapi

import "github.com/helmrdotdev/helmr/internal/disk"

// PrepareComputerSeedRequest selects authority, never a caller-chosen key or scope.
type PrepareComputerSeedRequest struct {
	ComputerInstanceID string `json:"computer_instance_id"`
	DesiredVersion     int64  `json:"desired_version"`
}

// ComputerKeyMaterial is host-only. Its owner must clear Key after use and must
// not log this response or forward it to the guest.
type ComputerKeyMaterial struct {
	Scope string `json:"scope"`
	ID    string `json:"id"`
	Key   []byte `json:"key"`
}

// ComputerSourceRequest selects the preparing Instance; source/key identities are
// resolved by the Control Plane, never chosen by the caller.
type ComputerSourceRequest struct {
	ComputerInstanceID string `json:"computer_instance_id"`
	DesiredVersion     int64  `json:"desired_version"`
}

type ComputerSourceMaterial struct {
	WriteKeyID string                `json:"write_key_id"`
	VersionID  string                `json:"version_id"`
	Root       disk.VersionRoot      `json:"root"`
	Keys       []ComputerKeyMaterial `json:"keys"`
}

// Clear releases host-only plaintext after the caller copies it into its owner.
func (m *ComputerSourceMaterial) Clear() {
	for _, k := range m.Keys {
		clear(k.Key)
	}
}

// ComputerSeedPreparation is either ready after adopting a shared root, waiting
// for another Instance, or convert with an exclusive host-only conversion key.
type ComputerSeedPreparation struct {
	Status string               `json:"status"`
	Key    *ComputerKeyMaterial `json:"key,omitempty"`
}
