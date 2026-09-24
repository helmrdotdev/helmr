package generation

import (
	"bytes"
	"encoding/json"
	"errors"
	"sort"

	"github.com/helmrdotdev/helmr/internal/computer/blockformat"
)

type directNode struct {
	node     packedNode
	children map[int]*directNode
	ref      blockformat.Ref
}

// CapturePacked directly updates a packed generation from complete 4 KiB blocks.
// It is a bounded single-owner experiment, with no publication or durability.
// All changes are validated before writing; a failed capture leaves the old root
// readable and may leave orphan objects. There is no cross-generation locator map.
func CapturePacked(c *Codec, data, packs *Store, base blockformat.Locator, changes map[uint64][]byte, limit int, internal bool) (blockformat.Locator, error) {
	shape, err := openPacked(c, packs, base)
	if err != nil {
		return blockformat.Locator{}, err
	}
	p, err := NewPacker(c, nil, packs, limit, internal)
	if err != nil {
		return blockformat.Locator{}, err
	}
	if len(changes) > maxDirty {
		return blockformat.Locator{}, errors.New("too many changed blocks")
	}
	blocks := make([]uint64, 0, len(changes))
	for b, v := range changes {
		if b >= uint64(shape.Capacity/BlockSize) || len(v) != BlockSize {
			return blockformat.Locator{}, errors.New("invalid changed block")
		}
		blocks = append(blocks, b)
	}
	if len(blocks) == 0 {
		return base, nil
	}
	sort.Slice(blocks, func(i, j int) bool { return blocks[i] < blocks[j] })
	values := make(map[uint64]*location, len(blocks))
	var records [][]byte
	var pending []uint64
	flushData := func() error {
		if len(records) == 0 {
			return nil
		}
		r, raw, e := c.seal(segmentKind, records)
		if e != nil {
			return e
		}
		if e = data.put(r, raw); e != nil {
			return e
		}
		for i, b := range pending {
			values[b] = &location{r, uint32(i)}
		}
		records = nil
		pending = nil
		return nil
	}
	zero := make([]byte, BlockSize)
	for _, b := range blocks {
		if bytes.Equal(changes[b], zero) {
			values[b] = nil
			continue
		}
		records = append(records, changes[b])
		pending = append(pending, b)
		if len(records) == maxRecords {
			if err = flushData(); err != nil {
				return blockformat.Locator{}, err
			}
		}
	}
	if err = flushData(); err != nil {
		return blockformat.Locator{}, err
	}
	geometry := &Disk{shape: rootShape(shape)}
	levels := make([][]*directNode, shape.Level+1)
	var update func(*blockformat.Locator, int, uint64, []uint64) (*directNode, error)
	update = func(old *blockformat.Locator, level int, start uint64, changed []uint64) (*directNode, error) {
		n := packedNode{Capacity: shape.Capacity, Fanout: shape.Fanout, Level: level, Start: start}
		if old != nil {
			var e error
			n, e = loadPacked(c, packs, *old, shape, level, start)
			if e != nil {
				return nil, e
			}
		}
		out := &directNode{node: n, children: map[int]*directNode{}}
		entries := map[int]packedEntry{}
		for _, e := range n.Entries {
			entries[e.Slot] = e
		}
		if level == 0 {
			locs := map[int]location{}
			for _, e := range n.Entries {
				locs[e.Slot] = location{n.Segments[e.Segment], e.Record}
			}
			for _, b := range changed {
				slot := int(b - start)
				if values[b] == nil {
					delete(locs, slot)
				} else {
					locs[slot] = *values[b]
				}
			}
			out.node.Entries = nil
			out.node.Segments = nil
			table := map[blockformat.Ref]int{}
			for slot := 0; slot < shape.Fanout; slot++ {
				v, ok := locs[slot]
				if !ok {
					continue
				}
				idx, ok := table[v.Segment]
				if !ok {
					idx = len(out.node.Segments)
					table[v.Segment] = idx
					out.node.Segments = append(out.node.Segments, v.Segment)
				}
				out.node.Entries = append(out.node.Entries, packedEntry{Slot: slot, Segment: idx, Record: v.Record})
			}
		} else {
			groups := map[int][]uint64{}
			for _, b := range changed {
				slot := int((b - start) / geometry.stride(level))
				groups[slot] = append(groups[slot], b)
			}
			for slot := 0; slot < shape.Fanout; slot++ {
				group := groups[slot]
				if len(group) == 0 {
					continue
				}
				child, e := update(entries[slot].Child, level-1, start+uint64(slot)*geometry.stride(level), group)
				if e != nil {
					return nil, e
				}
				if child == nil {
					delete(entries, slot)
				} else {
					out.children[slot] = child
					entries[slot] = packedEntry{Slot: slot}
				}
			}
			out.node.Entries = nil
			for slot := 0; slot < shape.Fanout; slot++ {
				if e, ok := entries[slot]; ok {
					out.node.Entries = append(out.node.Entries, e)
				}
			}
		}
		if len(out.node.Entries) == 0 {
			return nil, nil
		}
		levels[level] = append(levels[level], out)
		return out, nil
	}
	top, err := update(shape.Index, shape.Level, 0, blocks)
	if err != nil {
		return blockformat.Locator{}, err
	}
	for level, nodes := range levels {
		var batch []stagedPage
		empty, _, e := encodePack(level+1, nil)
		if e != nil {
			return blockformat.Locator{}, e
		}
		size := len(empty)
		for _, n := range nodes {
			for i, e := range n.node.Entries {
				if child := n.children[e.Slot]; child != nil {
					loc, ok := p.converted[child.ref]
					if !ok {
						return blockformat.Locator{}, errors.New("missing direct child")
					}
					n.node.Entries[i].Child = &loc
				}
			}
			plain, e := json.Marshal(n.node)
			if e != nil {
				return blockformat.Locator{}, e
			}
			ref, raw, e := c.seal(nodeKind, [][]byte{plain})
			if e != nil {
				return blockformat.Locator{}, e
			}
			n.ref = ref
			descriptor, e := json.Marshal(ref)
			if e != nil {
				return blockformat.Locator{}, e
			}
			addition := len(descriptor) + len(raw)
			if len(batch) > 0 {
				addition++
			}
			if size+addition > limit || (level > 0 && !internal && len(batch) > 0) {
				if e = p.publish(level+1, batch); e != nil {
					return blockformat.Locator{}, e
				}
				batch = nil
				size = len(empty)
				addition = len(descriptor) + len(raw)
			}
			if size+addition > limit {
				return blockformat.Locator{}, errors.New("metadata page exceeds pack budget")
			}
			batch = append(batch, stagedPage{ref, ref, raw})
			size += addition
		}
		if err = p.publish(level+1, batch); err != nil {
			return blockformat.Locator{}, err
		}
	}
	shape.Index = nil
	if top != nil {
		loc := p.converted[top.ref]
		shape.Index = &loc
	}
	plain, err := json.Marshal(shape)
	if err != nil {
		return blockformat.Locator{}, err
	}
	ref, raw, err := c.seal(rootKind, [][]byte{plain})
	if err != nil {
		return blockformat.Locator{}, err
	}
	if err = p.publish(shape.Level+2, []stagedPage{{ref, ref, raw}}); err != nil {
		return blockformat.Locator{}, err
	}
	return p.converted[ref], nil
}

// NewPacked creates an empty packed generation without an intermediate index.
func NewPacked(c *Codec, packs *Store, capacity int64, fanout, limit int) (blockformat.Locator, error) {
	if capacity <= 0 || capacity%BlockSize != 0 || capacity/BlockSize > maxBlocks || (fanout != 64 && fanout != 256) {
		return blockformat.Locator{}, errors.New("unsupported geometry")
	}
	p, err := NewPacker(c, nil, packs, limit, true)
	if err != nil {
		return blockformat.Locator{}, err
	}
	shape := packedRoot{Capacity: capacity, Fanout: fanout}
	for span := int64(fanout); span < capacity/BlockSize; span *= int64(fanout) {
		shape.Level++
	}
	plain, err := json.Marshal(shape)
	if err != nil {
		return blockformat.Locator{}, err
	}
	ref, raw, err := c.seal(rootKind, [][]byte{plain})
	if err != nil {
		return blockformat.Locator{}, err
	}
	if err = p.publish(shape.Level+2, []stagedPage{{ref, ref, raw}}); err != nil {
		return blockformat.Locator{}, err
	}
	return p.converted[ref], nil
}
