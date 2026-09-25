package cas

import (
	"bytes"
	"context"
	"crypto/sha256"
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/helmrdotdev/helmr/internal/sha256sum"
)

func TestImmutableObjectStagingRejectsCorruptExistingBytes(t *testing.T) {
	root := t.TempDir()
	store, err := NewFile(root)
	if err != nil {
		t.Fatal(err)
	}
	raw := []byte("immutable bytes")
	digest := sha256.Sum256(raw)
	if err = store.StoreObject(t.Context(), digest, raw); err != nil {
		t.Fatal(err)
	}
	path, _, err := store.path(sha256sum.FormatDigest(digest[:]))
	if err != nil {
		t.Fatal(err)
	}
	if err = os.Chmod(path, 0600); err != nil {
		t.Fatal(err)
	}
	corrupt := bytes.Repeat([]byte{'x'}, len(raw))
	if err = os.WriteFile(path, corrupt, 0600); err != nil {
		t.Fatal(err)
	}
	if err = store.StoreObject(t.Context(), digest, raw); !errors.Is(err, ErrDigestMismatch) {
		t.Fatalf("corrupt existing object: %v", err)
	}
	got, err := os.ReadFile(path)
	if err != nil || !bytes.Equal(got, corrupt) {
		t.Fatal("failed staging replaced existing bytes")
	}
	matches, err := filepath.Glob(filepath.Join(filepath.Dir(path), ".object-*"))
	if err != nil || len(matches) != 0 {
		t.Fatal("failed staging leaked temporary files")
	}
}
func TestImmutableObjectStagingRejectsInvalidInput(t *testing.T) {
	root := t.TempDir()
	store, err := NewFile(root)
	if err != nil {
		t.Fatal(err)
	}
	raw := []byte("bytes")
	digest := sha256.Sum256(raw)
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	if err = store.StoreObject(ctx, digest, raw); !errors.Is(err, context.Canceled) {
		t.Fatalf("cancel: %v", err)
	}
	if err = store.StoreObject(t.Context(), digest, []byte("changed")); !errors.Is(err, ErrDigestMismatch) {
		t.Fatalf("digest: %v", err)
	}
	entries, err := os.ReadDir(root)
	if err != nil || len(entries) != 0 {
		t.Fatal("invalid input created files")
	}
}
