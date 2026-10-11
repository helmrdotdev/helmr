//go:build !linux

package builder

import (
	"context"
	"errors"
	"iter"

	"github.com/helmrdotdev/helmr/internal/artifact"
	"github.com/helmrdotdev/helmr/internal/artifact/localcopy"
)

func encodeProgramTree(
	context.Context,
	string,
	string,
	artifact.Role,
	iter.Seq2[treeEntry, error],
	bool,
) (*localcopy.Artifact, error) {
	return nil, errors.New("program encoding requires Linux")
}
