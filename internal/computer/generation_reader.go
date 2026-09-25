package computer

import (
	"context"
	"errors"

	"github.com/helmrdotdev/helmr/internal/computer/blockformat"
)

// OpenGeneration binds authenticated disk geometry to the admitted descriptor.
// Authority and key retention must already be established by the caller; opening
// the reader does not grant permission to restore or publish the generation.
func OpenGeneration(ctx context.Context, source blockformat.RangeSource, scope string, keys map[string][]byte, root GenerationRoot, capacity int64) (*blockformat.Tree, error) {
	locator, err := root.Locator(capacity)
	if err != nil {
		return nil, err
	}
	tree, err := blockformat.OpenTree(ctx, source, scope, keys, locator)
	if err != nil {
		return nil, err
	}
	if tree.Capacity() != capacity {
		return nil, errors.New("authenticated generation capacity differs from admission")
	}
	return tree, nil
}
