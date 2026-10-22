package objectcache

import (
	"bytes"
	"context"
	"errors"
	"testing"
	"testing/synctest"
)

func TestResizeRetainsPinnedBytesUntilReadersClose(t *testing.T) {
	c := newCache(t, 16384)
	d := seed(t, c, []byte("pinned"))
	lease := c.Lease(t.Context(), "test", d)
	if lease == nil {
		t.Fatal("missing reader")
	}
	remaining, err := c.Resize(0)
	if err != nil || remaining != 4096 {
		t.Fatalf("resize freed live bytes: %d %v", remaining, err)
	}
	if again := c.Lease(t.Context(), "test", d); again != nil {
		again.Close()
		t.Fatal("zero cache admitted a new reader")
	}
	if err := lease.Close(); err != nil {
		t.Fatal(err)
	}
	if remaining, err = c.Resize(0); err != nil || remaining != 0 {
		t.Fatalf("joined reader retained charge: %d %v", remaining, err)
	}
	if _, err := c.Resize(16384); err != nil {
		t.Fatal(err)
	}
	seed(t, c, []byte("new policy"))
}

func TestResizeCancelsFillWithoutInventingRelease(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		c := newCache(t, 16384)
		started, resume, done := make(chan struct{}, 1), make(chan struct{}), make(chan error, 1)
		go func() {
			_, err := c.Seed(t.Context(), "test", descriptor([]byte("one")), blockedReader{started, resume, bytes.NewReader([]byte("one"))})
			done <- err
		}()
		<-started
		if remaining, err := c.Resize(0); err != nil || remaining != 4096 {
			t.Fatalf("pending fill lost charge: %d %v", remaining, err)
		}
		close(resume)
		if err := <-done; !errors.Is(err, context.Canceled) {
			t.Fatalf("resize did not cancel fill: %v", err)
		}
		if remaining, err := c.Resize(0); err != nil || remaining != 0 {
			t.Fatalf("joined fill retained charge: %d %v", remaining, err)
		}
	})
}
