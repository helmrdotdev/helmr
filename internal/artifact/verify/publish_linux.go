//go:build linux

package verify

import (
	"context"
	"errors"
	"fmt"
	"os"

	"github.com/helmrdotdev/helmr/internal/artifact"
	"github.com/helmrdotdev/helmr/internal/artifact/snapshot"
	"github.com/helmrdotdev/helmr/internal/cas"
)

// PublishPlatformRuntime publishes the operator-supplied Platform Runtime
// object at path after snapshotting it and verifying it in process against
// descriptor. Nothing reaches store unless verification succeeds.
func PublishPlatformRuntime(
	ctx context.Context,
	store cas.ImmutableStore,
	path string,
	descriptor artifact.RuntimeDescriptor,
) (returnErr error) {
	source, err := os.Open(path)
	if err != nil {
		return err
	}
	defer func() {
		returnErr = errors.Join(returnErr, source.Close())
	}()

	runtimeSnapshot, err := snapshot.CopyRuntime(ctx, "", descriptor, source)
	if err != nil {
		return err
	}
	defer func() {
		returnErr = errors.Join(returnErr, runtimeSnapshot.Close())
	}()

	verifier, _, err := runtimeSnapshot.VerifierFile()
	if err != nil {
		return err
	}
	reader, err := newSquashFSArtifactReader(
		ctx,
		verifier,
		descriptor.SizeBytes,
		artifact.RoleRuntime,
	)
	if err != nil {
		return fmt.Errorf("verify platform Runtime: %w", err)
	}
	index, err := verifyRuntimeArtifact(ctx, artifactInput{
		Digest: descriptor.Digest, SizeBytes: descriptor.SizeBytes,
		MediaType: descriptor.MediaType, Reader: reader,
	})
	if err != nil {
		return fmt.Errorf("verify platform Runtime: %w", err)
	}
	return runtimeSnapshot.PublishVerified(ctx, store, index)
}
