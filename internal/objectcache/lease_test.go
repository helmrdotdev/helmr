package objectcache

import (
	"bytes"
	"context"
	"sync"
	"testing"
	"testing/synctest"
)

func TestLeaseExactAuthorityAndIndependentOffsets(t *testing.T) {
	c := newCache(t, 16384)
	d := seed(t, c, []byte("abcdefgh"))
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	wrongSize, wrongMedia := d, d
	wrongSize.SizeBytes++
	wrongMedia.MediaType = "other"
	for _, lease := range []*ReadLease{c.Lease(ctx, "test", d), c.Lease(t.Context(), "other", d), c.Lease(t.Context(), "test", wrongSize), c.Lease(t.Context(), "test", wrongMedia), (*Cache)(nil).Lease(t.Context(), "test", d)} {
		if lease != nil {
			lease.Close()
			t.Fatal("unauthorized lease")
		}
	}
	if c.entries[key{"test", d.Digest}].pins != 0 {
		t.Fatal("rejected lease retained pin")
	}
	a, b := c.Lease(t.Context(), "test", d), c.Lease(t.Context(), "test", d)
	if a == nil || b == nil {
		t.Fatal("exact hit missing")
	}
	defer a.Close()
	defer b.Close()
	var wg sync.WaitGroup
	for i, lease := range []*ReadLease{a, b} {
		wg.Go(func() {
			for range 100 {
				buf := make([]byte, 4)
				if n, err := lease.ReadAt(buf, int64(i*4)); err != nil || n != 4 || string(buf) != []string{"abcd", "efgh"}[i] {
					t.Errorf("read %q %d %v", buf, n, err)
					return
				}
			}
		})
	}
	wg.Wait()
}

func TestLeasePinsUntilClose(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		c := newCache(t, 16384)
		releaseOthers := pinOtherEntries(t, c)
		d := seed(t, c, []byte("leased"))
		lease := c.Lease(t.Context(), "test", d)
		replacement := []byte("replacement")
		if ok, err := c.Seed(t.Context(), "test", descriptor(replacement), bytes.NewReader(replacement)); ok || err != nil {
			t.Fatalf("evicted lease %v %v", ok, err)
		}
		releaseOthers()
		closed := make(chan struct{})
		go func() {
			if err := c.Close(); err != nil {
				t.Error(err)
			}
			close(closed)
		}()
		synctest.Wait()
		select {
		case <-closed:
			t.Fatal("closed with live lease")
		default:
		}
		buf := make([]byte, 6)
		if _, err := lease.ReadAt(buf, 0); err != nil || string(buf) != "leased" {
			t.Fatalf("read during close: %q %v", buf, err)
		}
		if err := lease.Close(); err != nil {
			t.Fatal(err)
		}
		if err := lease.Close(); err != nil {
			t.Fatal(err)
		}
		<-closed
	})
}
