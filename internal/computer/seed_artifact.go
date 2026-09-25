package computer

import (
	"errors"
	"github.com/helmrdotdev/helmr/internal/oci"
	"math"

	"github.com/helmrdotdev/helmr/internal/cas"
)

const SeedProfile = "linux-amd64-ext4-v1"
const SeedCapacity = int64(32 << 30)

const SeedMediaType = "application/vnd.helmr.computer.seed.v0+filepack"

// SeedArtifact is a client-built deployment disk, never a committed Computer
// version. Admission binds its exact descriptor and config to a deployment.
type SeedArtifact struct {
	Object       cas.Descriptor
	LogicalBytes int64
}

func (a SeedArtifact) Validate(capacity int64) error {
	if err := cas.ValidateDescriptor(a.Object); err != nil {
		return err
	}
	limit, err := seedArtifactLimit(a.LogicalBytes)
	if err != nil {
		return err
	}
	if a.Object.MediaType != SeedMediaType || a.Object.SizeBytes > limit || a.LogicalBytes != capacity {
		return errors.New("computer seed exceeds capacity or has an invalid format")
	}
	return nil
}

// The format admits at most twice the logical capacity plus metadata allowance.
// Enforce the same bound on writes and reads, without trusting CAS size metadata.
func seedArtifactLimit(capacity int64) (int64, error) {
	const allowance = int64(2 << 20)
	if capacity <= 0 || capacity%4096 != 0 || capacity > (math.MaxInt64-allowance-1)/2 {
		return 0, errors.New("invalid computer disk capacity")
	}
	return capacity*2 + allowance, nil
}

// Seed binds an admitted deployment image to its runtime configuration.
type Seed struct {
	Artifact SeedArtifact
	Config   oci.RuntimeConfig
}
