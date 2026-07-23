package deployment

import (
	"context"
	"errors"
	"fmt"
	"io"
)

type toolchainSnapshot struct {
	descriptor ArtifactDescriptor
	content    *artifactSnapshot
}

func (snapshot *toolchainSnapshot) LinkInto(
	directory string,
	name string,
	uid int,
	gid int,
) error {
	if snapshot == nil || snapshot.content == nil {
		return errors.New("standard toolchain snapshot is closed")
	}
	return snapshot.content.LinkInto(directory, name, uid, gid)
}

func snapshotToolchain(
	ctx context.Context,
	directory string,
	descriptor ArtifactDescriptor,
	source io.Reader,
) (*toolchainSnapshot, error) {
	if err := validateToolArtifact(
		descriptor,
		ToolchainMediaType,
		"standard toolchain closure",
	); err != nil {
		return nil, err
	}
	content, err := snapshotArtifact(
		ctx,
		directory,
		toolchainArtifact,
		artifactSnapshotDescriptor{
			Digest:    descriptor.Digest,
			MediaType: descriptor.MediaType,
			SizeBytes: descriptor.SizeBytes,
		},
		source,
	)
	if err != nil {
		return nil, fmt.Errorf("snapshot standard toolchain closure: %w", err)
	}
	return &toolchainSnapshot{descriptor: descriptor, content: content}, nil
}

func (snapshot *toolchainSnapshot) Close() error {
	if snapshot == nil || snapshot.content == nil {
		return nil
	}
	err := snapshot.content.Close()
	snapshot.content = nil
	return err
}

func closeToolchainSnapshot(digest string, snapshot *toolchainSnapshot) error {
	if snapshot == nil {
		return errors.New("standard toolchain snapshot is nil")
	}
	if err := snapshot.Close(); err != nil {
		return fmt.Errorf("close standard toolchain closure %q: %w", digest, err)
	}
	return nil
}
