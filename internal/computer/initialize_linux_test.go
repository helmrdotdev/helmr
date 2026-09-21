//go:build linux

package computer

import (
	"bytes"
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/helmrdotdev/helmr/internal/cas"
	"github.com/helmrdotdev/helmr/internal/checkpoint"
	"github.com/helmrdotdev/helmr/internal/oci"
)

func TestInitializePreservesExistingDiskAndRemovesFailedCandidate(t *testing.T) {
	dir := t.TempDir()
	// Real ext4 preservation and restoration are exercised by TestComputerSeedProof.
	disk := bytes.Repeat([]byte{7}, 4096)
	seedPath := filepath.Join(dir, "seed")
	if err := os.WriteFile(seedPath, disk, 0600); err != nil {
		t.Fatal(err)
	}
	cipher, err := checkpoint.New(bytes.Repeat([]byte{7}, 32))
	if err != nil {
		t.Fatal(err)
	}
	objects, err := cas.NewFile(filepath.Join(dir, "objects"))
	if err != nil {
		t.Fatal(err)
	}
	store := DiskStore{CAS: objects, Cipher: cipher}
	seed := publishTestSeed(t, objects, seedPath, dir, oci.RuntimeConfig{User: "1000:1000"})
	if err := os.Remove(seedPath); err != nil {
		t.Fatal(err)
	}
	target := filepath.Join(dir, "disk")
	initial, err := store.Initialize(t.Context(), diskTestComputer, seed, target, dir, 4096)
	if err != nil {
		t.Fatal(err)
	}
	if initial.Disk.Artifact().Object.Digest == "" || initial.Config.User != "1000:1000" {
		t.Fatal("incomplete initial candidate")
	}
	if _, err := store.Initialize(t.Context(), diskTestComputer, seed, target, dir, 4096); err == nil {
		t.Fatal("existing disk accepted")
	}
	got, err := os.ReadFile(target)
	if err != nil || !bytes.Equal(got, disk) {
		t.Fatal("existing disk changed")
	}
	defer initial.Disk.Close()
	failedTarget := filepath.Join(dir, "failed")
	result, err := store.Initialize(t.Context(), diskTestComputer, seed, failedTarget, filepath.Join(dir, "missing-staging"), 4096)
	if !errors.Is(err, os.ErrNotExist) || result != nil {
		t.Fatalf("result=%+v err=%v", result, err)
	}
	if _, err := os.Lstat(failedTarget); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("failed disk remains: %v", err)
	}
	// A capacity mismatch must fail before allocating the writable disk.
	seed.Artifact.LogicalBytes *= 2
	if _, err := store.Initialize(t.Context(), diskTestComputer, seed, failedTarget, filepath.Join(dir, "missing-staging"), 4096); err == nil || errors.Is(err, os.ErrNotExist) {
		t.Fatalf("seed verification did not precede upload: %v", err)
	}
	if _, err := os.Lstat(failedTarget); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("invalid seed left a disk: %v", err)
	}
}

// Simulates admitted client artifacts, not a durable publication transaction.
func publishTestSeed(t *testing.T, objects cas.Store, source, staging string, config oci.RuntimeConfig) Seed {
	t.Helper()
	path := filepath.Join(staging, "seed.filepack")
	artifact, err := EncodeSeed(t.Context(), source, path)
	if err != nil {
		t.Fatal(err)
	}
	defer os.Remove(path)
	file, err := os.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer file.Close()
	object, err := objects.Put(t.Context(), SeedMediaType, file)
	if err != nil {
		t.Fatal(err)
	}
	if object.Digest != artifact.Object.Digest || object.SizeBytes != artifact.Object.SizeBytes {
		t.Fatal("uploaded seed differs")
	}
	return Seed{Artifact: artifact, Config: config}
}
