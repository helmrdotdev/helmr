package main

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
)

func TestUnpackOCIImageAppliesLayersAndConfig(t *testing.T) {
	first := tarBytes(t, map[string]string{
		"app/keep.txt":   "keep",
		"app/remove.txt": "remove",
	})
	second := tarBytes(t, map[string]string{
		"app/.wh.remove.txt": "",
		"app/new.txt":        "new",
	})
	image := ociTar(t, []ociTestLayer{
		{mediaType: "application/vnd.oci.image.layer.v1.tar+gzip", body: gzipBytes(t, first)},
		{mediaType: "application/vnd.oci.image.layer.v1.tar", body: second},
	}, []byte(`{"config":{"Env":["PATH=/bin","FOO=bar"],"WorkingDir":"/workspace","User":"agent"}}`))
	root := t.TempDir()
	oci, err := unpackOCIImage(bytes.NewReader(image), root)
	if err != nil {
		t.Fatal(err)
	}
	if got := readText(t, filepath.Join(root, "app/keep.txt")); got != "keep" {
		t.Fatalf("keep.txt = %q", got)
	}
	if got := readText(t, filepath.Join(root, "app/new.txt")); got != "new" {
		t.Fatalf("new.txt = %q", got)
	}
	if _, err := os.Stat(filepath.Join(root, "app/remove.txt")); !os.IsNotExist(err) {
		t.Fatalf("remove.txt exists after whiteout: %v", err)
	}
	if oci.Config.WorkingDir != "/workspace" || oci.Config.User != "agent" || len(oci.Config.Env) != 2 {
		t.Fatalf("config = %+v", oci.Config)
	}
}

func TestUnpackOCIImageRejectsMalformedArchive(t *testing.T) {
	image := ociTar(t, []ociTestLayer{{mediaType: "application/vnd.oci.image.layer.v1.tar", body: tarBytes(t, nil)}}, []byte(`{"config":{}}`))
	image = bytes.Replace(image, []byte("index.json"), []byte("index.jsox"), 1)
	_, err := unpackOCIImage(bytes.NewReader(image), t.TempDir())
	if err == nil {
		t.Fatal("expected malformed image error")
	}
}

func TestApplyLayerIgnoresRootDirectoryEntry(t *testing.T) {
	root := t.TempDir()
	layer := layerTarWithRootDir(t, "app/hello.txt", "hello")
	if err := applyLayerTar(bytes.NewReader(layer), root); err != nil {
		t.Fatal(err)
	}
	if got := readText(t, filepath.Join(root, "app/hello.txt")); got != "hello" {
		t.Fatalf("hello.txt = %q", got)
	}
}

func TestApplyLayerAllowsAbsoluteSymlinkTargetsInsideOCIImage(t *testing.T) {
	root := t.TempDir()
	layer := tarLayerEntries(t, []tar.Header{{
		Name:     "etc/alternatives/awk",
		Linkname: "/usr/bin/mawk",
		Typeflag: tar.TypeSymlink,
		Mode:     0o777,
	}})
	if err := applyLayerTar(bytes.NewReader(layer), root); err != nil {
		t.Fatal(err)
	}
	target, err := os.Readlink(filepath.Join(root, "etc/alternatives/awk"))
	if err != nil {
		t.Fatal(err)
	}
	if target != "/usr/bin/mawk" {
		t.Fatalf("symlink target = %q", target)
	}
}

func TestApplyLayerRejectsSymlinkParent(t *testing.T) {
	root := t.TempDir()
	if err := os.Symlink(t.TempDir(), filepath.Join(root, "link")); err != nil {
		t.Fatal(err)
	}
	layer := tarBytes(t, map[string]string{"link/escape.txt": "nope"})
	err := applyLayerTar(bytes.NewReader(layer), root)
	if err == nil {
		t.Fatal("expected symlink parent rejection")
	}
}

func TestApplyLayerRejectsOpaqueWhiteoutThroughAbsoluteSymlink(t *testing.T) {
	root := t.TempDir()
	outside := t.TempDir()
	victim := filepath.Join(outside, "victim.txt")
	if err := os.WriteFile(victim, []byte("keep"), 0o644); err != nil {
		t.Fatal(err)
	}
	layer := tarLayerEntries(t, []tar.Header{
		{
			Name:     "link",
			Linkname: outside,
			Typeflag: tar.TypeSymlink,
			Mode:     0o777,
		},
		{
			Name:     "link/.wh..wh..opq",
			Typeflag: tar.TypeReg,
			Mode:     0o644,
		},
	})
	err := applyLayerTar(bytes.NewReader(layer), root)
	if err == nil {
		t.Fatal("expected opaque whiteout symlink rejection")
	}
	if got := readText(t, victim); got != "keep" {
		t.Fatalf("victim.txt = %q", got)
	}
}

func TestApplyLayerRejectsDirectoryThroughAbsoluteSymlink(t *testing.T) {
	root := t.TempDir()
	outside := t.TempDir()
	layer := tarLayerEntries(t, []tar.Header{
		{
			Name:     "link",
			Linkname: outside,
			Typeflag: tar.TypeSymlink,
			Mode:     0o777,
		},
		{
			Name:     "link",
			Typeflag: tar.TypeDir,
			Mode:     0o755,
		},
	})
	err := applyLayerTar(bytes.NewReader(layer), root)
	if err == nil {
		t.Fatal("expected directory symlink rejection")
	}
}

func TestOpaqueWhiteoutKeepsCurrentLayerEntries(t *testing.T) {
	root := t.TempDir()
	if err := os.MkdirAll(filepath.Join(root, "dir"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "dir/lower.txt"), []byte("lower"), 0o644); err != nil {
		t.Fatal(err)
	}
	layer := tarEntries(t, []tarTestEntry{
		{path: "dir/current.txt", body: "current"},
		{path: "dir/.wh..wh..opq"},
	})
	if err := applyLayerTar(bytes.NewReader(layer), root); err != nil {
		t.Fatal(err)
	}
	if got := readText(t, filepath.Join(root, "dir/current.txt")); got != "current" {
		t.Fatalf("current.txt = %q", got)
	}
	if _, err := os.Stat(filepath.Join(root, "dir/lower.txt")); !os.IsNotExist(err) {
		t.Fatalf("lower.txt exists after opaque whiteout: %v", err)
	}
}

type ociTestLayer struct {
	mediaType string
	body      []byte
}

func ociTar(t *testing.T, layers []ociTestLayer, config []byte) []byte {
	t.Helper()
	configDigest := sha256Hex(config)
	layerDescriptors := make([]ociDescriptor, 0, len(layers))
	blobs := map[string][]byte{configDigest: config}
	for _, layer := range layers {
		digest := sha256Hex(layer.body)
		layerDescriptors = append(layerDescriptors, ociDescriptor{
			MediaType: layer.mediaType,
			Digest:    "sha256:" + digest,
		})
		blobs[digest] = layer.body
	}
	manifest := mustJSON(t, ociManifest{
		Config: ociDescriptor{MediaType: "application/vnd.oci.image.config.v1+json", Digest: "sha256:" + configDigest},
		Layers: layerDescriptors,
	})
	manifestDigest := sha256Hex(manifest)
	blobs[manifestDigest] = manifest
	index := mustJSON(t, ociIndex{Manifests: []ociDescriptor{{
		MediaType: "application/vnd.oci.image.manifest.v1+json",
		Digest:    "sha256:" + manifestDigest,
	}}})
	var buf bytes.Buffer
	writer := tar.NewWriter(&buf)
	writeTarFile(t, writer, "oci-layout", []byte(`{"imageLayoutVersion":"1.0.0"}`))
	writeTarFile(t, writer, "index.json", index)
	for digest, body := range blobs {
		writeTarFile(t, writer, "blobs/sha256/"+digest, body)
	}
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

type tarTestEntry struct {
	path string
	body string
}

func tarEntries(t *testing.T, entries []tarTestEntry) []byte {
	t.Helper()
	var buf bytes.Buffer
	writer := tar.NewWriter(&buf)
	for _, entry := range entries {
		writeTarFile(t, writer, entry.path, []byte(entry.body))
	}
	if err := writer.Close(); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}

func tarLayerEntries(t *testing.T, headers []tar.Header) []byte {
	t.Helper()
	var buf bytes.Buffer
	writer := tar.NewWriter(&buf)
	for _, header := range headers {
		header := header
		if err := writer.WriteHeader(&header); err != nil {
			t.Fatal(err)
		}
	}
	if err := writer.Close(); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}

func layerTarWithRootDir(t *testing.T, path string, body string) []byte {
	t.Helper()
	var buf bytes.Buffer
	writer := tar.NewWriter(&buf)
	if err := writer.WriteHeader(&tar.Header{Name: "./", Typeflag: tar.TypeDir, Mode: 0o755}); err != nil {
		t.Fatal(err)
	}
	writeTarFile(t, writer, path, []byte(body))
	if err := writer.Close(); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}

func gzipBytes(t *testing.T, body []byte) []byte {
	t.Helper()
	var buf bytes.Buffer
	writer := gzip.NewWriter(&buf)
	if _, err := writer.Write(body); err != nil {
		t.Fatal(err)
	}
	if err := writer.Close(); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}

func writeTarFile(t *testing.T, writer *tar.Writer, path string, body []byte) {
	t.Helper()
	if err := writer.WriteHeader(&tar.Header{Name: path, Mode: 0o644, Size: int64(len(body))}); err != nil {
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

func sha256Hex(body []byte) string {
	sum := sha256.Sum256(body)
	return hex.EncodeToString(sum[:])
}

func readText(t *testing.T, path string) string {
	t.Helper()
	body, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return string(body)
}
