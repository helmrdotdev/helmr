package blockformat

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"sort"
)

type directNode struct {
	node     Node
	children map[int]*directNode
	ref      Ref
}

// Capture stages a new immutable generation. It never advances a durable head.
// The source, scoped keys and input blocks must remain unchanged during the call.
// Failure returns no root and can leave unreferenced staged objects; the owning
// candidate must retain or discard them. Empty changes preserve the exact root.
func (w Writer) Capture(ctx context.Context, base Locator, capacity int64, changes map[uint64][]byte) (Locator, error) {
	if err := w.validate(); err != nil {
		return Locator{}, err
	}
	if err := ctx.Err(); err != nil {
		return Locator{}, err
	}
	if len(changes) > MaxChangedBlocks {
		return Locator{}, errors.New("too many changed blocks")
	}
	shape, err := ReadRoot(ctx, w.Source, w.Scope, w.Keys, base)
	if err != nil {
		return Locator{}, err
	}
	if shape.Capacity != capacity {
		return Locator{}, errors.New("capture capacity differs from admission")
	}
	p := &packWriter{writer: w, ctx: ctx, converted: make(map[Ref]Locator)}
	limit := w.PackLimit
	blocks := make([]uint64, 0, len(changes))
	for b, v := range changes {
		if b >= uint64(shape.Capacity/BlockSize) || len(v) != BlockSize {
			return Locator{}, errors.New("invalid changed block")
		}
		blocks = append(blocks, b)
	}
	if len(blocks) == 0 {
		return base, nil
	}
	sort.Slice(blocks, func(i, j int) bool { return blocks[i] < blocks[j] })
	values := make(map[uint64]*blockLocation, len(blocks))
	var records [][]byte
	var pending []uint64
	flushData := func() error {
		if len(records) == 0 {
			return nil
		}
		r, raw, e := w.seal(SegmentKind, records)
		if e != nil {
			return e
		}
		if e = w.store(ctx, r.Digest, raw); e != nil {
			return e
		}
		for i, b := range pending {
			values[b] = &blockLocation{r, uint32(i)}
		}
		records = nil
		pending = nil
		return nil
	}
	zero := make([]byte, BlockSize)
	for _, b := range blocks {
		if err := ctx.Err(); err != nil {
			return Locator{}, err
		}
		if bytes.Equal(changes[b], zero) {
			values[b] = nil
			continue
		}
		records = append(records, changes[b])
		pending = append(pending, b)
		if len(records) == MaxRecords {
			if err = flushData(); err != nil {
				return Locator{}, err
			}
		}
	}
	if err = flushData(); err != nil {
		return Locator{}, err
	}

	levels := make([][]*directNode, shape.Level+1)
	var update func(*Locator, int, uint64, []uint64) (*directNode, error)
	update = func(old *Locator, level int, start uint64, changed []uint64) (*directNode, error) {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		n := Node{Capacity: shape.Capacity, Fanout: shape.Fanout, Level: level, Start: start}
		if old != nil {
			var e error
			n, e = ReadNode(ctx, w.Source, w.Scope, w.Keys, *old, shape, level, start)
			if e != nil {
				return nil, e
			}
		}
		out := &directNode{node: n, children: map[int]*directNode{}}
		entries := map[int]Entry{}
		for _, e := range n.Entries {
			entries[e.Slot] = e
		}
		if level == 0 {
			locs := map[int]blockLocation{}
			for _, e := range n.Entries {
				locs[e.Slot] = blockLocation{n.Segments[e.Segment], e.Record}
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
			table := map[Ref]int{}
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
				out.node.Entries = append(out.node.Entries, Entry{Slot: slot, Segment: idx, Record: v.Record})
			}
		} else {
			groups := map[int][]uint64{}
			for _, b := range changed {
				slot := int((b - start) / stride(shape.Fanout, level))
				groups[slot] = append(groups[slot], b)
			}
			for slot := 0; slot < shape.Fanout; slot++ {
				group := groups[slot]
				if len(group) == 0 {
					continue
				}
				child, e := update(entries[slot].Child, level-1, start+uint64(slot)*stride(shape.Fanout, level), group)
				if e != nil {
					return nil, e
				}
				if child == nil {
					delete(entries, slot)
				} else {
					out.children[slot] = child
					entries[slot] = Entry{Slot: slot}
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
		return Locator{}, err
	}
	for level, nodes := range levels {
		var batch []stagedPage
		empty, _, e := encodePack(level+1, nil)
		if e != nil {
			return Locator{}, e
		}
		size := len(empty)
		for _, n := range nodes {
			for i, e := range n.node.Entries {
				if child := n.children[e.Slot]; child != nil {
					loc, ok := p.converted[child.ref]
					if !ok {
						return Locator{}, errors.New("missing direct child")
					}
					n.node.Entries[i].Child = &loc
				}
			}
			plain, e := json.Marshal(n.node)
			if e != nil {
				return Locator{}, e
			}
			ref, raw, e := w.seal(NodeKind, [][]byte{plain})
			if e != nil {
				return Locator{}, e
			}
			n.ref = ref
			descriptor, e := json.Marshal(ref)
			if e != nil {
				return Locator{}, e
			}
			addition := len(descriptor) + len(raw)
			if len(batch) > 0 {
				addition++
			}
			if size+addition > limit {
				if e = p.publish(level+1, batch); e != nil {
					return Locator{}, e
				}
				batch = nil
				size = len(empty)
				addition = len(descriptor) + len(raw)
			}
			if size+addition > limit {
				return Locator{}, errors.New("metadata page exceeds pack budget")
			}
			batch = append(batch, stagedPage{ref, raw})
			size += addition
		}
		if err = p.publish(level+1, batch); err != nil {
			return Locator{}, err
		}
	}
	shape.Index = nil
	if top != nil {
		loc := p.converted[top.ref]
		shape.Index = &loc
	}
	plain, err := json.Marshal(shape)
	if err != nil {
		return Locator{}, err
	}
	ref, raw, err := w.seal(RootKind, [][]byte{plain})
	if err != nil {
		return Locator{}, err
	}
	if err = p.publish(shape.Level+2, []stagedPage{{ref, raw}}); err != nil {
		return Locator{}, err
	}
	if err := ctx.Err(); err != nil {
		return Locator{}, err
	}
	return p.converted[ref], nil
}

// Empty stages an empty generation. It does not initialize or publish a Computer.
func (w Writer) Empty(ctx context.Context, capacity int64, fanout int) (Locator, error) {
	if capacity <= 0 || capacity%BlockSize != 0 || capacity/BlockSize > MaxBlocks || (fanout != 64 && fanout != 256) {
		return Locator{}, errors.New("unsupported geometry")
	}
	if err := w.validate(); err != nil {
		return Locator{}, err
	}
	if err := ctx.Err(); err != nil {
		return Locator{}, err
	}
	p := &packWriter{writer: w, ctx: ctx, converted: make(map[Ref]Locator)}
	shape := Root{Capacity: capacity, Fanout: fanout}
	for span := int64(fanout); span < capacity/BlockSize; span *= int64(fanout) {
		shape.Level++
	}
	plain, err := json.Marshal(shape)
	if err != nil {
		return Locator{}, err
	}
	ref, raw, err := w.seal(RootKind, [][]byte{plain})
	if err != nil {
		return Locator{}, err
	}
	if err = p.publish(shape.Level+2, []stagedPage{{ref, raw}}); err != nil {
		return Locator{}, err
	}
	if err := ctx.Err(); err != nil {
		return Locator{}, err
	}
	return p.converted[ref], nil
}
