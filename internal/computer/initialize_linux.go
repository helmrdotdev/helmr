//go:build linux

package computer

import (
	"context"
	"errors"
	"os"

	"github.com/helmrdotdev/helmr/internal/oci"
)

// Seed is the disk and config from one admitted deployment. Admission does not
// certify how a client derived its filesystem or whether the guest will boot.
type Seed struct {
	Artifact SeedArtifact
	Config   oci.RuntimeConfig
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
func (s DiskStore) Initialize(ctx context.Context, computerID string, seed Seed, target, stagingDir string, capacity int64) (*InitialDiskCandidate, error) {
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
	if err := (SeedStore{CAS: s.CAS}).Decode(ctx, seed.Artifact, target, capacity); err != nil {
		return nil, err
	}
	candidate, err := s.Capture(ctx, computerID, target, stagingDir)
	if err != nil {
		return nil, errors.Join(err, os.Remove(target))
	}
	return &InitialDiskCandidate{Disk: candidate, Config: seed.Config}, nil
}
