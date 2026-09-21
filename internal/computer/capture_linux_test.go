//go:build linux

package computer

import (
	"bytes"
	"context"
	"errors"
	"math/rand/v2"
	"os"
	"path/filepath"
	"testing"

	"github.com/helmrdotdev/helmr/internal/cas"
	"github.com/helmrdotdev/helmr/internal/checkpoint"
)

func TestDiskCandidateRetainsExactBytesAcrossUncertainUpload(t *testing.T) {
	dir := t.TempDir()
	objects, err := cas.NewFile(filepath.Join(dir, "objects"))
	if err != nil {
		t.Fatal(err)
	}
	cipher, err := checkpoint.New(bytes.Repeat([]byte{7}, 32))
	if err != nil {
		t.Fatal(err)
	}
	store := DiskStore{CAS: objects, Cipher: cipher}
	source := filepath.Join(dir, "source")
	original := bytes.Repeat([]byte{1}, 4096)
	if err := os.WriteFile(source, original, 0600); err != nil {
		t.Fatal(err)
	}
	candidate, err := store.Capture(t.Context(), diskTestComputer, source, dir)
	if err != nil {
		t.Fatal(err)
	}
	defer candidate.Close()
	artifact := candidate.Artifact()
	if err := artifact.Validate(int64(len(original))); err != nil {
		t.Fatal(err)
	}
	if _, err := objects.Stat(t.Context(), artifact.Object.Digest); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("capture wrote remote object: %v", err)
	}
	publisher := &uncertainDiskPublisher{diskTestPublisher: diskTestPublisher{objects}}
	if err := candidate.Upload(t.Context(), publisher); !errors.Is(err, errDiskUpload) {
		t.Fatalf("lost upload response: %v", err)
	}
	// An upload may have succeeded despite an error. Its descriptor and local
	// bytes remain available; even changing the source must not change a retry.
	if _, err := objects.Stat(t.Context(), artifact.Object.Digest); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(source, bytes.Repeat([]byte{2}, len(original)), 0600); err != nil {
		t.Fatal(err)
	}
	if err := candidate.Upload(t.Context(), publisher); err != nil {
		t.Fatal(err)
	}
	if publisher.calls != 2 || candidate.Artifact() != artifact {
		t.Fatal("retry changed candidate identity")
	}
	localPath := candidate.file.Name()
	if err := candidate.Close(); err != nil {
		t.Fatal(err)
	}
	if err := candidate.Close(); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(localPath); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("local stage remains: %v", err)
	}
	if err := candidate.Upload(t.Context(), publisher); !errors.Is(err, os.ErrClosed) {
		t.Fatalf("closed upload: %v", err)
	}
	restored := filepath.Join(dir, "restored")
	if err := store.Restore(t.Context(), diskTestComputer, artifact, restored, int64(len(original))); err != nil {
		t.Fatal(err)
	}
	got, err := os.ReadFile(restored)
	if err != nil || !bytes.Equal(got, original) {
		t.Fatalf("retry did not preserve original capture: %v", err)
	}
}

func TestCaptureRejectsMissingOwnerBeforeStaging(t *testing.T) {
	cipher, err := checkpoint.New(bytes.Repeat([]byte{7}, 32))
	if err != nil {
		t.Fatal(err)
	}
	store := DiskStore{Cipher: cipher}
	if _, err := store.Capture(t.Context(), diskTestComputer, "unused", ""); err == nil {
		t.Fatal("capture accepted implicit temporary directory")
	}
}

type uncertainDiskPublisher struct {
	diskTestPublisher
	calls int
}

func (p *uncertainDiskPublisher) Publish(ctx context.Context, expected cas.Descriptor, file *os.File) (cas.Object, error) {
	object, err := p.diskTestPublisher.Publish(ctx, expected, file)
	if err != nil {
		return object, err
	}
	p.calls++
	if p.calls == 1 {
		return cas.Object{}, errDiskUpload
	}
	return object, nil
}

func TestCaptureUsesEncoderStagingBound(t *testing.T) {
	cipher, err := checkpoint.New(bytes.Repeat([]byte{7}, 32))
	if err != nil {
		t.Fatal(err)
	}
	store := DiskStore{Cipher: cipher}
	const capacity = int64(32 << 30)
	limit, err := store.CaptureSizeLimit(capacity)
	if err != nil {
		t.Fatal(err)
	}
	if limit < capacity || limit > capacity+capacity/100 {
		t.Fatalf("unexpected ciphertext staging bound: %d", limit)
	}
	t.Logf("32 GiB disk ciphertext staging limit: %d bytes", limit)
	dir := t.TempDir()
	data := make([]byte, (4<<20)+4096)
	if _, err := rand.NewChaCha8([32]byte{9}).Read(data); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(dir, "disk")
	if err := os.WriteFile(path, data, 0600); err != nil {
		t.Fatal(err)
	}
	candidate, err := store.Capture(t.Context(), diskTestComputer, path, dir)
	if err != nil {
		t.Fatal(err)
	}
	defer candidate.Close()
	limit, err = store.CaptureSizeLimit(int64(len(data)))
	if err != nil {
		t.Fatal(err)
	}
	if candidate.Artifact().Object.SizeBytes > limit {
		t.Fatal("capture exceeded staging bound")
	}
	if _, err := (DiskStore{}).CaptureSizeLimit(4096); err == nil {
		t.Fatal("missing encryption accepted")
	}
}
