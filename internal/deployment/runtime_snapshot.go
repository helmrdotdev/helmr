package deployment

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
)

type RuntimeArtifactSnapshot struct {
	descriptor RuntimeDescriptor
	content    *artifactSnapshot
}

func SnapshotRuntimeArtifact(
	ctx context.Context,
	directory string,
	descriptor RuntimeDescriptor,
	source io.Reader,
) (*RuntimeArtifactSnapshot, error) {
	if err := ValidateRuntimeDescriptor(descriptor); err != nil {
		return nil, err
	}
	if descriptor.SizeBytes > maxRuntimePhysicalBytes {
		return nil, fmt.Errorf(
			"runtime Artifact size exceeds %d bytes",
			maxRuntimePhysicalBytes,
		)
	}
	content, err := snapshotArtifact(
		ctx,
		directory,
		runtimeArtifact,
		artifactSnapshotDescriptor{
			Digest:    descriptor.Digest,
			MediaType: descriptor.MediaType,
			SizeBytes: descriptor.SizeBytes,
		},
		source,
	)
	if err != nil {
		return nil, fmt.Errorf("snapshot runtime Artifact: %w", err)
	}
	return &RuntimeArtifactSnapshot{
		descriptor: descriptor,
		content:    content,
	}, nil
}

func (snapshot *RuntimeArtifactSnapshot) verifier() (*os.File, RuntimeDescriptor, error) {
	if snapshot == nil || snapshot.content == nil {
		return nil, RuntimeDescriptor{}, errors.New("runtime Artifact snapshot is closed")
	}
	// snapshotArtifact binds the descriptor to an immutable inode; the isolated
	// child re-reads all bytes while the parent retains this outer identity.
	file, err := snapshot.content.verifierFile()
	if err != nil {
		return nil, RuntimeDescriptor{}, err
	}
	return file, snapshot.descriptor, nil
}

func (snapshot *RuntimeArtifactSnapshot) Close() error {
	if snapshot == nil || snapshot.content == nil {
		return nil
	}
	err := snapshot.content.Close()
	snapshot.content = nil
	return err
}
