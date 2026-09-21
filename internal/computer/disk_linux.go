//go:build linux

package computer

import (
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"

	"github.com/helmrdotdev/helmr/internal/cas"
	"github.com/helmrdotdev/helmr/internal/checkpoint"
	"github.com/helmrdotdev/helmr/internal/filepack"
	"github.com/helmrdotdev/helmr/internal/ids"
	"github.com/helmrdotdev/helmr/internal/sha256sum"
	"golang.org/x/sys/unix"
)

const diskRole = "computer-disk"

type DiskStore struct {
	CAS    cas.Store
	Cipher *checkpoint.Encryptor
}

func (s DiskStore) validate(computerID string) error {
	if s.CAS == nil || s.Cipher == nil {
		return errors.New("computer disk storage and encryption are required")
	}
	return ids.Validate(computerID)
}

// Save requires an exclusively owned, stable disk throughout encoding. It does
// not freeze writers or publish a Computer version; the caller owns those fences.
// Encoding streams into the CAS stage without plaintext temporary artifacts.
func (s DiskStore) Save(ctx context.Context, computerID, disk string) (DiskArtifact, error) {
	if err := s.validate(computerID); err != nil {
		return DiskArtifact{}, err
	}
	fd, err := unix.Open(disk, unix.O_RDONLY|unix.O_CLOEXEC|unix.O_NOFOLLOW, 0)
	if err != nil {
		return DiskArtifact{}, err
	}
	source := os.NewFile(uintptr(fd), disk)
	defer source.Close()
	info, err := source.Stat()
	if err != nil {
		return DiskArtifact{}, err
	}
	if !info.Mode().IsRegular() {
		return DiskArtifact{}, errors.New("computer disk must be a regular file")
	}
	limit, err := diskArtifactLimit(info.Size())
	if err != nil {
		return DiskArtifact{}, err
	}
	stage, err := s.CAS.Stage(ctx, DiskMediaType)
	if err != nil {
		return DiskArtifact{}, err
	}
	defer stage.Abort(context.Background())
	reader, writer := io.Pipe()
	packed := make(chan error, 1)
	go func() {
		stats, err := filepack.PackTo(ctx, source, writer, diskRole)
		if err == nil && stats.LogicalBytes != info.Size() {
			err = errors.New("computer disk size changed during capture")
		}
		_ = writer.CloseWithError(err)
		packed <- err
	}()
	encryptErr := s.Cipher.Encrypt(ctx, reader, &boundedWriter{writer: stage, remaining: limit}, "computer-disk:"+computerID)
	_ = reader.CloseWithError(encryptErr)
	if err := errors.Join(encryptErr, <-packed); err != nil {
		return DiskArtifact{}, fmt.Errorf("encode computer disk: %w", err)
	}
	object, err := stage.Commit(ctx)
	if err != nil {
		return DiskArtifact{}, err
	}
	return DiskArtifact{Object: cas.Descriptor{Digest: object.Digest, SizeBytes: object.SizeBytes, MediaType: object.MediaType}, LogicalBytes: info.Size()}, nil
}

// Restore requires the exact artifact descriptor of the fenced committed version
// and capacity from Computer authority. Ciphertext alone does not prevent rollback.
// It creates an independent working disk, never reseeds/resizes it, and exposes
// target only after authentication, digest/size verification and decoding finish.
func (s DiskStore) Restore(ctx context.Context, computerID string, artifact DiskArtifact, target string, capacity int64) error {
	if err := s.validate(computerID); err != nil {
		return err
	}
	if err := artifact.Validate(capacity); err != nil {
		return err
	}
	if _, err := os.Lstat(target); err == nil {
		return os.ErrExist
	} else if !errors.Is(err, os.ErrNotExist) {
		return err
	}
	dir, err := os.MkdirTemp(filepath.Dir(target), ".computer-restore-*")
	if err != nil {
		return err
	}
	defer os.RemoveAll(dir)
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	body, err := s.CAS.Get(ctx, artifact.Object.Digest)
	if err != nil {
		return err
	}
	defer body.Close()
	reader, writer := io.Pipe()
	verified := make(chan error, 1)
	go func() {
		// CAS.Reader does not guarantee descriptor verification; enforce it here
		// before exposing a working disk, independently of the storage adapter.
		hash := sha256.New()
		bounded := &io.LimitedReader{R: &contextReader{ctx, body}, N: artifact.Object.SizeBytes + 1}
		err := s.Cipher.Decrypt(ctx, io.TeeReader(bounded, hash), writer, "computer-disk:"+computerID)
		if err == nil && (bounded.N != 1 || sha256sum.FormatDigest(hash.Sum(nil)) != artifact.Object.Digest) {
			err = errors.New("computer disk ciphertext descriptor mismatch")
		}
		_ = writer.CloseWithError(err)
		verified <- err
	}()
	restored := filepath.Join(dir, "disk.raw")
	_, unpackErr := filepack.UnpackFrom(ctx, reader, restored, diskRole, capacity)
	_ = reader.CloseWithError(unpackErr)
	if unpackErr != nil {
		cancel()
	}
	if err := errors.Join(unpackErr, <-verified); err != nil {
		return err
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	// Atomic, non-replacing publication of the local working file. CAS remains
	// the durable authority; no capacity-sized fsync is needed on this wake path.
	return os.Link(restored, target)
}

type boundedWriter struct {
	writer    io.Writer
	remaining int64
}

func (w *boundedWriter) Write(p []byte) (int, error) {
	if int64(len(p)) > w.remaining {
		return 0, errors.New("computer disk artifact exceeds its encoding bound")
	}
	n, err := w.writer.Write(p)
	w.remaining -= int64(n)
	return n, err
}

type contextReader struct {
	ctx    context.Context
	reader io.Reader
}

func (r *contextReader) Read(p []byte) (int, error) {
	if err := r.ctx.Err(); err != nil {
		return 0, err
	}
	return r.reader.Read(p)
}
