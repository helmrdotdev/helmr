//go:build !linux

package deployment

import (
	"context"
	"errors"
	"io"
)

func snapshotProgram(
	context.Context,
	string,
	artifactRole,
	ProgramDescriptor,
	io.Reader,
) (*programSnapshot, error) {
	return nil, errors.New("program snapshots require Linux O_TMPFILE")
}
