//go:build linux

package computer

import (
	"context"
	"errors"

	"github.com/helmrdotdev/helmr/internal/cas"
	"github.com/helmrdotdev/helmr/internal/checkpoint"
)

const seedRole = "computer-seed"

// SeedStore transfers shared initial disks. It does not grant preparation or
// runtime authority, grow disks, or replace per-Computer initial publication.
type SeedStore struct {
	CAS    cas.Reader
	Cipher *checkpoint.Encryptor
}

// SeedCandidate retains exact ciphertext for upload retries, independently of
// the preparation lease. Upload alone does not make a deployment runnable.
type SeedCandidate struct{ candidate *DiskCandidate }

func (c *SeedCandidate) Artifact() SeedArtifact {
	a := c.candidate.Artifact()
	return SeedArtifact{Object: a.Object, LogicalBytes: a.LogicalBytes}
}

func (c *SeedCandidate) Upload(ctx context.Context, publisher DiskPublisher) error {
	return c.candidate.Upload(ctx, publisher)
}

func (c *SeedCandidate) Close() error { return c.candidate.Close() }

// Encode requires an exclusively owned, stable verified disk and reserved private
// staging capacity. Encoding again creates a new ciphertext, not an upload retry.
func (s SeedStore) Encode(ctx context.Context, id SeedIdentity, disk, stagingDir string) (*SeedCandidate, error) {
	purpose, err := id.purpose()
	if err != nil {
		return nil, err
	}
	if s.Cipher == nil {
		return nil, errors.New("computer seed encryption is required")
	}
	candidate, err := (DiskStore{Cipher: s.Cipher}).capture(ctx, disk, stagingDir, purpose, seedRole, SeedMediaType)
	if err != nil {
		return nil, err
	}
	return &SeedCandidate{candidate: candidate}, nil
}

// Decode checks capacity before CAS access, verifies ciphertext and identity,
// then exposes an independent seed-sized disk. It never resizes or overwrites.
func (s SeedStore) Decode(ctx context.Context, id SeedIdentity, artifact SeedArtifact, target string, capacity int64) error {
	purpose, err := id.purpose()
	if err != nil {
		return err
	}
	if s.CAS == nil || s.Cipher == nil {
		return errors.New("computer seed storage and encryption are required")
	}
	if err := artifact.Validate(capacity); err != nil {
		return err
	}
	return (DiskStore{CAS: s.CAS, Cipher: s.Cipher}).restore(ctx,
		DiskArtifact{Object: artifact.Object, LogicalBytes: artifact.LogicalBytes},
		target, artifact.LogicalBytes, purpose, seedRole)
}
