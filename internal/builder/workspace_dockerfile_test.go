package builder

import (
	"strings"
	"testing"

	"github.com/helmrdotdev/helmr/internal/imagebuild"
)

func TestWorkspaceImageDockerfileUsesInstalledTreeAndPinnedBases(t *testing.T) {
	base := "docker.io/library/alpine@sha256:" + strings.Repeat("b", 64)
	build := imagebuild.Build{
		FormatVersion: imagebuild.FormatVersion,
		Root:          "root",
		Images: []imagebuild.Spec{{
			Key: "root", Platform: imagebuild.Platform{OS: "linux", Architecture: "x86_64"},
			Steps: []imagebuild.Step{
				{From: &imagebuild.From{Ref: base}},
				{CopySourceFile: &imagebuild.CopySourceFile{Path: "dist/app.js", Dst: "/app/app.js"}},
				{Env: &imagebuild.Env{Key: "MESSAGE", Value: "hello world"}},
				{Run: &imagebuild.Run{Argv: []string{"/bin/sh", "-c", "test -f /app/app.js"}}},
			},
		}},
	}
	raw, target, err := WorkspaceImageDockerfile(build)
	if err != nil {
		t.Fatal(err)
	}
	document := string(raw)
	for _, required := range []string{
		"# syntax=" + dockerfileFrontend,
		"FROM helmr_installed AS installed-tree",
		"FROM --platform=linux/amd64 " + base + " AS " + target,
		`COPY --from=installed-tree ["/workspace/project/dist/app.js","/app/app.js"]`,
		`ENV MESSAGE="hello world"`,
		`RUN ["/bin/sh","-c","test -f /app/app.js"]`,
	} {
		if !strings.Contains(document, required) {
			t.Fatalf("Workspace Dockerfile is missing %q:\n%s", required, document)
		}
	}
	for _, forbidden := range []string{"npm ci", "pnpm install", "yarn install", "bun install", "COPY --chown=65532:65532 . .", "AS materialized", "chmod -R"} {
		if strings.Contains(document, forbidden) {
			t.Fatalf("Workspace Dockerfile repeats producer operation %q:\n%s", forbidden, document)
		}
	}
}

func TestWorkspaceImageDockerfileRejectsMutableBase(t *testing.T) {
	build := imagebuild.Build{
		FormatVersion: imagebuild.FormatVersion, Root: "root",
		Images: []imagebuild.Spec{{
			Key: "root", Platform: imagebuild.Platform{OS: "linux", Architecture: "x86_64"},
			Steps: []imagebuild.Step{{From: &imagebuild.From{Ref: "docker.io/library/alpine:3.22"}}},
		}},
	}
	if _, _, err := WorkspaceImageDockerfile(build); err == nil {
		t.Fatal("WorkspaceImageDockerfile accepted a mutable base")
	}
}
