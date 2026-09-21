package computer

import (
	"errors"

	"github.com/helmrdotdev/helmr/internal/cas"
)

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
	limit, err := diskArtifactLimit(a.LogicalBytes)
	if err != nil {
		return err
	}
	if a.Object.MediaType != SeedMediaType || a.Object.SizeBytes > limit || a.LogicalBytes != capacity {
		return errors.New("computer seed exceeds capacity or has an invalid format")
	}
	return nil
}
