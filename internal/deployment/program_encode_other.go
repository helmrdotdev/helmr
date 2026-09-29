//go:build !linux

package deployment

import (
	"context"
	"errors"
	"iter"

	"github.com/helmrdotdev/helmr/internal/artifact"
	"github.com/helmrdotdev/helmr/internal/artifact/snapshot"
)

func encodeProgramTree(
	context.Context,
	string,
	string,
	artifact.Role,
	iter.Seq2[treeEntry, error],
	bool,
) (*snapshot.Artifact, error) {
	return nil, errors.New("program encoding requires Linux")
}
