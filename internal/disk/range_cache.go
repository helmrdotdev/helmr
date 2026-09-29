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

const generationReadCacheBytes = 8 << 20
const generationReadCacheEntries = 4096

type generationRangeKey struct {
	digest               string
	size, offset, length int64
}

type generationCachedRange struct {
	key  generationRangeKey
	data []byte
}

// generationRangeCache retains only immutable ciphertext for one local owner.
// Callers still authenticate every returned page/frame. Publication verification
// uses the uncached remote source, independently of these disposable read copies.
type generationRangeCache struct {
	source  blockformat.RangeSource
	mu      sync.Mutex
	entries map[generationRangeKey]*list.Element
	recent  list.List
	bytes   int
}

func newGenerationRangeCache(source blockformat.RangeSource) *generationRangeCache {
	return &generationRangeCache{source: source, entries: make(map[generationRangeKey]*list.Element)}
}

func (c *generationRangeCache) GetRange(ctx context.Context, digest string, size, offset, length int64) (io.ReadCloser, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	// Large sequential reads already amortize transport overhead. Keep small
	// index pages, segment headers and frequently requested block ranges bounded.
	if length <= 0 || length > 64<<10 || offset < 0 || size < length || offset > size-length {
		return c.source.GetRange(ctx, digest, size, offset, length)
	}
	key := generationRangeKey{digest, size, offset, length}
	c.mu.Lock()
	if e := c.entries[key]; e != nil {
		c.recent.MoveToFront(e)
		data := e.Value.(generationCachedRange).data
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
		return nil, errors.New("missing generation range response")
	}
	data, readErr := io.ReadAll(io.LimitReader(body, length+1))
	if err := errors.Join(readErr, body.Close(), ctx.Err()); err != nil {
		return nil, err
	}
	if int64(len(data)) != length {
		return nil, errors.New("generation range response size mismatch")
	}
	c.mu.Lock()
	if c.entries[key] == nil {
		for c.bytes+cap(data) > generationReadCacheBytes || len(c.entries) >= generationReadCacheEntries {
			e := c.recent.Back()
			old := e.Value.(generationCachedRange)
			delete(c.entries, old.key)
			c.bytes -= cap(old.data)
			c.recent.Remove(e)
		}
		c.entries[key] = c.recent.PushFront(generationCachedRange{key, data})
		c.bytes += cap(data)
	}
	c.mu.Unlock()
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	return io.NopCloser(bytes.NewReader(data)), nil
}

func (c *generationRangeCache) clear() {
	c.mu.Lock()
	defer c.mu.Unlock()
	clear(c.entries)
	c.recent.Init()
	c.bytes = 0
}
