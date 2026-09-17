package main

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/helmrdotdev/helmr/internal/builder"
	"github.com/helmrdotdev/helmr/internal/imagebuild"
	"github.com/spf13/cobra"
)

func TestBuildWorkspaceImagesBuildsUniqueRenderedInputOnce(t *testing.T) {
	original := runDockerBuildx
	t.Cleanup(func() { runDockerBuildx = original })
	var requests []dockerBuildxRequest
	runDockerBuildx = func(_ context.Context, _ *cobra.Command, request dockerBuildxRequest) error {
		requests = append(requests, request)
		return nil
	}
	workspaceBuild := func(declaredID, ref string) builder.WorkspaceBuild {
		return builder.WorkspaceBuild{
			DeclaredID: declaredID,
			Build: imagebuild.Build{
				Root: "root",
				Images: []imagebuild.Spec{{
					Key:      "root",
					Platform: imagebuild.Platform{OS: "linux", Architecture: "x86_64"},
					Steps:    []imagebuild.Step{{From: &imagebuild.From{Ref: ref}}},
				}},
			},
		}
	}
	stage := t.TempDir()
	workspaceContext := t.TempDir()
	inputs, err := buildWorkspaceImages(
		context.Background(),
		&cobra.Command{},
		stage,
		workspaceContext,
		t.TempDir(),
		map[string]string{"helmr_installed": "installed"},
		[]builder.WorkspaceBuild{
			workspaceBuild("first", "ubuntu:24.04"),
			workspaceBuild("middle", "alpine:3.22"),
			workspaceBuild("third", "docker.io/library/ubuntu:24.04"),
		},
	)
	if err != nil {
		t.Fatal(err)
	}
	if len(requests) != 2 {
		t.Fatalf("BuildKit requests = %d, want 2", len(requests))
	}
	firstPath := "/workspace/images/workspace-000.oci.tar"
	if len(inputs) != 3 ||
		inputs[0]["declaredId"] != "first" || inputs[0]["path"] != firstPath ||
		inputs[1]["declaredId"] != "middle" || inputs[1]["path"] != "/workspace/images/workspace-001.oci.tar" ||
		inputs[2]["declaredId"] != "third" || inputs[2]["path"] != firstPath {
		t.Fatalf("workspace inputs = %+v", inputs)
	}
	if requests[0].Output != filepath.Join(workspaceContext, "workspace-000.oci.tar") ||
		requests[1].Output != filepath.Join(workspaceContext, "workspace-001.oci.tar") {
		t.Fatalf("BuildKit requests = %+v", requests)
	}
}

func TestBuildUsesIndependentPrivateContextAndCleansFailure(t *testing.T) {
	originalRun, originalImage, originalTemp := runDockerBuildx, deploymentBundleBuilderImage, buildContextTempDir
	t.Cleanup(func() {
		runDockerBuildx = originalRun
		deploymentBundleBuilderImage = originalImage
		buildContextTempDir = originalTemp
	})
	deploymentBundleBuilderImage = "ghcr.io/helmrdotdev/bundle-builder@sha256:" + strings.Repeat("a", 64)
	source, temp := t.TempDir(), t.TempDir()
	buildContextTempDir = temp
	if err := os.WriteFile(filepath.Join(source, "package.json"), []byte(`{"name":"fixture","private":true}`), 0644); err != nil {
		t.Fatal(err)
	}
	leaf := strings.Repeat("x", 124)
	if err := os.WriteFile(filepath.Join(source, leaf), []byte("before"), 0644); err != nil {
		t.Fatal(err)
	}
	stopped := errors.New("stop at installer boundary")
	contextPath := ""
	runDockerBuildx = func(_ context.Context, _ *cobra.Command, request dockerBuildxRequest) error {
		contextPath = request.ContextDirectory
		info, err := os.Stat(contextPath)
		if err != nil || !info.IsDir() || info.Mode().Perm() != 0700 {
			t.Fatalf("private context: %v %v", info, err)
		}
		if contextPath == source {
			t.Fatal("Buildx received live checkout")
		}
		if err := os.WriteFile(filepath.Join(source, leaf), []byte("after"), 0644); err != nil {
			t.Fatal(err)
		}
		body, err := os.ReadFile(filepath.Join(contextPath, leaf))
		if err != nil || string(body) != "before" {
			t.Fatalf("captured source: %q %v", body, err)
		}
		return stopped
	}
	err := buildDeploymentBundleAt(t.Context(), &cobra.Command{}, source, filepath.Join(t.TempDir(), "bundle"), "npm install", nil, false)
	if !errors.Is(err, stopped) {
		t.Fatalf("build: %v", err)
	}
	if contextPath == "" {
		t.Fatal("installer was not reached")
	}
	if _, err := os.Lstat(contextPath); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("build failure retained context: %v", err)
	}
	children, err := os.ReadDir(temp)
	if err != nil || len(children) != 0 {
		t.Fatalf("build context cleanup: %v %v", children, err)
	}
}
