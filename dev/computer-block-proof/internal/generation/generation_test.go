package generation

import (
	"bytes"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"math/rand"
	"testing"

	"github.com/helmrdotdev/helmr/internal/computer/blockformat"
)

func fixture(t *testing.T) (*Codec, *Store) {
	t.Helper()
	c, err := NewCodec("org/environment", "k1", map[string][]byte{"k1": bytes.Repeat([]byte{7}, 32), "k2": bytes.Repeat([]byte{9}, 32)})
	if err != nil {
		t.Fatal(err)
	}
	return c, NewStore()
}
func mustDisk(t *testing.T, c *Codec, s *Store, size int64, f int) *Disk {
	t.Helper()
	d, e := New(c, s, size, f)
	if e != nil {
		t.Fatal(e)
	}
	return d
}
func mustCapture(t *testing.T, d *Disk) blockformat.Ref {
	t.Helper()
	r, e := d.Capture()
	if e != nil {
		t.Fatal(e)
	}
	return r
}
func TestOracleSnapshotsAndBranches(t *testing.T) {
	for _, fanout := range []int{64, 256} {
		t.Run(fmt.Sprint(fanout), func(t *testing.T) {
			c, s := fixture(t)
			d := mustDisk(t, c, s, 2<<20, fanout)
			oracle := make([]byte, 2<<20)
			rng := rand.New(rand.NewSource(37))
			var roots []blockformat.Ref
			var states [][]byte
			for step := 0; step < 90; step++ {
				n := rng.Intn(9000) + 1
				off := rng.Intn(len(oracle) - n)
				p := make([]byte, n)
				if step%7 != 0 {
					rng.Read(p)
				}
				if e := d.WriteAt(p, int64(off)); e != nil {
					t.Fatal(e)
				}
				copy(oracle[off:], p)
				if step%15 == 0 {
					roots = append(roots, mustCapture(t, d))
					states = append(states, bytes.Clone(oracle))
				}
			}
			roots = append(roots, mustCapture(t, d))
			states = append(states, bytes.Clone(oracle))
			for i, r := range roots {
				clone, e := Open(c, s, r)
				if e != nil {
					t.Fatal(e)
				}
				got := make([]byte, len(oracle))
				if e = clone.ReadAt(got, 0); e != nil {
					t.Fatal(e)
				}
				if !bytes.Equal(got, states[i]) {
					t.Fatalf("root %d changed", i)
				}
			}
			clone, e := Open(c, s, roots[0])
			if e != nil {
				t.Fatal(e)
			}
			if e = clone.WriteAt([]byte("branch"), 100); e != nil {
				t.Fatal(e)
			}
			branch := mustCapture(t, clone)
			if branch == roots[0] {
				t.Fatal("branch unchanged")
			}
			old, _ := Open(c, s, roots[0])
			got := make([]byte, 256)
			if e = old.ReadAt(got, 0); e != nil || !bytes.Equal(got, states[0][:256]) {
				t.Fatal("branch mutated source", e)
			}
			if r := mustCapture(t, d); r != roots[len(roots)-1] {
				t.Fatal("empty capture did not reuse exact ciphertext")
			}
		})
	}
}
func TestAuthenticatedRanges(t *testing.T) {
	c, s := fixture(t)
	p := bytes.Repeat([]byte{0xab}, BlockSize)
	r, b, e := c.seal(segmentKind, [][]byte{p, bytes.Repeat([]byte{0xcd}, BlockSize)})
	if e != nil {
		t.Fatal(e)
	}
	if e = s.put(r, b); e != nil {
		t.Fatal(e)
	}
	got, e := c.block(s, r, 1)
	if e != nil || got[0] != 0xcd {
		t.Fatal(e)
	}
	h, _ := c.header(r)
	frame := BlockSize + 20
	cases := map[string]func([]byte) []byte{
		"header":  func(x []byte) []byte { x[0] ^= 1; return x },
		"record":  func(x []byte) []byte { x[len(h)+4] ^= 1; return x },
		"ordinal": func(x []byte) []byte { copy(x[len(h):len(h)+frame], b[len(h)+frame:]); return x },
		"short":   func(x []byte) []byte { return x[:len(x)-1] },
		"long":    func(x []byte) []byte { return append(x, 0) },
	}
	for name, mutate := range cases {
		t.Run(name, func(t *testing.T) {
			s.objects[r.Digest] = mutate(bytes.Clone(b))
			if _, e = c.block(s, r, 0); e == nil {
				t.Fatal("corruption accepted")
			}
		})
	}
	s.objects[r.Digest] = b
	other, _ := NewCodec("foreign/environment", "k1", c.Keys)
	if _, e = other.block(s, r, 0); e == nil {
		t.Fatal("scope substitution")
	}
	other.ActiveKey = "k2"
	rr, bb, _ := other.seal(segmentKind, [][]byte{p, p})
	s.objects[r.Digest] = bb
	if _, e = c.block(s, r, 0); e == nil {
		t.Fatal("valid segment substitution", rr)
	}
	s.objects[r.Digest] = b
	wrong := r
	wrong.Count++
	if _, e = c.block(s, wrong, 0); e == nil {
		t.Fatal("count accepted")
	}
	wrong = r
	wrong.Key = "missing"
	if _, e = c.block(s, wrong, 0); e == nil {
		t.Fatal("key version accepted")
	}
	if e = s.put(r, b); e != nil || s.Metrics.Objects != 1 {
		t.Fatal("retry changed identity", e)
	}
	c.ActiveKey = "k2"
	r2, b2, e := c.seal(segmentKind, [][]byte{p})
	if e != nil {
		t.Fatal(e)
	}
	s.put(r2, b2)
	if _, e = c.block(s, r, 0); e != nil {
		t.Fatal("old retained key unreadable", e)
	}
	if r2.Salt == r.Salt {
		t.Fatal("object identity reused")
	}
}
func TestFailedReadsAndWritesAreAtomic(t *testing.T) {
	c, s := fixture(t)
	d := mustDisk(t, c, s, 4*BlockSize, 64)
	d.WriteAt(bytes.Repeat([]byte{1}, 2*BlockSize), 0)
	r := mustCapture(t, d)
	var seg blockformat.Ref
	refs, _ := c.Children(s, r)
	leaves, _ := c.Children(s, refs[0])
	seg = leaves[0]
	old := bytes.Clone(s.objects[seg.Digest])
	h, _ := c.header(seg)
	s.objects[seg.Digest][len(h)+BlockSize+20+4] ^= 1
	out := bytes.Repeat([]byte{9}, 2*BlockSize)
	if e := d.ReadAt(out, 0); e == nil || !bytes.Equal(out, bytes.Repeat([]byte{9}, len(out))) {
		t.Fatal("partial data escaped")
	}
	if e := d.WriteAt(bytes.Repeat([]byte{3}, 2*BlockSize), 0); e == nil || len(d.dirty) != 0 {
		t.Fatal("partial write committed")
	}
	s.objects[seg.Digest] = old
	if e := d.WriteAt([]byte{1}, -1); e == nil {
		t.Fatal("negative offset")
	}
	if e := d.ReadAt(make([]byte, 1), d.shape.Capacity); e == nil {
		t.Fatal("overflow")
	}
}
func TestMetadataValidationAndChildren(t *testing.T) {
	c, s := fixture(t)
	d := mustDisk(t, c, s, 32<<30, 256)
	d.WriteAt(bytes.Repeat([]byte{4}, BlockSize), 0)
	r := mustCapture(t, d)
	seen := map[blockformat.Ref]bool{}
	var visit func(blockformat.Ref)
	visit = func(ref blockformat.Ref) {
		if seen[ref] {
			return
		}
		seen[ref] = true
		if ref.Kind == segmentKind {
			return
		}
		children, e := c.Children(s, ref)
		if e != nil {
			t.Fatal(e)
		}
		for _, child := range children {
			visit(child)
		}
	}
	visit(r)
	if len(seen) != 5 {
		t.Fatalf("objects in closure %d", len(seen))
	}
	children, _ := c.Children(s, r)
	raw, _ := c.metadata(s, children[0])
	var n node
	json.Unmarshal(raw, &n)
	n.Level = 99
	bad, e := d.save(nodeKind, n)
	if e != nil {
		t.Fatal(e)
	}
	if _, e = c.Children(s, bad); e == nil {
		t.Fatal("rank accepted")
	}
	mutated := bytes.Clone(s.objects[r.Digest])
	mutated[len(mutated)-1] ^= 1
	s.objects[r.Digest] = mutated
	if _, e = Open(c, s, r); e == nil {
		t.Fatal("metadata digest ignored")
	}
	delete(s.objects, children[0].Digest)
	if _, e = c.Children(s, children[0]); e == nil {
		t.Fatal("missing object accepted")
	}
}
func TestZeroRemovesOldMapping(t *testing.T) {
	c, s := fixture(t)
	d := mustDisk(t, c, s, 1<<20, 64)
	d.WriteAt(bytes.Repeat([]byte{2}, BlockSize), 0)
	old := mustCapture(t, d)
	d.WriteAt(make([]byte, BlockSize), 0)
	now := mustCapture(t, d)
	clone, _ := Open(c, s, now)
	got := make([]byte, BlockSize)
	if e := clone.ReadAt(got, 0); e != nil || !bytes.Equal(got, make([]byte, BlockSize)) {
		t.Fatal(e)
	}
	prior, _ := Open(c, s, old)
	prior.ReadAt(got, 0)
	if got[0] != 2 {
		t.Fatal("old root changed")
	}
}
func TestCiphertextIdentity(t *testing.T) {
	c, s := fixture(t)
	r, b, e := c.seal(rootKind, [][]byte{[]byte("metadata")})
	if e != nil {
		t.Fatal(e)
	}
	r.Digest = sha256.Sum256([]byte("wrong"))
	if e = s.put(r, b); e == nil {
		t.Fatal("bad upload accepted")
	}
}

func TestMalformedNodeNeverBecomesZero(t *testing.T) {
	for _, body := range []string{"{}", "null"} {
		t.Run(body, func(t *testing.T) {
			c, s := fixture(t)
			d := mustDisk(t, c, s, 1<<20, 64)
			ref, b, e := c.seal(nodeKind, [][]byte{[]byte(body)})
			if e != nil {
				t.Fatal(e)
			}
			s.put(ref, b)
			d.shape.Index = &ref
			got := bytes.Repeat([]byte{5}, BlockSize)
			if e = d.ReadAt(got, 0); e == nil || !bytes.Equal(got, bytes.Repeat([]byte{5}, BlockSize)) {
				t.Fatal("malformed node yielded data", e)
			}
			if _, e = c.Children(s, ref); e == nil {
				t.Fatal("extractor accepted malformed node")
			}
		})
	}
}
func TestRequestBoundsBeforeAllocation(t *testing.T) {
	c, s := fixture(t)
	d := mustDisk(t, c, s, 32<<30, 256)
	p := make([]byte, maxRequestBytes+1)
	if e := d.ReadAt(p, 0); e == nil {
		t.Fatal("unbounded read")
	}
	if e := d.WriteAt(p, 0); e == nil || len(d.dirty) != 0 {
		t.Fatal("unbounded write")
	}
}
