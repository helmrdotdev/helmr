package disk

import (
	"bytes"
	"container/list"
	"context"
	"errors"
	"io"
	"sync"

	"github.com/helmrdotdev/helmr/internal/disk/blockformat"
)

const versionReadCacheBytes = 8 << 20
const versionReadCacheEntries = 4096

type versionRangeKey struct {
	digest               string
	size, offset, length int64
}

type versionCachedRange struct {
	key  versionRangeKey
	data []byte
}

// versionRangeCache retains only immutable ciphertext for one local owner.
// Callers still authenticate every returned page/frame. Publication verification
// uses the uncached remote source, independently of these disposable read copies.
type versionRangeCache struct {
	source  blockformat.RangeSource
	mu      sync.Mutex
	entries map[versionRangeKey]*list.Element
	recent  list.List
	bytes   int
}

func newVersionRangeCache(source blockformat.RangeSource) *versionRangeCache {
	return &versionRangeCache{source: source, entries: make(map[versionRangeKey]*list.Element)}
}

func (c *versionRangeCache) GetRange(ctx context.Context, digest string, size, offset, length int64) (io.ReadCloser, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	// Large sequential reads already amortize transport overhead. Keep small
	// index pages, segment headers and frequently requested block ranges bounded.
	if length <= 0 || length > 64<<10 || offset < 0 || size < length || offset > size-length {
		return c.source.GetRange(ctx, digest, size, offset, length)
	}
	key := versionRangeKey{digest, size, offset, length}
	c.mu.Lock()
	if e := c.entries[key]; e != nil {
		c.recent.MoveToFront(e)
		data := e.Value.(versionCachedRange).data
		c.mu.Unlock()
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		return io.NopCloser(bytes.NewReader(data)), nil
	}
	c.mu.Unlock()
	body, err := c.source.GetRange(ctx, digest, size, offset, length)
	if err != nil {
		return nil, err
	}
	if body == nil {
		return nil, errors.New("missing version range response")
	}
	data, readErr := io.ReadAll(io.LimitReader(body, length+1))
	if err := errors.Join(readErr, body.Close(), ctx.Err()); err != nil {
		return nil, err
	}
	if int64(len(data)) != length {
		return nil, errors.New("version range response size mismatch")
	}
	c.mu.Lock()
	if c.entries[key] == nil {
		for c.bytes+cap(data) > versionReadCacheBytes || len(c.entries) >= versionReadCacheEntries {
			e := c.recent.Back()
			old := e.Value.(versionCachedRange)
			delete(c.entries, old.key)
			c.bytes -= cap(old.data)
			c.recent.Remove(e)
		}
		c.entries[key] = c.recent.PushFront(versionCachedRange{key, data})
		c.bytes += cap(data)
	}
	c.mu.Unlock()
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	return io.NopCloser(bytes.NewReader(data)), nil
}

func (c *versionRangeCache) clear() {
	c.mu.Lock()
	defer c.mu.Unlock()
	clear(c.entries)
	c.recent.Init()
	c.bytes = 0
}
