package generation

import (
	"bytes"
	"encoding/binary"
	"errors"
	"sort"
)

// InspectedObject describes one fully verified physical object and its immediate
// ciphertext dependencies. It is inspection data, not SQL/publication authority.
type InspectedObject struct {
	Digest   [32]byte
	Size     int64
	Kind     string
	Rank     int
	Children [][32]byte
}

// Inspection binds a selected root and its geometry to the entire physical graph.
// Objects are ordered children first. No result escapes on failed verification.
type Inspection struct {
	Root     Locator
	Capacity int64
	Objects  []InspectedObject
}

// Inspect reuses Certify's complete authentication/geometry checks, then derives
// object edges from verified bytes. Stores and keys must remain single-owner and
// immutable throughout both passes. The duplicate reads are intentional for this
// development proof; this is not an incremental production certifier.
func Inspect(c *Codec, data, packs *Store, root Locator, maxObjects, maxBytes int64) (Inspection, error) {
	if _, err := Certify(c, data, packs, root, maxObjects, maxBytes); err != nil {
		return Inspection{}, err
	}
	shape, err := openPacked(c, packs, root)
	if err != nil {
		return Inspection{}, err
	}
	out := Inspection{Root: root, Capacity: shape.Capacity}
	objects := map[[32]byte]InspectedObject{}
	add := func(o InspectedObject) error {
		if old, ok := objects[o.Digest]; ok && (old.Size != o.Size || old.Kind != o.Kind || old.Rank != o.Rank) {
			return errors.New("conflicting physical descriptor")
		}
		objects[o.Digest] = o
		return nil
	}
	seen := map[PackRef]bool{}
	var visit func(PackRef) error
	visit = func(p PackRef) error {
		if seen[p] {
			return nil
		}
		seen[p] = true
		children, segments, err := PackChildren(c, packs, p)
		if err != nil {
			return err
		}
		raw, err := packs.get(Ref{Digest: p.Digest, Size: p.Size})
		if err != nil {
			return err
		}
		// The preceding verifier checked directory bounds and authenticated every page.
		n := int(binary.BigEndian.Uint32(raw[4:8]))
		var dir directory
		if err = decode(raw[8:8+n], &dir); err != nil {
			return err
		}
		o := InspectedObject{Digest: p.Digest, Size: p.Size, Kind: "index", Rank: p.Rank}
		for _, page := range dir.Pages {
			if page.Kind == rootKind {
				o.Kind = "root"
			}
		}
		edges := map[[32]byte]bool{}
		for _, s := range segments {
			if err = add(InspectedObject{Digest: s.Digest, Size: s.Size, Kind: "segment"}); err != nil {
				return err
			}
			edges[s.Digest] = true
		}
		for _, child := range children {
			if err = visit(child); err != nil {
				return err
			}
			edges[child.Digest] = true
		}
		for digest := range edges {
			o.Children = append(o.Children, digest)
		}
		sort.Slice(o.Children, func(i, j int) bool { return bytes.Compare(o.Children[i][:], o.Children[j][:]) < 0 })
		return add(o)
	}
	if err = visit(root.Pack); err != nil {
		return Inspection{}, err
	}
	for _, o := range objects {
		out.Objects = append(out.Objects, o)
	}
	sort.Slice(out.Objects, func(i, j int) bool {
		a, b := out.Objects[i], out.Objects[j]
		if a.Rank != b.Rank {
			return a.Rank < b.Rank
		}
		return bytes.Compare(a.Digest[:], b.Digest[:]) < 0
	})
	return out, nil
}
