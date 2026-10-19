package objectcache

import (
	"bytes"
	"context"
	"errors"
	"io"
	"os"
	"path/filepath"
	"testing"
	"testing/synctest"

	"github.com/helmrdotdev/helmr/internal/cas"
	"github.com/helmrdotdev/helmr/internal/sha256sum"
)

func descriptor(data []byte) cas.Descriptor {
	return cas.Descriptor{Digest: sha256sum.DigestBytes(data), SizeBytes: int64(len(data)), MediaType: "application/octet-stream"}
}
func newCache(t *testing.T, limit int64) *Cache {
	t.Helper()
	c, err := Open(filepath.Join(t.TempDir(), "cache"), limit)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := c.Close(); err != nil {
			t.Error(err)
		}
	})
	return c
}
func seed(t *testing.T, c *Cache, data []byte) cas.Descriptor {
	t.Helper()
	d := descriptor(data)
	if ok, err := c.Seed(t.Context(), "test", d, bytes.NewReader(data)); !ok || err != nil {
		t.Fatalf("seed: %v %v", ok, err)
	}
	return d
}
func TestPinsBudgetAndNamespace(t *testing.T) {
	c := newCache(t, 4096)
	data := []byte("retained")
	d := seed(t, c, data)
	r, _, ok := c.open("test", d.Digest)
	if !ok {
		t.Fatal("missing seed")
	}
	if _, _, ok := c.open("other", d.Digest); ok {
		t.Fatal("cross namespace hit")
	}
	if _, err := r.WriteAt([]byte("x"), 0); err == nil {
		t.Fatal("cache descriptor writable")
	}
	other := []byte("replacement")
	if ok, err := c.Seed(t.Context(), "test", descriptor(other), bytes.NewReader(other)); ok || err != nil {
		t.Fatalf("evicted pinned data: %v %v", ok, err)
	}
	got, err := io.ReadAll(r)
	if err != nil || !bytes.Equal(got, data) {
		t.Fatalf("pinned read: %q %v", got, err)
	}
	if err := r.Close(); err != nil {
		t.Fatal(err)
	}
	seed(t, c, other)
	if _, _, ok := c.open("test", d.Digest); ok {
		t.Fatal("old entry not evicted")
	}
	if c.used != 4096 {
		t.Fatalf("physical charge %d", c.used)
	}
}
func TestFailedSeedsReleaseBudget(t *testing.T) {
	c := newCache(t, 4096)
	for _, data := range [][]byte{[]byte("bad"), []byte("good-extra"), []byte("goo")} {
		if ok, err := c.Seed(t.Context(), "test", descriptor([]byte("good")), bytes.NewReader(data)); ok || !errors.Is(err, cas.ErrDigestMismatch) {
			t.Fatalf("bad seed: %v %v", ok, err)
		}
		if c.used != 0 {
			t.Fatalf("leaked budget %d", c.used)
		}
	}
	entries, err := os.ReadDir(c.directory)
	if err != nil || len(entries) != 0 {
		t.Fatalf("leaked files: %v %v", entries, err)
	}
}

type blockedReader struct {
	started chan struct{}
	resume  chan struct{}
	source  io.Reader
}

func (r blockedReader) Read(p []byte) (int, error) {
	select {
	case r.started <- struct{}{}:
	default:
	}
	<-r.resume
	return r.source.Read(p)
}
func TestPendingBudgetCancellationAndClose(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		c := newCache(t, 4096)
		ctx, cancel := context.WithCancel(t.Context())
		started, resume, done := make(chan struct{}, 1), make(chan struct{}), make(chan struct{})
		go func() {
			defer close(done)
			_, err := c.Seed(ctx, "test", descriptor([]byte("one")), blockedReader{started, resume, bytes.NewReader([]byte("one"))})
			if !errors.Is(err, context.Canceled) {
				t.Errorf("seed cancellation: %v", err)
			}
		}()
		<-started
		if ok, err := c.Seed(t.Context(), "test", descriptor([]byte("two")), bytes.NewReader([]byte("two"))); ok || err != nil {
			t.Fatalf("pending bytes not charged: %v %v", ok, err)
		}
		closed := false
		go func() {
			if err := c.Close(); err != nil {
				t.Error(err)
			}
			closed = true
		}()
		synctest.Wait()
		if closed {
			t.Fatal("close did not join fill")
		}
		cancel()
		close(resume)
		<-done
		synctest.Wait()
		if !closed || c.used != 0 {
			t.Fatal("close did not finish cleanly")
		}
	})
}
func TestExclusiveOwnerAndRestartCleanup(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "cache")
	c, err := Open(dir, 0)
	if err != nil {
		t.Fatal(err)
	}
	if err := c.Enable(4096); err != nil {
		t.Fatal(err)
	}
	d := seed(t, c, []byte("one"))
	if other, err := Open(dir, 4096); err == nil {
		other.Close()
		t.Fatal("second owner admitted")
	}
	r, _, ok := c.open("test", d.Digest)
	if !ok {
		t.Fatal("conflicting owner deleted bytes")
	}
	r.Close()
	// Model a process exit: its lock closes, named disposable bytes remain.
	if err := c.lock.Close(); err != nil {
		t.Fatal(err)
	}
	c.lock = nil
	restarted, err := Open(dir, 0)
	if err != nil {
		t.Fatal(err)
	}
	defer restarted.Close()
	files, err := os.ReadDir(restarted.directory)
	if err != nil || len(files) != 0 {
		t.Fatalf("stale cache retained: %v %v", files, err)
	}
}
