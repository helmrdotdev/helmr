package deployment

import (
	"context"
	"errors"
	"fmt"
	"os"

	"github.com/helmrdotdev/helmr/internal/cas"
)

func SnapshotProgram(
	ctx context.Context,
	store cas.Reader,
	directory string,
	program ProgramDescriptor,
) (*ArtifactSnapshot, error) {
	if store == nil {
		return nil, errors.New("program store is required")
	}
	snapshot, err := snapshotProgramObject(
		ctx,
		store,
		directory,
		program,
		programArtifact,
	)
	if err != nil {
		return nil, fmt.Errorf("snapshot Program: %w", err)
	}
	return snapshot, nil
}

func snapshotProgramObject(
	ctx context.Context,
	store cas.Reader,
	directory string,
	descriptor ProgramDescriptor,
	role artifactRole,
) (*ArtifactSnapshot, error) {
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
		return nil, fmt.Errorf("stat Program object: %w", err)
	}
	if object.Digest != descriptor.Digest ||
		object.SizeBytes != descriptor.SizeBytes ||
		object.MediaType != descriptor.MediaType {
		return nil, errors.New("Program object does not match its descriptor")
	}
	body, err := store.Get(ctx, descriptor.Digest)
	if err != nil {
		return nil, fmt.Errorf("open Program object: %w", err)
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
		return nil, fmt.Errorf("close Program object: %w", closeErr)
	}
	return &ArtifactSnapshot{content: content}, nil
}

func VerifyProgram(
	ctx context.Context,
	unitCgroupRoot string,
	leaseIdentity string,
	snapshot *ArtifactSnapshot,
) (ProgramIndex, error) {
	if ctx == nil {
		return ProgramIndex{}, errors.New("Program verification context is nil")
	}
	if snapshot == nil {
		return ProgramIndex{}, errors.New("program Artifact snapshot is closed")
	}
	artifact, err := snapshot.verifier()
	if err != nil {
		return ProgramIndex{}, err
	}
	result, err := runVerifierProcess(ctx, verifierProcessConfig{
		job:            programVerifierJob,
		unitCgroupRoot: unitCgroupRoot,
		leaseIdentity:  leaseIdentity,
		artifacts:      []*os.File{artifact},
	})
	if err != nil {
		return ProgramIndex{}, err
	}
	switch result.kind {
	case verifierVerified:
		verified, err := parseProgramVerification(result.payload)
		if err != nil {
			return ProgramIndex{}, fmt.Errorf("parse verified Program index: %w", err)
		}
		return verified.Index, nil
	case verifierInvalid:
		return ProgramIndex{}, &verifierInvalidError{diagnostic: result.diagnostic}
	case verifierFailed:
		return ProgramIndex{}, errors.New("Program verifier failed")
	default:
		return ProgramIndex{}, fmt.Errorf(
			"Program verifier returned unknown outcome %d",
			result.kind,
		)
	}
}
