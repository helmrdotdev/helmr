package generation

import (
	"bytes"
	"fmt"
	"math/rand"
	"testing"
)

type reachability struct {
	Objects, Edges, Bytes, SegmentBytes, LiveDataBytes int64
	Refs                                               map[Ref]bool
}

func closure(t *testing.T, c *Codec, s *Store, roots ...Ref) reachability {
	t.Helper()
	var out reachability
	seen := map[Ref]bool{}
	records := map[Ref]map[uint32]bool{}
	var visit func(Ref)
	visit = func(r Ref) {
		if seen[r] {
			return
		}
		seen[r] = true
		out.Objects++
		out.Bytes += r.Size
		if r.Kind == segmentKind {
			out.SegmentBytes += r.Size
			return
		}
		children, e := c.Children(s, r)
		if e != nil {
			t.Fatal(e)
		}
		out.Edges += int64(len(children))
		for _, child := range children {
			visit(child)
		}
		if r.Kind == nodeKind {
			raw, _ := c.metadata(s, r)
			var n node
			if e = decode(raw, &n); e != nil {
				t.Fatal(e)
			}
			if n.Level == 0 {
				for _, entry := range n.Entries {
					ref := n.Segments[entry.Segment]
					if records[ref] == nil {
						records[ref] = map[uint32]bool{}
					}
					records[ref][entry.Record] = true
				}
			}
		}
	}
	for _, r := range roots {
		visit(r)
	}
	for _, set := range records {
		out.LiveDataBytes += int64(len(set)) * BlockSize
	}
	out.Refs = seen
	return out
}
func TestMeasurementMatrix(t *testing.T) {
	// These are real encodings of sparse logical disks, not dense 32 GiB claims.
	for _, gib := range []int64{8, 32} {
		for _, f := range []int{64, 256} {
			for _, layout := range []string{"clustered", "dispersed"} {
				t.Run(fmt.Sprintf("%dGiB/f%d/%s", gib, f, layout), func(t *testing.T) {
					c, s := fixture(t)
					c.entropy = rand.New(rand.NewSource(19))
					d := mustDisk(t, c, s, gib<<30, f)
					rng := rand.New(rand.NewSource(20260923))
					blocks := make([]int64, 1024)
					chosen := map[int64]bool{}
					for i := range blocks {
						b := int64(i)
						if layout == "dispersed" {
							for {
								b = rng.Int63n(d.shape.Capacity / BlockSize)
								if !chosen[b] {
									break
								}
							}
						}
						chosen[b] = true
						blocks[i] = b
					}
					before := s.Metrics
					for _, b := range blocks {
						if e := d.WriteAt(bytes.Repeat([]byte{0x61}, BlockSize), b*BlockSize); e != nil {
							t.Fatal(e)
						}
					}
					r := mustCapture(t, d)
					created := s.Metrics
					first := closure(t, c, s, r)
					// A single changed block retains the rest of the old packed segment.
					d.WriteAt(bytes.Repeat([]byte{0x62}, BlockSize), blocks[0]*BlockSize)
					newRoot := mustCapture(t, d)
					latest := closure(t, c, s, newRoot)
					var changedNodes, changedEdges, changedBytes int64
					for ref := range latest.Refs {
						if first.Refs[ref] {
							continue
						}
						changedBytes += ref.Size
						if ref.Kind != segmentKind {
							changedNodes++
							children, err := c.Children(s, ref)
							if err != nil {
								t.Fatal(err)
							}
							changedEdges += int64(len(children))
						}
					}
					retained := closure(t, c, s, r, newRoot)
					s.Metrics.Gets = 0
					s.Metrics.Ranges = 0
					s.Metrics.ReadBytes = 0
					cold, e := Open(c, s, r)
					if e != nil {
						t.Fatal(e)
					}
					p := make([]byte, BlockSize)
					if e = cold.ReadAt(p, blocks[0]*BlockSize); e != nil || p[0] != 0x61 {
						t.Fatal(e)
					}
					if s.Metrics.Gets > int64(d.shape.Level+2) || s.Metrics.Ranges != 2 {
						t.Fatal("lookup depth unbounded", s.Metrics)
					}
					t.Logf("new_objects=%d encoded_bytes=%d first_edges=%d latest_segment_bytes=%d latest_live_data=%d pinned_total_bytes=%d cold_gets=%d cold_ranges=%d cold_bytes=%d update_metadata_objects=%d update_edges=%d update_bytes=%d", created.Objects-before.Objects, created.Bytes-before.Bytes, first.Edges, latest.SegmentBytes, latest.LiveDataBytes, retained.Bytes, s.Metrics.Gets, s.Metrics.Ranges, s.Metrics.ReadBytes, changedNodes, changedEdges, changedBytes)
				})
			}
		}
	}
}
func TestDenseSeedImport(t *testing.T) {
	c, s := fixture(t)
	c.entropy = rand.New(rand.NewSource(19))
	data := make([]byte, 8<<20)
	rand.New(rand.NewSource(11)).Read(data)
	r, e := Import(c, s, int64(len(data)), 256, bytes.NewReader(data))
	if e != nil {
		t.Fatal(e)
	}
	d, e := Open(c, s, r)
	if e != nil {
		t.Fatal(e)
	}
	got := make([]byte, len(data))
	if e = d.ReadAt(got, 0); e != nil || !bytes.Equal(data, got) {
		t.Fatal("seed roundtrip", e)
	}
	all := closure(t, c, s, r)
	t.Logf("dense8MiB objects=%d bytes=%d edges=%d live=%d", all.Objects, all.Bytes, all.Edges, all.LiveDataBytes)
	if _, e = Import(c, s, BlockSize, 64, bytes.NewReader(data[:BlockSize-1])); e == nil {
		t.Fatal("truncated seed")
	}
	if _, e = Import(c, s, BlockSize, 64, bytes.NewReader(data[:BlockSize+1])); e == nil {
		t.Fatal("oversized seed")
	}
}
