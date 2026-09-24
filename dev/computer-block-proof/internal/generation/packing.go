package generation

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/binary"
	"encoding/json"
	"errors"
	"sort"

	"github.com/helmrdotdev/helmr/internal/computer/blockformat"
)

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
			out := blockformat.Node{Capacity: n.Capacity, Fanout: n.Fanout, Level: n.Level, Start: n.Start, Segments: n.Segments}
			for _, e := range n.Entries {
				pe := blockformat.Entry{Slot: e.Slot, Segment: e.Segment, Record: e.Record}
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
			encoded, b, err := p.codec.seal(blockformat.NodeKind, [][]byte{plain})
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
	out := blockformat.Root{Capacity: shape.Capacity, Fanout: shape.Fanout, Level: shape.Level}
	if shape.Index != nil {
		loc := p.converted[*shape.Index]
		out.Index = &loc
	}
	plain, err := json.Marshal(out)
	if err != nil {
		return blockformat.Locator{}, err
	}
	ref, b, err := p.codec.seal(blockformat.RootKind, [][]byte{plain})
	if err != nil {
		return blockformat.Locator{}, err
	}
	if err = p.publish(shape.Level+2, []stagedPage{{input, ref, b}}); err != nil {
		return blockformat.Locator{}, err
	}
	return p.converted[input], nil
}
func pageBytes(c *Codec, s *Store, l blockformat.Locator) ([]byte, error) {
	return blockformat.ReadPage(context.Background(), storeRanges{s}, c.Scope, c.Keys[l.Page.Key], l)
}
func openPacked(c *Codec, s *Store, l blockformat.Locator) (blockformat.Root, error) {
	return blockformat.ReadRoot(context.Background(), storeRanges{s}, c.Scope, c.Keys, l)
}
func loadPacked(c *Codec, s *Store, l blockformat.Locator, shape blockformat.Root, level int, start uint64) (blockformat.Node, error) {
	return blockformat.ReadNode(context.Background(), storeRanges{s}, c.Scope, c.Keys, l, shape, level, start)
}
func ReadPacked(c *Codec, data, packs *Store, root blockformat.Locator, block uint64) ([]byte, error) {
	tree, err := blockformat.OpenTree(context.Background(), treeRanges{data, packs}, c.Scope, c.Keys, root)
	if err != nil {
		return nil, err
	}
	return tree.ReadBlock(context.Background(), block)
}
func rootShape(r blockformat.Root) root {
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
		if page.Kind == blockformat.RootKind {
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
			var n blockformat.Node
			if err = decode(b, &n); err != nil {
				return nil, nil, err
			}
			if n.Level < 0 || n.Level > 4 {
				return nil, nil, errors.New("node level")
			}
			shape := blockformat.Root{Capacity: n.Capacity, Fanout: n.Fanout}
			if shape.Capacity <= 0 || shape.Capacity%blockformat.BlockSize != 0 || shape.Capacity/blockformat.BlockSize > blockformat.MaxBlocks || (shape.Fanout != 64 && shape.Fanout != 256) {
				return nil, nil, errors.New("node geometry")
			}
			maxLevel := 0
			for span := int64(shape.Fanout); span < shape.Capacity/blockformat.BlockSize; span *= int64(shape.Fanout) {
				maxLevel++
			}
			if n.Level > maxLevel || n.Start >= uint64(shape.Capacity/blockformat.BlockSize) {
				return nil, nil, errors.New("node rank or range")
			}
			shape.Level = maxLevel
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
