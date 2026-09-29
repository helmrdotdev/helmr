package verify

import (
	"context"
	"fmt"

	"github.com/helmrdotdev/helmr/internal/artifact"
)

func verifyRuntimeArtifact(
	ctx context.Context,
	input artifactInput,
) (artifact.RuntimeIndex, error) {
	if err := validateArtifactDescriptor(input, artifact.RoleRuntime); err != nil {
		return artifact.RuntimeIndex{}, err
	}
	inspected, err := artifact.Inspect(
		ctx,
		input.Reader,
		artifact.RoleRuntime,
		input.SizeBytes,
	)
	if err != nil {
		return artifact.RuntimeIndex{}, fmt.Errorf("runtime artifact: %w", err)
	}
	return verifyRuntimeLayout(ctx, inspected)
}

func verifyRuntimeLayout(
	ctx context.Context,
	tree *artifact.Tree,
) (artifact.RuntimeIndex, error) {
	index, err := verifyRuntimeTopology(ctx, tree)
	if err != nil {
		return artifact.RuntimeIndex{}, err
	}
	if err := verifyRuntimeExecutables(ctx, tree, index.Architecture); err != nil {
		return artifact.RuntimeIndex{}, err
	}
	return index, nil
}
