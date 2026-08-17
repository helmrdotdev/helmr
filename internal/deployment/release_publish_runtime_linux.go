//go:build linux

package deployment

import (
	"context"
	"errors"
	"fmt"
	"os"

	"github.com/helmrdotdev/helmr/internal/cas"
)

func publishVerifiedPlatformRuntime(
	ctx context.Context,
	store cas.ImmutableStore,
	path string,
	descriptor RuntimeDescriptor,
) (returnErr error) {
	source, err := os.Open(path)
	if err != nil {
		return err
	}
	defer func() {
		returnErr = errors.Join(returnErr, source.Close())
	}()

	snapshot, err := SnapshotRuntimeArtifact(ctx, "", descriptor, source)
	if err != nil {
		return err
	}
	defer func() {
		returnErr = errors.Join(returnErr, snapshot.Close())
	}()

	verifier, _, err := snapshot.verifier()
	if err != nil {
		return err
	}
	reader, err := newSquashFSArtifactReader(
		ctx,
		verifier,
		descriptor.SizeBytes,
		runtimeArtifact,
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
	if index.Architecture != descriptor.Architecture ||
		index.RuntimeContract != descriptor.RuntimeContract {
		return errors.New("verified Platform Runtime does not match its descriptor")
	}

	if err := validateArtifactSnapshotPlatform(snapshot.content); err != nil {
		return err
	}
	_, err = store.Publish(ctx, cas.Descriptor{
		Digest: descriptor.Digest, MediaType: descriptor.MediaType,
		SizeBytes: descriptor.SizeBytes,
	}, snapshot.content.upload)
	return err
}
