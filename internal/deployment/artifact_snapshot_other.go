//go:build !linux

package deployment

import (
	"context"
	"errors"
	"io"
)

type artifactSnapshotPlatform struct{}

func snapshotArtifact(
	context.Context,
	string,
	artifactRole,
	artifactSnapshotDescriptor,
	io.Reader,
) (*artifactSnapshot, error) {
	return nil, errors.New("artifact snapshots require Linux")
}

func closeArtifactSnapshotPlatform(snapshot *artifactSnapshot) error {
	if snapshot != nil {
		snapshot.platform = artifactSnapshotPlatform{}
	}
	return nil
}

func validateArtifactSnapshotPlatform(*artifactSnapshot) error {
	return nil
}

func (*artifactSnapshot) LinkInto(string, string, int, int) error {
	return errors.New("artifact snapshots require Linux")
}
