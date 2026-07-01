package substrate

import (
	"archive/tar"
	"bytes"
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/helmrdotdev/helmr/internal/ociimage"
	"github.com/helmrdotdev/helmr/internal/sha256sum"
)

func TestResolverBuildsContentAddressedSubstrateAndReusesCache(t *testing.T) {
	tempDir := t.TempDir()
	countFile := filepath.Join(tempDir, "mkfs-count")
	mkfs := fakeMkfs(t, countFile)
	image := writeOCITar(t, tempDir)
	source := testSource()
	resolver := &Resolver{
		CacheDir:     filepath.Join(tempDir, "cache"),
		MkfsExt4Path: mkfs,
	}
	first, err := resolver.Resolve(context.Background(), image, source)
	if err != nil {
		t.Fatal(err)
	}
	if first.Format != Format || first.BuilderABI != BuilderABI || first.LayoutABI != LayoutABI {
		t.Fatalf("unexpected substrate identity: %+v", first)
	}
	if first.Digest == "" || first.Path == "" {
		t.Fatalf("missing result fields: %+v", first)
	}
	if _, err := os.Stat(first.Path); err != nil {
		t.Fatal(err)
	}
	second, err := resolver.Resolve(context.Background(), image, source)
	if err != nil {
		t.Fatal(err)
	}
	if second.Path != first.Path || second.Digest != first.Digest || second.CacheKey != first.CacheKey {
		t.Fatalf("cache result mismatch:\nfirst=%+v\nsecond=%+v", first, second)
	}
	countBody, err := os.ReadFile(countFile)
	if err != nil {
		t.Fatal(err)
	}
	if got := strings.TrimSpace(string(countBody)); got != "1" {
		t.Fatalf("mkfs calls = %q", got)
	}
}

func TestResolverSingleflightsConcurrentBuilds(t *testing.T) {
	tempDir := t.TempDir()
	countFile := filepath.Join(tempDir, "mkfs-count")
	mkfs := fakeMkfs(t, countFile)
	image := writeOCITar(t, tempDir)
	source := testSource()
	resolver := &Resolver{
		CacheDir:     filepath.Join(tempDir, "cache"),
		MkfsExt4Path: mkfs,
	}
	var started atomic.Int32
	results := make(chan Result, 8)
	errs := make(chan error, 8)
	for range 8 {
		go func() {
			started.Add(1)
			result, err := resolver.Resolve(context.Background(), image, source)
			if err != nil {
				errs <- err
				return
			}
			results <- result
		}()
	}
	for started.Load() < 8 {
	}
	var first Result
	for range 8 {
		select {
		case err := <-errs:
			t.Fatal(err)
		case result := <-results:
			if first.Digest == "" {
				first = result
				continue
			}
			if result.Digest != first.Digest || result.Path != first.Path {
				t.Fatalf("result mismatch: first=%+v result=%+v", first, result)
			}
		}
	}
	countBody, err := os.ReadFile(countFile)
	if err != nil {
		t.Fatal(err)
	}
	if got := strings.TrimSpace(string(countBody)); got != "1" {
		t.Fatalf("mkfs calls = %q", got)
	}
}

func TestResolverRejectsMismatchedCacheMetadataIdentity(t *testing.T) {
	tempDir := t.TempDir()
	countFile := filepath.Join(tempDir, "mkfs-count")
	mkfs := fakeMkfs(t, countFile)
	image := writeOCITar(t, tempDir)
	source := testSource()
	resolver := &Resolver{
		CacheDir:     filepath.Join(tempDir, "cache"),
		MkfsExt4Path: mkfs,
	}
	first, err := resolver.Resolve(context.Background(), image, source)
	if err != nil {
		t.Fatal(err)
	}
	keyPath, err := keyPath(resolver.CacheDir, first.CacheKey)
	if err != nil {
		t.Fatal(err)
	}
	var metadata cacheMetadata
	body, err := os.ReadFile(keyPath)
	if err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(body, &metadata); err != nil {
		t.Fatal(err)
	}
	metadata.Identity.Source.AdapterABI = "other-adapter"
	mutated, err := json.Marshal(metadata)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(keyPath, mutated, 0o644); err != nil {
		t.Fatal(err)
	}
	second, err := resolver.Resolve(context.Background(), image, source)
	if err != nil {
		t.Fatal(err)
	}
	if second.CacheKey != first.CacheKey {
		t.Fatalf("cache key = %s, want %s", second.CacheKey, first.CacheKey)
	}
	countBody, err := os.ReadFile(countFile)
	if err != nil {
		t.Fatal(err)
	}
	if got := strings.TrimSpace(string(countBody)); got != "2" {
		t.Fatalf("mkfs calls = %q, want rebuild after metadata identity mismatch", got)
	}
}

func TestCacheKeyNormalizesSource(t *testing.T) {
	source := testSource()
	trimmed, err := CacheKey(source)
	if err != nil {
		t.Fatal(err)
	}
	source.SandboxArtifactDigest = "  " + source.SandboxArtifactDigest + "  "
	source.AdapterABI = source.AdapterABI + " "
	withWhitespace, err := CacheKey(source)
	if err != nil {
		t.Fatal(err)
	}
	if withWhitespace != trimmed {
		t.Fatalf("cache key changed after whitespace normalization: %s != %s", withWhitespace, trimmed)
	}
}

func TestValidateCachedDigestFileRejectsSameSizeCorruption(t *testing.T) {
	path := filepath.Join(t.TempDir(), "substrate.ext4")
	expected := []byte("good")
	corrupt := []byte("evil")
	if len(corrupt) != len(expected) {
		t.Fatalf("test fixture sizes differ")
	}
	digest := sha256sum.DigestBytes(expected)
	if err := os.WriteFile(path, corrupt, 0o644); err != nil {
		t.Fatal(err)
	}
	if err := validateCachedDigestFile(path, digest, int64(len(expected))); err == nil {
		t.Fatal("validateCachedDigestFile accepted same-size corrupted cache entry")
	}
}

func TestEnforceSubstrateCacheBudgetEvictsOldDigestAndMetadata(t *testing.T) {
	cacheDir := t.TempDir()
	oldDigest := "sha256:1111111111111111111111111111111111111111111111111111111111111111"
	newDigest := "sha256:2222222222222222222222222222222222222222222222222222222222222222"
	oldPath, err := digestPath(cacheDir, oldDigest)
	if err != nil {
		t.Fatal(err)
	}
	newPath, err := digestPath(cacheDir, newDigest)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Dir(oldPath), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(oldPath, bytes.Repeat([]byte("o"), 10), 0o444); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(newPath, bytes.Repeat([]byte("n"), 10), 0o444); err != nil {
		t.Fatal(err)
	}
	oldTime := time.Now().Add(-time.Hour)
	if err := os.Chtimes(oldPath, oldTime, oldTime); err != nil {
		t.Fatal(err)
	}
	key, err := CacheKey(testSource())
	if err != nil {
		t.Fatal(err)
	}
	if err := publishMetadata(cacheDir, key, cacheMetadata{
		CacheKey:   key,
		Digest:     oldDigest,
		Format:     Format,
		BuilderABI: BuilderABI,
		LayoutABI:  LayoutABI,
		SizeBytes:  10,
		Identity: cacheIdentity{
			Source:     normalizeSource(testSource()),
			Format:     Format,
			BuilderABI: BuilderABI,
			LayoutABI:  LayoutABI,
		},
	}); err != nil {
		t.Fatal(err)
	}

	if err := enforceSubstrateCacheBudget(cacheDir, 10, map[string]bool{newPath: true}); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(oldPath); !os.IsNotExist(err) {
		t.Fatalf("old digest stat err = %v, want not exist", err)
	}
	if _, err := os.Stat(newPath); err != nil {
		t.Fatal(err)
	}
	keyPath, err := keyPath(cacheDir, key)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(keyPath); !os.IsNotExist(err) {
		t.Fatalf("metadata stat err = %v, want not exist", err)
	}
}

func TestEnforceSubstrateCacheBudgetPreservesLinkedDigest(t *testing.T) {
	cacheDir := t.TempDir()
	oldDigest := "sha256:1111111111111111111111111111111111111111111111111111111111111111"
	newDigest := "sha256:2222222222222222222222222222222222222222222222222222222222222222"
	oldPath, err := digestPath(cacheDir, oldDigest)
	if err != nil {
		t.Fatal(err)
	}
	newPath, err := digestPath(cacheDir, newDigest)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Dir(oldPath), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(oldPath, bytes.Repeat([]byte("o"), 10), 0o444); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(newPath, bytes.Repeat([]byte("n"), 10), 0o444); err != nil {
		t.Fatal(err)
	}
	runtimeLink := filepath.Join(cacheDir, "runtime-substrate.ext4")
	if err := os.Link(oldPath, runtimeLink); err != nil {
		t.Fatal(err)
	}
	oldTime := time.Now().Add(-time.Hour)
	if err := os.Chtimes(oldPath, oldTime, oldTime); err != nil {
		t.Fatal(err)
	}

	if err := enforceSubstrateCacheBudget(cacheDir, 10, nil); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(oldPath); err != nil {
		t.Fatalf("linked digest was evicted: %v", err)
	}
	if _, err := os.Stat(newPath); !os.IsNotExist(err) {
		t.Fatalf("unlinked digest stat err = %v, want not exist", err)
	}
}

func fakeMkfs(t *testing.T, countFile string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "fake-mkfs")
	script := `#!/bin/sh
set -eu
last=""
for arg in "$@"; do
  last="$arg"
done
count=0
if [ -f "` + countFile + `" ]; then
  count=$(cat "` + countFile + `")
fi
count=$((count + 1))
printf "%s\n" "$count" > "` + countFile + `"
printf "fake-ext4:%s\n" "$*" > "$last"
`
	if err := os.WriteFile(path, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	return path
}

func testSource() Source {
	return Source{
		SandboxArtifactDigest: "sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa",
		SandboxArtifactFormat: "oci-tar",
		ImageDigest:           "sha256:bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb",
		RootfsDigest:          "sha256:cccccccccccccccccccccccccccccccccccccccccccccccccccccccccccccccc",
		RuntimeABI:            "runtime-v1",
		GuestdABI:             "guestd-v1",
		AdapterABI:            "adapter-v1",
		WorkspaceMountPath:    "/workspace",
	}
}

func writeOCITar(t *testing.T, dir string) string {
	t.Helper()
	image := ociTar(t, map[string]string{"app/hello.txt": "hello"})
	path := filepath.Join(dir, "image.oci.tar")
	if err := os.WriteFile(path, image, 0o644); err != nil {
		t.Fatal(err)
	}
	return path
}

func ociTar(t *testing.T, files map[string]string) []byte {
	t.Helper()
	layer := tarBytes(t, files)
	config := []byte(`{"Config":{"Env":["PATH=/bin"],"WorkingDir":"/workspace","User":"agent"}}`)
	configDigest := sha256sum.HexBytes(config)
	layerDigest := sha256sum.HexBytes(layer)
	manifest := mustJSON(t, ociimage.Manifest{
		Config: ociimage.Descriptor{MediaType: "application/vnd.oci.image.Config.v1+json", Digest: "sha256:" + configDigest},
		Layers: []ociimage.Descriptor{{
			MediaType: "application/vnd.oci.image.layer.v1.tar",
			Digest:    "sha256:" + layerDigest,
		}},
	})
	manifestDigest := sha256sum.HexBytes(manifest)
	index := mustJSON(t, ociimage.Index{Manifests: []ociimage.Descriptor{{
		MediaType: "application/vnd.oci.image.manifest.v1+json",
		Digest:    "sha256:" + manifestDigest,
	}}})
	var buf bytes.Buffer
	writer := tar.NewWriter(&buf)
	writeTarFile(t, writer, "oci-layout", []byte(`{"imageLayoutVersion":"1.0.0"}`))
	writeTarFile(t, writer, "index.json", index)
	writeTarFile(t, writer, "blobs/sha256/"+configDigest, config)
	writeTarFile(t, writer, "blobs/sha256/"+layerDigest, layer)
	writeTarFile(t, writer, "blobs/sha256/"+manifestDigest, manifest)
	if err := writer.Close(); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}

func tarBytes(t *testing.T, files map[string]string) []byte {
	t.Helper()
	var buf bytes.Buffer
	writer := tar.NewWriter(&buf)
	for path, body := range files {
		writeTarFile(t, writer, path, []byte(body))
	}
	if err := writer.Close(); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}

func writeTarFile(t *testing.T, writer *tar.Writer, name string, body []byte) {
	t.Helper()
	header := &tar.Header{Name: name, Mode: 0o644, Size: int64(len(body)), Typeflag: tar.TypeReg}
	if err := writer.WriteHeader(header); err != nil {
		t.Fatal(err)
	}
	if _, err := writer.Write(body); err != nil {
		t.Fatal(err)
	}
}

func mustJSON(t *testing.T, value any) []byte {
	t.Helper()
	body, err := json.Marshal(value)
	if err != nil {
		t.Fatal(err)
	}
	return body
}
