//go:build !linux

package deployment

import (
	"context"
	"errors"
	"iter"
)

func encodeProgramTree(
	context.Context,
	string,
	string,
	artifactRole,
	iter.Seq2[treeEntry, error],
	bool,
) (*artifactSnapshot, error) {
	return nil, errors.New("program encoding requires Linux")
}
