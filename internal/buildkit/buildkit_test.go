package buildkit

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"maps"
	"os"
	"path/filepath"
	"testing"

	"github.com/helmrdotdev/helmr/internal/imagebuild"
	bkclient "github.com/moby/buildkit/client"
	"github.com/moby/buildkit/client/llb"
)

type testSolver struct {
	solve func(context.Context, *llb.Definition, bkclient.SolveOpt) (*bkclient.SolveResponse, error)
}

func (solver testSolver) Solve(
	ctx context.Context,
	definition *llb.Definition,
	options bkclient.SolveOpt,
	_ chan *bkclient.SolveStatus,
) (*bkclient.SolveResponse, error) {
	return solver.solve(ctx, definition, options)
}

func TestBuildImageWritesOCIArtifactAndManifest(t *testing.T) {
	solver := testSolver{solve: func(
		_ context.Context,
		definition *llb.Definition,
		options bkclient.SolveOpt,
	) (*bkclient.SolveResponse, error) {
		if definition == nil || len(definition.Def) == 0 {
			t.Fatal("BuildImage sent an empty build graph")
		}
		if len(options.Exports) != 1 || options.Exports[0].Type != bkclient.ExporterOCI {
			t.Fatalf("exports = %#v", options.Exports)
		}
		if len(options.CacheImports) != 0 || len(options.CacheExports) != 0 {
			t.Fatalf("unexpected cache options: imports=%#v exports=%#v", options.CacheImports, options.CacheExports)
		}
		output, err := options.Exports[0].Output(nil)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := output.Write([]byte("oci bytes")); err != nil {
			t.Fatal(err)
		}
		if err := output.Close(); err != nil {
			t.Fatal(err)
		}
		return &bkclient.SolveResponse{
			ExporterResponse: map[string]string{"digest": "sha256:test"},
		}, nil
	}}
	builder := New(solver, t.TempDir())
	artifact, err := builder.BuildImage(t.Context(), testImageRequest(t.TempDir()))
	if err != nil {
		t.Fatal(err)
	}
	image, err := os.ReadFile(artifact.ImageTarPath)
	if err != nil {
		t.Fatal(err)
	}
	if string(image) != "oci bytes" {
		t.Fatalf("image bytes = %q", image)
	}
	var manifest struct {
		Kind      string            `json:"kind"`
		RunID     string            `json:"runID"`
		ItemID    string            `json:"itemID"`
		SourceSHA string            `json:"sourceSHA"`
		Platform  string            `json:"platform"`
		Exporter  map[string]string `json:"exporter"`
	}
	raw, err := os.ReadFile(artifact.ManifestPath)
	if err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(raw, &manifest); err != nil {
		t.Fatal(err)
	}
	if manifest.Kind != "buildkit-oci-tar" ||
		manifest.RunID != "run" ||
		manifest.ItemID != "workspace" ||
		manifest.SourceSHA != "sha256:source" ||
		manifest.Platform != "linux/x86_64" ||
		manifest.Exporter["digest"] != "sha256:test" {
		t.Fatalf("manifest = %#v", manifest)
	}
	if artifact.RootPath != filepath.Dir(artifact.ImageTarPath) {
		t.Fatalf("artifact root = %q, image = %q", artifact.RootPath, artifact.ImageTarPath)
	}
}

func TestBuildImagePinsRegistryCacheOptions(t *testing.T) {
	const cacheRef = "123456789012.dkr.ecr.us-east-1.amazonaws.com/helmr/cache:workspace-v0"
	solver := testSolver{solve: func(
		_ context.Context,
		_ *llb.Definition,
		options bkclient.SolveOpt,
	) (*bkclient.SolveResponse, error) {
		if len(options.CacheImports) != 1 || options.CacheImports[0].Type != "registry" ||
			options.CacheImports[0].Attrs["ref"] != cacheRef {
			t.Fatalf("cache imports = %#v", options.CacheImports)
		}
		if len(options.CacheExports) != 1 || options.CacheExports[0].Type != "registry" {
			t.Fatalf("cache exports = %#v", options.CacheExports)
		}
		want := map[string]string{
			"ref":            cacheRef,
			"mode":           "max",
			"oci-mediatypes": "true",
			"image-manifest": "true",
			"ignore-error":   "true",
		}
		if !maps.Equal(options.CacheExports[0].Attrs, want) {
			t.Fatalf("cache export attrs = %#v, want %#v", options.CacheExports[0].Attrs, want)
		}
		output, err := options.Exports[0].Output(nil)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := output.Write([]byte("oci bytes")); err != nil {
			t.Fatal(err)
		}
		return &bkclient.SolveResponse{}, output.Close()
	}}
	request := testImageRequest(t.TempDir())
	request.Cache = &imagebuild.CacheBinding{
		Authority: "123456789012.dkr.ecr.us-east-1.amazonaws.com",
		Username:  "AWS",
		Ref:       cacheRef,
	}
	if _, err := New(solver, t.TempDir()).BuildImage(t.Context(), request); err != nil {
		t.Fatal(err)
	}
}

func TestBoundedOCIWriterStopsAtOutputContract(t *testing.T) {
	var output bytes.Buffer
	writer := &boundedWriteCloser{writer: &output, limit: 4}
	written, err := writer.Write([]byte("12345"))
	var quotaFailure *OutputQuotaFailure
	if written != 4 || !errors.As(err, &quotaFailure) || quotaFailure.LimitBytes != 4 {
		t.Fatalf("Write = (%d, %v), want 4-byte quota failure", written, err)
	}
	if output.String() != "1234" || !writer.exceededQuota() {
		t.Fatalf("output = %q, exceeded = %t", output.String(), writer.exceededQuota())
	}
	if written, err := writer.Write([]byte("6")); written != 0 || !errors.As(err, &quotaFailure) {
		t.Fatalf("second Write = (%d, %v), want quota failure", written, err)
	}
}

func TestBuildImageReturnsTypedOutputQuotaFailureAndRemovesPartialArtifact(t *testing.T) {
	solver := testSolver{solve: func(
		_ context.Context,
		_ *llb.Definition,
		options bkclient.SolveOpt,
	) (*bkclient.SolveResponse, error) {
		output, err := options.Exports[0].Output(nil)
		if err != nil {
			return nil, err
		}
		_, err = output.Write([]byte("12345"))
		return nil, err
	}}
	outputRoot := t.TempDir()
	builder := New(solver, outputRoot)
	builder.ociOutputLimit = 4
	_, err := builder.BuildImage(t.Context(), testImageRequest(t.TempDir()))
	var quotaFailure *OutputQuotaFailure
	if !errors.As(err, &quotaFailure) || quotaFailure.LimitBytes != 4 {
		t.Fatalf("BuildImage error = %v, want typed 4-byte quota failure", err)
	}
	if _, statErr := os.Stat(filepath.Join(outputRoot, "run", "workspace")); !errors.Is(statErr, os.ErrNotExist) {
		t.Fatalf("partial OCI output remains: %v", statErr)
	}
}

func testImageRequest(root string) imagebuild.Request {
	return imagebuild.Request{
		RunID:       "run",
		WorkspaceID: "workspace",
		Source: imagebuild.Source{
			ProjectRoot: root,
			SHA:         "sha256:source",
		},
		Build: imagebuild.Build{
			FormatVersion: imagebuild.FormatVersion,
			Root:          "workspace",
			Images: []imagebuild.Spec{{
				Key:      "workspace",
				Platform: imagebuild.Platform{OS: "linux", Architecture: "x86_64"},
				Steps: []imagebuild.Step{{
					From: &imagebuild.From{Ref: "docker.io/library/debian:bookworm"},
				}},
			}},
		},
	}
}
