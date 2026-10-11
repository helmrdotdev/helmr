// Package computercheckpoint defines the coherent encrypted continuation image
// exchanged by the Control Plane and the owning Worker.
package computercheckpoint

import (
	"bytes"
	"encoding/json"
	"errors"
	"github.com/helmrdotdev/helmr/internal/cas"
	"github.com/helmrdotdev/helmr/internal/disk"
	"github.com/helmrdotdev/helmr/internal/sha256sum"
	"github.com/helmrdotdev/helmr/internal/vm"
	"slices"
	"uuid"
)

var ErrInvalidManifest = errors.New("invalid Computer checkpoint manifest")

// Manifest binds the exact frozen resident processes to one
// full VM image and its coherent disk cut. Config keeps its exact bytes because
// the VM identity hashes them. Runtime objects are encrypted by the host's
// checkpoint pipeline before their descriptors are registered.
type Manifest struct {
	CheckpointID   uuid.UUID             `json:"checkpoint_id"`
	ComputerID     uuid.UUID             `json:"computer_id"`
	InstanceID     uuid.UUID             `json:"instance_id"`
	LeaseEpoch     int64                 `json:"lease_epoch"`
	ControlVersion int64                 `json:"control_version"`
	CaptureDigest  []byte                `json:"capture_digest"`
	Members        []Session             `json:"members"`
	Runtime        vm.CheckpointIdentity `json:"runtime"`
	Config         []byte                `json:"config"`
	Disk           disk.VersionRoot      `json:"disk"`
	VMConfig       Object                `json:"vm_config"`
	VMState        Object                `json:"vm_state"`
	Memory         Object                `json:"memory"`
	ScratchDisk    Object                `json:"scratch_disk"`
}

type Session struct {
	SessionID    uuid.UUID `json:"session_id"`
	ProcessEpoch int64     `json:"process_epoch"`
}
type Object struct {
	Digest    string `json:"digest"`
	SizeBytes int64  `json:"size_bytes"`
	MediaType string `json:"media_type"`
}
type RuntimeObject struct {
	Role, media string
	Object
}

func (m Manifest) Objects() []RuntimeObject {
	return []RuntimeObject{
		{"vm_config", cas.CheckpointVMConfigMediaType, m.VMConfig},
		{"vm_state", cas.CheckpointVMStateMediaType, m.VMState},
		{"memory", cas.CheckpointMemoryMediaType, m.Memory},
		{"scratch_disk", cas.CheckpointScratchDiskMediaType, m.ScratchDisk},
	}
}
func (m Manifest) Encode() ([]byte, error) {
	if m.CheckpointID == uuid.Nil() || m.ComputerID == uuid.Nil() || m.InstanceID == uuid.Nil() || m.LeaseEpoch <= 0 || m.ControlVersion <= 0 || len(m.CaptureDigest) != 32 || len(m.Members) == 0 || int64(len(m.Config)) > vm.SnapshotConfigLimit || !json.Valid(m.Config) || m.Runtime.VMConfigDigest != sha256sum.DigestBytes(m.Config) {
		return nil, ErrInvalidManifest
	}
	if _, err := m.Disk.Digest(); err != nil {
		return nil, ErrInvalidManifest
	}
	seen := map[string]bool{}
	for _, o := range m.Objects() {
		if !sha256sum.ValidDigest(o.Digest) || o.SizeBytes <= 0 || o.MediaType != o.media || seen[o.Digest] {
			return nil, ErrInvalidManifest
		}
		seen[o.Digest] = true
	}
	m.Members = slices.Clone(m.Members)
	slices.SortFunc(m.Members, func(a, b Session) int { return bytes.Compare(a.SessionID[:], b.SessionID[:]) })
	for i, member := range m.Members {
		if member.SessionID == uuid.Nil() || member.ProcessEpoch <= 0 || (i > 0 && m.Members[i-1].SessionID == member.SessionID) {
			return nil, ErrInvalidManifest
		}
	}
	raw, err := json.Marshal(m)
	if err != nil {
		return nil, err
	}
	if len(raw) > 1<<20 {
		return nil, ErrInvalidManifest
	}
	return raw, nil
}
