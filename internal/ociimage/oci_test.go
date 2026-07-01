package ociimage

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	"github.com/helmrdotdev/helmr/internal/sha256sum"
)

func TestUnpackAppliesLayersAndConfig(t *testing.T) {
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
	}, []byte(`{"Config":{"Env":["PATH=/bin","FOO=bar"],"WorkingDir":"/workspace","User":"agent"}}`))
	root := t.TempDir()
	oci, err := Unpack(bytes.NewReader(image), root)
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
		t.Fatalf("Config = %+v", oci.Config)
	}
}

func TestApplyLayerTarRejectsSymlinkParent(t *testing.T) {
	root := t.TempDir()
	if err := os.Symlink(t.TempDir(), filepath.Join(root, "link")); err != nil {
		t.Fatal(err)
	}
	layer := tarBytes(t, map[string]string{"link/escape.txt": "nope"})
	if err := ApplyLayerTar(bytes.NewReader(layer), root); err == nil {
		t.Fatal("expected symlink parent rejection")
	}
}

func TestApplyLayerTarRejectsWriteThroughAbsoluteSymlink(t *testing.T) {
	root := t.TempDir()
	outside := t.TempDir()
	layer := tarEntries(t,
		&tar.Header{Name: "link", Linkname: outside, Mode: 0o777, Typeflag: tar.TypeSymlink},
		&tar.Header{Name: "link/escape.txt", Mode: 0o644, Size: int64(len("nope")), Typeflag: tar.TypeReg},
	)
	if err := ApplyLayerTar(bytes.NewReader(layer), root); err == nil {
		t.Fatal("expected symlink parent rejection")
	}
	if _, err := os.Stat(filepath.Join(outside, "escape.txt")); !os.IsNotExist(err) {
		t.Fatalf("escape target exists err = %v, want missing", err)
	}
}

func TestConfinedLayerPathRejectsEscape(t *testing.T) {
	if _, err := ConfinedLayerPath(t.TempDir(), "../escape"); err == nil {
		t.Fatal("expected escape rejection")
	}
}

type ociTestLayer struct {
	mediaType string
	body      []byte
}

func ociTar(t *testing.T, layers []ociTestLayer, config []byte) []byte {
	t.Helper()
	configDigest := sha256Hex(config)
	layerDescriptors := make([]Descriptor, 0, len(layers))
	blobs := map[string][]byte{configDigest: config}
	for _, layer := range layers {
		digest := sha256Hex(layer.body)
		layerDescriptors = append(layerDescriptors, Descriptor{
			MediaType: layer.mediaType,
			Digest:    "sha256:" + digest,
		})
		blobs[digest] = layer.body
	}
	manifest := mustJSON(t, Manifest{
		Config: Descriptor{MediaType: "application/vnd.oci.image.Config.v1+json", Digest: "sha256:" + configDigest},
		Layers: layerDescriptors,
	})
	manifestDigest := sha256Hex(manifest)
	blobs[manifestDigest] = manifest
	index := mustJSON(t, Index{Manifests: []Descriptor{{
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

func tarEntries(t *testing.T, headers ...*tar.Header) []byte {
	t.Helper()
	var buf bytes.Buffer
	writer := tar.NewWriter(&buf)
	for _, header := range headers {
		if err := writer.WriteHeader(header); err != nil {
			t.Fatal(err)
		}
		if header.Typeflag == tar.TypeReg && header.Size > 0 {
			if _, err := writer.Write(bytes.Repeat([]byte("x"), int(header.Size))); err != nil {
				t.Fatal(err)
			}
		}
	}
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

func sha256Hex(body []byte) string {
	return sha256sum.HexBytes(body)
}

func readText(t *testing.T, path string) string {
	t.Helper()
	body, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return string(body)
}
