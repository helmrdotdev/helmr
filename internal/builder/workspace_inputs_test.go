package builder

import (
	"archive/tar"
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"github.com/helmrdotdev/helmr/internal/cas"
	"github.com/helmrdotdev/helmr/internal/computer"
	"os"
	"path/filepath"
	"reflect"
	"runtime"
	"testing"

	"github.com/helmrdotdev/helmr/internal/deployment"
	"github.com/helmrdotdev/helmr/internal/oci"
)

func TestReadWorkspaceImageInputsDerivesFinalArtifactIdentity(t *testing.T) {
	mkfs, config := workspaceTestTools(t)
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
	images, objects, err := ReadWorkspaceImageInputs(context.Background(), documentPath, root, mkfs, config)
	if err != nil {
		t.Fatal(err)
	}
	if len(objects) != 1 {
		t.Fatal(objects)
	}
	packed, err := os.ReadFile(objects[0].Path)
	if err != nil {
		t.Fatal(err)
	}
	digest := fmt.Sprintf("sha256:%x", sha256.Sum256(packed))
	if len(images) != 1 || images[0].DeclaredID != "sandbox" ||
		images[0].Artifact.Digest != digest ||
		images[0].Artifact.MediaType != deployment.WorkspaceImageArtifactMediaType ||
		images[0].Artifact.Profile != computer.SeedProfile || images[0].Artifact.Config.WorkingDir != "/workspace" ||
		images[0].Artifact.Architecture != deployment.ArchitectureX8664 ||
		len(objects) != 1 || objects[0].Digest != digest || objects[0].Path == imagePath {
		t.Fatalf("images = %+v objects = %+v", images, objects)
	}
}

func TestReadWorkspaceImageInputsDeduplicatesSharedObjectBytes(t *testing.T) {
	mkfs, config := workspaceTestTools(t)
	root := t.TempDir()
	image := workspaceOCIFixture(t)
	firstPath := filepath.Join(root, "first.oci.tar")
	secondPath := filepath.Join(root, "second.oci.tar")
	for _, path := range []string{firstPath, secondPath} {
		if err := os.WriteFile(path, image, 0o644); err != nil {
			t.Fatal(err)
		}
	}
	document, err := json.Marshal([]workspaceImageInput{
		{DeclaredID: "first", Path: firstPath},
		{DeclaredID: "second", Path: secondPath},
	})
	if err != nil {
		t.Fatal(err)
	}
	documentPath := filepath.Join(root, "images.json")
	if err := os.WriteFile(documentPath, document, 0o644); err != nil {
		t.Fatal(err)
	}

	images, objects, err := ReadWorkspaceImageInputs(context.Background(), documentPath, root, mkfs, config)
	if err != nil {
		t.Fatal(err)
	}
	if len(images) != 2 || len(objects) != 1 {
		t.Fatalf("images = %+v objects = %+v", images, objects)
	}
	if !reflect.DeepEqual(images[0].Artifact, images[1].Artifact) || objects[0].Digest != images[0].Artifact.Digest {
		t.Fatalf("images = %+v objects = %+v", images, objects)
	}
}

func TestReadWorkspaceImageInputsAcceptsSharedPath(t *testing.T) {
	mkfs, config := workspaceTestTools(t)
	root := t.TempDir()
	imagePath := filepath.Join(root, "shared.oci.tar")
	if err := os.WriteFile(imagePath, workspaceOCIFixture(t), 0o644); err != nil {
		t.Fatal(err)
	}
	document, err := json.Marshal([]workspaceImageInput{
		{DeclaredID: "first", Path: imagePath},
		{DeclaredID: "second", Path: imagePath},
	})
	if err != nil {
		t.Fatal(err)
	}
	documentPath := filepath.Join(root, "images.json")
	if err := os.WriteFile(documentPath, document, 0o644); err != nil {
		t.Fatal(err)
	}

	inspectCount := 0
	images, objects, err := readWorkspaceImageInputs(context.Background(), documentPath, func(path string) (deployment.BundleWorkspaceImageArtifact, string, error) {
		inspectCount++
		target := filepath.Join(root, "disk.filepack")
		artifact, err := buildWorkspaceDisk(t.Context(), path, target, root, mkfs, config)
		return artifact, target, err
	})
	if err != nil {
		t.Fatal(err)
	}
	if inspectCount != 1 || len(images) != 2 || len(objects) != 1 || !reflect.DeepEqual(images[0].Artifact, images[1].Artifact) || objects[0].Path == imagePath {
		t.Fatalf("inspect count = %d images = %+v objects = %+v", inspectCount, images, objects)
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

func workspaceTestTools(t *testing.T) (string, string) {
	t.Helper()
	mkfs, config := os.Getenv("HELMR_SUBSTRATE_MKFS_EXT4"), os.Getenv("HELMR_SUBSTRATE_MKE2FS_CONFIG")
	if runtime.GOOS != "linux" || mkfs == "" || config == "" {
		t.Skip("requires pinned Linux builder tools")
	}
	return mkfs, config
}

func TestDiskBuildFinalizeAndUpload(t *testing.T) {
	mkfs, config := workspaceTestTools(t)
	root := t.TempDir()
	source := filepath.Join(root, "source.oci.tar")
	if err := os.WriteFile(source, workspaceOCIFixture(t), 0600); err != nil {
		t.Fatal(err)
	}
	raw, err := json.Marshal([]workspaceImageInput{{DeclaredID: "sandbox", Path: source}})
	if err != nil {
		t.Fatal(err)
	}
	document := filepath.Join(root, "images.json")
	if err := os.WriteFile(document, raw, 0600); err != nil {
		t.Fatal(err)
	}
	images, objects, err := ReadWorkspaceImageInputs(t.Context(), document, root, mkfs, config)
	if err != nil {
		t.Fatal(err)
	}
	programPath, programBytes, index := writeVerifiedProgramFixture(t, root, images...)
	input := testBundleInput(programPath, programBytes)
	input.Program.Index = index
	input.WorkspaceImages = images
	input.Objects = append(input.Objects, objects...)
	result, err := FinalizeBundle(t.Context(), filepath.Join(root, "bundle"), input)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(result.Bundle.WorkspaceImages, images) {
		t.Fatal("disk contract changed during finalization")
	}
	store, err := cas.NewFile(filepath.Join(root, "cas"))
	if err != nil {
		t.Fatal(err)
	}
	for _, object := range result.Bundle.Objects {
		file, err := os.Open(result.Objects[object.Digest])
		if err != nil {
			t.Fatal(err)
		}
		uploaded, err := store.Put(t.Context(), object.MediaType, file)
		file.Close()
		if err != nil {
			t.Fatal(err)
		}
		if uploaded.Digest != object.Digest || uploaded.SizeBytes != object.SizeBytes {
			t.Fatal("uploaded bytes differ")
		}
	}
	image := images[0].Artifact
	body, err := store.Get(t.Context(), image.Digest)
	if err != nil {
		t.Fatal(err)
	}
	defer body.Close()
	if err := computer.VerifySeed(t.Context(), body, computer.SeedArtifact{Object: cas.Descriptor{Digest: image.Digest, SizeBytes: image.SizeBytes, MediaType: image.MediaType}, LogicalBytes: computer.SeedCapacity}, computer.SeedCapacity); err != nil {
		t.Fatal(err)
	}
}
