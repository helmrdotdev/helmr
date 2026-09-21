//go:build linux

package computer

import (
	"context"
	"errors"
	"os"

	"github.com/helmrdotdev/helmr/internal/oci"
)

// Seed is the immutable preparation receipt supplied by trusted deployment
// authority. Its config and artifact must come from the same published result;
// transfer verification alone cannot establish config or image derivation.
type Seed struct {
	Identity SeedIdentity
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
	if err := (SeedStore{CAS: s.CAS, Cipher: s.Cipher}).materialize(ctx, seed, target, capacity, resize2fs); err != nil {
		return nil, err
	}
	candidate, err := s.Capture(ctx, computerID, target, stagingDir)
	if err != nil {
		return nil, errors.Join(err, os.Remove(target))
	}
	return &InitialDiskCandidate{Disk: candidate, Config: seed.Config}, nil
}
