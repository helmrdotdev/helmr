package workerapi

import "github.com/helmrdotdev/helmr/internal/oci"

// RuntimeComputerSource pins preparation to one reserved version. Exactly one
// of Seed and Disk is present. Seed is only valid for an initializing root;
// a missing or invalid continuation disk must never cause reinitialization.
// This projection is not a grant to execute or to publish a version.
type RuntimeComputerSource struct {
	VersionID    string            `json:"version_id"`
	LogicalBytes int64             `json:"logical_bytes"`
	Config       oci.RuntimeConfig `json:"config"`
	Seed         *ComputerSeed     `json:"seed,omitempty"`
	Disk         *CASObject        `json:"disk,omitempty"`
}

type ComputerSeed struct {
	Profile string    `json:"profile"`
	Object  CASObject `json:"object"`
}
