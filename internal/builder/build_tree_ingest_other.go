//go:build !linux

package builder

import (
	"context"
	"errors"
	"io"
)

func ingestBuildTreeArchive(
	context.Context,
	string,
	string,
	string,
	int64,
	io.Reader,
) (*buildTree, error) {
	return nil, errors.New("build tree archive ingestion requires Linux")
}
