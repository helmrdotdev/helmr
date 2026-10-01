package disk

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"io"
	"testing"
	"uuid"

	"github.com/helmrdotdev/helmr/internal/cas"
	"github.com/helmrdotdev/helmr/internal/disk/blockformat"
)

type countedVersionSource struct {
	*cas.File
	calls int
}

func (s *countedVersionSource) GetRange(ctx context.Context, digest string, size, offset, length int64) (io.ReadCloser, error) {
	s.calls++
	return s.File.GetRange(ctx, digest, size, offset, length)
}

type versionFixture struct {
	t      *testing.T
	source *countedVersionSource
	key    []byte
	keyID  string
	keys   map[string][]byte
}

func newVersionFixture(t *testing.T) *versionFixture {
	t.Helper()
	store, err := cas.NewFile(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	key := bytes.Repeat([]byte{7}, 32)
	id := uuid.NewV7().String()
	return &versionFixture{t, &countedVersionSource{File: store}, key, id, map[string][]byte{id: key}}
}
func (f *versionFixture) seal(kind byte, records [][]byte) (blockformat.Ref, []byte) {
	f.t.Helper()
	ref := blockformat.Ref{Key: f.keyID, Kind: kind, Count: uint32(len(records))}
	if _, err := rand.Read(ref.Salt[:]); err != nil {
		f.t.Fatal(err)
	}
	ref, raw, err := blockformat.Seal("scope", f.key, ref, records)
	if err != nil {
		f.t.Fatal(err)
	}
	return ref, raw
}
func (f *versionFixture) page(kind byte, rank int, value any) blockformat.Locator {
	f.t.Helper()
	raw, err := json.Marshal(value)
	if err != nil {
		f.t.Fatal(err)
	}
	page, sealed := f.seal(kind, [][]byte{raw})
	// Read-path fixture: page authentication and offset are under test here;
	// complete pack-directory certification belongs to publication tests.
	pack := append(make([]byte, 64), sealed...)
	if _, err = f.source.Put(f.t.Context(), "application/octet-stream", bytes.NewReader(pack)); err != nil {
		f.t.Fatal(err)
	}
	return blockformat.Locator{Pack: blockformat.PackRef{Digest: sha256.Sum256(pack), Size: int64(len(pack)), Rank: rank}, Page: page, Offset: 64}
}
func (f *versionFixture) segment() blockformat.Ref {
	f.t.Helper()
	ref, raw := f.seal(blockformat.SegmentKind, [][]byte{bytes.Repeat([]byte{9}, 4096)})
	if _, err := f.source.Put(f.t.Context(), "application/octet-stream", bytes.NewReader(raw)); err != nil {
		f.t.Fatal(err)
	}
	return ref
}
func (f *versionFixture) root(shape blockformat.Root) VersionRoot {
	f.t.Helper()
	locator := f.page(blockformat.RootKind, shape.Level+2, shape)
	root, err := NewVersionRoot(locator, shape.Capacity)
	if err != nil {
		f.t.Fatal(err)
	}
	return root
}
func TestVersionTreeFileReads(t *testing.T) {
	f := newVersionFixture(t)
	const capacity = 128 * 4096
	segment := f.segment()
	leaf := f.page(blockformat.NodeKind, 1, blockformat.Node{Capacity: capacity, Fanout: 64, Level: 0, Start: 64, Segments: []blockformat.Ref{segment}, Entries: []blockformat.Entry{{Slot: 3}}})
	index := f.page(blockformat.NodeKind, 2, blockformat.Node{Capacity: capacity, Fanout: 64, Level: 1, Entries: []blockformat.Entry{{Slot: 1, Child: &leaf}}})
	root := f.root(blockformat.Root{Capacity: capacity, Fanout: 64, Level: 1, Index: &index})
	tree, err := OpenVersion(t.Context(), f.source, "scope", f.keys, root, capacity)
	if err != nil {
		t.Fatal(err)
	}
	got, err := tree.ReadBlock(t.Context(), 67)
	if err != nil || !bytes.Equal(got, bytes.Repeat([]byte{9}, 4096)) {
		t.Fatalf("tree read: %v", err)
	}
	if f.source.calls != 5 {
		t.Fatalf("read more than root, path and one block: %d", f.source.calls)
	}
	for _, block := range []uint64{0, 68} {
		got, err := tree.ReadBlock(t.Context(), block)
		if err != nil || !bytes.Equal(got, make([]byte, 4096)) {
			t.Fatalf("sparse read %d: %v", block, err)
		}
	}
	before := f.source.calls
	if _, err = tree.ReadBlock(t.Context(), 128); err == nil || f.source.calls != before {
		t.Fatal("out of bounds performed IO")
	}
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	if _, err = tree.ReadBlock(ctx, 0); err == nil || f.source.calls != before {
		t.Fatal("cancelled read performed IO")
	}
	// Correctly framed admission capacity cannot override authenticated capacity.
	changed := root
	changed.LogicalBytes = 2 * capacity
	if _, err = OpenVersion(t.Context(), f.source, "scope", f.keys, changed, 2*capacity); err == nil {
		t.Fatal("authenticated capacity mismatch accepted")
	}
	// An authenticated pointer to a missing object must never become a sparse hole.
	if err = f.source.Delete(t.Context(), "sha256:"+hex.EncodeToString(leaf.Pack.Digest[:])); err != nil {
		t.Fatal(err)
	}
	if got, err = tree.ReadBlock(t.Context(), 67); err == nil || got != nil {
		t.Fatal("missing child became data")
	}
}
func TestVersionTreeRejectsAuthenticatedMalformedNodes(t *testing.T) {
	for _, name := range []string{"capacity", "fanout", "level", "position", "negative slot", "duplicate slot", "outside slot", "segment index", "record", "child in leaf", "unused segment", "missing segment", "missing key"} {
		t.Run(name, func(t *testing.T) {
			f := newVersionFixture(t)
			const capacity = 64 * 4096
			segment := f.segment()
			n := blockformat.Node{Capacity: capacity, Fanout: 64, Segments: []blockformat.Ref{segment}, Entries: []blockformat.Entry{{Slot: 0}}}
			switch name {
			case "capacity":
				n.Capacity++
			case "fanout":
				n.Fanout = 256
			case "level":
				n.Level = 1
			case "position":
				n.Start = 64
			case "negative slot":
				n.Entries[0].Slot = -1
			case "duplicate slot":
				n.Entries = append(n.Entries, n.Entries[0])
			case "outside slot":
				n.Entries[0].Slot = 64
			case "segment index":
				n.Entries[0].Segment = 1
			case "record":
				n.Entries[0].Record = 1
			case "child in leaf":
				n.Entries[0].Child = &blockformat.Locator{}
			case "unused segment":
				n.Entries = nil
			}
			index := f.page(blockformat.NodeKind, 1, n)
			root := f.root(blockformat.Root{Capacity: capacity, Fanout: 64, Index: &index})
			tree, err := OpenVersion(t.Context(), f.source, "scope", f.keys, root, capacity)
			if err != nil {
				t.Fatal(err)
			}
			if name == "missing segment" {
				if err = f.source.Delete(t.Context(), "sha256:"+hex.EncodeToString(segment.Digest[:])); err != nil {
					t.Fatal(err)
				}
			}
			if name == "missing key" {
				delete(f.keys, f.keyID)
			}
			if got, err := tree.ReadBlock(t.Context(), 0); err == nil || got != nil {
				t.Fatal("malformed tree returned data")
			}
		})
	}
}

func TestVersionTreeRejectsMalformedRoots(t *testing.T) {
	for _, name := range []string{"zero capacity", "oversize", "unaligned", "fanout", "negative level", "excessive level", "rank", "index rank", "index kind", "unknown field", "wrong JSON type"} {
		t.Run(name, func(t *testing.T) {
			f := newVersionFixture(t)
			shape := blockformat.Root{Capacity: 64 * 4096, Fanout: 64}
			rank := 2
			switch name {
			case "zero capacity":
				shape.Capacity = 0
			case "oversize":
				shape.Capacity = (blockformat.MaxBlocks + 1) * 4096
			case "unaligned":
				shape.Capacity++
			case "fanout":
				shape.Fanout = 1
			case "negative level":
				shape.Level = -1
			case "excessive level":
				shape.Level = 1000000
			case "rank":
				rank = 3
			case "index rank":
				shape.Index = &blockformat.Locator{Pack: blockformat.PackRef{Rank: 2}, Page: blockformat.Ref{Kind: blockformat.NodeKind}}
			case "index kind":
				shape.Index = &blockformat.Locator{Pack: blockformat.PackRef{Rank: 1}, Page: blockformat.Ref{Kind: blockformat.RootKind}}
			}
			var value any = shape
			if name == "unknown field" {
				value = map[string]any{"Capacity": shape.Capacity, "Fanout": 64, "Level": 0, "Unexpected": 1}
			}
			if name == "wrong JSON type" {
				raw, _ := json.Marshal(shape)
				// JSON strings are valid metadata JSON but not a root object.
				value = string(append(raw, raw...))
			}
			loc := f.page(blockformat.RootKind, rank, value)
			if tree, err := blockformat.OpenTree(t.Context(), f.source, "scope", f.keys, loc); err == nil || tree != nil {
				t.Fatal("invalid authenticated root accepted")
			}
		})
	}
}

func TestVersionRangeReads(t *testing.T) {
	f := newVersionFixture(t)
	const capacity = 128 * 4096
	segment := f.segment()
	left := f.page(blockformat.NodeKind, 1, blockformat.Node{Capacity: capacity, Fanout: 64, Segments: []blockformat.Ref{segment}, Entries: []blockformat.Entry{{Slot: 63}}})
	right := f.page(blockformat.NodeKind, 1, blockformat.Node{Capacity: capacity, Fanout: 64, Start: 64, Segments: []blockformat.Ref{segment}, Entries: []blockformat.Entry{{Slot: 1}}})
	index := f.page(blockformat.NodeKind, 2, blockformat.Node{Capacity: capacity, Fanout: 64, Level: 1, Entries: []blockformat.Entry{{Slot: 0, Child: &left}, {Slot: 1, Child: &right}}})
	root := f.root(blockformat.Root{Capacity: capacity, Fanout: 64, Level: 1, Index: &index})
	tree, err := OpenVersion(t.Context(), f.source, "scope", f.keys, root, capacity)
	if err != nil {
		t.Fatal(err)
	}
	before := f.source.calls
	got, err := tree.ReadRange(t.Context(), 63*4096-2, 8196)
	want := make([]byte, 8196)
	copy(want[2:], bytes.Repeat([]byte{9}, 4096))
	copy(want[8194:], []byte{9, 9})
	if err != nil || !bytes.Equal(got, want) {
		t.Fatalf("unaligned cross-node range: %v", err)
	}
	// One index, two leaves and two (header, frame) pairs. No metadata path
	// reread for adjacent blocks, including the absent block between data.
	if calls := f.source.calls - before; calls != 7 {
		t.Fatalf("range read repeated paths: %d reads", calls)
	}
	before = f.source.calls
	for _, request := range []struct {
		offset int64
		length int
	}{
		{-1, 1}, {capacity, 1}, {capacity - 1, 2}, {0, -1}, {0, blockformat.MaxReadBytes + 1}, {1<<63 - 1, 1},
	} {
		if data, err := tree.ReadRange(t.Context(), request.offset, request.length); err == nil || data != nil {
			t.Fatalf("invalid read accepted: %+v", request)
		}
	}
	if data, err := tree.ReadRange(t.Context(), capacity, 0); err != nil || len(data) != 0 {
		t.Fatalf("empty boundary read: %v", err)
	}
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	if data, err := tree.ReadRange(ctx, 0, 4096); err == nil || data != nil {
		t.Fatal("cancelled range returned data")
	}
	if f.source.calls != before {
		t.Fatal("invalid, empty or cancelled read performed I/O")
	}
	// A missing branch outside the request is irrelevant. Crossing into it
	// must fail without returning the successfully read prefix as a valid range.
	if err := f.source.Delete(t.Context(), "sha256:"+hex.EncodeToString(right.Pack.Digest[:])); err != nil {
		t.Fatal(err)
	}
	if data, err := tree.ReadRange(t.Context(), 63*4096, 4096); err != nil || !bytes.Equal(data, bytes.Repeat([]byte{9}, 4096)) {
		t.Fatalf("unrequested missing branch affected read: %v", err)
	}
	if data, err := tree.ReadRange(t.Context(), 63*4096, 3*4096); err == nil || data != nil {
		t.Fatal("missing later branch returned partial data or a hole")
	}
}

func TestVersionRangeBatchesConsecutiveFrames(t *testing.T) {
	for _, fragmented := range []bool{false, true} {
		t.Run(map[bool]string{false: "contiguous", true: "holes and segments"}[fragmented], func(t *testing.T) {
			f := newVersionFixture(t)
			const capacity = 64 * blockformat.BlockSize
			records := make([][]byte, 66)
			for i := range records {
				records[i] = bytes.Repeat([]byte{byte(i + 1)}, blockformat.BlockSize)
			}
			segment, raw := f.seal(blockformat.SegmentKind, records)
			if _, err := f.source.Put(t.Context(), "application/octet-stream", bytes.NewReader(raw)); err != nil {
				t.Fatal(err)
			}
			segments := []blockformat.Ref{segment}
			entries := make([]blockformat.Entry, 64)
			for i := range entries {
				entries[i] = blockformat.Entry{Slot: i, Record: uint32(i + 1)}
			}
			expectedCalls := 3 // Leaf plus one header and one frame range.
			if fragmented {
				segments = append(segments, f.segment())
				entries = []blockformat.Entry{{Slot: 0, Record: 2}, {Slot: 1, Record: 3}, {Slot: 3, Record: 4}, {Slot: 4, Record: 8}, {Slot: 5, Segment: 1}, {Slot: 6, Record: 5}, {Slot: 7, Record: 6}}
				expectedCalls = 11
			}
			want := make([]byte, capacity)
			for _, entry := range entries {
				data := records[entry.Record]
				if entry.Segment == 1 {
					data = bytes.Repeat([]byte{9}, blockformat.BlockSize)
				}
				copy(want[entry.Slot*blockformat.BlockSize:], data)
			}
			leaf := f.page(blockformat.NodeKind, 1, blockformat.Node{Capacity: capacity, Fanout: 64, Segments: segments, Entries: entries})
			tree, err := OpenVersion(t.Context(), f.source, "scope", f.keys, f.root(blockformat.Root{Capacity: capacity, Fanout: 64, Index: &leaf}), capacity)
			if err != nil {
				t.Fatal(err)
			}
			before := f.source.calls
			got, err := tree.ReadRange(t.Context(), 7, capacity-14)
			if err != nil || !bytes.Equal(got, want[7:capacity-7]) {
				t.Fatalf("unaligned batch: %v", err)
			}
			if calls := f.source.calls - before; calls != expectedCalls {
				t.Fatalf("range used %d requests, want %d", calls, expectedCalls)
			}
		})
	}
}
