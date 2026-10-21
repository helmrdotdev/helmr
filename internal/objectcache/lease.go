package objectcache

import (
	"context"

	"github.com/helmrdotdev/helmr/internal/cas"
)

// ReadLease pins verified private bytes for a bounded transfer. It deliberately
// exposes neither a file descriptor nor a path that could be linked into a VM.
type ReadLease struct{ reader *reader }

// Lease returns only an immediate exact hit; a miss performs no remote I/O.
func (c *Cache) Lease(ctx context.Context, namespace string, expected cas.Descriptor) *ReadLease {
	if ctx.Err() != nil {
		return nil
	}
	body, descriptor, ok := c.open(namespace, expected.Digest)
	if !ok {
		return nil
	}
	if descriptor != expected {
		_ = body.Close()
		return nil
	}
	return &ReadLease{reader: body}
}

func (r *ReadLease) ReadAt(p []byte, offset int64) (int, error) { return r.reader.ReadAt(p, offset) }
func (r *ReadLease) Close() error {
	if r == nil {
		return nil
	}
	return r.reader.Close()
}
