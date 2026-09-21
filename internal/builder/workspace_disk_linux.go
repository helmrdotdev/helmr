//go:build linux

package builder

import (
	"context"
	"github.com/helmrdotdev/helmr/internal/computer"
	"github.com/helmrdotdev/helmr/internal/deployment"
	"github.com/helmrdotdev/helmr/internal/substrate"
)

func buildWorkspaceDisk(ctx context.Context, source, target, scratch, mkfs, config string) (deployment.BundleWorkspaceImageArtifact, error) {
	seed, err := substrate.BuildSeed(ctx, source, target, scratch, mkfs, config, computer.SeedCapacity)
	if err != nil {
		return deployment.BundleWorkspaceImageArtifact{}, err
	}
	return deployment.BundleWorkspaceImageArtifact{Architecture: deployment.ArchitectureX8664, Profile: computer.SeedProfile, Config: seed.Config, Digest: seed.Artifact.Object.Digest, SizeBytes: seed.Artifact.Object.SizeBytes, MediaType: seed.Artifact.Object.MediaType}, nil
}
