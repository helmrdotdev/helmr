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
	// Equal-capacity projection needs no filesystem mutation. Real ext4 growth
	// and restoration are exercised separately by TestComputerSeedProof.
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
	seed := publishTestSeed(t, SeedStore{Cipher: cipher}, objects, seedPath, dir, oci.RuntimeConfig{User: "1000:1000"})
	if err := os.Remove(seedPath); err != nil {
		t.Fatal(err)
	}
	target := filepath.Join(dir, "disk")
	initial, err := store.Initialize(t.Context(), diskTestComputer, seed, target, dir, 4096, "")
	if err != nil {
		t.Fatal(err)
	}
	if initial.Disk.Artifact().Object.Digest == "" || initial.Config.User != "1000:1000" {
		t.Fatal("incomplete initial candidate")
	}
	if _, err := store.Initialize(t.Context(), diskTestComputer, seed, target, dir, 4096, ""); err == nil {
		t.Fatal("existing disk accepted")
	}
	got, err := os.ReadFile(target)
	if err != nil || !bytes.Equal(got, disk) {
		t.Fatal("existing disk changed")
	}
	defer initial.Disk.Close()
	failedTarget := filepath.Join(dir, "failed")
	result, err := store.Initialize(t.Context(), diskTestComputer, seed, failedTarget, filepath.Join(dir, "missing-staging"), 4096, "")
	if !errors.Is(err, os.ErrNotExist) || result != nil {
		t.Fatalf("result=%+v err=%v", result, err)
	}
	if _, err := os.Lstat(failedTarget); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("failed disk remains: %v", err)
	}
	// Invalid preparation identity must fail before allocating the writable disk.
	seed.Identity.PreparationID = seed.Identity.EnvironmentID
	if _, err := store.Initialize(t.Context(), diskTestComputer, seed, failedTarget, filepath.Join(dir, "missing-staging"), 4096, ""); err == nil || errors.Is(err, os.ErrNotExist) {
		t.Fatalf("seed verification did not precede upload: %v", err)
	}
	if _, err := os.Lstat(failedTarget); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("invalid seed left a disk: %v", err)
	}
}

// Simulates a trusted preparation result, not a durable publication transaction.
func publishTestSeed(t *testing.T, store SeedStore, objects cas.Store, source, staging string, config oci.RuntimeConfig) Seed {
	t.Helper()
	id := SeedIdentity{EnvironmentID: diskTestComputer, PreparationID: "019c10d5-a6f7-7af1-8f5f-000000000502"}
	candidate, err := store.Encode(t.Context(), id, source, staging)
	if err != nil {
		t.Fatal(err)
	}
	defer candidate.Close()
	if err := candidate.Upload(t.Context(), diskTestPublisher{objects}); err != nil {
		t.Fatal(err)
	}
	return Seed{Identity: id, Artifact: candidate.Artifact(), Config: config}
}
