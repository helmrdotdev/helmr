package builder

import (
	"strings"
	"testing"

	"github.com/helmrdotdev/helmr/internal/imagebuild"
)

func TestComputerImageDockerfileUsesInstalledTreeAndDigestPinnedBase(t *testing.T) {
	base := "docker.io/library/alpine@sha256:" + strings.Repeat("b", 64)
	build := imagebuild.Build{
		Root: "root",
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
	raw, target, err := ComputerImageDockerfile(build)
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
			t.Fatalf("Computer Dockerfile is missing %q:\n%s", required, document)
		}
	}
	for _, forbidden := range []string{"npm ci", "pnpm install", "yarn install", "bun install", "COPY --chown=65532:65532 . .", "AS materialized", "chmod -R"} {
		if strings.Contains(document, forbidden) {
			t.Fatalf("Computer Dockerfile repeats producer operation %q:\n%s", forbidden, document)
		}
	}
}

func TestComputerImageDockerfileAcceptsTaggedBase(t *testing.T) {
	build := imagebuild.Build{
		Root: "root",
		Images: []imagebuild.Spec{{
			Key: "root", Platform: imagebuild.Platform{OS: "linux", Architecture: "x86_64"},
			Steps: []imagebuild.Step{{From: &imagebuild.From{Ref: "node:24-bookworm-slim"}}},
		}},
	}
	raw, _, err := ComputerImageDockerfile(build)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(raw), "FROM --platform=linux/amd64 docker.io/library/node:24-bookworm-slim") {
		t.Fatalf("Computer Dockerfile did not preserve tagged base:\n%s", raw)
	}
}

func TestComputerImageDockerfileDisambiguatesInternalNames(t *testing.T) {
	for _, base := range []string{"helmr_installed", "installed-tree", "helmr_computer_0"} {
		t.Run(base, func(t *testing.T) {
			build := imagebuild.Build{
				Root: "root",
				Images: []imagebuild.Spec{{
					Key: "root", Platform: imagebuild.Platform{OS: "linux", Architecture: "x86_64"},
					Steps: []imagebuild.Step{{From: &imagebuild.From{Ref: base}}},
				}},
			}
			raw, _, err := ComputerImageDockerfile(build)
			if err != nil {
				t.Fatal(err)
			}
			want := "FROM --platform=linux/amd64 docker.io/library/" + base
			if !strings.Contains(string(raw), want) {
				t.Fatalf("Computer Dockerfile did not disambiguate %q:\n%s", base, raw)
			}
		})
	}
}

func TestComputerImageDockerfilePreservesScratchBase(t *testing.T) {
	build := imagebuild.Build{
		Root: "root",
		Images: []imagebuild.Spec{{
			Key: "root", Platform: imagebuild.Platform{OS: "linux", Architecture: "x86_64"},
			Steps: []imagebuild.Step{{From: &imagebuild.From{Ref: "scratch"}}},
		}},
	}
	raw, _, err := ComputerImageDockerfile(build)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(raw), "FROM --platform=linux/amd64 scratch") {
		t.Fatalf("Computer Dockerfile did not preserve scratch base:\n%s", raw)
	}
}

func TestComputerImageDockerfileRejectsInvalidBase(t *testing.T) {
	build := imagebuild.Build{
		Root: "root",
		Images: []imagebuild.Spec{{
			Key: "root", Platform: imagebuild.Platform{OS: "linux", Architecture: "x86_64"},
			Steps: []imagebuild.Step{{From: &imagebuild.From{Ref: "not a valid ref"}}},
		}},
	}
	if _, _, err := ComputerImageDockerfile(build); err == nil {
		t.Fatal("ComputerImageDockerfile accepted an invalid base")
	}
}
