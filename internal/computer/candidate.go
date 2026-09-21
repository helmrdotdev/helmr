package computer

import (
	"context"
	"errors"
	"github.com/helmrdotdev/helmr/internal/cas"
	"github.com/helmrdotdev/helmr/internal/checkpoint"
	"github.com/helmrdotdev/helmr/internal/filepack"
	"os"
)

const diskRole = "computer-disk"

type DiskStore struct {
	CAS    cas.Reader
	Cipher *checkpoint.Encryptor
}

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
	path     string
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
	var closeErr error
	if c.file != nil {
		c.path = c.file.Name()
		closeErr = c.file.Close()
		c.file = nil
	}
	if c.path == "" {
		return closeErr
	}
	removeErr := os.Remove(c.path)
	if errors.Is(removeErr, os.ErrNotExist) {
		removeErr = nil
	}
	if removeErr == nil {
		c.path = ""
	}
	return errors.Join(closeErr, removeErr)
}

// CaptureSizeLimit is the temporary ciphertext space needed in the worst case.
// The writable source disk and any VM/RAM snapshot staging are separate charges.
func (s DiskStore) CaptureSizeLimit(capacity int64) (int64, error) {
	admissionLimit, err := diskArtifactLimit(capacity)
	if err != nil {
		return 0, err
	}
	packed, err := filepack.PackedSizeLimit(capacity, diskRole)
	if err != nil {
		return 0, err
	}
	encoded, err := s.Cipher.EncryptedSize(packed)
	if err != nil {
		return 0, err
	}
	if encoded > admissionLimit {
		return 0, errors.New("computer encoder exceeds artifact admission limit")
	}
	return encoded, nil
}
