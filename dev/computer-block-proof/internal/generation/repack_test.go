package generation

import (
	"bytes"
	"fmt"
	"math/rand"
	"testing"

	"github.com/helmrdotdev/helmr/internal/computer/blockformat"
)

// retainPacked copies the physical closure into empty stores. Reads after this
// operation cannot accidentally use unreachable objects left in the fixture.
func retainPacked(t *testing.T, c *Codec, data, packs *Store, roots ...blockformat.Locator) (*Store, *Store) {
	t.Helper()
	ds, ps := NewStore(), NewStore()
	closure := packedClosure(t, c, packs, roots...)
	for r := range closure.Seen {
		ref := blockformat.Ref{Digest: r.Digest, Size: r.Size}
		b, err := packs.get(ref)
		if err != nil {
			t.Fatal(err)
		}
		if err = ps.put(ref, b); err != nil {
			t.Fatal(err)
		}
		_, segments, err := PackChildren(c, packs, r)
		if err != nil {
			t.Fatal(err)
		}
		for _, seg := range segments {
			b, err := data.get(seg)
			if err != nil {
				t.Fatal(err)
			}
			if err = ds.put(seg, b); err != nil {
				t.Fatal(err)
			}
		}
	}
	return ds, ps
}

func TestRepackRetention(t *testing.T) {
	for _, internal := range []bool{true, false} {
		t.Run(fmt.Sprint(internal), func(t *testing.T) {
			c, data := fixture(t)
			c.entropy = rand.New(rand.NewSource(19))
			d := mustDisk(t, c, data, 32<<30, 256)
			rng := rand.New(rand.NewSource(51))
			blocks := make([]uint64, 1024)
			seen := map[uint64]bool{}
			want := map[uint64]byte{}
			for i := range blocks {
				for {
					b := uint64(rng.Int63n(d.shape.Capacity / blockformat.BlockSize))
					if !seen[b] {
						seen[b] = true
						blocks[i] = b
						break
					}
				}
				want[blocks[i]] = 0x61
				if err := d.WriteAt(bytes.Repeat([]byte{0x61}, blockformat.BlockSize), int64(blocks[i])*blockformat.BlockSize); err != nil {
					t.Fatal(err)
				}
			}
			packs := NewStore()
			p, err := NewPacker(c, data, packs, 1<<20, internal)
			if err != nil {
				t.Fatal(err)
			}
			first, err := p.Convert(mustCapture(t, d))
			if err != nil {
				t.Fatal(err)
			}
			pins := []blockformat.Locator{first}
			latest := first
			for cut := 1; cut <= 64; cut++ {
				for range 32 {
					b := blocks[rng.Intn(len(blocks))]
					want[b] = byte(cut)
					if err = d.WriteAt(bytes.Repeat([]byte{byte(cut)}, blockformat.BlockSize), int64(b)*blockformat.BlockSize); err != nil {
						t.Fatal(err)
					}
				}
				latest, err = p.Convert(mustCapture(t, d))
				if err != nil {
					t.Fatal(err)
				}
				pins = append(pins, latest)
			}
			before := packedClosure(t, c, packs, latest)
			allBefore := packedClosure(t, c, packs, pins...)
			dm, pm := data.Metrics, packs.Metrics
			next, staging, err := Repack(c, data, packs, latest, 1<<20, internal, 1024)
			if err != nil {
				t.Fatal(err)
			}
			readBytes := data.Metrics.ReadBytes - dm.ReadBytes + packs.Metrics.ReadBytes - pm.ReadBytes
			requests := data.Metrics.Gets - dm.Gets + data.Metrics.Ranges - dm.Ranges + packs.Metrics.Gets - pm.Gets + packs.Metrics.Ranges - pm.Ranges
			written := data.Metrics.Bytes - dm.Bytes + packs.Metrics.Bytes - pm.Bytes
			after := packedClosure(t, c, packs, next)
			both := packedClosure(t, c, packs, latest, next)
			allAfter := packedClosure(t, c, packs, append(pins, next)...)
			if after.Bytes >= before.Bytes {
				t.Fatal("no reclamation")
			}
			if allAfter.Bytes != allBefore.Bytes+after.Bytes {
				t.Fatal("rewrite unexpectedly shares or drops pinned objects")
			}
			t.Logf("before_bytes=%d after_bytes=%d before_metadata=%d after_metadata=%d before_data=%d after_data=%d before_packs=%d after_packs=%d read_bytes=%d read_requests=%d written_bytes=%d staging_written_bytes=%d latest_plus_repacked=%d all_pins_before=%d all_pins_after=%d", before.Bytes, after.Bytes, before.MetadataBytes, after.MetadataBytes, before.DataBytes, after.DataBytes, before.Packs, after.Packs, readBytes, requests, written, staging.Bytes, both.Bytes, allBefore.Bytes, allAfter.Bytes)
			// Historical pins and the new root remain independently readable after pruning.
			ds, ps := retainPacked(t, c, data, packs, first, latest, next)
			for _, b := range blocks {
				for _, r := range []blockformat.Locator{first, latest, next} {
					expected := want[b]
					if r == first {
						expected = 0x61
					}
					got, e := ReadPacked(c, ds, ps, r, b)
					if e != nil || !bytes.Equal(got, bytes.Repeat([]byte{expected}, blockformat.BlockSize)) {
						t.Fatalf("read block %d: %v", b, e)
					}
				}
			}
			ds, ps = retainPacked(t, c, data, packs, next)
			if _, e := ReadPacked(c, ds, ps, latest, blocks[0]); e == nil {
				t.Fatal("released root remains available")
			}
			again, _, e := Repack(c, ds, ps, next, 1<<20, internal, 1024)
			if e != nil {
				t.Fatal(e)
			}
			for _, b := range blocks {
				got, e := ReadPacked(c, ds, ps, again, b)
				if e != nil || !bytes.Equal(got, bytes.Repeat([]byte{want[b]}, blockformat.BlockSize)) {
					t.Fatal("repeated rewrite", e)
				}
			}
		})
	}
}

func TestRepackBoundsZeroAndFailure(t *testing.T) {
	c, data := fixture(t)
	d := mustDisk(t, c, data, 32<<20, 64)
	packs := NewStore()
	p, _ := NewPacker(c, data, packs, 64<<10, true)
	empty, err := p.Convert(mustCapture(t, d))
	if err != nil {
		t.Fatal(err)
	}
	r, _, err := Repack(c, data, packs, empty, 64<<10, true, 1)
	if err != nil {
		t.Fatal(err)
	}
	got, err := ReadPacked(c, data, packs, r, 0)
	if err != nil || !bytes.Equal(got, make([]byte, blockformat.BlockSize)) {
		t.Fatal("empty", err)
	}
	// Crosses the 1,024-record staging capture boundary; every second block is a hole.
	for i := 0; i < 1025; i++ {
		if err = d.WriteAt(bytes.Repeat([]byte{byte(i%255 + 1)}, blockformat.BlockSize), int64(i*2)*blockformat.BlockSize); err != nil {
			t.Fatal(err)
		}
	}
	old, err := p.Convert(mustCapture(t, d))
	if err != nil {
		t.Fatal(err)
	}
	for _, bound := range []uint64{0, 1024, blockformat.MaxBlocks + 1} {
		out, _, e := Repack(c, data, packs, old, 64<<10, true, bound)
		if e == nil || out != (blockformat.Locator{}) {
			t.Fatal("work bound")
		}
	}
	next, _, err := Repack(c, data, packs, old, 64<<10, true, 1025)
	if err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 2050; i++ {
		expected := byte(0)
		if i%2 == 0 {
			expected = byte((i/2)%255 + 1)
		}
		got, e := ReadPacked(c, data, packs, next, uint64(i))
		if e != nil || !bytes.Equal(got, bytes.Repeat([]byte{expected}, blockformat.BlockSize)) {
			t.Fatal("batch/hole", i, e)
		}
	}
	// Missing data cannot yield a successful candidate or alter the old root.
	missing := NewStore()
	out, _, e := Repack(c, missing, packs, old, 64<<10, true, 1025)
	if e == nil || out != (blockformat.Locator{}) {
		t.Fatal("missing data accepted")
	}
	got, err = ReadPacked(c, data, packs, old, 0)
	if err != nil || got[0] != 1 {
		t.Fatal("old root changed", err)
	}
}
