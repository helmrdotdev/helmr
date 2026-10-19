package objectcache

import (
	"context"
	"errors"
	"io"

	"github.com/helmrdotdev/helmr/internal/cas"
	"github.com/helmrdotdev/helmr/internal/disk/blockformat"
)

// Reader never fills on demand. Consumers still verify their own immutable copy.
func (c *Cache) Reader(namespace string, fallback cas.Reader) cas.Reader {
	if c == nil {
		return fallback
	}
	return cachedReader{c, namespace, fallback}
}

type cachedReader struct {
	cache     *Cache
	namespace string
	fallback  cas.Reader
}

func (r cachedReader) Stat(ctx context.Context, digest string) (cas.Object, error) {
	if err := ctx.Err(); err != nil {
		return cas.Object{}, err
	}
	if d, ok := r.cache.descriptor(r.namespace, digest); ok {
		return cas.Object{Digest: d.Digest, SizeBytes: d.SizeBytes, MediaType: d.MediaType}, nil
	}
	return r.fallback.Stat(ctx, digest)
}
func (r cachedReader) Get(ctx context.Context, digest string) (io.ReadCloser, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if body, _, ok := r.cache.open(r.namespace, digest); ok {
		return cas.NewVerifyingReadCloser(cachedBody{contextReader{ctx, body}, body}, digest), nil
	}
	return r.fallback.Get(ctx, digest)
}

// Ranges is exclusively a read accelerator. Never use it as evidence of remote
// backing, nor as the BaseSource of a LocalVersion.
func (c *Cache) Ranges(namespace string, fallback blockformat.RangeSource) blockformat.RangeSource {
	if c == nil {
		return fallback
	}
	return cachedRanges{c, namespace, fallback}
}

type cachedRanges struct {
	cache     *Cache
	namespace string
	fallback  blockformat.RangeSource
}

func (r cachedRanges) GetRange(ctx context.Context, digest string, size, offset, length int64) (io.ReadCloser, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if size < 0 || offset < 0 || length < 0 || offset > size || length > size-offset {
		return nil, errors.New("invalid cached object range")
	}
	if body, d, ok := r.cache.open(r.namespace, digest); ok {
		if d.SizeBytes == size {
			return cachedBody{contextReader{ctx, io.NewSectionReader(body, offset, length)}, body}, nil
		}
		_ = body.Close()
	}
	return r.fallback.GetRange(ctx, digest, size, offset, length)
}

type cachedBody struct {
	io.Reader
	io.Closer
}
