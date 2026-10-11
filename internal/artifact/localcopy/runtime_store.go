package localcopy

import (
	"context"
	"errors"
	"fmt"

	"github.com/helmrdotdev/helmr/internal/artifact"
	"github.com/helmrdotdev/helmr/internal/cas"
)

func ReadRuntime(
	ctx context.Context,
	store cas.Reader,
	directory string,
	descriptor artifact.RuntimeDescriptor,
) (*Runtime, error) {
	if store == nil {
		return nil, errors.New("runtime store is required")
	}
	if err := artifact.ValidateRuntimeDescriptor(descriptor); err != nil {
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
	local, copyErr := CopyRuntime(ctx, directory, descriptor, body)
	closeErr := body.Close()
	if copyErr != nil {
		return nil, copyErr
	}
	if closeErr != nil {
		_ = local.Close()
		return nil, fmt.Errorf("close runtime object: %w", closeErr)
	}
	return local, nil
}
