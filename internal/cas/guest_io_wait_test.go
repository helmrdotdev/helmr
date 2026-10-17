package cas

import (
	"bytes"
	"context"
	"crypto/sha256"
	"errors"
	"testing"
)

func TestImmutableGuestIOWaitBracketsNativeCalls(t *testing.T) {
	store, err := NewFile(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	depth, calls := 0, 0
	ctx := WithGuestIOWait(t.Context(), func(waiting bool) {
		if waiting {
			depth++
			calls++
		} else {
			depth--
		}
		if depth < 0 || depth > 1 {
			t.Error("unbalanced native wait")
		}
	})
	raw := bytes.Repeat([]byte{7}, 4096)
	digest := sha256.Sum256(raw)
	for range 2 { // Existing-byte verification reads must also end their waits.
		if err := store.StoreObject(ctx, digest, raw); err != nil {
			t.Fatal(err)
		}
		if depth != 0 || calls < 6 {
			t.Fatalf("depth=%d calls=%d", depth, calls)
		}
	}
	calls = 0
	if err := store.StoreObject(ctx, [32]byte{}, raw); !errors.Is(err, ErrDigestMismatch) {
		t.Fatal(err)
	}
	if calls != 0 {
		t.Fatal("digest computation/rejection entered storage wait")
	}
	canceled, cancel := context.WithCancel(ctx)
	cancel()
	if err := store.StoreObject(canceled, digest, raw); !errors.Is(err, context.Canceled) {
		t.Fatal(err)
	}
	if calls != 0 {
		t.Fatal("canceled operation entered storage wait")
	}
}
