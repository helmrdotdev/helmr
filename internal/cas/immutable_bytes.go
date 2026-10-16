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
// does not publish a disk version, register a remote object or advance a head.
func (c *File) StoreObject(ctx context.Context, digest [32]byte, raw []byte) error {
	return c.storeObject(ctx, digest, raw, true)
}

func (c *File) storeObject(ctx context.Context, digest [32]byte, raw []byte, syncDirectories bool) error {
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
	wait := BeginGuestIOWait(ctx)
	err = os.MkdirAll(dir, 0700)
	wait.End()
	if err != nil {
		return err
	}
	wait = BeginGuestIOWait(ctx)
	stage, err := os.CreateTemp(dir, ".object-*")
	wait.End()
	if err != nil {
		return err
	}
	defer func() {
		wait := BeginGuestIOWait(ctx)
		_ = stage.Close()
		_ = os.Remove(stage.Name())
		wait.End()
	}()
	wait = BeginGuestIOWait(ctx)
	_, err = stage.Write(raw)
	if err == nil {
		err = stage.Chmod(0400)
	}
	if err == nil {
		err = stage.Sync()
	}
	if err == nil {
		err = stage.Close()
	}
	wait.End()
	if err != nil {
		return err
	}
	if err = ctx.Err(); err != nil {
		return err
	}
	wait = BeginGuestIOWait(ctx)
	err = os.Link(stage.Name(), path)
	wait.End()
	if err != nil {
		if !errors.Is(err, os.ErrExist) {
			return err
		}
		wait = BeginGuestIOWait(ctx)
		existing, openErr := os.Open(path)
		wait.End()
		if openErr != nil {
			return openErr
		}
		defer func() { wait := BeginGuestIOWait(ctx); _ = existing.Close(); wait.End() }()
		wait = BeginGuestIOWait(ctx)
		stat, statErr := existing.Stat()
		wait.End()
		if statErr != nil {
			return statErr
		}
		if !stat.Mode().IsRegular() || stat.Size() != int64(len(raw)) {
			return ErrDigestMismatch
		}
		hash := sha256.New()
		// The reader suspends the request only around native reads. Digest
		// computation in the copy's writer runs outside the storage wait.
		if _, err = io.Copy(hash, io.LimitReader(guestIOReader{ctx, existing}, int64(len(raw))+1)); err != nil {
			return err
		}
		if sha256sum.FormatDigest(hash.Sum(nil)) != sha256sum.FormatDigest(digest[:]) {
			return ErrDigestMismatch
		}
	}
	if !syncDirectories {
		return ctx.Err()
	}
	return c.syncObjectDirectories(ctx)
}

func (c *File) syncObjectDirectories(ctx context.Context) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	// Persist newly created hash-directory ancestors too. Syncing only the
	// leaf can leave a successfully stored object unreachable after restart.
	// The owner is responsible for the CAS root's entry in its parent directory.
	for current := filepath.Join(c.root, "sha256"); ; current = filepath.Dir(current) {
		wait := BeginGuestIOWait(ctx)
		directory, err := os.Open(current)
		if err == nil {
			err = errors.Join(directory.Sync(), directory.Close())
		}
		wait.End()
		if err = errors.Join(err, ctx.Err()); err != nil {
			return err
		}
		if current == filepath.Clean(c.root) {
			return nil
		}
	}
}

// ObjectBatch consolidates directory durability for one caller-owned capture.
// Each object file is still synchronized before its immutable name is installed.
// The caller must Sync successfully before adopting a root that references the
// batch. Failed or abandoned batches leave ordinary, charged staging objects.
// A batch is single-caller state; independent batches may share a File.
type ObjectBatch struct {
	file    *File
	pending bool
}

func (c *File) NewObjectBatch() *ObjectBatch { return &ObjectBatch{file: c} }

func (b *ObjectBatch) StoreObject(ctx context.Context, digest [32]byte, raw []byte) error {
	if err := b.file.storeObject(ctx, digest, raw, false); err != nil {
		return err
	}
	b.pending = true
	return nil
}

func (b *ObjectBatch) Sync(ctx context.Context) error {
	if !b.pending {
		return ctx.Err()
	}
	if err := b.file.syncObjectDirectories(ctx); err != nil {
		return err
	}
	b.pending = false
	return nil
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
