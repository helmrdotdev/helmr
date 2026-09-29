//go:build linux

package verify

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/helmrdotdev/helmr/internal/artifact"
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
	descriptor, err := artifact.ParseRuntimeDescriptor(descriptorRaw)
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
		artifact.RoleRuntime,
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
