//go:build linux

package builder

import (
	"context"

	"github.com/helmrdotdev/helmr/internal/bundle"
	"github.com/helmrdotdev/helmr/internal/computer"
	"github.com/helmrdotdev/helmr/internal/definition"
)

func buildComputerDisk(ctx context.Context, source, target, scratch, mkfs, config string) (bundle.ComputerImageArtifact, error) {
	seed, err := buildSeed(ctx, source, target, scratch, mkfs, config, computer.SeedCapacity)
	if err != nil {
		return bundle.ComputerImageArtifact{}, err
	}
	return bundle.ComputerImageArtifact{Architecture: definition.ArchitectureX8664, Profile: definition.ComputerSeedProfile, Config: seed.Config, Digest: seed.Artifact.Object.Digest, SizeBytes: seed.Artifact.Object.SizeBytes, MediaType: seed.Artifact.Object.MediaType}, nil
}
