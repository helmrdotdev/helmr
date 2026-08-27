package main

import (
	"context"
	"path/filepath"
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
