package blockformat

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"errors"
	"io"
)

// PageInspection describes an authenticated pack member and its immediate
// dependencies. Shape/Level/Start allow the owner to check every incoming node
// reference against the child's actual position, including reused objects.
// Root pages have Level -1. This is byte evidence, not publication authority.
type PageInspection struct {
	Locator  Locator
	Shape    Root
	Level    int
	Start    uint64
	Children []NodeReference
	Segments []Ref
}

type NodeReference struct {
	Locator Locator
	Shape   Root
	Level   int
	Start   uint64
}

// PackInspection covers every physical page, including pages not selected by the
// current root. The caller must verify/retain all dependencies before publication.
// It must not treat a matching object digest alone as proof of child membership
// or geometry. Keys contains only direct ciphertext keys, not transitive keys.
type PackInspection struct {
	Pages []PageInspection
	Keys  []string
}

// inspectionBytes intentionally reads one complete, bounded object for publication
// inspection. This is not a fallback for the demand-read path.
func inspectionBytes(ctx context.Context, source RangeSource, digest [32]byte, size int64) (_ []byte, err error) {
	if err = ctx.Err(); err != nil {
		return nil, err
	}
	if source == nil || size < 8 || size > 4<<20 {
		return nil, errors.New("invalid inspection size")
	}
	body, err := source.GetRange(ctx, "sha256:"+hex.EncodeToString(digest[:]), size, 0, size)
	if err != nil {
		return nil, err
	}
	if body == nil {
		return nil, errors.New("missing inspection response")
	}
	defer func() { err = errors.Join(err, body.Close(), ctx.Err()) }()
	raw, err := io.ReadAll(io.LimitReader(body, size+1))
	if err != nil {
		return nil, err
	}
	if int64(len(raw)) != size || sha256.Sum256(raw) != digest {
		return nil, errors.New("inspection identity mismatch")
	}
	return raw, nil
}

type inspectedBytes struct {
	ref PackRef
	raw []byte
}

func (s inspectedBytes) GetRange(ctx context.Context, digest string, size, offset, length int64) (io.ReadCloser, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if digest != "sha256:"+hex.EncodeToString(s.ref.Digest[:]) || size != int64(len(s.raw)) || offset < 0 || length < 0 || offset > size-length {
		return nil, errors.New("inspection range mismatch")
	}
	return io.NopCloser(bytes.NewReader(s.raw[offset : offset+length])), nil
}

// InspectPack checks the complete pack digest, directory, every encrypted page
// and local tree invariants. It does not fetch children or register certificates.
// The source and scoped keys must remain stable and retained during inspection.
func InspectPack(ctx context.Context, source RangeSource, scope string, keys map[string][]byte, ref PackRef) (PackInspection, error) {
	fail := PackInspection{}
	if ref.Rank < 1 || ref.Rank > 6 {
		return fail, errors.New("invalid pack rank")
	}
	raw, err := inspectionBytes(ctx, source, ref.Digest, ref.Size)
	if err != nil {
		return fail, err
	}
	if string(raw[:4]) != "HGP0" {
		return fail, errors.New("invalid pack framing")
	}
	n := int64(binary.BigEndian.Uint32(raw[4:8]))
	if n > int64(len(raw))-8 {
		return fail, errors.New("invalid pack directory size")
	}
	var dir packDirectory
	if err = decodeTree(raw[8:8+n], &dir); err != nil {
		return fail, err
	}
	if dir.Rank != ref.Rank || len(dir.Pages) == 0 {
		return fail, errors.New("invalid pack directory")
	}
	local := inspectedBytes{ref, raw}
	seen := map[Ref]bool{}
	seenKeys := map[string]bool{}
	result := PackInspection{}
	offset := 8 + n
	for _, page := range dir.Pages {
		if err = ctx.Err(); err != nil {
			return fail, err
		}
		if seen[page] {
			return fail, errors.New("duplicate pack page")
		}
		seen[page] = true
		loc := Locator{Pack: ref, Page: page, Offset: offset}
		plain, e := ReadPage(ctx, local, scope, keys[page.Key], loc)
		if e != nil {
			return fail, e
		}
		offset += page.Size
		inspected := PageInspection{Locator: loc}
		if page.Kind == RootKind {
			shape, e := ReadRoot(ctx, local, scope, keys, loc)
			if e != nil {
				return fail, e
			}
			inspected.Shape, inspected.Level = shape, -1
			if shape.Index != nil {
				inspected.Children = append(inspected.Children, NodeReference{*shape.Index, shape, shape.Level, 0})
			}
		} else {
			var node Node
			if e = decodeTree(plain, &node); e != nil {
				return fail, e
			}
			shape := Root{Capacity: node.Capacity, Fanout: node.Fanout}
			// Check geometry before multiplying to derive depth.
			if shape.Capacity <= 0 || shape.Capacity%BlockSize != 0 || shape.Capacity/BlockSize > MaxBlocks || (shape.Fanout != 64 && shape.Fanout != 256) {
				return fail, errors.New("invalid inspected geometry")
			}
			for span := int64(shape.Fanout); span < shape.Capacity/BlockSize; span *= int64(shape.Fanout) {
				shape.Level++
			}
			node, e = ReadNode(ctx, local, scope, keys, loc, shape, node.Level, node.Start)
			if e != nil {
				return fail, e
			}
			inspected.Shape, inspected.Level, inspected.Start = shape, node.Level, node.Start
			inspected.Segments = node.Segments
			for _, segment := range node.Segments {
				h, e := Header(scope, segment)
				if e != nil || segment.Kind != SegmentKind || segment.Size != int64(len(h))+int64(segment.Count)*(BlockSize+20) {
					return fail, errors.New("invalid inspected segment")
				}
			}
			for _, entry := range node.Entries {
				if entry.Child != nil {
					inspected.Children = append(inspected.Children, NodeReference{*entry.Child, shape, node.Level - 1, node.Start + uint64(entry.Slot)*stride(shape.Fanout, node.Level)})
				}
			}
		}
		result.Pages = append(result.Pages, inspected)
		if !seenKeys[page.Key] {
			seenKeys[page.Key] = true
			result.Keys = append(result.Keys, page.Key)
		}
	}
	if offset != ref.Size {
		return fail, errors.New("unlisted pack bytes")
	}
	if err = ctx.Err(); err != nil {
		return fail, err
	}
	return result, nil
}

// InspectSegment authenticates every record and the complete ciphertext digest
// using one bounded stream and a single block buffer. No plaintext is retained.
func InspectSegment(ctx context.Context, source RangeSource, scope string, key []byte, ref Ref) (err error) {
	if err = ctx.Err(); err != nil {
		return err
	}
	h, err := Header(scope, ref)
	if err != nil {
		return err
	}
	const frame = BlockSize + 20
	if source == nil || len(key) != 32 || ref.Kind != SegmentKind || ref.Size != int64(len(h))+int64(ref.Count)*frame {
		return errors.New("invalid inspected segment")
	}
	body, err := source.GetRange(ctx, "sha256:"+hex.EncodeToString(ref.Digest[:]), ref.Size, 0, ref.Size)
	if err != nil {
		return err
	}
	if body == nil {
		return errors.New("missing inspection response")
	}
	defer func() { err = errors.Join(err, body.Close(), ctx.Err()) }()
	hash := sha256.New()
	stream := io.TeeReader(io.LimitReader(body, ref.Size+1), hash)
	stored := make([]byte, len(h))
	if _, err = io.ReadFull(stream, stored); err != nil {
		return err
	}
	buffer := make([]byte, frame)
	for i := uint32(0); i < ref.Count; i++ {
		if err = ctx.Err(); err != nil {
			return err
		}
		if _, err = io.ReadFull(stream, buffer); err != nil {
			return err
		}
		plain, e := OpenBlock(scope, key, ref, i, stored, buffer)
		clear(plain)
		if e != nil {
			return e
		}
	}
	var extra [1]byte
	if n, e := stream.Read(extra[:]); n != 0 || e != io.EOF {
		return errors.Join(errors.New("segment response length mismatch"), e)
	}
	if !bytes.Equal(hash.Sum(nil), ref.Digest[:]) {
		return errors.New("segment digest mismatch")
	}
	return nil
}

// CheckNode verifies exact directory membership and the position expected by a
// parent. A previously inspected pack may be reused only within the caller's
// retained Computer scope; this check itself supplies no retention authority.
func (p PackInspection) CheckNode(expected NodeReference) error {
	for _, page := range p.Pages {
		if page.Locator == expected.Locator && page.Locator.Page.Kind == NodeKind &&
			page.Shape.Capacity == expected.Shape.Capacity && page.Shape.Fanout == expected.Shape.Fanout && page.Shape.Level == expected.Shape.Level && page.Level == expected.Level && page.Start == expected.Start {
			return nil
		}
	}
	return errors.New("node absent from inspected position")
}

// CheckRoot binds an exact inspected root to the separately admitted capacity.
func (p PackInspection) CheckRoot(root Locator, capacity int64) error {
	for _, page := range p.Pages {
		if page.Locator == root && page.Locator.Page.Kind == RootKind && page.Shape.Capacity == capacity {
			return nil
		}
	}
	return errors.New("root absent from inspected capacity")
}

// ObjectInspection is complete trusted-host byte evidence for one physical
// object. Exactly one member is populated. It grants no publication authority.
type ObjectInspection struct {
	Segment *Ref
	Pack    *PackInspection
}
