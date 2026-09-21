package computer

import (
	"errors"

	"github.com/helmrdotdev/helmr/internal/cas"
	"github.com/helmrdotdev/helmr/internal/ids"
)

const SeedMediaType = "application/vnd.helmr.computer.seed.v0+filepack+aesgcm"

// SeedIdentity binds shared initial bytes to their owning environment and preparation.
// It is not a Computer identity or permission to initialize a Computer.
type SeedIdentity struct {
	EnvironmentID string
	PreparationID string
}

func (id SeedIdentity) purpose() (string, error) {
	if err := ids.Validate(id.EnvironmentID); err != nil {
		return "", err
	}
	if err := ids.Validate(id.PreparationID); err != nil {
		return "", err
	}
	return "computer-seed:" + id.EnvironmentID + ":" + id.PreparationID, nil
}

// SeedArtifact is a prepared image, never a committed Computer version.
// Trusted preparation publication separately establishes its source and config.
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
	if a.Object.MediaType != SeedMediaType || a.Object.SizeBytes > limit || a.LogicalBytes > capacity {
		return errors.New("computer seed exceeds capacity or has an invalid format")
	}
	return nil
}
