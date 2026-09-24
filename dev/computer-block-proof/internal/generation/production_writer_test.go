package generation

import (
	"bytes"
	"context"
	"crypto/sha256"
	"fmt"
	"math/rand"
	"testing"

	"github.com/helmrdotdev/helmr/internal/computer/blockformat"
)

type productionObjects struct{ data, packs *Store }

func (s productionObjects) StoreObject(ctx context.Context, digest [32]byte, raw []byte) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	target := s.data
	if bytes.HasPrefix(raw, []byte("HGP0")) {
		target = s.packs
	}
	return target.put(blockformat.Ref{Digest: digest, Size: int64(len(raw))}, raw)
}
func TestProductionWriterAgainstOracle(t *testing.T) {
	for _, fanout := range []int{64, 256} {
		for _, limit := range []int{blockformat.MinPackLimit, 4 << 20} {
			t.Run(fmt.Sprintf("%d/%d", fanout, limit), func(t *testing.T) {
				c, data := fixture(t)
				packs := NewStore()
				source := treeRanges{data, packs}
				w := blockformat.Writer{Source: source, Sink: productionObjects{data, packs}, Scope: c.Scope, ActiveKey: c.ActiveKey, Keys: c.Keys, PackLimit: limit}
				const capacity = 32 << 30
				root, err := w.Empty(t.Context(), capacity, fanout)
				if err != nil {
					t.Fatal(err)
				}
				oracle := mustDisk(t, c, NewStore(), capacity, fanout)
				rng := rand.New(rand.NewSource(77))
				values := map[uint64][]byte{}
				for cut := 0; cut < 4; cut++ {
					changes := map[uint64][]byte{}
					for range 96 {
						block := uint64(rng.Intn(512)) * 8192
						value := bytes.Repeat([]byte{byte(cut + 1)}, 4096)
						if rng.Intn(4) == 0 {
							clear(value)
						}
						changes[block] = value
						values[block] = value
					}
					for block, value := range changes {
						if err := oracle.WriteAt(value, int64(block)*4096); err != nil {
							t.Fatal(err)
						}
					}
					mustCapture(t, oracle)
					root, err = w.Capture(t.Context(), root, capacity, changes)
					if err != nil {
						t.Fatal(err)
					}
					// Independent physical closure verification checks every directory/page and
					// segment record, including content not selected by the individual reads.
					expected, err := Certify(c, data, packs, root, 10000, 128<<20)
					if err != nil {
						t.Fatal(err)
					}
					if actual := inspectProductionClosure(t, c, source, root); actual != expected {
						t.Fatalf("physical closure mismatch: %+v != %+v", actual, expected)
					}
					for block := range values {
						want := make([]byte, 4096)
						if err = oracle.ReadAt(want, int64(block)*4096); err != nil {
							t.Fatal(err)
						}
						got, err := ReadPacked(c, data, packs, root, block)
						if err != nil || !bytes.Equal(got, want) {
							t.Fatalf("oracle mismatch block %d: %v", block, err)
						}
					}
				}
				// Every staged object remains content addressed after captures.
				for digest, raw := range packs.objects {
					if sha256.Sum256(raw) != digest {
						t.Fatal("metadata object modified")
					}
				}
				for digest, raw := range data.objects {
					if sha256.Sum256(raw) != digest {
						t.Fatal("data object modified")
					}
				}
			})
		}
	}
}

// inspectProductionClosure exercises the production per-object verifier against
// the independent complete-closure proof. Its cache is local to this immutable
// fixture; it supplies no persisted certificate or retention authority.
func inspectProductionClosure(t *testing.T, c *Codec, source treeRanges, root blockformat.Locator) Certification {
	t.Helper()
	packs := map[blockformat.PackRef]blockformat.PackInspection{}
	segments := map[blockformat.Ref]bool{}
	var report Certification
	var visit func(blockformat.PackRef) blockformat.PackInspection
	visit = func(ref blockformat.PackRef) blockformat.PackInspection {
		if p, ok := packs[ref]; ok {
			return p
		}
		p, err := blockformat.InspectPack(t.Context(), source, c.Scope, c.Keys, ref)
		if err != nil {
			t.Fatal(err)
		}
		packs[ref] = p
		report.Packs++
		report.Bytes += ref.Size
		for _, page := range p.Pages {
			for _, segment := range page.Segments {
				if segments[segment] {
					continue
				}
				if err = blockformat.InspectSegment(t.Context(), source, c.Scope, c.Keys[segment.Key], segment); err != nil {
					t.Fatal(err)
				}
				segments[segment] = true
				report.Segments++
				report.Bytes += segment.Size
			}
			for _, child := range page.Children {
				if child.Locator.Pack.Rank >= ref.Rank {
					t.Fatal("non-decreasing dependency")
				}
				if err = visit(child.Locator.Pack).CheckNode(child); err != nil {
					t.Fatal(err)
				}
			}
		}
		return p
	}
	if err := visit(root.Pack).CheckRoot(root, 32<<30); err != nil {
		t.Fatal(err)
	}
	return report
}
