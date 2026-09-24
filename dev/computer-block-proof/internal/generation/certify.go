package generation

import (
	"crypto/sha256"
	"encoding/binary"
	"errors"

	"github.com/helmrdotdev/helmr/internal/computer/blockformat"
)

// Certification is a local inspection result, never publication/retention authority.
// Counts cover the physical closure, including obsolete pages in reachable packs.
type Certification struct{ Packs, Segments, Bytes int64 }

// Certify authenticates the complete physical closure and every incoming locator.
// The single-owner immutable stores must remain present throughout inspection.
// This experiment has no concurrent GC, pin transaction or persisted certificate.
func Certify(c *Codec, data, packs *Store, root blockformat.Locator, maxObjects, maxBytes int64) (Certification, error) {
	var report Certification
	fail := Certification{}
	if maxObjects <= 0 || maxBytes <= 0 {
		return fail, errors.New("invalid certification budget")
	}
	members := map[blockformat.PackRef]map[blockformat.Locator]bool{}
	segments := map[blockformat.Ref]bool{}
	charge := func(size int64) error {
		if size <= 0 || report.Packs+report.Segments >= maxObjects || size > maxBytes-report.Bytes {
			return errors.New("certification budget exceeded")
		}
		report.Bytes += size
		return nil
	}
	segment := func(r blockformat.Ref) error {
		if segments[r] {
			return nil
		}
		if r.Kind != blockformat.SegmentKind {
			return errors.New("segment required")
		}
		h, err := blockformat.Header(c.Scope, r)
		if err != nil {
			return err
		}
		if r.Size != int64(len(h))+int64(r.Count)*(blockformat.BlockSize+20) {
			return errors.New("segment geometry mismatch")
		}
		if err = charge(r.Size); err != nil {
			return err
		}
		report.Segments++
		raw, err := data.get(r)
		if err != nil {
			return err
		}
		if sha256.Sum256(raw) != r.Digest {
			return errors.New("segment digest mismatch")
		}
		for i := uint32(0); i < r.Count; i++ {
			if _, err = c.block(data, r, i); err != nil {
				return err
			}
		}
		segments[r] = true
		return nil
	}
	var visit func(blockformat.PackRef) error
	var child func(blockformat.Locator, blockformat.Root, int, uint64) error
	child = func(l blockformat.Locator, shape blockformat.Root, level int, start uint64) error {
		if l.Pack.Rank != level+1 {
			return errors.New("child rank mismatch")
		}
		if err := visit(l.Pack); err != nil {
			return err
		}
		if !members[l.Pack][l] {
			return errors.New("locator absent from directory")
		}
		_, err := loadPacked(c, packs, l, shape, level, start)
		return err
	}
	visit = func(ref blockformat.PackRef) error {
		if _, ok := members[ref]; ok {
			return nil
		}
		if ref.Size < 8 || ref.Size > 4<<20 || ref.Rank < 1 || ref.Rank > 6 {
			return errors.New("invalid pack descriptor")
		}
		if err := charge(ref.Size); err != nil {
			return err
		}
		report.Packs++
		_, refs, err := PackChildren(c, packs, ref)
		if err != nil {
			return err
		}
		// PackChildren has verified the full hash, directory bounds and every page.
		// Re-reading here deliberately reuses that verifier instead of adding a second
		// parser. Its duplicated I/O is measured, not a production performance claim.
		raw, err := packs.get(blockformat.Ref{Digest: ref.Digest, Size: ref.Size})
		if err != nil {
			return err
		}
		n := int(binary.BigEndian.Uint32(raw[4:8]))
		var dir directory
		if err = decode(raw[8:8+n], &dir); err != nil {
			return err
		}
		set := map[blockformat.Locator]bool{}
		offset := int64(8 + n)
		locators := make([]blockformat.Locator, 0, len(dir.Pages))
		for _, page := range dir.Pages {
			l := blockformat.Locator{Pack: ref, Page: page, Offset: offset}
			offset += page.Size
			set[l] = true
			locators = append(locators, l)
		}
		members[ref] = set
		for _, r := range refs {
			if err = segment(r); err != nil {
				return err
			}
		}
		for _, l := range locators {
			if l.Page.Kind == blockformat.RootKind {
				shape, e := openPacked(c, packs, l)
				if e != nil {
					return e
				}
				if shape.Index != nil {
					if e = child(*shape.Index, shape, shape.Level, 0); e != nil {
						return e
					}
				}
				continue
			}
			raw, e := pageBytes(c, packs, l)
			if e != nil {
				return e
			}
			var node blockformat.Node
			if e = decode(raw, &node); e != nil {
				return e
			}
			shape := blockformat.Root{Capacity: node.Capacity, Fanout: node.Fanout}
			for span := int64(shape.Fanout); span < shape.Capacity/blockformat.BlockSize; span *= int64(shape.Fanout) {
				shape.Level++
			}
			d := &Disk{shape: rootShape(shape)}
			for _, entry := range node.Entries {
				if entry.Child != nil {
					if e = child(*entry.Child, shape, node.Level-1, node.Start+uint64(entry.Slot)*d.stride(node.Level)); e != nil {
						return e
					}
				}
			}
		}
		return nil
	}
	if err := visit(root.Pack); err != nil {
		return fail, err
	}
	if !members[root.Pack][root] {
		return fail, errors.New("root absent from directory")
	}
	if _, err := openPacked(c, packs, root); err != nil {
		return fail, err
	}
	return report, nil
}
