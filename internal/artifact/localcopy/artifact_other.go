//go:build !linux

package localcopy

import (
	"context"
	"errors"
	"github.com/helmrdotdev/helmr/internal/cas"
	"io"

	"github.com/helmrdotdev/helmr/internal/artifact"
)

type artifactPlatform struct{}

func copyArtifact(
	context.Context,
	string,
	artifact.Role,
	cas.Descriptor,
	io.Reader,
) (*Artifact, error) {
	return nil, errors.New("artifact snapshots require Linux")
}

func closeArtifactCopyPlatform(local *Artifact) error {
	if local != nil {
		local.platform = artifactPlatform{}
	}
	return nil
}

func validateArtifactCopyPlatform(*Artifact) error {
	return nil
}

func (*Artifact) LinkInto(string, string, int, int) error {
	return errors.New("artifact snapshots require Linux")
}
