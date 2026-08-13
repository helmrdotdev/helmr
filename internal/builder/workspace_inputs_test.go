package builder

import (
	"archive/tar"
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"testing"

	"github.com/helmrdotdev/helmr/internal/deployment"
	"github.com/helmrdotdev/helmr/internal/oci"
)

func TestReadWorkspaceImageInputsDerivesFinalArtifactIdentity(t *testing.T) {
	root := t.TempDir()
	imagePath := filepath.Join(root, "workspace.oci.tar")
	image := workspaceOCIFixture(t)
	if err := os.WriteFile(imagePath, image, 0o644); err != nil {
		t.Fatal(err)
	}
	document, err := json.Marshal([]workspaceImageInput{{DeclaredID: "sandbox", Path: imagePath}})
	if err != nil {
		t.Fatal(err)
	}
	documentPath := filepath.Join(root, "images.json")
	if err := os.WriteFile(documentPath, document, 0o644); err != nil {
		t.Fatal(err)
	}
	images, objects, err := ReadWorkspaceImageInputs(context.Background(), documentPath)
	if err != nil {
		t.Fatal(err)
	}
	digest := fmt.Sprintf("sha256:%x", sha256.Sum256(image))
	if len(images) != 1 || images[0].DeclaredID != "sandbox" ||
		images[0].Artifact.Digest != digest ||
		images[0].Artifact.MediaType != deployment.WorkspaceImageArtifactMediaType ||
		images[0].Artifact.Architecture != deployment.ArchitectureX8664 ||
		len(objects) != 1 || objects[0].Digest != digest || objects[0].Path != imagePath {
		t.Fatalf("images = %+v objects = %+v", images, objects)
	}
}

func workspaceOCIFixture(t *testing.T) []byte {
	t.Helper()
	layer := tarFixture(t, "hello.txt", []byte("hello"))
	config := []byte(`{"Config":{"WorkingDir":"/workspace"}}`)
	configDigest := fmt.Sprintf("sha256:%x", sha256.Sum256(config))
	layerDigest := fmt.Sprintf("sha256:%x", sha256.Sum256(layer))
	manifest, _ := json.Marshal(oci.Manifest{
		Config: oci.Descriptor{MediaType: "application/vnd.oci.image.config.v1+json", Digest: configDigest, Size: int64(len(config))},
		Layers: []oci.Descriptor{{MediaType: "application/vnd.oci.image.layer.v1.tar", Digest: layerDigest, Size: int64(len(layer))}},
	})
	manifestDigest := fmt.Sprintf("sha256:%x", sha256.Sum256(manifest))
	index, _ := json.Marshal(oci.Index{Manifests: []oci.Descriptor{{
		MediaType: "application/vnd.oci.image.manifest.v1+json", Digest: manifestDigest,
		Size: int64(len(manifest)), Platform: &oci.Platform{Architecture: "amd64", OS: "linux"},
	}}})
	var output bytes.Buffer
	writer := tar.NewWriter(&output)
	for name, body := range map[string][]byte{
		"oci-layout":                         []byte(`{"imageLayoutVersion":"1.0.0"}`),
		"index.json":                         index,
		"blobs/sha256/" + configDigest[7:]:   config,
		"blobs/sha256/" + layerDigest[7:]:    layer,
		"blobs/sha256/" + manifestDigest[7:]: manifest,
	} {
		if err := writer.WriteHeader(&tar.Header{Name: name, Mode: 0o644, Size: int64(len(body))}); err != nil {
			t.Fatal(err)
		}
		if _, err := writer.Write(body); err != nil {
			t.Fatal(err)
		}
	}
	if err := writer.Close(); err != nil {
		t.Fatal(err)
	}
	return output.Bytes()
}

func tarFixture(t *testing.T, name string, body []byte) []byte {
	t.Helper()
	var output bytes.Buffer
	writer := tar.NewWriter(&output)
	if err := writer.WriteHeader(&tar.Header{Name: name, Mode: 0o644, Size: int64(len(body))}); err != nil {
		t.Fatal(err)
	}
	if _, err := writer.Write(body); err != nil {
		t.Fatal(err)
	}
	if err := writer.Close(); err != nil {
		t.Fatal(err)
	}
	return output.Bytes()
}
