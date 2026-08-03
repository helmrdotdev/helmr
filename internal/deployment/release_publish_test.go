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

func TestPublishPlatformReleasePublishesPolicyLast(t *testing.T) {
	directory, manifest := platformReleaseFixture(t)
	store := &releasePublishStore{}

	if err := PublishPlatformRelease(context.Background(), store, directory); err != nil {
		t.Fatal(err)
	}
	want := []cas.Descriptor{
		releaseCASDescriptor(manifest.RuntimeHarness),
		releaseCASDescriptor(manifest.ToolchainBase),
		releaseCASDescriptor(manifest.Policy),
	}
	if len(store.published) != len(want) {
		t.Fatalf("published %d objects, want %d", len(store.published), len(want))
	}
	for index := range want {
		if store.published[index] != want[index] {
			t.Fatalf("published[%d] = %+v, want %+v", index, store.published[index], want[index])
		}
	}
}

func TestPublishPlatformReleaseRejectsPolicyInputMismatch(t *testing.T) {
	directory, manifest := platformReleaseFixture(t)
	manifest.RuntimeHarness.Digest = testDigest("other-runtime-harness")
	writePlatformReleaseManifest(t, directory, manifest)

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
	path := platformReleaseObjectPath(directory, manifest.RuntimeHarness.Digest)
	target := path + ".target"
	if err := os.Rename(path, target); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(target, path); err != nil {
		t.Fatal(err)
	}

	if err := PublishPlatformRelease(
		context.Background(),
		&releasePublishStore{},
		directory,
	); err == nil {
		t.Fatal("symlink Platform release object was published")
	}
}

type releasePublishStore struct {
	published []cas.Descriptor
}

func (store *releasePublishStore) Stat(context.Context, string) (cas.Object, error) {
	return cas.Object{}, errors.New("unexpected Stat")
}

func (store *releasePublishStore) Get(context.Context, string) (io.ReadCloser, error) {
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
	runtimeHarness := []byte("runtime harness")
	toolchainBase := []byte("toolchain base")
	runtimeDescriptor := ArtifactDescriptor{
		Digest: digestBytes(runtimeHarness), MediaType: PlatformTreeInputMediaType,
		SizeBytes: int64(len(runtimeHarness)),
	}
	toolchainDescriptor := ArtifactDescriptor{
		Digest: digestBytes(toolchainBase), MediaType: PlatformTreeInputMediaType,
		SizeBytes: int64(len(toolchainBase)),
	}
	policy, err := ComposeBuildPolicy(
		RuntimeInputs{Harness: runtimeDescriptor},
		ToolchainInputs{
			Base:     toolchainDescriptor,
			Compiler: testCompilerInputs(),
		},
		[]byte("node release keyring"),
		[]string{"00112233445566778899AABBCCDDEEFF00112233"},
	)
	if err != nil {
		t.Fatal(err)
	}
	policyDescriptor := ArtifactDescriptor{
		Digest: digestBytes(policy), MediaType: BuildPolicyMediaType,
		SizeBytes: int64(len(policy)),
	}
	for descriptor, raw := range map[ArtifactDescriptor][]byte{
		runtimeDescriptor:   runtimeHarness,
		toolchainDescriptor: toolchainBase,
		policyDescriptor:    policy,
	} {
		path := platformReleaseObjectPath(directory, descriptor.Digest)
		if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, raw, 0o600); err != nil {
			t.Fatal(err)
		}
	}
	manifest := platformReleaseManifest{
		FormatVersion:  0,
		Policy:         policyDescriptor,
		RuntimeHarness: runtimeDescriptor,
		ToolchainBase:  toolchainDescriptor,
	}
	writePlatformReleaseManifest(t, directory, manifest)
	return directory, manifest
}

func writePlatformReleaseManifest(
	t *testing.T,
	directory string,
	manifest platformReleaseManifest,
) {
	t.Helper()
	raw, err := json.Marshal(manifest)
	if err != nil {
		t.Fatal(err)
	}
	canonical, err := jsoncanon.Transform(raw)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(
		filepath.Join(directory, platformReleaseManifestFile),
		canonical,
		0o600,
	); err != nil {
		t.Fatal(err)
	}
}

func platformReleaseObjectPath(directory, digest string) string {
	return filepath.Join(directory, "objects", "sha256", digest[len("sha256:"):])
}
