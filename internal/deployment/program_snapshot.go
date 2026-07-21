package deployment

import (
	"context"
	"errors"
	"fmt"
	"os"

	"github.com/helmrdotdev/helmr/internal/cas"
)

type ProgramArtifactSnapshot struct {
	Code         *ReadOnlyArtifactSnapshot
	Dependencies *ReadOnlyArtifactSnapshot
}

type ReadOnlyArtifactSnapshot struct {
	content *artifactSnapshot
}

func SnapshotProgramObjects(
	ctx context.Context,
	store cas.Reader,
	directory string,
	code ProgramDescriptor,
	dependencies ProgramDescriptor,
) (*ProgramArtifactSnapshot, error) {
	if store == nil {
		return nil, errors.New("Program store is required")
	}
	codeSnapshot, err := snapshotProgramObject(ctx, store, directory, code, codeArtifact)
	if err != nil {
		return nil, fmt.Errorf("snapshot Program code: %w", err)
	}
	dependencySnapshot, err := snapshotProgramObject(
		ctx,
		store,
		directory,
		dependencies,
		dependencyArtifact,
	)
	if err != nil {
		return nil, errors.Join(
			fmt.Errorf("snapshot Program dependencies: %w", err),
			codeSnapshot.Close(),
		)
	}
	return &ProgramArtifactSnapshot{
		Code:         codeSnapshot,
		Dependencies: dependencySnapshot,
	}, nil
}

func snapshotProgramObject(
	ctx context.Context,
	store cas.Reader,
	directory string,
	descriptor ProgramDescriptor,
	role artifactRole,
) (*ReadOnlyArtifactSnapshot, error) {
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
	return &ReadOnlyArtifactSnapshot{content: content}, nil
}

func VerifyProgramArtifacts(
	ctx context.Context,
	unitCgroupRoot string,
	leaseIdentity string,
	snapshot *ProgramArtifactSnapshot,
) (ProgramIndex, error) {
	if ctx == nil {
		return ProgramIndex{}, errors.New("Program verification context is nil")
	}
	code, dependencies, err := snapshot.verifiers()
	if err != nil {
		return ProgramIndex{}, err
	}
	result, err := runVerifierProcess(ctx, verifierProcessConfig{
		job:            programVerifierJob,
		unitCgroupRoot: unitCgroupRoot,
		leaseIdentity:  leaseIdentity,
		artifacts:      []*os.File{code, dependencies},
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

func (snapshot *ProgramArtifactSnapshot) verifiers() (*os.File, *os.File, error) {
	if snapshot == nil || snapshot.Code == nil || snapshot.Dependencies == nil {
		return nil, nil, errors.New("Program Artifact snapshot is closed")
	}
	code, err := snapshot.Code.verifier()
	if err != nil {
		return nil, nil, err
	}
	dependencies, err := snapshot.Dependencies.verifier()
	if err != nil {
		return nil, nil, err
	}
	return code, dependencies, nil
}

func (snapshot *ProgramArtifactSnapshot) Close() error {
	if snapshot == nil {
		return nil
	}
	var err error
	if snapshot.Code != nil {
		err = errors.Join(err, snapshot.Code.Close())
		snapshot.Code = nil
	}
	if snapshot.Dependencies != nil {
		err = errors.Join(err, snapshot.Dependencies.Close())
		snapshot.Dependencies = nil
	}
	return err
}

func (snapshot *ReadOnlyArtifactSnapshot) verifier() (*os.File, error) {
	if snapshot == nil || snapshot.content == nil {
		return nil, errors.New("Artifact snapshot is closed")
	}
	return snapshot.content.verifierFile()
}

func (snapshot *ReadOnlyArtifactSnapshot) LinkInto(
	directory string,
	name string,
	uid int,
	gid int,
) error {
	if snapshot == nil || snapshot.content == nil {
		return errors.New("Artifact snapshot is closed")
	}
	return snapshot.content.LinkInto(directory, name, uid, gid)
}

func (snapshot *ReadOnlyArtifactSnapshot) Close() error {
	if snapshot == nil || snapshot.content == nil {
		return nil
	}
	err := snapshot.content.Close()
	snapshot.content = nil
	return err
}
