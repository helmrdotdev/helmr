//go:build linux

package computer

import (
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"io"
	"os"

	"github.com/helmrdotdev/helmr/internal/cas"
	"github.com/helmrdotdev/helmr/internal/filepack"
	"github.com/helmrdotdev/helmr/internal/sha256sum"
	"golang.org/x/sys/unix"
)

// DiskPublisher uploads a previously encoded file without another local stage.
// Publication in object storage does not commit a Computer version.
type DiskPublisher interface {
	Publish(context.Context, cas.Descriptor, *os.File) (cas.Object, error)
}

// DiskCandidate owns one local ciphertext file. Its owner must register the exact
// descriptor durably before Upload, and resolve uncertain publication using that
// descriptor. Close removes only local staging, never uploaded objects. Methods
// require exclusive ownership; Close must not race Upload.
type DiskCandidate struct {
	artifact DiskArtifact
	file     *os.File
}

func (c *DiskCandidate) Artifact() DiskArtifact { return c.artifact }

func (c *DiskCandidate) Upload(ctx context.Context, publisher DiskPublisher) error {
	if c.file == nil {
		return os.ErrClosed
	}
	if publisher == nil {
		return errors.New("computer disk publisher is required")
	}
	object, err := publisher.Publish(ctx, c.artifact.Object, c.file)
	if err != nil {
		return err
	}
	if object.Digest != c.artifact.Object.Digest || object.SizeBytes != c.artifact.Object.SizeBytes || object.MediaType != c.artifact.Object.MediaType {
		return errors.New("uploaded computer disk descriptor mismatch")
	}
	return nil
}

func (c *DiskCandidate) Close() error {
	if c.file == nil {
		return nil
	}
	file := c.file
	if err := os.Remove(file.Name()); err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	c.file = nil
	return file.Close()
}

// Capture encodes an exclusively owned stable disk once, without remote writes.
// stagingDir must be a private directory owned by this runtime, outside the guest;
// its owner reserves encoding capacity before calling and cleans it after crashes.
// The returned descriptor can be registered before upload. Retry Upload on this
// candidate, not Capture: fresh encryption produces a different identity.
func (s DiskStore) Capture(ctx context.Context, computerID, disk, stagingDir string) (_ *DiskCandidate, retErr error) {
	if err := s.validate(computerID); err != nil {
		return nil, err
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if stagingDir == "" {
		return nil, errors.New("runtime-owned computer staging directory is required")
	}
	fd, err := unix.Open(disk, unix.O_RDONLY|unix.O_CLOEXEC|unix.O_NOFOLLOW, 0)
	if err != nil {
		return nil, err
	}
	source := os.NewFile(uintptr(fd), disk)
	defer source.Close()
	info, err := source.Stat()
	if err != nil {
		return nil, err
	}
	if !info.Mode().IsRegular() {
		return nil, errors.New("computer disk must be a regular file")
	}
	limit, err := diskArtifactLimit(info.Size())
	if err != nil {
		return nil, err
	}
	stage, err := os.CreateTemp(stagingDir, "computer-disk-*")
	if err != nil {
		return nil, err
	}
	stageOpen := true
	defer func() {
		if retErr != nil {
			if stageOpen {
				retErr = errors.Join(retErr, stage.Close())
			}
			retErr = errors.Join(retErr, os.Remove(stage.Name()))
		}
	}()
	hash := sha256.New()
	bounded := &boundedWriter{writer: io.MultiWriter(stage, hash), remaining: limit}
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
	encryptErr := s.Cipher.Encrypt(ctx, reader, bounded, "computer-disk:"+computerID)
	_ = reader.CloseWithError(encryptErr)
	if err := errors.Join(encryptErr, <-packed); err != nil {
		return nil, fmt.Errorf("encode computer disk: %w", err)
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if err := stage.Chmod(0400); err != nil {
		return nil, err
	}
	// The candidate is recoverable from the source until durable upload. No
	// fsync is needed on this ephemeral host staging path.
	readOnly, err := os.Open(stage.Name())
	if err != nil {
		return nil, err
	}
	closeErr := stage.Close()
	stageOpen = false
	if closeErr != nil {
		_ = readOnly.Close()
		return nil, closeErr
	}
	return &DiskCandidate{file: readOnly, artifact: DiskArtifact{
		Object:       cas.Descriptor{Digest: sha256sum.FormatDigest(hash.Sum(nil)), SizeBytes: limit - bounded.remaining, MediaType: DiskMediaType},
		LogicalBytes: info.Size(),
	}}, nil
}
