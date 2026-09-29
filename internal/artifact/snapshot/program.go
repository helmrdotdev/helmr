package snapshot

import (
	"context"
	"errors"
	"fmt"

	"github.com/helmrdotdev/helmr/internal/artifact"
	"github.com/helmrdotdev/helmr/internal/cas"
)

func ReadProgram(
	ctx context.Context,
	store cas.Reader,
	directory string,
	program artifact.ProgramDescriptor,
) (*Program, error) {
	if store == nil {
		return nil, errors.New("program store is required")
	}
	snapshot, err := snapshotProgramObject(
		ctx,
		store,
		directory,
		program,
		artifact.RoleProgram,
	)
	if err != nil {
		return nil, fmt.Errorf("snapshot program: %w", err)
	}
	return snapshot, nil
}

func snapshotProgramObject(
	ctx context.Context,
	store cas.Reader,
	directory string,
	descriptor artifact.ProgramDescriptor,
	role artifact.Role,
) (*Program, error) {
	spec, err := artifactSnapshotSpecForRole(role)
	if err != nil {
		return nil, err
	}
	if err := validateArtifactSnapshotDescriptor(
		spec,
		artifactSnapshotDescriptor(descriptor),
	); err != nil {
		return nil, err
	}
	object, err := store.Stat(ctx, descriptor.Digest)
	if err != nil {
		return nil, fmt.Errorf("stat program object: %w", err)
	}
	if object.Digest != descriptor.Digest ||
		object.SizeBytes != descriptor.SizeBytes ||
		object.MediaType != descriptor.MediaType {
		return nil, errors.New("program object does not match its descriptor")
	}
	body, err := store.Get(ctx, descriptor.Digest)
	if err != nil {
		return nil, fmt.Errorf("open program object: %w", err)
	}
	content, snapshotErr := snapshotArtifact(
		ctx,
		directory,
		role,
		artifactSnapshotDescriptor(descriptor),
		body,
	)
	closeErr := body.Close()
	if snapshotErr != nil {
		return nil, snapshotErr
	}
	if closeErr != nil {
		_ = content.Close()
		return nil, fmt.Errorf("close program object: %w", closeErr)
	}
	return &Program{content: content}, nil
}
