//go:build linux

package builder

import (
	"context"

	"github.com/helmrdotdev/helmr/internal/computer"
	"github.com/helmrdotdev/helmr/internal/definition"
	"github.com/helmrdotdev/helmr/internal/deployment"
	"github.com/helmrdotdev/helmr/internal/substrate"
)

func buildComputerDisk(ctx context.Context, source, target, scratch, mkfs, config string) (deployment.BundleComputerImageArtifact, error) {
	seed, err := substrate.BuildSeed(ctx, source, target, scratch, mkfs, config, computer.SeedCapacity)
	if err != nil {
		return deployment.BundleComputerImageArtifact{}, err
	}
	return deployment.BundleComputerImageArtifact{Architecture: definition.ArchitectureX8664, Profile: definition.ComputerSeedProfile, Config: seed.Config, Digest: seed.Artifact.Object.Digest, SizeBytes: seed.Artifact.Object.SizeBytes, MediaType: seed.Artifact.Object.MediaType}, nil
}
