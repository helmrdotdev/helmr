package blockformat_test

import (
	"bytes"
	"context"
	"crypto/sha256"
	"testing"

	"github.com/helmrdotdev/helmr/internal/cas"
	"github.com/helmrdotdev/helmr/internal/computer/blockformat"
)

func TestFileRangesAuthenticateRecords(t *testing.T) {
	store, err := cas.NewFile(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	key := bytes.Repeat([]byte{8}, 32)
	ref, raw, err := blockformat.Seal("scope", key, blockformat.Ref{Key: "key", Kind: blockformat.SegmentKind, Count: 2, Salt: [32]byte{4}}, [][]byte{bytes.Repeat([]byte{1}, 4096), bytes.Repeat([]byte{2}, 4096)})
	if err != nil {
		t.Fatal(err)
	}
	obj, err := store.Put(t.Context(), "application/octet-stream", bytes.NewReader(raw))
	if err != nil {
		t.Fatal(err)
	}
	got, err := blockformat.ReadBlock(t.Context(), store, "scope", key, ref, 1)
	if err != nil || !bytes.Equal(got, bytes.Repeat([]byte{2}, 4096)) {
		t.Fatalf("file segment read: %v", err)
	}
	if _, err := store.GetRange(t.Context(), obj.Digest, obj.SizeBytes+1, 0, 1); err == nil {
		t.Fatal("incorrect full size accepted")
	}
	body, err := store.GetRange(t.Context(), obj.Digest, obj.SizeBytes, 3, 4)
	if err != nil {
		t.Fatal(err)
	}
	if err = body.Close(); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(t.Context())
	body, err = store.GetRange(ctx, obj.Digest, obj.SizeBytes, 3, 4)
	if err != nil {
		t.Fatal(err)
	}
	defer body.Close()
	cancel()
	if _, err = body.Read(make([]byte, 4)); err == nil {
		t.Fatal("cancelled range continued")
	}
	page, raw, err := blockformat.Seal("scope", key, blockformat.Ref{Key: "key", Kind: blockformat.RootKind, Count: 1, Salt: [32]byte{5}}, [][]byte{[]byte("metadata")})
	if err != nil {
		t.Fatal(err)
	}
	pack := append(make([]byte, 64), raw...)
	if _, err = store.Put(t.Context(), "application/octet-stream", bytes.NewReader(pack)); err != nil {
		t.Fatal(err)
	}
	loc := blockformat.Locator{Pack: blockformat.PackRef{Digest: sha256.Sum256(pack), Size: int64(len(pack)), Rank: 2}, Page: page, Offset: 64}
	got, err = blockformat.ReadPage(t.Context(), store, "scope", key, loc)
	if err != nil || string(got) != "metadata" {
		t.Fatalf("file metadata read: %v", err)
	}
}
