package computer

import (
	"bytes"
	"context"
	"errors"
	"os"
	"testing"

	"github.com/helmrdotdev/helmr/internal/computer/blockformat"
)

func TestInitialGenerationSparseDisk(t *testing.T) {
	disk, err := os.CreateTemp(t.TempDir(), "disk")
	if err != nil {
		t.Fatal(err)
	}
	defer disk.Close()
	const capacity = int64(32 << 30)
	if err = disk.Truncate(capacity); err != nil {
		t.Fatal(err)
	}
	// Cross encryption-block boundaries, with leading/interior/trailing holes
	// and explicitly allocated zero bytes. Readback must match the disk exactly.
	for _, offset := range []int64{4095, 4<<20 - 1, 16<<30 + 4095, capacity - 4097} {
		if _, err = disk.WriteAt([]byte{7, 9}, offset); err != nil {
			t.Fatal(err)
		}
	}
	if _, err = disk.WriteAt(make([]byte, 4096), 8<<30); err != nil {
		t.Fatal(err)
	}
	var scanned, lastEnd int64
	err = walkInitialDiskData(t.Context(), disk, capacity, func(offset, size int64) error {
		if offset < lastEnd || offset%4096 != 0 || size%4096 != 0 || size <= 0 || size > 4<<20 || offset+size > capacity {
			t.Fatalf("invalid or overlapping read: offset=%d size=%d", offset, size)
		}
		lastEnd = offset + size
		scanned += size
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	// This is local filesystem evidence, not a promise about how every
	// filesystem reports allocation. Unsupported seeking fails explicitly.
	if scanned == 0 || scanned > 1<<20 {
		t.Fatalf("sparse source did not skip holes: read %d of %d bytes", scanned, capacity)
	}
	t.Logf("extent scan: %d bytes of %d logical bytes", scanned, capacity)
	candidate, err := CaptureInitialGeneration(t.Context(), GenerationCapture{Disk: disk, Capacity: capacity, StagingParent: t.TempDir(), Scope: "scope", KeyID: "key", Key: bytes.Repeat([]byte{3}, 32), Fanout: 64, PackLimit: blockformat.MinPackLimit, MaxStagedBytes: 4 << 20, MaxObjects: 100})
	if err != nil {
		t.Fatal(err)
	}
	defer candidate.Close()
	tree, err := blockformat.OpenTree(t.Context(), candidate.store, "scope", map[string][]byte{"key": candidate.key}, candidate.root)
	if err != nil {
		t.Fatal(err)
	}
	for _, block := range []uint64{0, 1, 2, 1023, 1024, 1025, 8 << 18, 16 << 18, 16<<18 + 1, uint64(capacity/4096 - 2), uint64(capacity/4096 - 1)} {
		want := make([]byte, 4096)
		if _, err = disk.ReadAt(want, int64(block)*4096); err != nil {
			t.Fatal(err)
		}
		got, err := tree.ReadBlock(t.Context(), block)
		if err != nil || !bytes.Equal(want, got) {
			t.Fatalf("block %d differs from sparse source: %v", block, err)
		}
	}
}

func TestInitialDiskDataFailure(t *testing.T) {
	disk, err := os.CreateTemp(t.TempDir(), "disk")
	if err != nil {
		t.Fatal(err)
	}
	defer disk.Close()
	if _, err = disk.Write(make([]byte, 4096)); err != nil {
		t.Fatal(err)
	}
	want := errors.New("capture failed")
	if err = walkInitialDiskData(t.Context(), disk, 4096, func(_, _ int64) error { return want }); !errors.Is(err, want) {
		t.Fatalf("visit failure lost: %v", err)
	}
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	if err = walkInitialDiskData(ctx, disk, 4096, func(_, _ int64) error { t.Fatal("visit after cancel"); return nil }); !errors.Is(err, context.Canceled) {
		t.Fatalf("cancellation lost: %v", err)
	}
	if err = disk.Close(); err != nil {
		t.Fatal(err)
	}
	if err = walkInitialDiskData(t.Context(), disk, 4096, func(_, _ int64) error { t.Fatal("visit invalid source"); return nil }); err == nil {
		t.Fatal("invalid source accepted as empty disk")
	}
}
