package blockformat

import (
	"context"
	"errors"
)

const MaxReadBytes = 4 << 20

// ReadRange returns an authenticated byte range of this exact generation. Only
// intersecting paths are read, each node once per request; absent entries are
// holes. Requests are bounded independently of disk capacity. Unaligned requests
// authenticate complete boundary blocks before copying the requested bytes.
// Failure returns no data, including when earlier blocks were already read.
// The caller retains source/key ownership, as with ReadBlock.
func (t *Tree) ReadRange(ctx context.Context, offset int64, length int) ([]byte, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if length < 0 || length > MaxReadBytes || offset < 0 || offset > t.shape.Capacity-int64(length) {
		return nil, errors.New("generation read range bounds")
	}
	out := make([]byte, length)
	if length == 0 {
		return out, nil
	}
	end := offset + int64(length)
	first, last := uint64(offset/BlockSize), uint64((end-1)/BlockSize)
	var visit func(Locator, int, uint64) error
	visit = func(locator Locator, level int, start uint64) error {
		node, err := ReadNode(ctx, t.source, t.scope, t.keys, locator, t.shape, level, start)
		if err != nil {
			return err
		}
		span := stride(t.shape.Fanout, level)
		for _, entry := range node.Entries {
			if err := ctx.Err(); err != nil {
				return err
			}
			block := start + uint64(entry.Slot)*span
			if block > last {
				break
			}
			if block+span <= first {
				continue
			}
			if level > 0 {
				if err := visit(*entry.Child, level-1, block); err != nil {
					return err
				}
				continue
			}
			segment := node.Segments[entry.Segment]
			data, err := ReadBlock(ctx, t.source, t.scope, t.keys[segment.Key], segment, entry.Record)
			if err != nil {
				return err
			}
			blockOffset := int64(block) * BlockSize
			from, to := max(offset, blockOffset), min(end, blockOffset+BlockSize)
			copy(out[from-offset:to-offset], data[from-blockOffset:to-blockOffset])
		}
		return nil
	}
	if t.shape.Index != nil {
		if err := visit(*t.shape.Index, t.shape.Level, 0); err != nil {
			clear(out)
			return nil, err
		}
	}
	if err := ctx.Err(); err != nil {
		clear(out)
		return nil, err
	}
	return out, nil
}
