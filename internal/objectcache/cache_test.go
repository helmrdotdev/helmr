package objectcache

import (
	"bytes"
	"context"
	"errors"
	"fmt"
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
	c := newCache(t, 16384)
	pinOtherEntries(t, c)
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
	if c.used != 16384 {
		t.Fatalf("physical charge %d", c.used)
	}
}
func TestFailedSeedsReleaseBudget(t *testing.T) {
	c := newCache(t, 16384)
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
		c := newCache(t, 16384)
		releaseOthers := pinOtherEntries(t, c)
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
		releaseOthers()
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
func TestRestartCleanup(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "cache")
	c, err := Open(dir, 0)
	if err != nil {
		t.Fatal(err)
	}
	if err := c.Enable(16384); err != nil {
		t.Fatal(err)
	}
	seed(t, c, []byte("one"))
	// Model process exit: the prior owner is gone, leaving only disposable files.
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

func pinOtherEntries(t *testing.T, c *Cache) func() {
	t.Helper()
	var readers []io.Closer
	for i := 0; i < 3; i++ {
		d := seed(t, c, []byte(fmt.Sprintf("pinned-%d", i)))
		r, _, ok := c.open("test", d.Digest)
		if !ok {
			t.Fatal("missing seed")
		}
		readers = append(readers, r)
	}
	release := func() {
		for _, r := range readers {
			if err := r.Close(); err != nil {
				t.Error(err)
			}
		}
	}
	t.Cleanup(release)
	return release
}

func TestLargeSeedDoesNotEvictWorkingSet(t *testing.T) {
	c := newCache(t, 16384)
	d := seed(t, c, []byte("retained"))
	large := bytes.Repeat([]byte("x"), 4097)
	if ok, err := c.Seed(t.Context(), "test", descriptor(large), bytes.NewReader(large)); ok || err != nil {
		t.Fatalf("oversized admission: %v %v", ok, err)
	}
	r, _, ok := c.open("test", d.Digest)
	if !ok {
		t.Fatal("large declined seed evicted retained data")
	}
	r.Close()
}
