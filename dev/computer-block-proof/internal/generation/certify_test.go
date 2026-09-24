package generation

import (
	"bytes"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"slices"
	"strings"
	"testing"
)

func certifyRoot(t *testing.T, c *Codec, packs *Store, shape packedRoot) Locator {
	t.Helper()
	plain, e := json.Marshal(shape)
	if e != nil {
		t.Fatal(e)
	}
	r, b, e := c.seal(rootKind, [][]byte{plain})
	if e != nil {
		t.Fatal(e)
	}
	p, _ := NewPacker(c, nil, packs, 1<<20, true)
	if e = p.publish(shape.Level+2, []stagedPage{{r, r, b}}); e != nil {
		t.Fatal(e)
	}
	return p.converted[r]
}
func TestCertificationClosure(t *testing.T) {
	for _, internal := range []bool{true, false} {
		t.Run(fmt.Sprint(internal), func(t *testing.T) {
			c, data := fixture(t)
			packs := NewStore()
			r, e := NewPacked(c, packs, 32<<30, 256, 1<<20)
			if e != nil {
				t.Fatal(e)
			}
			changes := map[uint64][]byte{}
			for i := uint64(0); i < 1024; i++ {
				changes[i*8192] = bytes.Repeat([]byte{1}, BlockSize)
			}
			r, e = CapturePacked(c, data, packs, r, changes, 1<<20, internal)
			if e != nil {
				t.Fatal(e)
			}
			old := r
			r, e = CapturePacked(c, data, packs, r, map[uint64][]byte{0: bytes.Repeat([]byte{2}, BlockSize)}, 1<<20, internal)
			if e != nil {
				t.Fatal(e)
			}
			beforeD, beforeP := data.Metrics, packs.Metrics
			proof, e := Certify(c, data, packs, r, 10000, 64<<20)
			if e != nil {
				t.Fatal(e)
			}
			read := data.Metrics.ReadBytes - beforeD.ReadBytes + packs.Metrics.ReadBytes - beforeP.ReadBytes
			requests := data.Metrics.Gets - beforeD.Gets + data.Metrics.Ranges - beforeD.Ranges + packs.Metrics.Gets - beforeP.Gets + packs.Metrics.Ranges - beforeP.Ranges
			closure := packedClosure(t, c, packs, r)
			if proof.Bytes != closure.Bytes || proof.Packs != closure.Packs {
				t.Fatal("physical closure mismatch")
			}
			t.Logf("packs=%d segments=%d closure_bytes=%d read_bytes=%d read_calls=%d", proof.Packs, proof.Segments, proof.Bytes, read, requests)
			ds, ps := retainPacked(t, c, data, packs, r, old)
			for _, root := range []Locator{old, r} {
				if _, e = Certify(c, ds, ps, root, 10000, 64<<20); e != nil {
					t.Fatal(e)
				}
			}
			for _, budget := range [][2]int64{{proof.Packs + proof.Segments - 1, proof.Bytes}, {proof.Packs + proof.Segments, proof.Bytes - 1}} {
				out, e := Certify(c, data, packs, r, budget[0], budget[1])
				if e == nil || out != (Certification{}) {
					t.Fatal("budget accepted")
				}
			}
			if _, e = Certify(c, data, packs, r, proof.Packs+proof.Segments, proof.Bytes); e != nil {
				t.Fatal("exact budget", e)
			}
		})
	}
}
func TestCertificationRefusesInvalidEdges(t *testing.T) {
	c, data := fixture(t)
	packs := NewStore()
	r, e := NewPacked(c, packs, 128*BlockSize, 64, 1<<20)
	if e != nil {
		t.Fatal(e)
	}
	r, e = CapturePacked(c, data, packs, r, map[uint64][]byte{0: bytes.Repeat([]byte{7}, BlockSize)}, 1<<20, true)
	if e != nil {
		t.Fatal(e)
	}
	shape, e := openPacked(c, packs, r)
	if e != nil {
		t.Fatal(e)
	}
	original := *shape.Index
	bad := original
	bad.Offset++
	shape.Index = &bad
	forged := certifyRoot(t, c, packs, shape)
	if _, e = Certify(c, data, packs, forged, 100, 8<<20); e == nil || !strings.Contains(e.Error(), "absent from directory") {
		t.Fatal("directory membership", e)
	}
	shape.Index = &original
	shape.Capacity = 192 * BlockSize
	forged = certifyRoot(t, c, packs, shape)
	if _, e = Certify(c, data, packs, forged, 100, 8<<20); e == nil {
		t.Fatal("cross-geometry splice")
	}
	// A missing transitive pack cannot pass just because the root is available.
	saved := packs.objects[original.Pack.Digest]
	delete(packs.objects, original.Pack.Digest)
	if out, e := Certify(c, data, packs, r, 100, 8<<20); e == nil || out != (Certification{}) {
		t.Fatal("missing pack")
	}
	packs.objects[original.Pack.Digest] = saved
	for digest, raw := range data.objects {
		raw[0] ^= 1
		if _, e = Certify(c, data, packs, r, 100, 8<<20); e == nil {
			t.Fatal("corrupt segment")
		}
		raw[0] ^= 1
		delete(data.objects, digest)
		if _, e = Certify(c, data, packs, r, 100, 8<<20); e == nil {
			t.Fatal("missing segment")
		}
		data.objects[digest] = raw
		break
	}
	beforeD, beforeP := data.Metrics, packs.Metrics
	for _, root := range []Locator{{Pack: PackRef{Size: -1}}, {Pack: PackRef{Size: 5 << 20, Rank: 1}}} {
		if _, e = Certify(c, data, packs, root, 100, 8<<20); e == nil {
			t.Fatal("descriptor accepted")
		}
	}
	if data.Metrics != beforeD || packs.Metrics != beforeP {
		t.Fatal("invalid descriptor fetched")
	}
	if _, e = Certify(c, data, packs, r, 100, 8<<20); e != nil {
		t.Fatal("restored fixture", e)
	}
}
func TestCertificationChecksObsoletePages(t *testing.T) {
	c, data := fixture(t)
	packs := NewStore()
	p, _ := NewPacker(c, nil, packs, 1<<20, true)
	var pages []stagedPage
	var refs []Ref
	var segments []Ref
	oldKey := c.ActiveKey
	for i := 0; i < 2; i++ {
		if i == 1 {
			c.ActiveKey = "rotated"
			c.Keys["rotated"] = bytes.Repeat([]byte{0x71}, 32)
		}
		seg, raw, e := c.seal(segmentKind, [][]byte{bytes.Repeat([]byte{byte(i + 1)}, BlockSize)})
		if e != nil {
			t.Fatal(e)
		}
		if e = data.put(seg, raw); e != nil {
			t.Fatal(e)
		}
		segments = append(segments, seg)
		n := packedNode{Capacity: 64 * BlockSize, Fanout: 64, Segments: []Ref{seg}, Entries: []packedEntry{{Slot: 0}}}
		plain, e := json.Marshal(n)
		if e != nil {
			t.Fatal(e)
		}
		ref, b, e := c.seal(nodeKind, [][]byte{plain})
		if e != nil {
			t.Fatal(e)
		}
		refs = append(refs, ref)
		pages = append(pages, stagedPage{ref, ref, b})
	}
	if e := p.publish(1, pages); e != nil {
		t.Fatal(e)
	}
	loc := p.converted[refs[0]]
	root := certifyRoot(t, c, packs, packedRoot{Capacity: 64 * BlockSize, Fanout: 64, Index: &loc})
	if proof, e := Certify(c, data, packs, root, 100, 8<<20); e != nil || proof.Segments != 2 {
		t.Fatal("physical dependencies", e)
	}

	inspected, err := Inspect(c, data, packs, root, 100, 8<<20)
	if err != nil {
		t.Fatal(err)
	}
	edges := map[[32]byte]bool{}
	for _, o := range inspected.Objects {
		if o.Digest == loc.Pack.Digest {
			expected := []string{oldKey, "rotated"}
			slices.Sort(expected)
			if !slices.Equal(o.Keys, expected) {
				t.Fatalf("mixed pack lost key dependencies: %v", o.Keys)
			}
			for _, d := range o.Children {
				edges[d] = true
			}
		}
	}
	if inspected.Root != root || inspected.Capacity != 64*BlockSize || !edges[segments[0].Digest] || !edges[segments[1].Digest] {
		t.Fatal("inspection omitted obsolete physical dependency")
	}
	delete(data.objects, segments[1].Digest)
	if got, e := ReadPacked(c, data, packs, root, 0); e != nil || got[0] != 1 {
		t.Fatal("selected tree unexpectedly broken", e)
	}
	if _, e := Certify(c, data, packs, root, 100, 8<<20); e == nil {
		t.Fatal("obsolete dependency ignored")
	}
	if out, err := Inspect(c, data, packs, root, 100, 8<<20); err == nil || len(out.Objects) != 0 || out.Root != (Locator{}) {
		t.Fatal("failed inspection exposed a partial graph")
	}

}

func TestCertificationAuthenticatesUnusedRecord(t *testing.T) {
	c, data := fixture(t)
	packs := NewStore()
	seg, raw, e := c.seal(segmentKind, [][]byte{bytes.Repeat([]byte{1}, BlockSize), bytes.Repeat([]byte{2}, BlockSize)})
	if e != nil {
		t.Fatal(e)
	}
	raw[len(raw)-1] ^= 1
	seg.Digest = sha256.Sum256(raw)
	if e = data.put(seg, raw); e != nil {
		t.Fatal(e)
	}
	n := packedNode{Capacity: 64 * BlockSize, Fanout: 64, Segments: []Ref{seg}, Entries: []packedEntry{{Slot: 0}}}
	plain, e := json.Marshal(n)
	if e != nil {
		t.Fatal(e)
	}
	ref, b, e := c.seal(nodeKind, [][]byte{plain})
	if e != nil {
		t.Fatal(e)
	}
	p, _ := NewPacker(c, nil, packs, 1<<20, true)
	if e = p.publish(1, []stagedPage{{ref, ref, b}}); e != nil {
		t.Fatal(e)
	}
	loc := p.converted[ref]
	root := certifyRoot(t, c, packs, packedRoot{Capacity: 64 * BlockSize, Fanout: 64, Index: &loc})
	if got, e := ReadPacked(c, data, packs, root, 0); e != nil || got[0] != 1 {
		t.Fatal("selected record", e)
	}
	if _, e = Certify(c, data, packs, root, 100, 8<<20); e == nil || !strings.Contains(e.Error(), "authentication") {
		t.Fatal("unused record was not authenticated", e)
	}
}
