package cas

import (
	"context"
	"errors"
	"io"
	"os"
)

// ValidateRange rejects empty, out-of-bounds and overflowing object ranges.
func ValidateRange(size, offset, length int64) error {
	if size <= 0 || length <= 0 || offset < 0 || length > size || offset > size-length {
		return errors.New("invalid object range")
	}
	return nil
}

// GetRange checks object size but cannot verify its full digest from a range.
// Callers must authenticate the returned range against a trusted descriptor.
func (c *File) GetRange(ctx context.Context, digest string, size, offset, length int64) (io.ReadCloser, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if err := ValidateRange(size, offset, length); err != nil {
		return nil, err
	}
	path, _, err := c.path(digest)
	if err != nil {
		return nil, err
	}
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	info, err := f.Stat()
	if err != nil || !info.Mode().IsRegular() || info.Size() != size {
		_ = f.Close()
		if err != nil {
			return nil, err
		}
		return nil, errors.New("object range size mismatch")
	}
	return &fileRange{ctx: ctx, file: f, reader: io.NewSectionReader(f, offset, length)}, nil
}

type fileRange struct {
	ctx    context.Context
	file   *os.File
	reader *io.SectionReader
}

func (r *fileRange) Read(p []byte) (int, error) {
	if err := r.ctx.Err(); err != nil {
		return 0, err
	}
	return r.reader.Read(p)
}
func (r *fileRange) Close() error { return r.file.Close() }
