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

// InitialDiskCandidate retains encoded bytes until their exact descriptor has
// been registered, uploaded and resolved by the publication owner.
type InitialDiskCandidate struct {
	Disk   *DiskCandidate
	Config oci.RuntimeConfig
}

// Initialize prepares an exclusively owned working disk and local ciphertext.
// It performs no remote writes. Failure removes this invocation's working file;
// success transfers both working disk and candidate ownership to the caller.
// The owner must publish the artifact and config before permitting user execution.
func (s DiskStore) Initialize(ctx context.Context, computerID string, seed Seed, target, stagingDir string, capacity int64, resize2fs string) (*InitialDiskCandidate, error) {
	if err := s.validate(computerID); err != nil {
		return nil, err
	}
	if _, err := diskArtifactLimit(capacity); err != nil {
		return nil, err
	}
	if _, err := os.Lstat(target); err == nil {
		return nil, os.ErrExist
	} else if !errors.Is(err, os.ErrNotExist) {
		return nil, err
	}
	config, err := oci.ReadVerifiedConfig(ctx, seed.ImagePath, seed.Image.Digest, seed.Image.SizeBytes)
	if err != nil {
		return nil, err
	}
	if err := seedDisk(ctx, seed.Disk, target, capacity, resize2fs); err != nil {
		return nil, err
	}
	candidate, err := s.Capture(ctx, computerID, target, stagingDir)
	if err != nil {
		return nil, errors.Join(err, os.Remove(target))
	}
	return &InitialDiskCandidate{Disk: candidate, Config: config}, nil
}
