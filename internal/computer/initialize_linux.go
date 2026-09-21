//go:build linux

package computer

import (
	"context"
	"errors"
	"os"

	"github.com/helmrdotdev/helmr/internal/cas"
	"github.com/helmrdotdev/helmr/internal/oci"
	"github.com/helmrdotdev/helmr/internal/substrate"
)

// Seed identifies the pinned Sandbox image and its verified ext4 projection.
// The resolver must obtain both from the same deployment definition.
type Seed struct {
	ImagePath string
	Image     cas.Descriptor
	Disk      *substrate.DiskSource
}

// InitialDisk is an uploaded candidate, not permission to boot. The owning
// transaction must publish its artifact and configuration together against the
// initializing version and live publisher fence before the working disk is used.
type InitialDisk struct {
	Artifact DiskArtifact
	Config   oci.RuntimeConfig
}

// Initialize prepares an exclusively owned working disk and uploads it before
// any customer process starts. Failure removes only this invocation's working
// file. Success transfers that file and the uploaded candidate to the caller.
// A publication retry must reuse this result: encrypting again changes identity.
// This method never restores or overwrites an existing Computer.
func (s DiskStore) Initialize(ctx context.Context, computerID string, seed Seed, target string, capacity int64, resize2fs string) (InitialDisk, error) {
	if err := s.validate(computerID); err != nil {
		return InitialDisk{}, err
	}
	if _, err := diskArtifactLimit(capacity); err != nil {
		return InitialDisk{}, err
	}
	if _, err := os.Lstat(target); err == nil {
		return InitialDisk{}, os.ErrExist
	} else if !errors.Is(err, os.ErrNotExist) {
		return InitialDisk{}, err
	}
	config, err := oci.ReadVerifiedConfig(ctx, seed.ImagePath, seed.Image.Digest, seed.Image.SizeBytes)
	if err != nil {
		return InitialDisk{}, err
	}
	if err := seedDisk(ctx, seed.Disk, target, capacity, resize2fs); err != nil {
		return InitialDisk{}, err
	}
	artifact, err := s.Save(ctx, computerID, target)
	if err != nil {
		return InitialDisk{}, errors.Join(err, os.Remove(target))
	}
	return InitialDisk{Artifact: artifact, Config: config}, nil
}
