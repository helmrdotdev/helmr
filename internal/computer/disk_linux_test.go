//go:build linux

package computer

import (
	"bytes"
	"context"
	"errors"
	"github.com/helmrdotdev/helmr/internal/filepack"
	"io"
	"os"
	"path/filepath"
	"testing"

	"github.com/helmrdotdev/helmr/internal/cas"
	"github.com/helmrdotdev/helmr/internal/checkpoint"
)

const diskTestComputer = "019c10d5-a6f7-7af1-8f5f-000000000501"

func TestDiskStoreRestoresWithoutSeedAndBindsComputer(t *testing.T) {
	dir := t.TempDir()
	storage, err := cas.NewFile(filepath.Join(dir, "cas"))
	if err != nil {
		t.Fatal(err)
	}
	cipher, err := checkpoint.New(bytes.Repeat([]byte{7}, 32))
	if err != nil {
		t.Fatal(err)
	}
	store := DiskStore{CAS: storage, Cipher: cipher}
	source := filepath.Join(dir, "source")
	file, err := os.Create(source)
	if err != nil {
		t.Fatal(err)
	}
	const capacity = 16 << 20
	if err := file.Truncate(capacity); err != nil {
		t.Fatal(err)
	}
	payload := []byte("customer state outside workspace")
	if _, err := file.WriteAt(payload, 9<<20); err != nil {
		t.Fatal(err)
	}
	if err := file.Close(); err != nil {
		t.Fatal(err)
	}
	artifact, err := captureAndUpload(t.Context(), store, diskTestComputer, source, dir, storage)
	if err != nil {
		t.Fatal(err)
	}
	if artifact.Object.MediaType != DiskMediaType || artifact.LogicalBytes != capacity {
		t.Fatalf("artifact = %+v", artifact)
	}
	if err := os.Remove(source); err != nil {
		t.Fatal(err)
	}
	target := filepath.Join(dir, "restored")
	if err := store.Restore(t.Context(), diskTestComputer, artifact, target, capacity); err != nil {
		t.Fatal(err)
	}
	content, err := os.ReadFile(target)
	if err != nil {
		t.Fatal(err)
	}
	expected := make([]byte, capacity)
	copy(expected[9<<20:], payload)
	if !bytes.Equal(content, expected) {
		t.Fatal("restored logical bytes differ")
	}
	for _, test := range []struct {
		name, id string
		capacity int64
	}{
		{"wrong-owner", "019c10d5-a6f7-7af1-8f5f-000000000502", capacity},
		{"wrong-capacity", diskTestComputer, capacity * 2},
	} {
		t.Run(test.name, func(t *testing.T) {
			output := filepath.Join(dir, test.name)
			if err := store.Restore(t.Context(), test.id, artifact, output, test.capacity); err == nil {
				t.Fatal("invalid restore accepted")
			}
			if _, err := os.Lstat(output); !os.IsNotExist(err) {
				t.Fatalf("failed restore left a disk: %v", err)
			}
		})
	}
	if err := store.Restore(t.Context(), diskTestComputer, artifact, target, capacity); !os.IsExist(err) {
		t.Fatalf("collision = %v", err)
	}
	actual, err := os.ReadFile(target)
	if err != nil || !bytes.Equal(actual, expected) {
		t.Fatal("collision altered existing disk")
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := store.Capture(ctx, diskTestComputer, target, dir); err == nil {
		t.Fatal("cancelled save accepted")
	}
	for _, name := range []string{"wrong-size", "wrong-type", "oversized", "header-capacity"} {
		t.Run(name, func(t *testing.T) {
			changed := artifact
			expectedCapacity := int64(capacity)
			switch name {
			case "wrong-size":
				changed.Object.SizeBytes++
			case "wrong-type":
				changed.Object.MediaType = "application/octet-stream"
			case "oversized":
				changed.Object.SizeBytes = capacity * 3
			case "header-capacity":
				changed.LogicalBytes = capacity * 2
				expectedCapacity = capacity * 2
			}
			output := filepath.Join(dir, name)
			if err := store.Restore(t.Context(), diskTestComputer, changed, output, expectedCapacity); err == nil {
				t.Fatal("invalid descriptor accepted")
			}
			if _, err := os.Lstat(output); !os.IsNotExist(err) {
				t.Fatalf("partial target exposed: %v", err)
			}
			assertNoDiskTemps(t, dir)
		})
	}
	// A valid checkpoint ciphertext must not become a Computer disk by relabeling.
	var packed, ciphertext bytes.Buffer
	raw, err := os.Open(target)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := filepack.PackTo(t.Context(), raw, &packed, diskRole); err != nil {
		t.Fatal(err)
	}
	_ = raw.Close()
	if err := cipher.Encrypt(t.Context(), &packed, &ciphertext, "helmr.checkpoint.test"); err != nil {
		t.Fatal(err)
	}
	other, err := storage.Put(t.Context(), DiskMediaType, &ciphertext)
	if err != nil {
		t.Fatal(err)
	}
	wrongPurpose := DiskArtifact{Object: cas.Descriptor{Digest: other.Digest, SizeBytes: other.SizeBytes, MediaType: other.MediaType}, LogicalBytes: capacity}
	if err := store.Restore(t.Context(), diskTestComputer, wrongPurpose, filepath.Join(dir, "purpose"), capacity); err == nil {
		t.Fatal("checkpoint-purpose ciphertext accepted")
	}
	assertNoDiskTemps(t, dir)
	// Return a valid but different object at the requested digest, independently
	// of whether the storage implementation verifies its own digest namespace.
	mismatched := store
	mismatched.CAS = redirectedDiskStore{Store: storage, digest: artifact.Object.Digest}
	changed := artifact
	changed.Object.Digest = other.Digest
	if err := mismatched.Restore(t.Context(), diskTestComputer, changed, filepath.Join(dir, "digest"), capacity); err == nil {
		t.Fatal("digest mismatch accepted")
	}
	assertNoDiskTemps(t, dir)
	racingTarget := filepath.Join(dir, "racing-target")
	racing := store
	racing.CAS = onGetDiskStore{Store: storage, run: func() {
		if err := os.WriteFile(racingTarget, []byte("other owner"), 0600); err != nil {
			t.Fatal(err)
		}
	}}
	if err := racing.Restore(t.Context(), diskTestComputer, artifact, racingTarget, capacity); !errors.Is(err, os.ErrExist) {
		t.Fatalf("late collision = %v", err)
	}
	existing, err := os.ReadFile(racingTarget)
	if err != nil || string(existing) != "other owner" {
		t.Fatal("late collision replaced another owner")
	}
	assertNoDiskTemps(t, dir)
	if _, err := captureAndUpload(t.Context(), store, diskTestComputer, target, dir, failedDiskStore{Store: storage}); !errors.Is(err, errDiskUpload) {
		t.Fatalf("failed upload = %v", err)
	}
	stages, err := os.ReadDir(filepath.Join(dir, "cas", ".staging"))
	if err != nil || len(stages) != 0 {
		t.Fatalf("failed upload left stages: %v %v", stages, err)
	}
	objectKey, err := cas.ObjectKey("", artifact.Object.Digest)
	if err != nil {
		t.Fatal(err)
	}
	objectPath := filepath.Join(dir, "cas", filepath.FromSlash(objectKey))
	damaged, err := os.ReadFile(objectPath)
	if err != nil {
		t.Fatal(err)
	}
	truncated := damaged[:len(damaged)-1]
	truncatedStore := store
	truncatedStore.CAS = bytesDiskStore{Store: storage, data: truncated}
	if err := truncatedStore.Restore(t.Context(), diskTestComputer, artifact, filepath.Join(dir, "truncated"), capacity); err == nil {
		t.Fatal("truncated ciphertext accepted")
	}
	assertNoDiskTemps(t, dir)
	damaged[len(damaged)/2] ^= 1
	if err := os.WriteFile(objectPath, damaged, 0600); err != nil {
		t.Fatal(err)
	}
	corruptTarget := filepath.Join(dir, "corrupt")
	if err := store.Restore(t.Context(), diskTestComputer, artifact, corruptTarget, capacity); err == nil {
		t.Fatal("corrupt artifact restored")
	}
	if _, err := os.Lstat(corruptTarget); !os.IsNotExist(err) {
		t.Fatalf("corrupt disk left behind: %v", err)
	}

}

func assertNoDiskTemps(t *testing.T, dir string) {
	t.Helper()
	paths, err := filepath.Glob(filepath.Join(dir, ".computer-restore-*"))
	if err != nil || len(paths) != 0 {
		t.Fatalf("restore residue %v: %v", paths, err)
	}
}

type redirectedDiskStore struct {
	cas.Store
	digest string
}

func (s redirectedDiskStore) Get(ctx context.Context, _ string) (io.ReadCloser, error) {
	return s.Store.Get(ctx, s.digest)
}

type bytesDiskStore struct {
	cas.Store
	data []byte
}

func (s bytesDiskStore) Get(context.Context, string) (io.ReadCloser, error) {
	return io.NopCloser(bytes.NewReader(s.data)), nil
}

type onGetDiskStore struct {
	cas.Store
	run func()
}

func (s onGetDiskStore) Get(ctx context.Context, digest string) (io.ReadCloser, error) {
	s.run()
	return s.Store.Get(ctx, digest)
}

var errDiskUpload = errors.New("injected disk upload failure")

type failedDiskStore struct{ cas.Store }

func (s failedDiskStore) Stage(ctx context.Context, media string) (cas.Stage, error) {
	stage, err := s.Store.Stage(ctx, media)
	if err != nil {
		return nil, err
	}
	return failedDiskStage{Stage: stage}, nil
}

type failedDiskStage struct{ cas.Stage }

func (failedDiskStage) Write([]byte) (int, error) { return 0, errDiskUpload }

// The test adapter uses the real file CAS and verifies the same read-only input
// contract as the production immutable publisher.
type diskTestPublisher struct{ cas.Store }

func (p diskTestPublisher) Publish(ctx context.Context, expected cas.Descriptor, file *os.File) (cas.Object, error) {
	if _, err := cas.InspectPublishedFile(file); err != nil {
		return cas.Object{}, err
	}
	if err := cas.VerifyDescriptorFile(ctx, expected, file); err != nil {
		return cas.Object{}, err
	}
	stage, err := p.Stage(ctx, expected.MediaType)
	if err != nil {
		return cas.Object{}, err
	}
	return cas.WriteStage(ctx, stage, io.NewSectionReader(file, 0, expected.SizeBytes))
}
func captureAndUpload(ctx context.Context, store DiskStore, id, source, staging string, objects cas.Store) (DiskArtifact, error) {
	candidate, err := store.Capture(ctx, id, source, staging)
	if err != nil {
		return DiskArtifact{}, err
	}
	defer candidate.Close()
	if err := candidate.Upload(ctx, diskTestPublisher{objects}); err != nil {
		return DiskArtifact{}, err
	}
	return candidate.Artifact(), nil
}
