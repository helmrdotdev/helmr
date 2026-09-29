//go:build !linux

package snapshot

import (
	"context"
	"errors"
	"io"

	"github.com/helmrdotdev/helmr/internal/artifact"
)

type artifactSnapshotPlatform struct{}

func snapshotArtifact(
	context.Context,
	string,
	artifact.Role,
	artifactSnapshotDescriptor,
	io.Reader,
) (*Artifact, error) {
	return nil, errors.New("artifact snapshots require Linux")
}

func closeArtifactSnapshotPlatform(snapshot *Artifact) error {
	if snapshot != nil {
		snapshot.platform = artifactSnapshotPlatform{}
	}
	return nil
}

func validateArtifactSnapshotPlatform(*Artifact) error {
	return nil
}

func (*Artifact) LinkInto(string, string, int, int) error {
	return errors.New("artifact snapshots require Linux")
}
