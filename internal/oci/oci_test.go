package oci

import (
	"archive/tar"
	"bytes"
	"encoding/json"
	"testing"

	"github.com/helmrdotdev/helmr/internal/sha256sum"
)

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
			Size:      int64(len(layer.body)),
		})
		blobs[digest] = layer.body
	}
	manifest := mustJSON(t, Manifest{
		Config: Descriptor{
			MediaType: "application/vnd.oci.image.config.v1+json",
			Digest:    "sha256:" + configDigest,
			Size:      int64(len(config)),
		},
		Layers: layerDescriptors,
	})
	manifestDigest := sha256Hex(manifest)
	blobs[manifestDigest] = manifest
	index := mustJSON(t, Index{Manifests: []Descriptor{{
		MediaType: "application/vnd.oci.image.manifest.v1+json",
		Digest:    "sha256:" + manifestDigest,
		Size:      int64(len(manifest)),
		Platform:  &Platform{Architecture: "amd64", OS: "linux"},
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
