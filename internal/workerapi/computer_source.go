package workerapi

import (
	"github.com/helmrdotdev/helmr/internal/disk"
	"github.com/helmrdotdev/helmr/internal/oci"
)

// InstanceComputerSource pins preparation to one reserved version. Seed is present
// only for initialization. Continuation obtains the retained disk version and keys
// through the authenticated source broker; missing state never causes reseeding.
// This projection is not a grant to execute or to publish a version.
type InstanceComputerSource struct {
	VersionID    string            `json:"version_id"`
	LogicalBytes int64             `json:"logical_bytes"`
	Config       oci.RuntimeConfig `json:"config"`
	Seed         *ComputerSeed     `json:"seed,omitempty"`
	Root         *disk.VersionRoot `json:"root,omitempty"`
}

type ComputerSeed struct {
	Profile string    `json:"profile"`
	Object  CASObject `json:"object"`
}
