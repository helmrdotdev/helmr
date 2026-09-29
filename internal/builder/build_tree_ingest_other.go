//go:build !linux

package builder

import (
	"context"
	"errors"
	"io"
)

func IngestBuildTreeArchive(
	context.Context,
	string,
	string,
	string,
	int64,
	io.Reader,
) (*BuildTree, error) {
	return nil, errors.New("build tree archive ingestion requires Linux")
}
