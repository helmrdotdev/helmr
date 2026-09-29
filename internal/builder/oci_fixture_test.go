package builder

import (
	"archive/tar"
	"bytes"
	"encoding/json"
	"github.com/helmrdotdev/helmr/internal/oci"
	"github.com/helmrdotdev/helmr/internal/sha256sum"
	"strings"
	"testing"
)

func ociTarFromLayers(t *testing.T, layers ...[]byte) []byte {
	t.Helper()
	config := []byte(`{"Config":{"Env":["PATH=/bin"],"WorkingDir":"/workspace","User":"agent"}}`)
	configDigest := sha256sum.HexBytes(config)
	layerDescriptors := make([]oci.Descriptor, 0, len(layers))
	for _, layer := range layers {
		layerDescriptors = append(layerDescriptors, oci.Descriptor{
			MediaType: "application/vnd.oci.image.layer.v1.tar",
			Digest:    "sha256:" + sha256sum.HexBytes(layer),
		})
	}
	manifest := mustJSON(t, oci.Manifest{
		Config: oci.Descriptor{MediaType: "application/vnd.oci.image.Config.v1+json", Digest: "sha256:" + configDigest},
		Layers: layerDescriptors,
	})
	manifestDigest := sha256sum.HexBytes(manifest)
	index := mustJSON(t, oci.Index{Manifests: []oci.Descriptor{{
		MediaType: "application/vnd.oci.image.manifest.v1+json",
		Digest:    "sha256:" + manifestDigest,
		Platform:  &oci.Platform{OS: "linux", Architecture: "amd64"},
	}}})
	var buf bytes.Buffer
	writer := tar.NewWriter(&buf)
	writeTarFile(t, writer, "oci-layout", []byte(`{"imageLayoutVersion":"1.0.0"}`))
	writeTarFile(t, writer, "index.json", index)
	writeTarFile(t, writer, "blobs/sha256/"+configDigest, config)
	for index, layer := range layers {
		writeTarFile(t, writer, "blobs/sha256/"+strings.TrimPrefix(layerDescriptors[index].Digest, "sha256:"), layer)
	}
	writeTarFile(t, writer, "blobs/sha256/"+manifestDigest, manifest)
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
