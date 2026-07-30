package buildkit

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/helmrdotdev/helmr/internal/imagebuild"
	bkclient "github.com/moby/buildkit/client"
	"github.com/moby/buildkit/client/llb"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
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

func TestBuildImageClassifiesUnavailableBuildKitAsWorkerFatal(t *testing.T) {
	solveErr := status.Error(codes.Unavailable, "daemon stopped")
	if got := solveErrorCode(solveErr); got != codes.Unavailable {
		t.Fatalf("solve error code = %s, want Unavailable", got)
	}
	solver := testSolver{solve: func(
		context.Context,
		*llb.Definition,
		bkclient.SolveOpt,
	) (*bkclient.SolveResponse, error) {
		return nil, solveErr
	}}
	outputRoot := t.TempDir()
	builder := New(solver, outputRoot)
	_, err := builder.BuildImage(t.Context(), testImageRequest(t.TempDir()))
	var failure *ServiceFailure
	if !errors.As(err, &failure) || !failure.FatalWorker() {
		t.Fatalf("BuildImage error = %v, want fatal service failure", err)
	}
	if _, statErr := os.Stat(filepath.Join(outputRoot, "run", "workspace")); !errors.Is(statErr, os.ErrNotExist) {
		t.Fatalf("failed build output remains: %v", statErr)
	}
}

func TestConfigRejectsDockerEndpoints(t *testing.T) {
	for _, addr := range []string{
		"unix:///var/run/docker.sock",
		"unix:///run/docker.sock",
		"docker-container://builder",
		"npipe:////./pipe/docker_engine",
	} {
		if _, err := (Config{Addr: addr}).endpoint(); err == nil {
			t.Errorf("endpoint accepted %q", addr)
		}
	}
	if got, err := (Config{}).endpoint(); err != nil || got != defaultBuildKitAddr {
		t.Fatalf("default endpoint = %q, %v", got, err)
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
