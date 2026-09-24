package generation

import (
	"errors"

	"github.com/helmrdotdev/helmr/internal/computer/blockformat"
)

// Repack rewrites the selected live records into new segments and metadata. It is
// a single-owner, development-only full rewrite, not a production compactor. The
// caller retains the old root until it explicitly chooses the returned root.
// Failure never returns a usable root; immutable unreferenced objects may remain.
// maxLive bounds the work accepted by this experiment, not disk capacity.
func Repack(c *Codec, data, packs *Store, current blockformat.Locator, limit int, internal bool, maxLive uint64) (blockformat.Locator, Metrics, error) {
	var none blockformat.Locator
	if maxLive == 0 || maxLive > maxBlocks {
		return none, Metrics{}, errors.New("invalid repack work bound")
	}
	staging := NewStore()
	p, err := NewPacker(c, staging, packs, limit, internal)
	if err != nil {
		return none, staging.Metrics, err
	}
	shape, err := openPacked(c, packs, current)
	if err != nil {
		return none, staging.Metrics, err
	}
	d, err := New(c, staging, shape.Capacity, shape.Fanout)
	if err != nil {
		return none, staging.Metrics, err
	}
	var live uint64
	var walk func(*blockformat.Locator, int, uint64) error
	walk = func(l *blockformat.Locator, level int, start uint64) error {
		if l == nil {
			return nil
		}
		n, err := loadPacked(c, packs, *l, shape, level, start)
		if err != nil {
			return err
		}
		for _, e := range n.Entries {
			block := start + uint64(e.Slot)*d.stride(level)
			if level > 0 {
				if err := walk(e.Child, level-1, block); err != nil {
					return err
				}
				continue
			}
			if live == maxLive {
				return errors.New("repack work bound exceeded")
			}
			b, err := c.block(data, n.Segments[e.Segment], e.Record)
			if err != nil {
				return err
			}
			if err = d.WriteAt(b, int64(block)*blockformat.BlockSize); err != nil {
				return err
			}
			live++
			if live%blockformat.MaxRecords == 0 {
				if _, err = d.Capture(); err != nil {
					return err
				}
			}
		}
		return nil
	}
	if err = walk(shape.Index, shape.Level, 0); err != nil {
		return none, staging.Metrics, err
	}
	r, err := d.Capture()
	if err != nil {
		return none, staging.Metrics, err
	}
	// Copy only segments selected by the final tree. Intermediate source index
	// objects stay in staging; their cost is returned separately for measurement.
	var copyData func(*blockformat.Ref, int, uint64) error
	seen := make(map[blockformat.Ref]bool)
	copyData = func(ref *blockformat.Ref, level int, start uint64) error {
		n, err := d.load(ref, level, start)
		if err != nil {
			return err
		}
		for _, segment := range n.Segments {
			if seen[segment] {
				continue
			}
			seen[segment] = true
			b, err := staging.get(segment)
			if err != nil {
				return err
			}
			if err = data.put(segment, b); err != nil {
				return err
			}
		}
		if level > 0 {
			for _, e := range n.Entries {
				if err = copyData(e.Child, level-1, start+uint64(e.Slot)*d.stride(level)); err != nil {
					return err
				}
			}
		}
		return nil
	}
	if err = copyData(d.shape.Index, shape.Level, 0); err != nil {
		return none, staging.Metrics, err
	}
	out, err := p.Convert(r)
	return out, staging.Metrics, err
}
