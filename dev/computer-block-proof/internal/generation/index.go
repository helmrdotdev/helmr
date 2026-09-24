package generation

import (
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"sort"

	"github.com/helmrdotdev/helmr/internal/computer/blockformat"
)

const maxBlocks = 1 << 28 // finite experimental geometry, up to 1 TiB
const maxDirty = 16384
const maxRequestBytes = 8 << 20 // bounds scratch allocation in atomic read/write calls

type root struct {
	Capacity int64
	Fanout   int
	Level    int
	Index    *blockformat.Ref
}
type entry struct {
	Slot    int
	Child   *blockformat.Ref `json:",omitempty"`
	Segment int              `json:",omitempty"`
	Record  uint32           `json:",omitempty"`
}
type node struct {
	Capacity int64
	Fanout   int
	Level    int
	Start    uint64
	Segments []blockformat.Ref `json:",omitempty"`
	Entries  []entry
}
type location struct {
	Segment blockformat.Ref
	Record  uint32
}

// Disk is a single-owner development fixture. Dirty memory is bounded; it has
// neither concurrent mutation nor persistent WRITE/FLUSH semantics.
type Disk struct {
	codec    *Codec
	store    *Store
	shape    root
	snapshot blockformat.Ref
	dirty    map[uint64][]byte
}

func New(c *Codec, s *Store, capacity int64, fanout int) (*Disk, error) {
	if capacity <= 0 || capacity%blockformat.BlockSize != 0 || capacity/blockformat.BlockSize > maxBlocks || (fanout != 64 && fanout != 256) {
		return nil, errors.New("unsupported geometry")
	}
	level := 0
	for span := uint64(fanout); span < uint64(capacity/blockformat.BlockSize); span *= uint64(fanout) {
		level++
	}
	d := &Disk{codec: c, store: s, shape: root{Capacity: capacity, Fanout: fanout, Level: level}, dirty: make(map[uint64][]byte)}
	r, err := d.save(blockformat.RootKind, d.shape)
	if err != nil {
		return nil, err
	}
	d.snapshot = r
	return d, nil
}
func decode(p []byte, out any) error {
	dec := json.NewDecoder(bytes.NewReader(p))
	dec.DisallowUnknownFields()
	if err := dec.Decode(out); err != nil {
		return err
	}
	if dec.Decode(new(any)) != io.EOF {
		return errors.New("trailing metadata")
	}
	return nil
}
func loadRoot(c *Codec, s *Store, r blockformat.Ref) (root, error) {
	var out root
	if r.Kind != blockformat.RootKind {
		return out, errors.New("root required")
	}
	b, err := c.metadata(s, r)
	if err != nil {
		return out, err
	}
	if err = decode(b, &out); err != nil {
		return out, err
	}
	if out.Capacity <= 0 || out.Capacity%blockformat.BlockSize != 0 || out.Capacity/blockformat.BlockSize > maxBlocks || (out.Fanout != 64 && out.Fanout != 256) {
		return out, errors.New("root geometry")
	}
	level := 0
	for span := int64(out.Fanout); span < out.Capacity/blockformat.BlockSize; span *= int64(out.Fanout) {
		level++
	}
	if out.Level != level || (out.Index != nil && out.Index.Kind != blockformat.NodeKind) {
		return out, errors.New("root index")
	}
	return out, nil
}
func Open(c *Codec, s *Store, r blockformat.Ref) (*Disk, error) {
	shape, err := loadRoot(c, s, r)
	if err != nil {
		return nil, err
	}
	return &Disk{codec: c, store: s, shape: shape, snapshot: r, dirty: make(map[uint64][]byte)}, nil
}
func (d *Disk) save(kind byte, v any) (blockformat.Ref, error) {
	p, err := json.Marshal(v)
	if err != nil {
		return blockformat.Ref{}, err
	}
	r, b, err := d.codec.seal(kind, [][]byte{p})
	if err != nil {
		return blockformat.Ref{}, err
	}
	return r, d.store.put(r, b)
}
func (d *Disk) stride(level int) uint64 {
	n := uint64(1)
	for range level {
		n *= uint64(d.shape.Fanout)
	}
	return n
}
func (d *Disk) load(r *blockformat.Ref, level int, start uint64) (node, error) {
	n := node{Level: level, Start: start, Capacity: d.shape.Capacity, Fanout: d.shape.Fanout}
	if r == nil {
		return n, nil
	}
	if r.Kind != blockformat.NodeKind {
		return n, errors.New("index required")
	}
	b, err := d.codec.metadata(d.store, *r)
	if err != nil {
		return n, err
	}
	n = node{}
	if err = decode(b, &n); err != nil {
		return n, err
	}
	return d.validateNode(n, level, start)
}
func (d *Disk) validateNode(n node, level int, start uint64) (node, error) {
	if n.Capacity != d.shape.Capacity || n.Fanout != d.shape.Fanout || n.Level != level || n.Start != start || start%d.stride(level+1) != 0 || len(n.Entries) > d.shape.Fanout || len(n.Segments) > d.shape.Fanout {
		return n, errors.New("invalid node geometry")
	}
	used := make(map[int]bool)
	seen := make(map[blockformat.Ref]bool)
	last := -1
	for _, e := range n.Entries {
		if e.Slot <= last || e.Slot >= d.shape.Fanout || start+uint64(e.Slot)*d.stride(level) >= uint64(d.shape.Capacity/blockformat.BlockSize) {
			return n, errors.New("invalid slot")
		}
		last = e.Slot
		if level > 0 {
			if e.Child == nil || e.Child.Kind != blockformat.NodeKind || e.Segment != 0 || e.Record != 0 {
				return n, errors.New("invalid child")
			}
		} else {
			if e.Child != nil || e.Segment < 0 || e.Segment >= len(n.Segments) {
				return n, errors.New("invalid segment index")
			}
			ref := n.Segments[e.Segment]
			if ref.Kind != blockformat.SegmentKind || e.Record >= ref.Count {
				return n, errors.New("invalid record")
			}
			used[e.Segment] = true
		}
	}
	if level > 0 && len(n.Segments) > 0 {
		return n, errors.New("internal segment table")
	}
	for i, ref := range n.Segments {
		if !used[i] || seen[ref] {
			return n, errors.New("noncanonical segment table")
		}
		seen[ref] = true
	}
	return n, nil
}
func (d *Disk) readBlock(block uint64) ([]byte, error) {
	if p, ok := d.dirty[block]; ok {
		return bytes.Clone(p), nil
	}
	ref := d.shape.Index
	start := uint64(0)
	for level := d.shape.Level; level >= 0; level-- {
		n, err := d.load(ref, level, start)
		if err != nil {
			return nil, err
		}
		slot := int((block - start) / d.stride(level))
		i := sort.Search(len(n.Entries), func(i int) bool { return n.Entries[i].Slot >= slot })
		if i == len(n.Entries) || n.Entries[i].Slot != slot {
			return make([]byte, blockformat.BlockSize), nil
		}
		e := n.Entries[i]
		if level == 0 {
			return d.codec.block(d.store, n.Segments[e.Segment], e.Record)
		}
		ref = e.Child
		start += uint64(slot) * d.stride(level)
	}
	return nil, errors.New("invalid depth")
}
func (d *Disk) ReadAt(p []byte, off int64) error {
	if len(p) > maxRequestBytes || off < 0 || int64(len(p)) > d.shape.Capacity || off > d.shape.Capacity-int64(len(p)) {
		return errors.New("read bounds")
	}
	// Return no partial plaintext to the caller on a later authentication failure.
	out := make([]byte, len(p))
	for pos := 0; pos < len(p); {
		at := off + int64(pos)
		b, err := d.readBlock(uint64(at / blockformat.BlockSize))
		if err != nil {
			return err
		}
		n := copy(out[pos:], b[at%blockformat.BlockSize:])
		pos += n
	}
	copy(p, out)
	return nil
}
func (d *Disk) WriteAt(p []byte, off int64) error {
	if len(p) > maxRequestBytes || off < 0 || int64(len(p)) > d.shape.Capacity || off > d.shape.Capacity-int64(len(p)) {
		return errors.New("write bounds")
	}
	next := make(map[uint64][]byte)
	newCount := 0
	for pos := 0; pos < len(p); {
		at := off + int64(pos)
		block := uint64(at / blockformat.BlockSize)
		if _, ok := d.dirty[block]; !ok {
			newCount++
		}
		if len(d.dirty)+newCount > maxDirty {
			return errors.New("dirty capacity exhausted")
		}
		b, err := d.readBlock(block)
		if err != nil {
			return err
		}
		n := copy(b[at%blockformat.BlockSize:], p[pos:])
		next[block] = b
		pos += n
	}
	for block, p := range next {
		d.dirty[block] = p
	}
	return nil
}
func (d *Disk) update(old *blockformat.Ref, level int, start uint64, changes map[uint64]*location) (*blockformat.Ref, error) {
	n, err := d.load(old, level, start)
	if err != nil {
		return nil, err
	}
	entries := make(map[int]entry)
	for _, e := range n.Entries {
		entries[e.Slot] = e
	}
	if level == 0 {
		values := make(map[int]location)
		for _, e := range n.Entries {
			values[e.Slot] = location{n.Segments[e.Segment], e.Record}
		}
		for block, loc := range changes {
			slot := int(block - start)
			if loc == nil {
				delete(values, slot)
			} else {
				values[slot] = *loc
			}
		}
		n.Entries = nil
		n.Segments = nil
		table := make(map[blockformat.Ref]int)
		for slot := 0; slot < d.shape.Fanout; slot++ {
			loc, ok := values[slot]
			if !ok {
				continue
			}
			idx, ok := table[loc.Segment]
			if !ok {
				idx = len(n.Segments)
				table[loc.Segment] = idx
				n.Segments = append(n.Segments, loc.Segment)
			}
			n.Entries = append(n.Entries, entry{Slot: slot, Segment: idx, Record: loc.Record})
		}
	} else {
		groups := make(map[int]map[uint64]*location)
		for block, loc := range changes {
			slot := int((block - start) / d.stride(level))
			if groups[slot] == nil {
				groups[slot] = make(map[uint64]*location)
			}
			groups[slot][block] = loc
		}
		for slot := 0; slot < d.shape.Fanout; slot++ {
			group, ok := groups[slot]
			if !ok {
				continue
			}
			e := entries[slot]
			child, err := d.update(e.Child, level-1, start+uint64(slot)*d.stride(level), group)
			if err != nil {
				return nil, err
			}
			if child == nil {
				delete(entries, slot)
			} else {
				entries[slot] = entry{Slot: slot, Child: child}
			}
		}
		n.Entries = nil
		for slot := 0; slot < d.shape.Fanout; slot++ {
			if e, ok := entries[slot]; ok {
				n.Entries = append(n.Entries, e)
			}
		}
	}
	if len(n.Entries) == 0 {
		return nil, nil
	}
	r, err := d.save(blockformat.NodeKind, n)
	return &r, err
}

// Capture is an immutable cut, not a durable commit or remote publication.
func (d *Disk) Capture() (blockformat.Ref, error) {
	if len(d.dirty) == 0 {
		return d.snapshot, nil
	}
	blocks := make([]uint64, 0, len(d.dirty))
	for block := range d.dirty {
		blocks = append(blocks, block)
	}
	sort.Slice(blocks, func(i, j int) bool { return blocks[i] < blocks[j] })
	changes := make(map[uint64]*location)
	var records [][]byte
	var pending []uint64
	flush := func() error {
		if len(records) == 0 {
			return nil
		}
		r, b, err := d.codec.seal(blockformat.SegmentKind, records)
		if err != nil {
			return err
		}
		if err = d.store.put(r, b); err != nil {
			return err
		}
		for i, block := range pending {
			changes[block] = &location{r, uint32(i)}
		}
		records = nil
		pending = nil
		return nil
	}
	for _, block := range blocks {
		p := d.dirty[block]
		if bytes.Equal(p, make([]byte, blockformat.BlockSize)) {
			changes[block] = nil
			continue
		}
		records = append(records, p)
		pending = append(pending, block)
		if len(records) == blockformat.MaxRecords {
			if err := flush(); err != nil {
				return blockformat.Ref{}, err
			}
		}
	}
	if err := flush(); err != nil {
		return blockformat.Ref{}, err
	}
	index, err := d.update(d.shape.Index, d.shape.Level, 0, changes)
	if err != nil {
		return blockformat.Ref{}, err
	}
	shape := d.shape
	shape.Index = index
	r, err := d.save(blockformat.RootKind, shape)
	if err != nil {
		return blockformat.Ref{}, err
	}
	d.shape = shape
	d.snapshot = r
	clear(d.dirty)
	return r, nil
}

// Children authenticates metadata before deriving its unique immediate edges.
// Geometry is checked against an expected root/node by the index reader as well.
func (c *Codec) Children(s *Store, r blockformat.Ref) ([]blockformat.Ref, error) {
	var refs []blockformat.Ref
	if r.Kind == blockformat.RootKind {
		shape, err := loadRoot(c, s, r)
		if err != nil {
			return nil, err
		}
		if shape.Index != nil {
			refs = append(refs, *shape.Index)
		}
	} else if r.Kind == blockformat.NodeKind {
		p, err := c.metadata(s, r)
		if err != nil {
			return nil, err
		}
		var n node
		if err = decode(p, &n); err != nil {
			return nil, err
		}
		if n.Capacity <= 0 || n.Capacity%blockformat.BlockSize != 0 || n.Capacity/blockformat.BlockSize > maxBlocks || (n.Fanout != 64 && n.Fanout != 256) || n.Level < 0 || n.Level > 4 {
			return nil, errors.New("node geometry")
		}
		maxLevel := 0
		for span := int64(n.Fanout); span < n.Capacity/blockformat.BlockSize; span *= int64(n.Fanout) {
			maxLevel++
		}
		if n.Level > maxLevel || n.Start >= uint64(n.Capacity/blockformat.BlockSize) {
			return nil, errors.New("node rank")
		}
		d := &Disk{codec: c, store: s, shape: root{Capacity: n.Capacity, Fanout: n.Fanout}}
		if _, err = d.validateNode(n, n.Level, n.Start); err != nil {
			return nil, err
		}
		refs = append(refs, n.Segments...)
		for _, e := range n.Entries {
			if e.Child != nil {
				refs = append(refs, *e.Child)
			}
		}
	} else {
		return nil, errors.New("metadata required")
	}
	seen := make(map[blockformat.Ref]bool)
	out := make([]blockformat.Ref, 0, len(refs))
	for _, ref := range refs {
		if _, err := blockformat.Header(c.Scope, ref); err != nil {
			return nil, err
		}
		if !seen[ref] {
			seen[ref] = true
			out = append(out, ref)
		}
	}
	return out, nil
}

// Import reads exactly the declared raw capacity without a filepack bridge.
// It streams finite batches; incomplete imports return no published root.
func Import(c *Codec, s *Store, capacity int64, fanout int, source io.Reader) (blockformat.Ref, error) {
	d, err := New(c, s, capacity, fanout)
	if err != nil {
		return blockformat.Ref{}, err
	}
	batch := make([]byte, blockformat.MaxRecords*blockformat.BlockSize)
	for off := int64(0); off < capacity; {
		n := min(int64(len(batch)), capacity-off)
		if _, err = io.ReadFull(source, batch[:n]); err != nil {
			return blockformat.Ref{}, err
		}
		if err = d.WriteAt(batch[:n], off); err != nil {
			return blockformat.Ref{}, err
		}
		if _, err = d.Capture(); err != nil {
			return blockformat.Ref{}, err
		}
		off += n
	}
	var extra [1]byte
	n, err := source.Read(extra[:])
	if n != 0 || err != io.EOF {
		return blockformat.Ref{}, errors.New("seed length differs from capacity")
	}
	return d.snapshot, nil
}
