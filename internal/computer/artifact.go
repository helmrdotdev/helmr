// Package computer owns durable customer disk artifacts, independently of VM memory.
package computer

import (
	"errors"
	"math"

	"github.com/helmrdotdev/helmr/internal/cas"
	"github.com/helmrdotdev/helmr/internal/oci"
)

const DiskMediaType = "application/vnd.helmr.computer.disk.v0+filepack+aesgcm"

// DiskArtifact describes encrypted storage bytes and the exact restored capacity.
// It is a candidate until the owning fenced database transaction commits it.
type DiskArtifact struct {
	Object       cas.Descriptor
	LogicalBytes int64
}

// InitialDisk is an uploaded candidate, not permission to boot. The owning
// transaction must publish its artifact and configuration together against the
// initializing version and live publisher fence before the working disk is used.
type InitialDisk struct {
	Artifact DiskArtifact
	Config   oci.RuntimeConfig
}

// Validate checks metadata against the capacity selected by Computer authority.
// It does not authenticate stored bytes, prove publication or authorize execution.
func (a DiskArtifact) Validate(capacity int64) error {
	if err := cas.ValidateDescriptor(a.Object); err != nil {
		return err
	}
	limit, err := diskArtifactLimit(capacity)
	if err != nil {
		return err
	}
	if a.Object.MediaType != DiskMediaType || a.Object.SizeBytes > limit || a.LogicalBytes != capacity {
		return errors.New("computer disk artifact does not match its capacity or format")
	}
	return nil
}

// The format admits at most twice the logical capacity plus metadata allowance.
// Enforce the same bound on writes and reads, without trusting CAS size metadata.
func diskArtifactLimit(capacity int64) (int64, error) {
	const allowance = int64(2 << 20)
	if capacity <= 0 || capacity%4096 != 0 || capacity > (math.MaxInt64-allowance-1)/2 {
		return 0, errors.New("invalid computer disk capacity")
	}
	return capacity*2 + allowance, nil
}
