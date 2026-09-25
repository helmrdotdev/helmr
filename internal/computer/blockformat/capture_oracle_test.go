package blockformat_test

import (
	"bytes"
	"crypto/sha256"
	"fmt"
	"io"
	"math/rand"
	"testing"

	"github.com/helmrdotdev/helmr/internal/computer/blockformat"
)

func TestCaptureAgainstByteOracle(t *testing.T) {
	for _, fanout := range []int{64, 256} {
		for _, limit := range []int{blockformat.MinPackLimit, 4 << 20} {
			t.Run(fmt.Sprintf("%d/%d", fanout, limit), func(t *testing.T) {
				w, sink := writerFixture(t)
				w.PackLimit = limit
				const capacity = 32 << 30
				root, err := w.Empty(t.Context(), capacity, fanout)
				if err != nil {
					t.Fatal(err)
				}
				rng := rand.New(rand.NewSource(77))
				expected := map[uint64]byte{}
				for cut := 0; cut < 4; cut++ {
					changes := map[uint64][]byte{}
					for range 96 {
						block := uint64(rng.Intn(512)) * 8192
						value := byte(cut + 1)
						if rng.Intn(4) == 0 {
							value = 0
						}
						changes[block] = bytes.Repeat([]byte{value}, blockformat.BlockSize)
						expected[block] = value
					}
					root, err = w.Capture(t.Context(), root, capacity, changes)
					if err != nil {
						t.Fatal(err)
					}
					for block, value := range expected {
						readWritten(t, w, root, block, value)
					}
				}
				for digest := range sink.objects {
					body, err := sink.store.Get(t.Context(), fmt.Sprintf("sha256:%x", digest))
					if err != nil {
						t.Fatal(err)
					}
					raw, err := io.ReadAll(body)
					closeErr := body.Close()
					if err != nil || closeErr != nil || sha256.Sum256(raw) != digest {
						t.Fatalf("captured object changed: read=%v close=%v", err, closeErr)
					}
				}
			})
		}
	}
}
