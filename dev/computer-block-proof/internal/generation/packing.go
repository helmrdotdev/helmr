package generation

import (
	"bytes"
	"crypto/sha256"
	"encoding/binary"
	"encoding/json"
	"errors"
	"sort"

	"github.com/helmrdotdev/helmr/internal/computer/blockformat"
)

type packedEntry struct {
	Slot    int
	Child   *blockformat.Locator `json:",omitempty"`
	Segment int                  `json:",omitempty"`
	Record  uint32               `json:",omitempty"`
}
type packedNode struct {
	Capacity      int64
	Fanout, Level int
	Start         uint64
	Segments      []blockformat.Ref `json:",omitempty"`
	Entries       []packedEntry
}
type packedRoot struct {
	Capacity      int64
	Fanout, Level int
	Index         *blockformat.Locator
}
type directory struct {
	Rank  int
	Pages []blockformat.Ref
}
type stagedPage struct {
	input blockformat.Ref
	ref   blockformat.Ref
	bytes []byte
}

// Packer converts newly changed nodes of existing fixture generations. The input
// map is conversion bookkeeping only: cold readers need only the returned root.
// It is not a second production backend or a proposal for a global locator table.
type Packer struct {
	codec         *Codec
	source, packs *Store
	limit         int
	packInternal  bool
	converted     map[blockformat.Ref]blockformat.Locator
}

func NewPacker(c *Codec, source, packs *Store, limit int, packInternal bool) (*Packer, error) {
	if limit < 64<<10 || limit > 4<<20 {
		return nil, errors.New("invalid pack budget")
	}
	return &Packer{c, source, packs, limit, packInternal, make(map[blockformat.Ref]blockformat.Locator)}, nil
}
func encodePack(rank int, pages []stagedPage) ([]byte, int, error) {
	dir := directory{Rank: rank, Pages: make([]blockformat.Ref, 0, len(pages))}
	for _, page := range pages {
		dir.Pages = append(dir.Pages, page.ref)
	}
	raw, err := json.Marshal(dir)
	if err != nil {
		return nil, 0, err
	}
	var out bytes.Buffer
	out.WriteString("HGP0")
	_ = binary.Write(&out, binary.BigEndian, uint32(len(raw)))
	out.Write(raw)
	start := out.Len()
	for _, page := range pages {
		out.Write(page.bytes)
	}
	return out.Bytes(), start, nil
}
func (p *Packer) publish(rank int, pages []stagedPage) error {
	if len(pages) == 0 {
		return nil
	}
	raw, offset, err := encodePack(rank, pages)
	if err != nil {
		return err
	}
	if len(raw) > p.limit {
		return errors.New("metadata page exceeds pack budget")
	}
	ref := blockformat.PackRef{Digest: sha256.Sum256(raw), Size: int64(len(raw)), Rank: rank}
	if err = p.packs.put(blockformat.Ref{Digest: ref.Digest, Size: ref.Size}, raw); err != nil {
		return err
	}
	for _, page := range pages {
		p.converted[page.input] = blockformat.Locator{Pack: ref, Page: page.ref, Offset: int64(offset)}
		offset += len(page.bytes)
	}
	return nil
}

// Convert performs bottom-up packing of only unknown pages. Old locators remain
// valid; it neither repacks old packs nor advances any authoritative head.
func (p *Packer) Convert(input blockformat.Ref) (blockformat.Locator, error) {
	if loc, ok := p.converted[input]; ok {
		return loc, nil
	}
	shape, err := loadRoot(p.codec, p.source, input)
	if err != nil {
		return blockformat.Locator{}, err
	}
	d := &Disk{codec: p.codec, store: p.source, shape: shape}
	levels := make([]map[blockformat.Ref]node, shape.Level+1)
	for i := range levels {
		levels[i] = map[blockformat.Ref]node{}
	}
	var visit func(*blockformat.Ref, int, uint64) error
	visit = func(ref *blockformat.Ref, level int, start uint64) error {
		if ref == nil {
			return nil
		}
		if _, ok := p.converted[*ref]; ok {
			return nil
		}
		n, err := d.load(ref, level, start)
		if err != nil {
			return err
		}
		levels[level][*ref] = n
		if level > 0 {
			for _, e := range n.Entries {
				if err = visit(e.Child, level-1, start+uint64(e.Slot)*d.stride(level)); err != nil {
					return err
				}
			}
		}
		return nil
	}
	if err = visit(shape.Index, shape.Level, 0); err != nil {
		return blockformat.Locator{}, err
	}
	for level, nodes := range levels {
		refs := make([]blockformat.Ref, 0, len(nodes))
		for ref := range nodes {
			refs = append(refs, ref)
		}
		sort.Slice(refs, func(i, j int) bool { return nodes[refs[i]].Start < nodes[refs[j]].Start })
		var batch []stagedPage
		base, _, err := encodePack(level+1, nil)
		if err != nil {
			return blockformat.Locator{}, err
		}
		batchSize := len(base)
		for _, ref := range refs {
			n := nodes[ref]
			out := packedNode{Capacity: n.Capacity, Fanout: n.Fanout, Level: n.Level, Start: n.Start, Segments: n.Segments}
			for _, e := range n.Entries {
				pe := packedEntry{Slot: e.Slot, Segment: e.Segment, Record: e.Record}
				if e.Child != nil {
					loc, ok := p.converted[*e.Child]
					if !ok {
						return blockformat.Locator{}, errors.New("missing child placement")
					}
					pe.Child = &loc
				}
				out.Entries = append(out.Entries, pe)
			}
			plain, err := json.Marshal(out)
			if err != nil {
				return blockformat.Locator{}, err
			}
			encoded, b, err := p.codec.seal(nodeKind, [][]byte{plain})
			if err != nil {
				return blockformat.Locator{}, err
			}
			page := stagedPage{ref, encoded, b}
			descriptor, err := json.Marshal(encoded)
			if err != nil {
				return blockformat.Locator{}, err
			}
			addition := len(descriptor) + len(b)
			if len(batch) > 0 {
				addition++
			}
			if batchSize+addition > p.limit || (level > 0 && !p.packInternal && len(batch) > 0) {
				if err = p.publish(level+1, batch); err != nil {
					return blockformat.Locator{}, err
				}
				batch = nil
				batchSize = len(base)
				addition = len(descriptor) + len(b)
			}
			if batchSize+addition > p.limit {
				return blockformat.Locator{}, errors.New("metadata page exceeds pack budget")
			}
			batchSize += addition
			batch = append(batch, page)
		}
		if err = p.publish(level+1, batch); err != nil {
			return blockformat.Locator{}, err
		}
	}
	out := packedRoot{Capacity: shape.Capacity, Fanout: shape.Fanout, Level: shape.Level}
	if shape.Index != nil {
		loc := p.converted[*shape.Index]
		out.Index = &loc
	}
	plain, err := json.Marshal(out)
	if err != nil {
		return blockformat.Locator{}, err
	}
	ref, b, err := p.codec.seal(rootKind, [][]byte{plain})
	if err != nil {
		return blockformat.Locator{}, err
	}
	if err = p.publish(shape.Level+2, []stagedPage{{input, ref, b}}); err != nil {
		return blockformat.Locator{}, err
	}
	return p.converted[input], nil
}
func pageBytes(c *Codec, s *Store, l blockformat.Locator) ([]byte, error) {
	if l.Pack.Size < 8 || l.Pack.Size > 4<<20 || l.Pack.Rank < 1 || l.Pack.Rank > 6 || l.Page.Size <= 0 || l.Page.Size > maxMetadata+1024 || l.Offset < 8 || l.Offset > l.Pack.Size-l.Page.Size {
		return nil, errors.New("invalid locator")
	}
	b, err := s.readRange(blockformat.Ref{Digest: l.Pack.Digest, Size: l.Pack.Size}, l.Offset, l.Page.Size)
	if err != nil {
		return nil, err
	}
	// The authenticated parent binds this exact page identity. A range cannot
	// verify the full pack hash; the page's own digest and AEAD are checked instead.
	tmp := NewStore()
	if err = tmp.put(l.Page, b); err != nil {
		return nil, err
	}
	return c.metadata(tmp, l.Page)
}
func openPacked(c *Codec, s *Store, l blockformat.Locator) (packedRoot, error) {
	var out packedRoot
	if l.Page.Kind != rootKind {
		return out, errors.New("packed root required")
	}
	b, err := pageBytes(c, s, l)
	if err != nil {
		return out, err
	}
	if err = decode(b, &out); err != nil {
		return out, err
	}
	if out.Capacity <= 0 || out.Capacity%BlockSize != 0 || out.Capacity/BlockSize > maxBlocks || (out.Fanout != 64 && out.Fanout != 256) {
		return out, errors.New("packed root geometry")
	}
	level := 0
	for span := int64(out.Fanout); span < out.Capacity/BlockSize; span *= int64(out.Fanout) {
		level++
	}
	if out.Level != level || l.Pack.Rank != level+2 {
		return out, errors.New("packed root rank")
	}
	return out, nil
}
func loadPacked(c *Codec, s *Store, l blockformat.Locator, shape packedRoot, level int, start uint64) (packedNode, error) {
	var out packedNode
	if l.Page.Kind != nodeKind || l.Pack.Rank != level+1 {
		return out, errors.New("packed node rank")
	}
	b, err := pageBytes(c, s, l)
	if err != nil {
		return out, err
	}
	if err = decode(b, &out); err != nil {
		return out, err
	}
	n := node{Capacity: out.Capacity, Fanout: out.Fanout, Level: out.Level, Start: out.Start, Segments: out.Segments}
	for _, e := range out.Entries {
		plain := entry{Slot: e.Slot, Segment: e.Segment, Record: e.Record}
		if e.Child != nil {
			if e.Child.Pack.Rank != level {
				return out, errors.New("child pack rank")
			}
			ref := e.Child.Page
			plain.Child = &ref
		}
		n.Entries = append(n.Entries, plain)
	}
	d := &Disk{shape: root{Capacity: shape.Capacity, Fanout: shape.Fanout}}
	_, err = d.validateNode(n, level, start)
	return out, err
}

// ReadPacked reads one logical block without the converter's placement map.
func ReadPacked(c *Codec, data, packs *Store, root blockformat.Locator, block uint64) ([]byte, error) {
	shape, err := openPacked(c, packs, root)
	if err != nil {
		return nil, err
	}
	if block >= uint64(shape.Capacity/BlockSize) {
		return nil, errors.New("block bounds")
	}
	ref := shape.Index
	start := uint64(0)
	d := &Disk{shape: rootShape(shape)}
	for level := shape.Level; level >= 0; level-- {
		if ref == nil {
			return make([]byte, BlockSize), nil
		}
		n, err := loadPacked(c, packs, *ref, shape, level, start)
		if err != nil {
			return nil, err
		}
		slot := int((block - start) / d.stride(level))
		i := sort.Search(len(n.Entries), func(i int) bool { return n.Entries[i].Slot >= slot })
		if i == len(n.Entries) || n.Entries[i].Slot != slot {
			return make([]byte, BlockSize), nil
		}
		e := n.Entries[i]
		if level == 0 {
			return c.block(data, n.Segments[e.Segment], e.Record)
		}
		ref = e.Child
		start += uint64(slot) * d.stride(level)
	}
	return nil, errors.New("invalid tree")
}
func rootShape(r packedRoot) root {
	return root{Capacity: r.Capacity, Fanout: r.Fanout, Level: r.Level}
}

// PackChildren is an offline certification experiment: it hashes a whole pack,
// checks every directory entry and encrypted page, and derives unique physical
// dependencies. Full tree range/geometry checks remain the reader's responsibility.
func PackChildren(c *Codec, s *Store, ref blockformat.PackRef) ([]blockformat.PackRef, []blockformat.Ref, error) {
	if ref.Size < 8 || ref.Size > 4<<20 || ref.Rank < 1 || ref.Rank > 6 {
		return nil, nil, errors.New("invalid pack descriptor")
	}
	raw, err := s.get(blockformat.Ref{Digest: ref.Digest, Size: ref.Size})
	if err != nil {
		return nil, nil, err
	}
	if len(raw) < 8 || len(raw) > 4<<20 || sha256.Sum256(raw) != ref.Digest || string(raw[:4]) != "HGP0" {
		return nil, nil, errors.New("pack identity")
	}
	size := int(binary.BigEndian.Uint32(raw[4:8]))
	if size > len(raw)-8 {
		return nil, nil, errors.New("directory bounds")
	}
	var dir directory
	if err = decode(raw[8:8+size], &dir); err != nil {
		return nil, nil, err
	}
	if dir.Rank != ref.Rank || len(dir.Pages) == 0 {
		return nil, nil, errors.New("directory rank")
	}
	seen := map[blockformat.Ref]bool{}
	packSet := map[blockformat.PackRef]bool{}
	dataSet := map[blockformat.Ref]bool{}
	offset := int64(8 + size)
	for _, page := range dir.Pages {
		if seen[page] {
			return nil, nil, errors.New("duplicate page")
		}
		seen[page] = true
		loc := blockformat.Locator{Pack: ref, Page: page, Offset: offset}
		b, err := pageBytes(c, s, loc)
		if err != nil {
			return nil, nil, err
		}
		offset += page.Size
		if page.Kind == rootKind {
			r, err := openPacked(c, s, loc)
			if err != nil {
				return nil, nil, err
			}
			if r.Index != nil {
				if r.Index.Pack.Rank != ref.Rank-1 {
					return nil, nil, errors.New("root child rank")
				}
				packSet[r.Index.Pack] = true
			}
		} else {
			var n packedNode
			if err = decode(b, &n); err != nil {
				return nil, nil, err
			}
			if n.Level < 0 || n.Level > 4 {
				return nil, nil, errors.New("node level")
			}
			shape := packedRoot{Capacity: n.Capacity, Fanout: n.Fanout}
			if shape.Capacity <= 0 || shape.Capacity%BlockSize != 0 || shape.Capacity/BlockSize > maxBlocks || (shape.Fanout != 64 && shape.Fanout != 256) {
				return nil, nil, errors.New("node geometry")
			}
			maxLevel := 0
			for span := int64(shape.Fanout); span < shape.Capacity/BlockSize; span *= int64(shape.Fanout) {
				maxLevel++
			}
			if n.Level > maxLevel || n.Start >= uint64(shape.Capacity/BlockSize) {
				return nil, nil, errors.New("node rank or range")
			}
			n, err = loadPacked(c, s, loc, shape, n.Level, n.Start)
			if err != nil {
				return nil, nil, err
			}
			for _, entry := range n.Entries {
				if entry.Child != nil {
					packSet[entry.Child.Pack] = true
				}
			}
			for _, data := range n.Segments {
				dataSet[data] = true
			}
		}
	}
	if offset != int64(len(raw)) {
		return nil, nil, errors.New("unlisted pack bytes")
	}
	var packs []blockformat.PackRef
	for p := range packSet {
		packs = append(packs, p)
	}
	var data []blockformat.Ref
	for r := range dataSet {
		data = append(data, r)
	}
	return packs, data, nil
}
