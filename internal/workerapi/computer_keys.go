package workerapi

import "github.com/helmrdotdev/helmr/internal/disk"

// ComputerKeyMaterial is host-only. Its owner must clear Key after use and must
// not log this response or forward it to the guest.
type ComputerKeyMaterial struct {
	Scope string `json:"scope"`
	ID    string `json:"id"`
	Key   []byte `json:"key"`
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
