package generation

import (
	"bytes"
	"encoding/json"
	"fmt"
	"math/rand"
	"testing"
)

type packReach struct {
	Packs, Edges, Bytes, MetadataBytes, DataBytes int64
	Seen                                          map[PackRef]bool
}

func packedClosure(t *testing.T, c *Codec, s *Store, roots ...Locator) packReach {
	t.Helper()
	out := packReach{Seen: map[PackRef]bool{}}
	data := map[Ref]bool{}
	var visit func(PackRef)
	visit = func(ref PackRef) {
		if out.Seen[ref] {
			return
		}
		out.Seen[ref] = true
		out.Packs++
		out.Bytes += ref.Size
		out.MetadataBytes += ref.Size
		children, segments, err := PackChildren(c, s, ref)
		if err != nil {
			t.Fatal(err)
		}
		out.Edges += int64(len(children) + len(segments))
		for _, child := range children {
			visit(child)
		}
		for _, seg := range segments {
			if !data[seg] {
				data[seg] = true
				out.Bytes += seg.Size
				out.DataBytes += seg.Size
			}
		}
	}
	for _, r := range roots {
		visit(r.Pack)
	}
	return out
}
func TestPackedRoundtripAndImmutableReuse(t *testing.T) {
	c, s := fixture(t)
	d := mustDisk(t, c, s, 32<<30, 256)
	want := bytes.Repeat([]byte{0x63}, BlockSize)
	for i := int64(0); i < 1024; i++ {
		if err := d.WriteAt(want, i*8192*BlockSize); err != nil {
			t.Fatal(err)
		}
	}
	first := mustCapture(t, d)
	packs := NewStore()
	p, err := NewPacker(c, s, packs, 256<<10, true)
	if err != nil {
		t.Fatal(err)
	}
	old, err := p.Convert(first)
	if err != nil {
		t.Fatal(err)
	}
	for _, block := range []uint64{0, 8192, 8192 * 1023} {
		got, e := ReadPacked(c, s, packs, old, block)
		if e != nil || !bytes.Equal(got, want) {
			t.Fatal("read", e)
		}
	}
	before := packs.Metrics
	again, err := p.Convert(first)
	if err != nil || again != old || packs.Metrics.Objects != before.Objects {
		t.Fatal("retry altered packs", err)
	}
	d.WriteAt(bytes.Repeat([]byte{0x42}, BlockSize), 0)
	next, err := p.Convert(mustCapture(t, d))
	if err != nil {
		t.Fatal(err)
	}
	got, err := ReadPacked(c, s, packs, old, 0)
	if err != nil || got[0] != 0x63 {
		t.Fatal("old changed", err)
	}
	got, err = ReadPacked(c, s, packs, next, 0)
	if err != nil || got[0] != 0x42 {
		t.Fatal("update missing", err)
	}
	// The reader never consults the conversion placement map.
	clear(p.converted)
	got, err = ReadPacked(c, s, packs, old, 8192)
	if err != nil || got[0] != 0x63 {
		t.Fatal("cold reader depended on map", err)
	}
	packedClosure(t, c, packs, old, next)
}
func TestPackedCorruptionAndDirectory(t *testing.T) {
	c, s := fixture(t)
	d := mustDisk(t, c, s, 1<<20, 64)
	d.WriteAt(bytes.Repeat([]byte{1}, BlockSize), 0)
	packs := NewStore()
	p, _ := NewPacker(c, s, packs, 64<<10, true)
	r, e := p.Convert(mustCapture(t, d))
	if e != nil {
		t.Fatal(e)
	}
	raw := bytes.Clone(packs.objects[r.Pack.Digest])
	packs.objects[r.Pack.Digest][r.Offset] ^= 1
	if _, e = ReadPacked(c, s, packs, r, 0); e == nil {
		t.Fatal("corrupt page accepted")
	}
	if _, _, e = PackChildren(c, packs, r.Pack); e == nil {
		t.Fatal("corrupt pack accepted")
	}
	packs.objects[r.Pack.Digest] = raw
	wrong := r
	wrong.Offset++
	if _, e = ReadPacked(c, s, packs, wrong, 0); e == nil {
		t.Fatal("wrong offset accepted")
	}
	wrong = r
	wrong.Pack.Rank--
	if _, e = ReadPacked(c, s, packs, wrong, 0); e == nil {
		t.Fatal("wrong rank accepted")
	}
	for _, delta := range []int{-1, 1} {
		bad := bytes.Clone(raw)
		if delta < 0 {
			bad = bad[:len(bad)-1]
		} else {
			bad = append(bad, 0)
		}
		packs.objects[r.Pack.Digest] = bad
		if _, e = ReadPacked(c, s, packs, r, 0); e == nil {
			t.Fatal("size accepted")
		}
	}
	packs.objects[r.Pack.Digest] = raw
	delete(packs.objects, r.Pack.Digest)
	if _, e = ReadPacked(c, s, packs, r, 0); e == nil {
		t.Fatal("missing pack accepted")
	}
}
func TestPackingMeasurement(t *testing.T) {
	for _, gib := range []int64{8, 32} {
		for _, layout := range []string{"clustered", "dispersed"} {
			for _, limit := range []int{256 << 10, 1 << 20, 4 << 20} {
				t.Run(fmt.Sprintf("%dGiB/%s/%dKiB", gib, layout, limit>>10), func(t *testing.T) {
					c, s := fixture(t)
					c.entropy = rand.New(rand.NewSource(19))
					d := mustDisk(t, c, s, gib<<30, 256)
					rng := rand.New(rand.NewSource(20260923))
					blocks := make([]int64, 1024)
					seen := map[int64]bool{}
					for i := range blocks {
						b := int64(i)
						if layout == "dispersed" {
							for {
								b = rng.Int63n(d.shape.Capacity / BlockSize)
								if !seen[b] {
									break
								}
							}
						}
						seen[b] = true
						blocks[i] = b
						d.WriteAt(bytes.Repeat([]byte{0x61}, BlockSize), b*BlockSize)
					}
					root := mustCapture(t, d)
					plain := closure(t, c, s, root)
					pc, _ := NewCodec(c.Scope, c.ActiveKey, c.Keys)
					pc.entropy = rand.New(rand.NewSource(47))
					packs := NewStore()
					p, _ := NewPacker(pc, s, packs, limit, true)
					r, e := p.Convert(root)
					if e != nil {
						t.Fatal(e)
					}
					first := packedClosure(t, pc, packs, r)
					before := packs.Metrics
					d.WriteAt(bytes.Repeat([]byte{0x62}, BlockSize), blocks[0]*BlockSize)
					newRoot := mustCapture(t, d)
					next, e := p.Convert(newRoot)
					if e != nil {
						t.Fatal(e)
					}
					update := packs.Metrics
					latest := packedClosure(t, pc, packs, next)
					both := packedClosure(t, pc, packs, r, next)
					packs.Metrics.Ranges = 0
					packs.Metrics.ReadBytes = 0
					s.Metrics.Ranges = 0
					s.Metrics.ReadBytes = 0
					got, e := ReadPacked(pc, s, packs, r, uint64(blocks[0]))
					if e != nil || got[0] != 0x61 {
						t.Fatal(e)
					}
					t.Logf("baseline_objects=%d baseline_bytes=%d baseline_edges=%d pack_objects=%d packed_bytes=%d pack_edges=%d update_packs=%d update_bytes=%d latest_retained_bytes=%d pinned_retained_bytes=%d cold_requests=%d cold_bytes=%d", plain.Objects, plain.Bytes, plain.Edges, first.Packs+1, first.Bytes, first.Edges, update.Objects-before.Objects, update.Bytes-before.Bytes, latest.Bytes, both.Bytes, packs.Metrics.Ranges+s.Metrics.Ranges, packs.Metrics.ReadBytes+s.Metrics.ReadBytes)
				})
			}
		}
	}
}

// logicalPackedBytes counts only pages actually selected by the current tree,
// and whole data segments those pages reference. Physical GC must instead retain
// the transitive union of all pages inside each reachable pack.
func logicalPackedBytes(t *testing.T, c *Codec, packs *Store, r Locator) (int64, int64) {
	t.Helper()
	shape, err := openPacked(c, packs, r)
	if err != nil {
		t.Fatal(err)
	}
	pages := map[Locator]bool{r: true}
	data := map[Ref]bool{}
	d := &Disk{shape: rootShape(shape)}
	var walk func(*Locator, int, uint64)
	walk = func(l *Locator, level int, start uint64) {
		if l == nil {
			return
		}
		pages[*l] = true
		n, err := loadPacked(c, packs, *l, shape, level, start)
		if err != nil {
			t.Fatal(err)
		}
		for _, e := range n.Entries {
			if level == 0 {
				data[n.Segments[e.Segment]] = true
			} else {
				walk(e.Child, level-1, start+uint64(e.Slot)*d.stride(level))
			}
		}
	}
	walk(shape.Index, shape.Level, 0)
	var meta, bytes int64
	for l := range pages {
		meta += l.Page.Size
	}
	for r := range data {
		bytes += r.Size
	}
	return meta, bytes
}
func TestPackingRetentionComparison(t *testing.T) {
	for _, internal := range []bool{true, false} {
		t.Run(fmt.Sprintf("packInternal=%v", internal), func(t *testing.T) {
			c, s := fixture(t)
			c.entropy = rand.New(rand.NewSource(19))
			d := mustDisk(t, c, s, 32<<30, 256)
			rng := rand.New(rand.NewSource(51))
			blocks := make([]int64, 1024)
			seen := map[int64]bool{}
			for i := range blocks {
				for {
					b := rng.Int63n(d.shape.Capacity / BlockSize)
					if !seen[b] {
						seen[b] = true
						blocks[i] = b
						break
					}
				}
				d.WriteAt(bytes.Repeat([]byte{0x61}, BlockSize), blocks[i]*BlockSize)
			}
			pc, _ := NewCodec(c.Scope, c.ActiveKey, c.Keys)
			pc.entropy = rand.New(rand.NewSource(47))
			packs := NewStore()
			p, _ := NewPacker(pc, s, packs, 1<<20, internal)
			r, err := p.Convert(mustCapture(t, d))
			if err != nil {
				t.Fatal(err)
			}
			initial := packedClosure(t, pc, packs, r)
			var pins []Locator
			pins = append(pins, r)
			for cut := 0; cut < 16; cut++ {
				for _, b := range blocks[cut*64 : (cut+1)*64] {
					if err = d.WriteAt(bytes.Repeat([]byte{byte(cut + 1)}, BlockSize), b*BlockSize); err != nil {
						t.Fatal(err)
					}
				}
				r, err = p.Convert(mustCapture(t, d))
				if err != nil {
					t.Fatal(err)
				}
				pins = append(pins, r)
			}
			latest := packedClosure(t, pc, packs, r)
			retained := packedClosure(t, pc, packs, pins...)
			meta, data := logicalPackedBytes(t, pc, packs, r)
			t.Logf("initial_packs=%d initial_edges=%d latest_physical_packs=%d latest_physical_edges=%d latest_physical_metadata=%d latest_logical_metadata=%d latest_physical_data=%d latest_logical_data=%d all_roots_bytes=%d", initial.Packs, initial.Edges, latest.Packs, latest.Edges, latest.MetadataBytes, meta, latest.DataBytes, data, retained.Bytes)
			if latest.DataBytes < data || latest.MetadataBytes < meta {
				t.Fatal("physical closure undercounts live bytes")
			}
			for i, b := range blocks {
				got, err := ReadPacked(pc, s, packs, r, uint64(b))
				if err != nil || got[0] != byte(i/64+1) {
					t.Fatal("lost update", err)
				}
			}
		})
	}
}

func TestPackBoundsBeforeFetch(t *testing.T) {
	c, _ := fixture(t)
	s := NewStore()
	for _, ref := range []PackRef{{Size: 5 << 20, Rank: 1}, {Size: 7, Rank: 1}, {Size: 10, Rank: 9}} {
		if _, _, err := PackChildren(c, s, ref); err == nil {
			t.Fatal("invalid pack accepted")
		}
	}
	if s.Metrics.Gets != 0 {
		t.Fatal("fetched before validating bounds")
	}
}

func TestPackingHotColdAndZero(t *testing.T) {
	for _, internal := range []bool{true, false} {
		t.Run(fmt.Sprintf("packInternal=%v", internal), func(t *testing.T) {
			c, s := fixture(t)
			c.entropy = rand.New(rand.NewSource(91))
			d := mustDisk(t, c, s, 32<<30, 256)
			// One block per 256 MiB region, leaving a long-lived cold page in many packs.
			for i := int64(0); i < 128; i++ {
				d.WriteAt(bytes.Repeat([]byte{0x31}, BlockSize), i*(256<<20))
			}
			pc, _ := NewCodec(c.Scope, c.ActiveKey, c.Keys)
			pc.entropy = rand.New(rand.NewSource(99))
			packs := NewStore()
			p, _ := NewPacker(pc, s, packs, 1<<20, internal)
			r, err := p.Convert(mustCapture(t, d))
			if err != nil {
				t.Fatal(err)
			}
			pins := []Locator{r}
			for cut := 0; cut < 32; cut++ {
				d.WriteAt(bytes.Repeat([]byte{byte(cut + 1)}, BlockSize), 0)
				value := bytes.Repeat([]byte{0x52}, BlockSize)
				if cut%3 == 0 {
					clear(value)
				}
				d.WriteAt(value, int64(cut+1)*(256<<20))
				r, err = p.Convert(mustCapture(t, d))
				if err != nil {
					t.Fatal(err)
				}
				pins = append(pins, r)
			}
			physical := packedClosure(t, pc, packs, r)
			all := packedClosure(t, pc, packs, pins...)
			meta, data := logicalPackedBytes(t, pc, packs, r)
			t.Logf("hotcold_latest_packs=%d physical_metadata=%d logical_metadata=%d physical_data=%d logical_data=%d all_pins_bytes=%d", physical.Packs, physical.MetadataBytes, meta, physical.DataBytes, data, all.Bytes)
			for i := uint64(0); i < 128; i++ {
				got, err := ReadPacked(pc, s, packs, r, i*(256<<20)/BlockSize)
				if err != nil {
					t.Fatal(err)
				}
				want := byte(0x31)
				if i == 0 {
					want = 32
				} else if i <= 32 {
					want = 0x52
					if (i-1)%3 == 0 {
						want = 0
					}
				}
				if got[0] != want {
					t.Fatal("hot/cold data mismatch")
				}
			}
		})
	}
}

func BenchmarkPacking(b *testing.B) {
	key := bytes.Repeat([]byte{7}, 32)
	c, _ := NewCodec("org/environment", "k1", map[string][]byte{"k1": key})
	c.entropy = rand.New(rand.NewSource(19))
	s := NewStore()
	d, _ := New(c, s, 32<<30, 256)
	rng := rand.New(rand.NewSource(20260923))
	seen := map[int64]bool{}
	for len(seen) < 1024 {
		block := rng.Int63n(d.shape.Capacity / BlockSize)
		if seen[block] {
			continue
		}
		seen[block] = true
		if err := d.WriteAt(bytes.Repeat([]byte{0x61}, BlockSize), block*BlockSize); err != nil {
			b.Fatal(err)
		}
	}
	root, err := d.Capture()
	if err != nil {
		b.Fatal(err)
	}
	for _, internal := range []bool{true, false} {
		b.Run(fmt.Sprintf("packInternal=%v", internal), func(b *testing.B) {
			b.ReportAllocs()
			b.ResetTimer()
			for i := 0; i < b.N; i++ {
				pc, _ := NewCodec(c.Scope, c.ActiveKey, c.Keys)
				pc.entropy = rand.New(rand.NewSource(47))
				p, _ := NewPacker(pc, s, NewStore(), 1<<20, internal)
				if _, err := p.Convert(root); err != nil {
					b.Fatal(err)
				}
			}
		})
	}
}

func TestPackRejectsOutOfGeometryNode(t *testing.T) {
	for _, n := range []packedNode{{Capacity: 4096, Fanout: 64, Level: 1}, {Capacity: 4096, Fanout: 64, Start: 64}} {
		c, source := fixture(t)
		packs := NewStore()
		p, _ := NewPacker(c, source, packs, 64<<10, true)
		plain, err := json.Marshal(n)
		if err != nil {
			t.Fatal(err)
		}
		ref, raw, err := c.seal(nodeKind, [][]byte{plain})
		if err != nil {
			t.Fatal(err)
		}
		if err = p.publish(n.Level+1, []stagedPage{{ref, ref, raw}}); err != nil {
			t.Fatal(err)
		}
		if _, _, err = PackChildren(c, packs, p.converted[ref].Pack); err == nil {
			t.Fatal("out-of-geometry node accepted")
		}
	}
}
func TestPackingRandomOverwriteTrend(t *testing.T) {
	c, s := fixture(t)
	c.entropy = rand.New(rand.NewSource(19))
	d := mustDisk(t, c, s, 32<<30, 256)
	rng := rand.New(rand.NewSource(51))
	blocks := make([]int64, 1024)
	seen := map[int64]bool{}
	for i := range blocks {
		for {
			block := rng.Int63n(d.shape.Capacity / BlockSize)
			if !seen[block] {
				seen[block] = true
				blocks[i] = block
				break
			}
		}
		d.WriteAt(bytes.Repeat([]byte{0x61}, BlockSize), blocks[i]*BlockSize)
	}
	sourceRoot := mustCapture(t, d)
	sourcePins := []Ref{sourceRoot}
	var packers []*Packer
	var pins [][]Locator
	for _, internal := range []bool{true, false} {
		pc, _ := NewCodec(c.Scope, c.ActiveKey, c.Keys)
		pc.entropy = rand.New(rand.NewSource(47))
		p, _ := NewPacker(pc, s, NewStore(), 1<<20, internal)
		r, err := p.Convert(sourceRoot)
		if err != nil {
			t.Fatal(err)
		}
		packers = append(packers, p)
		pins = append(pins, []Locator{r})
	}
	for cut := 1; cut <= 64; cut++ {
		// Replacement permits repeats and cold survivors, unlike a full permutation.
		for range 32 {
			block := blocks[rng.Intn(len(blocks))]
			if err := d.WriteAt(bytes.Repeat([]byte{byte(cut)}, BlockSize), block*BlockSize); err != nil {
				t.Fatal(err)
			}
		}
		sourceRoot = mustCapture(t, d)
		sourcePins = append(sourcePins, sourceRoot)
		for i, p := range packers {
			r, err := p.Convert(sourceRoot)
			if err != nil {
				t.Fatal(err)
			}
			pins[i] = append(pins[i], r)
		}
		if cut%16 != 0 {
			continue
		}
		base := closure(t, c, s, sourceRoot)
		allBase := closure(t, c, s, sourcePins...)
		for i, p := range packers {
			latest := packedClosure(t, p.codec, p.packs, pins[i][len(pins[i])-1])
			all := packedClosure(t, p.codec, p.packs, pins[i]...)
			meta, data := logicalPackedBytes(t, p.codec, p.packs, pins[i][len(pins[i])-1])
			t.Logf("cut=%d packInternal=%v base_metadata=%d base_data=%d base_all_pins=%d packed_metadata=%d packed_logical_metadata=%d packed_data=%d packed_logical_data=%d packed_packs=%d packed_edges=%d packed_all_pins=%d", cut, p.packInternal, base.Bytes-base.SegmentBytes, base.SegmentBytes, allBase.Bytes, latest.MetadataBytes, meta, latest.DataBytes, data, latest.Packs, latest.Edges, all.Bytes)
			if data != base.SegmentBytes {
				t.Fatal("logical data differs from baseline")
			}
		}
	}
}
