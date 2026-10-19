package objectcache

import (
	"bytes"
	"context"
	"errors"
	"io"
	"os"
	"testing"
)

type rangeFallback func(context.Context, string, int64, int64, int64) (io.ReadCloser, error)

func (f rangeFallback) GetRange(ctx context.Context, d string, s, o, l int64) (io.ReadCloser, error) {
	return f(ctx, d, s, o, l)
}
func TestRangesKeepMissExactAndHitPrivate(t *testing.T) {
	c := newCache(t, 4096)
	data := []byte("immutable bytes")
	d := seed(t, c, data)
	failure := errors.New("raw miss")
	calls := 0
	fallback := rangeFallback(func(_ context.Context, digest string, size, offset, length int64) (io.ReadCloser, error) {
		calls++
		if digest != d.Digest || size != d.SizeBytes || offset != 2 || length != 3 {
			t.Fatal("miss widened or identity changed")
		}
		return nil, failure
	})
	ranges := c.Ranges("test", fallback)
	r, err := ranges.GetRange(t.Context(), d.Digest, d.SizeBytes, 2, 3)
	if err != nil {
		t.Fatal(err)
	}
	got, err := io.ReadAll(r)
	closeErr := r.Close()
	if err != nil || closeErr != nil || !bytes.Equal(got, data[2:5]) || calls != 0 {
		t.Fatalf("cache hit: %q %v %v", got, err, closeErr)
	}
	if _, err := c.Ranges("other", fallback).GetRange(t.Context(), d.Digest, d.SizeBytes, 2, 3); !errors.Is(err, failure) || calls != 1 {
		t.Fatalf("fallback: %v", err)
	}
}
func TestWholeReaderVerifiesTampering(t *testing.T) {
	c := newCache(t, 4096)
	d := seed(t, c, []byte("immutable"))
	e := c.entries[key{"test", d.Digest}]
	if err := os.Chmod(e.path, 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(e.path, []byte("corrupted"), 0600); err != nil {
		t.Fatal(err)
	}
	r, err := c.Reader("test", nil).Get(t.Context(), d.Digest)
	if err != nil {
		t.Fatal(err)
	}
	_, err = io.ReadAll(r)
	if err == nil || r.Close() == nil {
		t.Fatal("corrupt cached object accepted")
	}
}

func TestCancelledReaderCloseDoesNotDrain(t *testing.T) {
	c := newCache(t, 4096)
	d := seed(t, c, bytes.Repeat([]byte("x"), 4096))
	ctx, cancel := context.WithCancel(t.Context())
	r, err := c.Reader("test", nil).Get(ctx, d.Digest)
	if err != nil {
		t.Fatal(err)
	}
	var first [1]byte
	if _, err := r.Read(first[:]); err != nil {
		t.Fatal(err)
	}
	cancel()
	if err := r.Close(); !errors.Is(err, context.Canceled) {
		t.Fatalf("cancelled Close: %v", err)
	}
	if c.entries[key{"test", d.Digest}].pins != 0 {
		t.Fatal("cancelled reader retained pin")
	}
}
