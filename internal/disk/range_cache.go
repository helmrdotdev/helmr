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
const segmentReadWindowBytes = 1 << 20
const segmentReadCacheBytes = 4 << 20

type versionRangeKey struct {
	digest               string
	size, offset, length int64
}

type versionCachedRange struct {
	key    versionRangeKey
	data   []byte
	window bool
}

// versionRangeCache retains only immutable ciphertext for one local owner.
// Callers still authenticate every returned page/frame. These disposable read
// copies do not establish publication authority.
type versionRangeCache struct {
	source      blockformat.RangeSource
	mu          sync.Mutex
	entries     map[versionRangeKey]*list.Element
	recent      list.List
	bytes       int
	windowBytes int
}

func newVersionRangeCache(source blockformat.RangeSource) *versionRangeCache {
	return &versionRangeCache{source: source, entries: make(map[versionRangeKey]*list.Element)}
}

func (c *versionRangeCache) GetRange(ctx context.Context, digest string, size, offset, length int64) (io.ReadCloser, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	// Keep complete metadata packs and small segment ranges within one budget.
	if length <= 0 || (length > 64<<10 && (offset != 0 || length != size || size > 4<<20)) || offset < 0 || size < length || offset > size-length {
		return c.source.GetRange(ctx, digest, size, offset, length)
	}
	return c.getCachedRange(ctx, digest, size, offset, length, false)
}

func (c *versionRangeCache) getCachedRange(ctx context.Context, digest string, size, offset, length int64, window bool) (io.ReadCloser, error) {
	key := versionRangeKey{digest, size, offset, length}
	c.mu.Lock()
	// A full-pack fetch also serves every later page in that exact object.
	if full := c.entries[versionRangeKey{digest, size, 0, size}]; full != nil {
		c.recent.MoveToFront(full)
		data := full.Value.(versionCachedRange).data[offset : offset+length]
		c.mu.Unlock()
		return io.NopCloser(bytes.NewReader(data)), nil
	}
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
		for window && c.windowBytes+cap(data) > segmentReadCacheBytes {
			e := c.recent.Back()
			for !e.Value.(versionCachedRange).window {
				e = e.Prev()
			}
			c.remove(e)
		}
		for c.bytes+cap(data) > versionReadCacheBytes || len(c.entries) >= versionReadCacheEntries {
			c.remove(c.recent.Back())
		}
		c.entries[key] = c.recent.PushFront(versionCachedRange{key: key, data: data, window: window})
		c.bytes += cap(data)
		if window {
			c.windowBytes += cap(data)
		}
	}
	c.mu.Unlock()
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	return io.NopCloser(bytes.NewReader(data)), nil
}

// remove is called with mu held. Outstanding readers retain their immutable bytes.
func (c *versionRangeCache) remove(e *list.Element) {
	old := e.Value.(versionCachedRange)
	delete(c.entries, old.key)
	c.bytes -= cap(old.data)
	if old.window {
		c.windowBytes -= cap(old.data)
	}
	c.recent.Remove(e)
}

// GetSegmentRange amortizes neighboring frame reads within one bounded window.
// Cross-window requests retain exact range semantics.
func (c *versionRangeCache) GetSegmentRange(ctx context.Context, digest string, size, offset, length int64) (io.ReadCloser, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if offset < 0 || length <= 0 || size < length || offset > size-length {
		return nil, errors.New("invalid cached segment range")
	}
	start := offset / segmentReadWindowBytes * segmentReadWindowBytes
	windowLength := min(int64(segmentReadWindowBytes), size-start)
	if length > windowLength-(offset-start) {
		return c.GetRange(ctx, digest, size, offset, length)
	}
	r, err := c.getCachedRange(ctx, digest, size, start, windowLength, true)
	if err != nil {
		return nil, err
	}
	if _, err = io.CopyN(io.Discard, r, offset-start); err != nil {
		return nil, errors.Join(err, r.Close())
	}
	return &packRangeReader{Reader: io.LimitReader(r, length), Closer: r}, nil
}

func (c *versionRangeCache) clear() {
	c.mu.Lock()
	defer c.mu.Unlock()
	clear(c.entries)
	c.recent.Init()
	c.bytes = 0
	c.windowBytes = 0
}

// GetPackRange explicitly fetches a bounded metadata pack. Segment frame
// read-ahead is separate and requires GetSegmentRange.
func (c *versionRangeCache) GetPackRange(ctx context.Context, digest string, size, offset, length int64) (io.ReadCloser, error) {
	if size < 8 || size > 4<<20 || offset < 0 || length <= 0 || offset > size-length {
		return nil, errors.New("invalid cached pack range")
	}
	r, err := c.GetRange(ctx, digest, size, 0, size)
	if err != nil {
		return nil, err
	}
	if _, err = io.CopyN(io.Discard, r, offset); err != nil {
		return nil, errors.Join(err, r.Close())
	}
	return &packRangeReader{Reader: io.LimitReader(r, length), Closer: r}, nil
}

type packRangeReader struct {
	io.Reader
	io.Closer
}
