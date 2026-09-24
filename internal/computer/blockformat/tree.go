package blockformat

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"sort"
)

const MaxBlocks = 1 << 28 // At most 1 TiB with 4 KiB blocks.

type Entry struct {
	Slot    int
	Child   *Locator `json:",omitempty"`
	Segment int      `json:",omitempty"`
	Record  uint32   `json:",omitempty"`
}
type Node struct {
	Capacity      int64
	Fanout, Level int
	Start         uint64
	Segments      []Ref `json:",omitempty"`
	Entries       []Entry
}
type Root struct {
	Capacity      int64
	Fanout, Level int
	Index         *Locator
}

func decodeTree(raw []byte, out any) error {
	d := json.NewDecoder(bytes.NewReader(raw))
	d.DisallowUnknownFields()
	if err := d.Decode(out); err != nil {
		return err
	}
	if err := d.Decode(new(any)); err != io.EOF {
		return errors.New("trailing tree metadata")
	}
	return nil
}
func validateRoot(r Root) error {
	if r.Capacity <= 0 || r.Capacity%BlockSize != 0 || r.Capacity/BlockSize > MaxBlocks || (r.Fanout != 64 && r.Fanout != 256) {
		return errors.New("invalid generation geometry")
	}
	level := 0
	for span := int64(r.Fanout); span < r.Capacity/BlockSize; span *= int64(r.Fanout) {
		level++
	}
	if r.Level != level {
		return errors.New("invalid generation depth")
	}
	if r.Index != nil && (r.Index.Page.Kind != NodeKind || r.Index.Pack.Rank != level+1) {
		return errors.New("invalid generation index")
	}
	return nil
}

// ReadRoot authenticates the exact root locator and its finite tree geometry.
// Its capacity must still match the separately admitted Computer capacity.
func ReadRoot(ctx context.Context, source RangeSource, scope string, keys map[string][]byte, l Locator) (Root, error) {
	var r Root
	if l.Page.Kind != RootKind {
		return r, errors.New("generation root required")
	}
	b, err := ReadPage(ctx, source, scope, keys[l.Page.Key], l)
	if err != nil {
		return r, err
	}
	if err = decodeTree(b, &r); err != nil {
		return Root{}, err
	}
	if err = validateRoot(r); err != nil {
		return Root{}, err
	}
	if l.Pack.Rank != r.Level+2 {
		return Root{}, errors.New("generation root rank mismatch")
	}
	return r, nil
}
func stride(fanout, level int) uint64 {
	n := uint64(1)
	for range level {
		n *= uint64(fanout)
	}
	return n
}

// ReadNode checks the authenticated node against its position in an already
// authenticated root. Missing objects and malformed nodes are errors, not holes.
func ReadNode(ctx context.Context, source RangeSource, scope string, keys map[string][]byte, l Locator, shape Root, level int, start uint64) (Node, error) {
	var n Node
	if err := validateRoot(shape); err != nil {
		return n, err
	}
	if level < 0 || level > shape.Level || start >= uint64(shape.Capacity/BlockSize) || start%stride(shape.Fanout, level+1) != 0 || l.Page.Kind != NodeKind || l.Pack.Rank != level+1 {
		return n, errors.New("invalid node position")
	}
	b, err := ReadPage(ctx, source, scope, keys[l.Page.Key], l)
	if err != nil {
		return n, err
	}
	if err = decodeTree(b, &n); err != nil {
		return Node{}, err
	}
	if n.Capacity != shape.Capacity || n.Fanout != shape.Fanout || n.Level != level || n.Start != start || len(n.Entries) > shape.Fanout || len(n.Segments) > shape.Fanout {
		return Node{}, errors.New("invalid node geometry")
	}
	used := make(map[int]bool)
	seen := make(map[Ref]bool)
	last := -1
	for _, e := range n.Entries {
		if e.Slot <= last || e.Slot >= shape.Fanout || start+uint64(e.Slot)*stride(shape.Fanout, level) >= uint64(shape.Capacity/BlockSize) {
			return Node{}, errors.New("invalid node slot")
		}
		last = e.Slot
		if level > 0 {
			if e.Child == nil || e.Child.Page.Kind != NodeKind || e.Child.Pack.Rank != level || e.Segment != 0 || e.Record != 0 {
				return Node{}, errors.New("invalid child reference")
			}
		} else {
			if e.Child != nil || e.Segment < 0 || e.Segment >= len(n.Segments) {
				return Node{}, errors.New("invalid segment reference")
			}
			ref := n.Segments[e.Segment]
			if ref.Kind != SegmentKind || e.Record >= ref.Count {
				return Node{}, errors.New("invalid segment record")
			}
			used[e.Segment] = true
		}
	}
	if level > 0 && len(n.Segments) > 0 {
		return Node{}, errors.New("internal segment table")
	}
	for i, ref := range n.Segments {
		if !used[i] || seen[ref] {
			return Node{}, errors.New("noncanonical segment table")
		}
		seen[ref] = true
	}
	return n, nil
}

// Tree is a read-only view of one authenticated generation. The caller owns the
// source and scoped keys and must keep them unchanged and live during reads.
// It has no mutable page cache and is safe for concurrent reads if the source is.
type Tree struct {
	source RangeSource
	scope  string
	keys   map[string][]byte
	shape  Root
}

func OpenTree(ctx context.Context, source RangeSource, scope string, keys map[string][]byte, root Locator) (*Tree, error) {
	shape, err := ReadRoot(ctx, source, scope, keys, root)
	if err != nil {
		return nil, err
	}
	return &Tree{source: source, scope: scope, keys: keys, shape: shape}, nil
}
func (t *Tree) Capacity() int64 { return t.shape.Capacity }
func (t *Tree) ReadBlock(ctx context.Context, block uint64) ([]byte, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if block >= uint64(t.shape.Capacity/BlockSize) {
		return nil, errors.New("generation block bounds")
	}
	ref := t.shape.Index
	start := uint64(0)
	for level := t.shape.Level; level >= 0; level-- {
		if ref == nil {
			return make([]byte, BlockSize), nil
		}
		n, err := ReadNode(ctx, t.source, t.scope, t.keys, *ref, t.shape, level, start)
		if err != nil {
			return nil, err
		}
		slot := int((block - start) / stride(t.shape.Fanout, level))
		i := sort.Search(len(n.Entries), func(i int) bool { return n.Entries[i].Slot >= slot })
		if i == len(n.Entries) || n.Entries[i].Slot != slot {
			return make([]byte, BlockSize), nil
		}
		e := n.Entries[i]
		if level == 0 {
			segment := n.Segments[e.Segment]
			return ReadBlock(ctx, t.source, t.scope, t.keys[segment.Key], segment, e.Record)
		}
		ref = e.Child
		start += uint64(slot) * stride(t.shape.Fanout, level)
	}
	return nil, errors.New("invalid generation tree")
}
