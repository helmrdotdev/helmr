package deployment

import (
	"context"
	"errors"
	"fmt"

	"github.com/helmrdotdev/helmr/internal/cas"
)

func SnapshotRuntimeObject(
	ctx context.Context,
	store cas.Reader,
	directory string,
	descriptor RuntimeDescriptor,
) (*RuntimeArtifactSnapshot, error) {
	if store == nil {
		return nil, errors.New("runtime store is required")
	}
	if err := ValidateRuntimeDescriptor(descriptor); err != nil {
		return nil, err
	}
	object, err := store.Stat(ctx, descriptor.Digest)
	if err != nil {
		return nil, fmt.Errorf("stat runtime object: %w", err)
	}
	if object.Digest != descriptor.Digest ||
		object.SizeBytes != descriptor.SizeBytes ||
		object.MediaType != descriptor.MediaType {
		return nil, errors.New("runtime object does not match its descriptor")
	}
	body, err := store.Get(ctx, descriptor.Digest)
	if err != nil {
		return nil, fmt.Errorf("open runtime object: %w", err)
	}
	snapshot, snapshotErr := SnapshotRuntimeArtifact(ctx, directory, descriptor, body)
	closeErr := body.Close()
	if snapshotErr != nil {
		return nil, snapshotErr
	}
	if closeErr != nil {
		_ = snapshot.Close()
		return nil, fmt.Errorf("close runtime object: %w", closeErr)
	}
	return snapshot, nil
}
