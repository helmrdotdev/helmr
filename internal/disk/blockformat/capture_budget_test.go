package blockformat_test

import (
	"bytes"
	"context"
	"fmt"
	"testing"

	"github.com/helmrdotdev/helmr/internal/disk/blockformat"
)

type captureByteCounter struct {
	blockformat.ObjectSink
	bytes int64
}

func (s *captureByteCounter) StoreObject(ctx context.Context, digest [32]byte, raw []byte) error {
	s.bytes += int64(len(raw))
	return s.ObjectSink.StoreObject(ctx, digest, raw)
}

func TestCaptureBudgetBoundsDenseSparseAndExistingReferences(t *testing.T) {
	for _, fanout := range []int{64, 256} {
		for _, sparse := range []bool{false, true} {
			t.Run(fmt.Sprintf("fanout%d-sparse%t", fanout, sparse), func(t *testing.T) {
				writer, _ := writerFixture(t)
				capacity := int64(blockformat.MaxBlocks) * blockformat.BlockSize
				root, err := writer.Empty(t.Context(), capacity, fanout)
				if err != nil {
					t.Fatal(err)
				}
				// Separate historical segments populate references that new metadata must
				// carry even when only a subset of their data changes in the next cut.
				for i := range fanout {
					root, err = writer.Capture(t.Context(), root, capacity, map[uint64][]byte{uint64(i): bytes.Repeat([]byte{byte(i%250 + 1)}, 4096)})
					if err != nil {
						t.Fatal(err)
					}
				}
				shape, err := blockformat.ReadRoot(t.Context(), writer.Source, writer.Scope, writer.Keys, root)
				if err != nil {
					t.Fatal(err)
				}
				reservation, err := blockformat.NewCaptureBudget(shape, writer.Scope, writer.Keys)
				if err != nil {
					t.Fatal(err)
				}
				changes := make(map[uint64][]byte)
				blocks := make([]uint64, 0, blockformat.MaxChangedBlocks)
				for i := range blockformat.MaxChangedBlocks {
					block := uint64(i)
					if sparse {
						block *= uint64(blockformat.MaxBlocks / blockformat.MaxChangedBlocks)
					}
					blocks = append(blocks, block)
					changes[block] = bytes.Repeat([]byte{13}, 4096)
				}
				counter := &captureByteCounter{ObjectSink: writer.Sink}
				writer.Sink = counter
				if _, err = writer.Capture(t.Context(), root, capacity, changes); err != nil {
					t.Fatal(err)
				}
				if bound := reservation.Bytes(blocks); counter.bytes > bound {
					t.Fatalf("encoded=%d exceeds reserved=%d", counter.bytes, bound)
				} else {
					t.Logf("encoded=%d reserved=%d", counter.bytes, bound)
				}
			})
		}
	}
}
