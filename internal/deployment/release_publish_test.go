package deployment

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"os"
	"path/filepath"
	"testing"

	"github.com/helmrdotdev/helmr/internal/cas"
	"github.com/helmrdotdev/helmr/internal/jsoncanon"
)

func TestPublishPlatformReleasePublishesOnlyRuntime(t *testing.T) {
	directory, manifest := platformReleaseFixture(t)
	store := &releasePublishStore{}

	if err := PublishPlatformRelease(context.Background(), store, directory); err != nil {
		t.Fatal(err)
	}
	if len(store.published) != 1 {
		t.Fatalf("published %d objects, want 1", len(store.published))
	}
	want := cas.Descriptor{
		Digest: manifest.Runtime.Digest, MediaType: manifest.Runtime.MediaType,
		SizeBytes: manifest.Runtime.SizeBytes,
	}
	if store.published[0] != want {
		t.Fatalf("published = %+v, want %+v", store.published[0], want)
	}
}

func TestPublishPinnedPlatformRelease(t *testing.T) {
	directory := os.Getenv("HELMR_PLATFORM_RELEASE_DIR")
	if directory == "" {
		t.Skip("HELMR_PLATFORM_RELEASE_DIR is not set")
	}
	store := &releasePublishStore{}
	if err := PublishPlatformRelease(context.Background(), store, directory); err != nil {
		t.Fatal(err)
	}
	if len(store.published) != 1 {
		t.Fatalf("published %d objects, want 1", len(store.published))
	}
}

func TestPublishPlatformReleaseRejectsInvalidRuntimeObject(t *testing.T) {
	directory, manifest := platformReleaseFixture(t)
	path := platformReleaseObjectPath(directory, manifest.Runtime.Digest)
	if err := os.WriteFile(path, []byte("changed"), 0o600); err != nil {
		t.Fatal(err)
	}

	store := &releasePublishStore{}
	if err := PublishPlatformRelease(context.Background(), store, directory); err == nil {
		t.Fatal("mismatched Platform release was published")
	}
	if len(store.published) != 0 {
		t.Fatalf("published %d objects before validation completed", len(store.published))
	}
}

func TestPublishPlatformReleaseRejectsSymlinkObject(t *testing.T) {
	directory, manifest := platformReleaseFixture(t)
	path := platformReleaseObjectPath(directory, manifest.Runtime.Digest)
	target := path + ".target"
	if err := os.Rename(path, target); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(target, path); err != nil {
		t.Fatal(err)
	}

	if err := PublishPlatformRelease(context.Background(), &releasePublishStore{}, directory); err == nil {
		t.Fatal("symlink Platform release object was published")
	}
}

type releasePublishStore struct {
	published []cas.Descriptor
}

func (*releasePublishStore) Stat(context.Context, string) (cas.Object, error) {
	return cas.Object{}, errors.New("unexpected Stat")
}

func (*releasePublishStore) Get(context.Context, string) (io.ReadCloser, error) {
	return nil, errors.New("unexpected Get")
}

func (store *releasePublishStore) Publish(
	_ context.Context,
	descriptor cas.Descriptor,
	file *os.File,
) (cas.Object, error) {
	raw, err := io.ReadAll(file)
	if err != nil {
		return cas.Object{}, err
	}
	if digestBytes(raw) != descriptor.Digest || int64(len(raw)) != descriptor.SizeBytes {
		return cas.Object{}, errors.New("published bytes do not match descriptor")
	}
	store.published = append(store.published, descriptor)
	return cas.Object{
		Digest: descriptor.Digest, MediaType: descriptor.MediaType, SizeBytes: descriptor.SizeBytes,
	}, nil
}

func platformReleaseFixture(t *testing.T) (string, platformReleaseManifest) {
	t.Helper()
	directory := t.TempDir()
	runtime := []byte("runtime squashfs")
	descriptor := RuntimeDescriptor{
		Architecture: ArchitectureX8664, Digest: digestBytes(runtime),
		FormatVersion: RuntimeDescriptorFormatVersion, MediaType: RuntimeArtifactMediaType,
		RuntimeContract: RuntimeContract, SizeBytes: int64(len(runtime)),
	}
	path := platformReleaseObjectPath(directory, descriptor.Digest)
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, runtime, 0o600); err != nil {
		t.Fatal(err)
	}
	manifest := platformReleaseManifest{FormatVersion: 0, Runtime: descriptor}
	raw, err := json.Marshal(manifest)
	if err != nil {
		t.Fatal(err)
	}
	canonical, err := jsoncanon.Transform(raw)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(directory, platformReleaseManifestFile), canonical, 0o600); err != nil {
		t.Fatal(err)
	}
	return directory, manifest
}

func platformReleaseObjectPath(directory, digest string) string {
	return filepath.Join(directory, "objects", "sha256", digest[len("sha256:"):])
}
