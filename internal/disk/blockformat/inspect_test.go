package blockformat

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/binary"
	"encoding/json"
	"errors"
	"testing"
)

func inspectPackFixture(t *testing.T, rank int, nodes ...any) (PackRef, []byte, map[string][]byte) {
	t.Helper()
	var pages []stagedPage
	var key []byte
	for _, node := range nodes {
		kind := NodeKind
		if _, ok := node.(Root); ok {
			kind = RootKind
		}
		plain, err := json.Marshal(node)
		if err != nil {
			t.Fatal(err)
		}
		ref, raw, k := recordsFixture(t, kind, [][]byte{plain})
		key = k
		pages = append(pages, stagedPage{ref, raw})
	}
	raw, _, err := encodePack(rank, pages)
	if err != nil {
		t.Fatal(err)
	}
	return PackRef{Digest: sha256.Sum256(raw), Size: int64(len(raw)), Rank: rank}, raw, map[string][]byte{"key-version": key}
}

func TestInspectPackPhysicalMembers(t *testing.T) {
	// Both pages must be inspected even if a root selects only the first one.
	ref, raw, keys := inspectPackFixture(t, 1, Node{Capacity: 1 << 20, Fanout: 64, Level: 0, Start: 0}, Node{Capacity: 1 << 20, Fanout: 64, Level: 0, Start: 64})
	source := &rangeReply{raw: raw}
	got, err := InspectPack(t.Context(), source, "scope", keys, ref)
	if err != nil || len(got.Pages) != 2 || len(got.Keys) != 1 {
		t.Fatalf("inspection: %+v %v", got, err)
	}
	if source.calls != 1 || source.closed != 1 || source.requested != ref.Size {
		t.Fatalf("unexpected source reads: %+v", source)
	}
	first := got.Pages[0]
	link := NodeReference{first.Locator, first.Shape, first.Level, first.Start}
	if err = got.CheckNode(link); err != nil {
		t.Fatal(err)
	}
	for _, mutate := range []func(*NodeReference){func(l *NodeReference) { l.Start++ }, func(l *NodeReference) { l.Level++ }, func(l *NodeReference) { l.Shape.Capacity *= 2 }, func(l *NodeReference) { l.Locator.Offset++ }} {
		wrong := link
		mutate(&wrong)
		if got.CheckNode(wrong) == nil {
			t.Fatal("wrong child accepted")
		}
	}
	rootRef, rootRaw, rootKeys := inspectPackFixture(t, 3, Root{Capacity: 1 << 20, Fanout: 64, Level: 1})
	root, err := InspectPack(t.Context(), &rangeReply{raw: rootRaw}, "scope", rootKeys, rootRef)
	if err != nil {
		t.Fatal(err)
	}
	if root.CheckRoot(root.Pages[0].Locator, 1<<20) != nil || root.CheckRoot(root.Pages[0].Locator, 2<<20) == nil {
		t.Fatal("root admission mismatch")
	}
}

func TestInspectPackRejectsMalformedContent(t *testing.T) {
	for _, tc := range []struct {
		name  string
		nodes []any
		rank  int
	}{
		{"duplicate", []any{Root{Capacity: 4096, Fanout: 64}, Root{Capacity: 4096, Fanout: 64}}, 2},
		{"hidden malformed page", []any{Node{Capacity: 1 << 20, Fanout: 64}, Node{Capacity: 1 << 20, Fanout: 7}}, 1},
		{"bad rank", []any{Root{Capacity: 4096, Fanout: 64}}, 1},
		{"invalid start", []any{Node{Capacity: 1 << 20, Fanout: 64, Start: 1}}, 1},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ref, raw, keys := inspectPackFixture(t, tc.rank, tc.nodes...)
			got, err := InspectPack(t.Context(), &rangeReply{raw: raw}, "scope", keys, ref)
			if err == nil || len(got.Pages) != 0 {
				t.Fatal("malformed pack accepted")
			}
		})
	}
	ref, raw, keys := inspectPackFixture(t, 2, Root{Capacity: 4096, Fanout: 64})
	for _, name := range []string{"digest", "page authentication", "directory bounds", "trailing bytes", "short", "long", "close", "cancel", "wrong key", "wrong scope"} {
		t.Run(name, func(t *testing.T) {
			r := ref
			b := bytes.Clone(raw)
			scope := "scope"
			k := keys
			source := &rangeReply{}
			ctx, cancel := context.WithCancel(t.Context())
			defer cancel()
			switch name {
			case "digest":
				b[len(b)-1] ^= 1
			case "page authentication":
				b[len(b)-1] ^= 1
				r.Digest = sha256.Sum256(b)
			case "directory bounds":
				binary.BigEndian.PutUint32(b[4:8], ^uint32(0))
				r.Digest = sha256.Sum256(b)
			case "trailing bytes":
				b = append(b, 0)
				r.Size = int64(len(b))
				r.Digest = sha256.Sum256(b)
			case "short":
				source.mutate = func(_ int, b []byte) []byte { return b[:len(b)-1] }
			case "long":
				source.mutate = func(_ int, b []byte) []byte { return append(b, 0) }
			case "close":
				source.closeErr = errors.New("close failed")
			case "cancel":
				source.cancel = cancel
			case "wrong key":
				k = map[string][]byte{"key-version": bytes.Repeat([]byte{1}, 32)}
			case "wrong scope":
				scope = "other"
			}
			source.raw = b
			got, err := InspectPack(ctx, source, scope, k, r)
			if err == nil || len(got.Pages) != 0 {
				t.Fatal("invalid pack accepted")
			}
			if source.closed != source.calls {
				t.Fatal("response leaked")
			}
		})
	}
}

func TestInspectSegmentCompleteStream(t *testing.T) {
	records := make([][]byte, MaxRecords)
	for i := range records {
		records[i] = bytes.Repeat([]byte{byte(i)}, BlockSize)
	}
	ref, raw, key := recordsFixture(t, SegmentKind, records)
	source := &rangeReply{raw: raw}
	if err := InspectSegment(t.Context(), source, "scope", key, ref); err != nil {
		t.Fatal(err)
	}
	if source.calls != 1 || source.closed != 1 || source.requested != ref.Size {
		t.Fatalf("stream count: %+v", source)
	}
	for _, name := range []string{"last record", "digest", "short", "long", "close", "cancel"} {
		t.Run(name, func(t *testing.T) {
			r := ref
			source := &rangeReply{raw: raw}
			ctx, cancel := context.WithCancel(t.Context())
			defer cancel()
			switch name {
			case "last record":
				source.mutate = func(_ int, b []byte) []byte { b[len(b)-1] ^= 1; return b }
			case "digest":
				r.Digest[0] ^= 1
			case "short":
				source.mutate = func(_ int, b []byte) []byte { return b[:len(b)-1] }
			case "long":
				source.mutate = func(_ int, b []byte) []byte { return append(b, 0) }
			case "close":
				source.closeErr = errors.New("close failure")
			case "cancel":
				source.cancel = cancel
			}
			if err := InspectSegment(ctx, source, "scope", key, r); err == nil {
				t.Fatal("invalid segment accepted")
			}
			if source.closed != 1 {
				t.Fatal("response leaked")
			}
		})
	}
}
