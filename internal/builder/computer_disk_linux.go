//go:build linux

package builder

import (
	"context"

	"github.com/helmrdotdev/helmr/internal/bundle"
	"github.com/helmrdotdev/helmr/internal/definition"
	"github.com/helmrdotdev/helmr/internal/disk"
)

func buildComputerDisk(ctx context.Context, source, target, scratch, mkfs, config string) (bundle.ComputerSeedArtifact, error) {
	seed, err := buildSeed(ctx, source, target, scratch, mkfs, config, disk.SeedCapacity)
	if err != nil {
		return bundle.ComputerSeedArtifact{}, err
	}
	return bundle.ComputerSeedArtifact{Architecture: definition.ArchitectureX8664, Profile: definition.ComputerSeedProfile, Config: seed.Config, Digest: seed.Artifact.Object.Digest, SizeBytes: seed.Artifact.Object.SizeBytes, MediaType: seed.Artifact.Object.MediaType}, nil
}
