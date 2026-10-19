package disk

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"sync"
	"testing"

	"github.com/helmrdotdev/helmr/internal/disk/blockformat"
)

type cacheRangeSource func(context.Context, string, int64, int64, int64) (io.ReadCloser, error)

func (f cacheRangeSource) GetRange(ctx context.Context, digest string, size, offset, length int64) (io.ReadCloser, error) {
	return f(ctx, digest, size, offset, length)
}

func readCachedRange(t *testing.T, c *versionRangeCache, digest string, size, offset, length int64) []byte {
	t.Helper()
	r, err := c.GetRange(t.Context(), digest, size, offset, length)
	if err != nil {
		t.Fatal(err)
	}
	b, err := io.ReadAll(r)
	if err = errors.Join(err, r.Close()); err != nil {
		t.Fatal(err)
	}
	return b
}

func TestVersionRangeCacheExactIdentityAndIndependentReaders(t *testing.T) {
	calls := 0
	c := newVersionRangeCache(cacheRangeSource(func(_ context.Context, _ string, _, _, length int64) (io.ReadCloser, error) {
		calls++
		return io.NopCloser(bytes.NewReader(bytes.Repeat([]byte{byte(calls)}, int(length)))), nil
	}))
	for range 2 {
		b := readCachedRange(t, c, "a", 100, 2, 4)
		if !bytes.Equal(b, []byte{1, 1, 1, 1}) {
			t.Fatal(b)
		}
		b[0] = 99
	}
	if calls != 1 {
		t.Fatalf("calls=%d", calls)
	}
	for _, k := range []versionRangeKey{{"b", 100, 2, 4}, {"a", 101, 2, 4}, {"a", 100, 3, 4}, {"a", 100, 2, 5}} {
		readCachedRange(t, c, k.digest, k.size, k.offset, k.length)
	}
	if calls != 5 {
		t.Fatalf("identity collapsed: %d", calls)
	}
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	if _, err := c.GetRange(ctx, "a", 100, 2, 4); !errors.Is(err, context.Canceled) {
		t.Fatal(err)
	}
}

type cacheCloseError struct{ io.Reader }

func (cacheCloseError) Close() error { return errors.New("close failed") }

func TestVersionRangeCacheDoesNotRetainFailedResponses(t *testing.T) {
	for _, failure := range []string{"short", "long", "close", "cancel"} {
		t.Run(failure, func(t *testing.T) {
			calls := 0
			ctx, cancel := context.WithCancel(t.Context())
			defer cancel()
			c := newVersionRangeCache(cacheRangeSource(func(_ context.Context, _ string, _, _, _ int64) (io.ReadCloser, error) {
				calls++
				if calls > 1 {
					return io.NopCloser(bytes.NewReader([]byte("good"))), nil
				}
				switch failure {
				case "short":
					return io.NopCloser(bytes.NewReader([]byte("bad"))), nil
				case "long":
					return io.NopCloser(bytes.NewReader([]byte("extra"))), nil
				case "close":
					return cacheCloseError{bytes.NewReader([]byte("good"))}, nil
				default:
					cancel()
					return io.NopCloser(bytes.NewReader([]byte("good"))), nil
				}
			}))
			if _, err := c.GetRange(ctx, "a", 4, 0, 4); err == nil {
				t.Fatal("failed response accepted")
			}
			if got := readCachedRange(t, c, "a", 4, 0, 4); string(got) != "good" || calls != 2 {
				t.Fatalf("got=%q calls=%d", got, calls)
			}
		})
	}
}

func TestVersionRangeCacheBoundsAndConcurrentReads(t *testing.T) {
	c := newVersionRangeCache(cacheRangeSource(func(_ context.Context, _ string, _, _, length int64) (io.ReadCloser, error) {
		return io.NopCloser(bytes.NewReader(make([]byte, length))), nil
	}))
	var wg sync.WaitGroup
	for worker := range 4 {
		wg.Go(func() {
			for i := range 1100 {
				readCachedRange(t, c, fmt.Sprintf("%d:%d", worker, i), 65536, 0, 65536)
			}
		})
	}
	wg.Wait()
	if c.bytes > versionReadCacheBytes || len(c.entries) > versionReadCacheEntries {
		t.Fatalf("unbounded bytes=%d entries=%d", c.bytes, len(c.entries))
	}
	for i := range versionReadCacheEntries + 1 {
		readCachedRange(t, c, fmt.Sprint(i), 1, 0, 1)
	}
	if len(c.entries) > versionReadCacheEntries {
		t.Fatal("entry limit exceeded")
	}
	c.clear()
	if c.bytes != 0 || len(c.entries) != 0 || c.recent.Len() != 0 {
		t.Fatal("cache retained after close")
	}
}

func TestVersionRangeCacheSharesCompletePackAcrossPages(t *testing.T) {
	calls := 0
	raw := bytes.Repeat([]byte{71}, 1<<20)
	c := newVersionRangeCache(cacheRangeSource(func(_ context.Context, _ string, size, offset, length int64) (io.ReadCloser, error) {
		calls++
		if size != 1<<20 || offset != 0 || length != size {
			t.Errorf("metadata fetch was not a complete bounded pack: %d %d %d", size, offset, length)
		}
		return io.NopCloser(bytes.NewReader(raw)), nil
	}))
	for _, offset := range []int64{32, 12345, 500000} {
		reader, err := c.GetPackRange(t.Context(), "pack", 1<<20, offset, 4096)
		if err != nil {
			t.Fatal(err)
		}
		b, err := io.ReadAll(reader)
		if err = errors.Join(err, reader.Close()); err != nil {
			t.Fatal(err)
		}
		if !bytes.Equal(b, raw[offset:offset+4096]) {
			t.Fatal("wrong pack page")
		}
	}
	if calls != 1 {
		t.Fatalf("pack downloaded %d times", calls)
	}
	if got := readCachedRange(t, c, "pack", 1<<20, 99, 100); !bytes.Equal(got, raw[99:199]) || calls != 1 {
		t.Fatal("range did not reuse full pack")
	}
}

func TestSegmentReadWindows(t *testing.T) {
	var calls []versionRangeKey
	c := newVersionRangeCache(cacheRangeSource(func(_ context.Context, d string, size, offset, length int64) (io.ReadCloser, error) {
		calls = append(calls, versionRangeKey{d, size, offset, length})
		b := make([]byte, length)
		for i := range b {
			b[i] = byte((offset + int64(i)) % 251)
		}
		return io.NopCloser(bytes.NewReader(b)), nil
	}))
	read := func(d string, size, offset, length int64) {
		t.Helper()
		r, err := c.GetSegmentRange(t.Context(), d, size, offset, length)
		if err != nil {
			t.Fatal(err)
		}
		b, err := io.ReadAll(r)
		if err = errors.Join(err, r.Close()); err != nil {
			t.Fatal(err)
		}
		if int64(len(b)) != length {
			t.Fatal(len(b))
		}
		for i, v := range b {
			if v != byte((offset+int64(i))%251) {
				t.Fatal("wrong slice")
			}
		}
	}
	const w = int64(segmentReadWindowBytes)
	read("a", 2*w+13, 100, 4096)
	read("a", 2*w+13, 8192, 4096)
	if len(calls) != 1 || calls[0].offset != 0 || calls[0].length != w {
		t.Fatal(calls)
	}
	read("a", 2*w+13, w-10, 20)
	if calls[1].offset != w-10 || calls[1].length != 20 {
		t.Fatal(calls)
	}
	read("a", 2*w+13, 2*w+3, 10)
	if calls[2].offset != 2*w || calls[2].length != 13 {
		t.Fatal(calls)
	}
	read("b", 2*w+13, 100, 4096)
	read("a", 2*w+14, 100, 4096)
	if len(calls) != 5 {
		t.Fatal(calls)
	}
	for _, k := range []versionRangeKey{{"a", 10, -1, 1}, {"a", 10, 0, 0}, {"a", 10, 9, 2}, {"a", 10, 0, 11}, {"a", 10, 1<<63 - 1, 1}} {
		if _, err := c.GetSegmentRange(t.Context(), k.digest, k.size, k.offset, k.length); err == nil {
			t.Fatal(k)
		}
	}
	if len(calls) != 5 {
		t.Fatal("invalid request fetched")
	}
}

func TestSegmentWindowReadersSurviveEvictionAndConcurrentBudget(t *testing.T) {
	c := newVersionRangeCache(cacheRangeSource(func(_ context.Context, _ string, _, _, length int64) (io.ReadCloser, error) {
		return io.NopCloser(bytes.NewReader(bytes.Repeat([]byte{37}, int(length)))), nil
	}))
	r, err := c.GetSegmentRange(t.Context(), "pinned", 2<<20, 100, 4096)
	if err != nil {
		t.Fatal(err)
	}
	var wg sync.WaitGroup
	for worker := range 4 {
		wg.Go(func() {
			for i := range 8 {
				r, err := c.GetSegmentRange(t.Context(), fmt.Sprintf("%d:%d", worker, i), 2<<20, 100, 4096)
				if err != nil {
					t.Error(err)
					return
				}
				if err := r.Close(); err != nil {
					t.Error(err)
				}
			}
		})
	}
	wg.Wait()
	if c.windowBytes > segmentReadCacheBytes || c.bytes > versionReadCacheBytes {
		t.Fatalf("bytes=%d windows=%d", c.bytes, c.windowBytes)
	}
	c.clear()
	b, err := io.ReadAll(r)
	if err = errors.Join(err, r.Close()); err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(b, bytes.Repeat([]byte{37}, 4096)) {
		t.Fatal("eviction changed open reader")
	}
	if c.windowBytes != 0 {
		t.Fatal("retained window accounting")
	}
}

func TestSegmentWindowReadAmplification(t *testing.T) {
	for _, sparse := range []bool{false, true} {
		t.Run(fmt.Sprint(sparse), func(t *testing.T) {
			calls, total := 0, int64(0)
			c := newVersionRangeCache(cacheRangeSource(func(_ context.Context, _ string, _, _, length int64) (io.ReadCloser, error) {
				calls++
				total += length
				return io.NopCloser(bytes.NewReader(make([]byte, length))), nil
			}))
			for i := range 32 {
				offset := int64(i * 4096)
				if sparse {
					offset = int64(i) * segmentReadWindowBytes
				}
				r, err := c.GetSegmentRange(t.Context(), "a", 64<<20, offset, 4096)
				if err != nil {
					t.Fatal(err)
				}
				r.Close()
			}
			want := 1
			if sparse {
				want = 32
			}
			if calls != want || total != int64(want)*segmentReadWindowBytes {
				t.Fatalf("calls=%d bytes=%d", calls, total)
			}
			t.Logf("requested=131072 remote_calls=%d remote_bytes=%d", calls, total)
		})
	}
}

func TestSegmentWindowAuthenticatesCachedNeighbor(t *testing.T) {
	key := bytes.Repeat([]byte{1}, 32)
	ref, raw, err := blockformat.Seal("scope", key, blockformat.Ref{Key: "key", Kind: blockformat.SegmentKind, Count: 2, Salt: [32]byte{4}}, [][]byte{bytes.Repeat([]byte{1}, 4096), bytes.Repeat([]byte{2}, 4096)})
	if err != nil {
		t.Fatal(err)
	}
	raw[len(raw)-1] ^= 1
	calls := 0
	c := newVersionRangeCache(cacheRangeSource(func(_ context.Context, _ string, _, offset, length int64) (io.ReadCloser, error) {
		calls++
		return io.NopCloser(bytes.NewReader(raw[offset : offset+length])), nil
	}))
	b, err := blockformat.ReadBlock(t.Context(), c, "scope", key, ref, 0)
	if err != nil || !bytes.Equal(b, bytes.Repeat([]byte{1}, 4096)) {
		t.Fatalf("first block: %v", err)
	}
	before := calls
	if _, err := blockformat.ReadBlock(t.Context(), c, "scope", key, ref, 1); err == nil {
		t.Fatal("cached corrupt neighbor accepted")
	}
	if calls != before {
		t.Fatal("neighbor did not use cached ciphertext")
	}
}
