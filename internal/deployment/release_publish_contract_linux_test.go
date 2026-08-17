//go:build linux

package deployment

import (
	"context"
	"os"
	"path/filepath"
	"testing"
)

func TestVerifyPinnedRuntimeRelease(t *testing.T) {
	directory := os.Getenv("HELMR_RUNTIME_RELEASE_DIR")
	if directory == "" {
		t.Skip("HELMR_RUNTIME_RELEASE_DIR is not set")
	}
	descriptorRaw, err := os.ReadFile(filepath.Join(directory, "runtime.descriptor.json"))
	if err != nil {
		t.Fatal(err)
	}
	descriptor, err := ParseRuntimeDescriptor(descriptorRaw)
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(directory, "runtime.squashfs")
	file, err := os.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer file.Close()
	info, err := file.Stat()
	if err != nil {
		t.Fatal(err)
	}
	if info.Size() != descriptor.SizeBytes {
		t.Fatalf("runtime size = %d, want %d", info.Size(), descriptor.SizeBytes)
	}
	reader, err := newSquashFSArtifactReader(
		context.Background(),
		file,
		descriptor.SizeBytes,
		runtimeArtifact,
	)
	if err != nil {
		t.Fatal(err)
	}
	index, err := verifyRuntimeArtifact(context.Background(), artifactInput{
		Digest: descriptor.Digest, SizeBytes: descriptor.SizeBytes,
		MediaType: descriptor.MediaType, Reader: reader,
	})
	if err != nil {
		t.Fatal(err)
	}
	if index.Architecture != descriptor.Architecture ||
		index.RuntimeContract != descriptor.RuntimeContract {
		t.Fatalf("verified Runtime index = %+v, descriptor = %+v", index, descriptor)
	}
}

func TestPublishPlatformReleaseRejectsDescriptorValidInvalidRuntimeBeforeStoreWrite(t *testing.T) {
	directory, _ := platformReleaseFixture(t)
	store := &releasePublishStore{}
	if err := PublishPlatformRelease(context.Background(), store, directory); err == nil {
		t.Fatal("descriptor-valid invalid Runtime was published")
	}
	if len(store.published) != 0 {
		t.Fatalf("published %d objects before deep verification completed", len(store.published))
	}
}
