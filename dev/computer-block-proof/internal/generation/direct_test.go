package generation

import (
	"bytes"
	"encoding/json"
	"fmt"
	"math/rand"
	"testing"

	"github.com/helmrdotdev/helmr/internal/computer/blockformat"
)

func TestDirectGenerations(t *testing.T) {
	for _, fanout := range []int{64, 256} {
		for _, internal := range []bool{true, false} {
			t.Run(fmt.Sprintf("%d/%v", fanout, internal), func(t *testing.T) {
				c, data := fixture(t)
				packs := NewStore()
				r, err := NewPacked(c, packs, 32<<30, fanout, 1<<20)
				if err != nil {
					t.Fatal(err)
				}
				oracle := mustDisk(t, c, NewStore(), 32<<30, fanout)
				rng := rand.New(rand.NewSource(51))
				want := map[uint64]byte{}
				var roots []blockformat.Locator
				var states []map[uint64]byte
				for cut := 0; cut < 8; cut++ {
					changes := map[uint64][]byte{}
					for i := 0; i < 128; i++ {
						b := uint64(rng.Intn(256)) * 8192
						v := byte(cut + 1)
						if i%5 == 0 {
							v = 0
						}
						changes[b] = bytes.Repeat([]byte{v}, blockformat.BlockSize)
						want[b] = v
					}
					for b, v := range changes {
						if err = oracle.WriteAt(v, int64(b)*blockformat.BlockSize); err != nil {
							t.Fatal(err)
						}
					}
					mustCapture(t, oracle)
					r, err = CapturePacked(c, data, packs, r, changes, 1<<20, internal)
					if err != nil {
						t.Fatal(err)
					}
					state := map[uint64]byte{}
					for b, v := range want {
						state[b] = v
						got, e := ReadPacked(c, data, packs, r, b)
						expected := make([]byte, blockformat.BlockSize)
						if e != nil {
							t.Fatal(e)
						}
						if e = oracle.ReadAt(expected, int64(b)*blockformat.BlockSize); e != nil || !bytes.Equal(got, expected) {
							t.Fatal("oracle mismatch", e)
						}
					}
					roots = append(roots, r)
					states = append(states, state)
				}
				ds, ps := retainPacked(t, c, data, packs, roots...)
				for i, r := range roots {
					for b, v := range states[i] {
						got, e := ReadPacked(c, ds, ps, r, b)
						if e != nil || !bytes.Equal(got, bytes.Repeat([]byte{v}, blockformat.BlockSize)) {
							t.Fatal("historical mismatch", e)
						}
					}
				}
				// A branch from an old generation leaves the newest generation untouched.
				branch, e := CapturePacked(c, data, packs, roots[0], map[uint64][]byte{0: bytes.Repeat([]byte{99}, blockformat.BlockSize)}, 1<<20, internal)
				if e != nil {
					t.Fatal(e)
				}
				got, e := ReadPacked(c, data, packs, branch, 0)
				if e != nil || got[0] != 99 {
					t.Fatal(e)
				}
				old := r
				beforeD, beforeP := data.Metrics.Bytes, packs.Metrics.Bytes
				if _, e = CapturePacked(c, data, packs, r, map[uint64][]byte{0: {1}}, 1<<20, internal); e == nil {
					t.Fatal("partial block accepted")
				}
				if data.Metrics.Bytes != beforeD || packs.Metrics.Bytes != beforeP {
					t.Fatal("invalid request wrote objects")
				}
				same, e := CapturePacked(c, data, packs, r, nil, 1<<20, internal)
				if e != nil || same != old {
					t.Fatal("empty capture")
				}
				// Failure while encoding must leave the prior root readable.
				c.entropy = bytes.NewReader(nil)
				failed, e := CapturePacked(c, data, packs, r, map[uint64][]byte{0: bytes.Repeat([]byte{1}, blockformat.BlockSize)}, 1<<20, internal)
				if e == nil || failed != (blockformat.Locator{}) {
					t.Fatal("failed capture returned root")
				}
				if _, e = ReadPacked(c, data, packs, old, 0); e != nil {
					t.Fatal(e)
				}
			})
		}
	}
}

func TestDirectMeasurement(t *testing.T) {
	for _, internal := range []bool{true, false} {
		t.Run(fmt.Sprint(internal), func(t *testing.T) {
			c, data := fixture(t)
			c.entropy = rand.New(rand.NewSource(19))
			packs := NewStore()
			base, e := NewPacked(c, packs, 32<<30, 256, 1<<20)
			if e != nil {
				t.Fatal(e)
			}
			changes := map[uint64][]byte{}
			rng := rand.New(rand.NewSource(51))
			for len(changes) < 1024 {
				changes[uint64(rng.Int63n((32<<30)/blockformat.BlockSize))] = bytes.Repeat([]byte{1}, blockformat.BlockSize)
			}
			dm, pm := data.Metrics, packs.Metrics
			r, e := CapturePacked(c, data, packs, base, changes, 1<<20, internal)
			if e != nil {
				t.Fatal(e)
			}
			directWritten := data.Metrics.Bytes - dm.Bytes + packs.Metrics.Bytes - pm.Bytes
			closure := packedClosure(t, c, packs, r)
			// Equivalent conversion pipeline using the same compact locator format.
			cc, source := fixture(t)
			cc.entropy = rand.New(rand.NewSource(19))
			d := mustDisk(t, cc, source, 32<<30, 256)
			for b, v := range changes {
				if e = d.WriteAt(v, int64(b)*blockformat.BlockSize); e != nil {
					t.Fatal(e)
				}
			}
			raw := mustCapture(t, d)
			converted := NewStore()
			p, _ := NewPacker(cc, source, converted, 1<<20, internal)
			cr, e := p.Convert(raw)
			if e != nil {
				t.Fatal(e)
			}
			legacy := packedClosure(t, cc, converted, cr)
			before := packs.Metrics
			beforeData := data.Metrics
			selected := uint64(blockformat.MaxBlocks)
			for b := range changes {
				if b < selected {
					selected = b
				}
			}
			{
				b := selected
				r, e = CapturePacked(c, data, packs, r, map[uint64][]byte{b: make([]byte, blockformat.BlockSize)}, 1<<20, internal)
				if e != nil {
					t.Fatal(e)
				}
			}
			t.Logf("direct_written=%d direct_closure=%d direct_packs=%d conversion_staging=%d conversion_packs_written=%d conversion_closure=%d zero_update_written=%d zero_update_reads=%d", directWritten, closure.Bytes, closure.Packs, source.Metrics.Bytes, converted.Metrics.Bytes, legacy.Bytes, packs.Metrics.Bytes-before.Bytes+data.Metrics.Bytes-beforeData.Bytes, packs.Metrics.ReadBytes-before.ReadBytes+data.Metrics.ReadBytes-beforeData.ReadBytes)
		})
	}
}

func TestCompactLocator(t *testing.T) {
	c, _ := fixture(t)
	p := NewStore()
	r, e := NewPacked(c, p, 4096, 64, 64<<10)
	if e != nil {
		t.Fatal(e)
	}
	raw, e := json.Marshal(r)
	if e != nil {
		t.Fatal(e)
	}
	var got blockformat.Locator
	if e = json.Unmarshal(raw, &got); e != nil || got != r {
		t.Fatal("roundtrip", e)
	}
	for _, bad := range [][]byte{[]byte("null"), []byte("{}"), []byte(`"AA=="`), bytes.Repeat([]byte{' '}, 347)} {
		before := got
		if e = json.Unmarshal(bad, &got); e == nil || got != before {
			t.Fatal("invalid locator mutated destination")
		}
	}
	// All maximum-length descriptor fields round-trip without truncation.
	r.Page.Key = string(bytes.Repeat([]byte{'k'}, 128))
	r.Pack.Size = 1 << 62
	r.Page.Size = 1 << 62
	r.Offset = 1 << 62
	r.Pack.Rank = 255
	r.Page.Count = ^uint32(0)
	raw, e = json.Marshal(r)
	if e != nil {
		t.Fatal(e)
	}
	if e = json.Unmarshal(raw, &got); e != nil || got != r {
		t.Fatal("maximum descriptor", e)
	}
}

func TestDirectFailureAndCompleteZero(t *testing.T) {
	c, data := fixture(t)
	packs := NewStore()
	base, err := NewPacked(c, packs, 64*blockformat.BlockSize, 64, 64<<10)
	if err != nil {
		t.Fatal(err)
	}
	change := map[uint64][]byte{0: bytes.Repeat([]byte{7}, blockformat.BlockSize)}
	old, err := CapturePacked(c, data, packs, base, change, 64<<10, true)
	if err != nil {
		t.Fatal(err)
	}
	tooMany := make(map[uint64][]byte, maxDirty+1)
	for i := 0; i <= maxDirty; i++ {
		tooMany[uint64(i)] = nil
	}
	beforeD, beforeP := data.Metrics.Bytes, packs.Metrics.Bytes
	for _, invalid := range []map[uint64][]byte{tooMany, {64: make([]byte, blockformat.BlockSize)}} {
		r, e := CapturePacked(c, data, packs, old, invalid, 64<<10, true)
		if e == nil || r != (blockformat.Locator{}) {
			t.Fatal("invalid input accepted")
		}
	}
	if data.Metrics.Bytes != beforeD || packs.Metrics.Bytes != beforeP {
		t.Fatal("invalid input persisted objects")
	}
	// One segment and leaf can be sealed; the root seal then runs out of entropy.
	c.entropy = bytes.NewReader(bytes.Repeat([]byte{0xab}, 64))
	r, err := CapturePacked(c, data, packs, old, change, 64<<10, true)
	if err == nil || r != (blockformat.Locator{}) || packs.Metrics.Bytes <= beforeP {
		t.Fatal("late failure not exercised")
	}
	got, err := ReadPacked(c, data, packs, old, 0)
	if err != nil || got[0] != 7 {
		t.Fatal("old root after late failure", err)
	}
	c.entropy = rand.New(rand.NewSource(91))
	empty, err := CapturePacked(c, data, packs, old, map[uint64][]byte{0: make([]byte, blockformat.BlockSize)}, 64<<10, true)
	if err != nil {
		t.Fatal(err)
	}
	shape, err := openPacked(c, packs, empty)
	if err != nil || shape.Index != nil {
		t.Fatal("empty subtree retained", err)
	}
	ds, ps := retainPacked(t, c, data, packs, empty)
	if len(ds.objects) != 0 {
		t.Fatal("zero tree retains data")
	}
	got, err = ReadPacked(c, ds, ps, empty, 0)
	if err != nil || !bytes.Equal(got, make([]byte, blockformat.BlockSize)) {
		t.Fatal(err)
	}
}
