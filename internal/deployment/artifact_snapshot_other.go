//go:build !linux

package deployment

import (
	"context"
	"errors"
	"io"
)

func snapshotArtifact(
	context.Context,
	string,
	artifactRole,
	artifactSnapshotDescriptor,
	io.Reader,
) (*artifactSnapshot, error) {
	return nil, errors.New("artifact snapshots require Linux O_TMPFILE")
}
