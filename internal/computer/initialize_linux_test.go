//go:build linux

package computer

import (
	"bytes"
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/helmrdotdev/helmr/internal/cas"
	"github.com/helmrdotdev/helmr/internal/checkpoint"
	"github.com/helmrdotdev/helmr/internal/sha256sum"
	"github.com/helmrdotdev/helmr/internal/substrate"
)

func TestInitializePreservesExistingDiskAndRemovesFailedCandidate(t *testing.T) {
	dir := t.TempDir()
	image := seedConfigImage(t)
	imagePath := filepath.Join(dir, "image.tar")
	if err := os.WriteFile(imagePath, image, 0600); err != nil {
		t.Fatal(err)
	}
	// Equal-capacity projection needs no filesystem mutation. Real ext4 growth
	// and restoration are exercised separately by TestComputerSeedProof.
	disk := bytes.Repeat([]byte{7}, 4096)
	seedPath := filepath.Join(dir, "seed")
	if err := os.WriteFile(seedPath, disk, 0600); err != nil {
		t.Fatal(err)
	}
	seed := Seed{ImagePath: imagePath, Image: cas.Descriptor{Digest: sha256sum.DigestBytes(image), SizeBytes: int64(len(image))}, Disk: substrate.NewDiskSource(seedPath, sha256sum.DigestBytes(disk), int64(len(disk)))}
	objects, err := cas.NewFile(filepath.Join(dir, "objects"))
	if err != nil {
		t.Fatal(err)
	}
	cipher, err := checkpoint.New(bytes.Repeat([]byte{7}, 32))
	if err != nil {
		t.Fatal(err)
	}
	store := DiskStore{CAS: objects, Cipher: cipher}
	target := filepath.Join(dir, "disk")
	initial, err := store.Initialize(t.Context(), diskTestComputer, seed, target, 4096, "")
	if err != nil {
		t.Fatal(err)
	}
	if initial.Artifact.Object.Digest == "" || initial.Config.User != "1000:1000" {
		t.Fatal("incomplete initial candidate")
	}
	if _, err := store.Initialize(t.Context(), diskTestComputer, seed, target, 4096, ""); err == nil {
		t.Fatal("existing disk accepted")
	}
	got, err := os.ReadFile(target)
	if err != nil || !bytes.Equal(got, disk) {
		t.Fatal("existing disk changed")
	}
	failure := errors.New("injected stage failure")
	store.CAS = failedInitialStage{Store: objects, err: failure}
	failedTarget := filepath.Join(dir, "failed")
	result, err := store.Initialize(t.Context(), diskTestComputer, seed, failedTarget, 4096, "")
	if !errors.Is(err, failure) || result.Artifact.Object.Digest != "" {
		t.Fatalf("result=%+v err=%v", result, err)
	}
	if _, err := os.Lstat(failedTarget); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("failed disk remains: %v", err)
	}
	// Invalid image identity must fail before allocating the writable projection.
	seed.Image.Digest = sha256sum.DigestBytes([]byte("different image"))
	if _, err := store.Initialize(t.Context(), diskTestComputer, seed, failedTarget, 4096, ""); err == nil || errors.Is(err, failure) {
		t.Fatalf("image verification did not precede upload: %v", err)
	}
	if _, err := os.Lstat(failedTarget); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("invalid seed left a disk: %v", err)
	}
}

type failedInitialStage struct {
	cas.Store
	err error
}

func (s failedInitialStage) Stage(context.Context, string) (cas.Stage, error) { return nil, s.err }
