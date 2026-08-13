package deployment

import (
	"context"
	"fmt"
)

func verifyRuntimeArtifact(
	ctx context.Context,
	artifact artifactInput,
) (RuntimeIndex, error) {
	if err := validateArtifactDescriptor(artifact, runtimeArtifact); err != nil {
		return RuntimeIndex{}, err
	}
	inspected, err := inspectArtifact(
		ctx,
		artifact.Reader,
		runtimeArtifact,
		maxRuntimeLogicalBytes,
		artifact.SizeBytes,
	)
	if err != nil {
		return RuntimeIndex{}, fmt.Errorf("runtime artifact: %w", err)
	}
	return verifyRuntimeLayout(ctx, inspected)
}

func verifyRuntimeLayout(
	ctx context.Context,
	artifact *inspectedArtifact,
) (RuntimeIndex, error) {
	index, err := verifyRuntimeTopology(ctx, artifact)
	if err != nil {
		return RuntimeIndex{}, err
	}
	if err := verifyRuntimeExecutables(ctx, artifact, index.Architecture); err != nil {
		return RuntimeIndex{}, err
	}
	return index, nil
}
