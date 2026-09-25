package computerkey

import (
	"bytes"
	"context"
	"testing"
)

func TestLocalEnvelopeBindsScopeAndVersion(t *testing.T) {
	l, err := NewLocal("operator-key", bytes.Repeat([]byte{9}, Size))
	if err != nil {
		t.Fatal(err)
	}
	plain := bytes.Repeat([]byte{42}, Size)
	e, err := l.Wrap(t.Context(), "computer-a", "version-a", plain)
	if err != nil {
		t.Fatal(err)
	}
	got, err := l.Unwrap(t.Context(), "computer-a", "version-a", e)
	if err != nil || !bytes.Equal(got, plain) {
		t.Fatalf("roundtrip: %v", err)
	}
	again, err := l.Wrap(t.Context(), "computer-a", "version-a", plain)
	if err != nil {
		t.Fatal(err)
	}
	if bytes.Equal(e.Ciphertext, again.Ciphertext) {
		t.Fatal("wrapping nonce reused")
	}
	for _, identity := range [][2]string{{"computer-b", "version-a"}, {"computer-a", "version-b"}} {
		if _, err = l.Unwrap(t.Context(), identity[0], identity[1], e); err == nil {
			t.Fatal("foreign envelope accepted")
		}
	}
	for _, mutate := range []func(*Envelope){func(e *Envelope) { e.WrappingKeyID = "other" }, func(e *Envelope) { e.Ciphertext[0] ^= 1 }, func(e *Envelope) { e.Ciphertext = e.Ciphertext[:len(e.Ciphertext)-1] }} {
		bad := Envelope{e.WrappingKeyID, bytes.Clone(e.Ciphertext)}
		mutate(&bad)
		if _, err = l.Unwrap(t.Context(), "computer-a", "version-a", bad); err == nil {
			t.Fatal("invalid envelope accepted")
		}
	}
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	if _, err = l.Wrap(ctx, "computer-a", "version-a", plain); err == nil {
		t.Fatal("cancelled wrap")
	}
	if _, err = l.Unwrap(ctx, "computer-a", "version-a", e); err == nil {
		t.Fatal("cancelled unwrap")
	}
}
