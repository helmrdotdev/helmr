package cas

import (
	"context"
	"crypto/sha256"
	"errors"
	"io"
	"os"
	"path/filepath"

	"github.com/helmrdotdev/helmr/internal/sha256sum"
)

// StoreObject stages exact immutable bytes in a local object store. It never
// replaces an existing object, and verifies existing content before reusing it.
// Callers own the private staging directory and candidate cleanup. This operation
// does not publish a generation, register a remote object or advance a head.
func (c *File) StoreObject(ctx context.Context, digest [32]byte, raw []byte) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if sha256.Sum256(raw) != digest {
		return ErrDigestMismatch
	}
	path, _, err := c.path(sha256sum.FormatDigest(digest[:]))
	if err != nil {
		return err
	}
	dir := filepath.Dir(path)
	if err = os.MkdirAll(dir, 0700); err != nil {
		return err
	}
	stage, err := os.CreateTemp(dir, ".object-*")
	if err != nil {
		return err
	}
	defer os.Remove(stage.Name())
	defer stage.Close()
	if _, err = stage.Write(raw); err != nil {
		return err
	}
	if err = stage.Chmod(0400); err != nil {
		return err
	}
	if err = stage.Sync(); err != nil {
		return err
	}
	if err = stage.Close(); err != nil {
		return err
	}
	if err = ctx.Err(); err != nil {
		return err
	}
	if err = os.Link(stage.Name(), path); err != nil {
		if !errors.Is(err, os.ErrExist) {
			return err
		}
		existing, openErr := os.Open(path)
		if openErr != nil {
			return openErr
		}
		defer existing.Close()
		stat, statErr := existing.Stat()
		if statErr != nil {
			return statErr
		}
		if !stat.Mode().IsRegular() || stat.Size() != int64(len(raw)) {
			return ErrDigestMismatch
		}
		hash := sha256.New()
		if _, err = io.Copy(hash, io.LimitReader(existing, int64(len(raw))+1)); err != nil {
			return err
		}
		if sha256sum.FormatDigest(hash.Sum(nil)) != sha256sum.FormatDigest(digest[:]) {
			return ErrDigestMismatch
		}
	}
	directory, err := os.Open(dir)
	if err != nil {
		return err
	}
	err = errors.Join(directory.Sync(), directory.Close(), ctx.Err())
	return err
}

// OpenImmutable opens a locally staged object read-only for exact-byte upload.
// The owning private staging directory must remain stable until the file closes.
// The publisher still verifies the digest; this method checks file shape only.
func (c *File) OpenImmutable(ctx context.Context, expected Descriptor) (*os.File, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if err := ValidateDescriptor(expected); err != nil {
		return nil, err
	}
	path, _, err := c.path(expected.Digest)
	if err != nil {
		return nil, err
	}
	file, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	info, err := file.Stat()
	if err == nil && (!info.Mode().IsRegular() || info.Size() != expected.SizeBytes || info.Mode().Perm() != 0400) {
		err = errors.New("immutable stage shape mismatch")
	}
	if err = errors.Join(err, ctx.Err()); err != nil {
		return nil, errors.Join(err, file.Close())
	}
	return file, nil
}
