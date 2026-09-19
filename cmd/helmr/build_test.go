package main

import (
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/helmrdotdev/helmr/internal/builder"
	"github.com/helmrdotdev/helmr/internal/hostconfig"
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
		dockerBuildRunner{},
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
	fakeDocker(t, "running")
	originalRun, originalImage, originalTemp, originalEvaluate := runDockerBuildx, deploymentBundleBuilderImage, buildContextTempDir, evaluateHostConfig
	t.Cleanup(func() {
		runDockerBuildx = originalRun
		deploymentBundleBuilderImage = originalImage
		buildContextTempDir = originalTemp
		evaluateHostConfig = originalEvaluate
	})
	evaluateHostConfig = func(context.Context, string, io.Writer) (hostconfig.Document, error) {
		return resolvedDocument(), nil
	}
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
	err := buildDeploymentBundleAt(t.Context(), &cobra.Command{}, source, filepath.Join(t.TempDir(), "bundle"), false)
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

func resolvedDocument(steps ...hostconfig.Step) hostconfig.Document {
	document := hostconfig.Document{Build: hostconfig.Build{Builder: hostconfig.Builder{Steps: steps}, Secrets: []string{}}}
	if steps == nil {
		document.Build.Builder.Steps = []hostconfig.Step{}
	}
	document.Discovery.Dirs = []string{"src"}
	document.Discovery.IgnorePatterns = []string{}
	return document
}

// The config is evaluated once against the live project; its builder steps are
// materialized once from the captured source and that exact image, not the
// steps, feeds the install. Settings come from the config, not from flags.
func TestBuildEvaluatesConfigOnceAndMaterializesOneEnvironment(t *testing.T) {
	fakeDocker(t, "running")
	originalRun, originalImage, originalTemp, originalEvaluate := runDockerBuildx, deploymentBundleBuilderImage, buildContextTempDir, evaluateHostConfig
	t.Cleanup(func() {
		runDockerBuildx = originalRun
		deploymentBundleBuilderImage = originalImage
		buildContextTempDir = originalTemp
		evaluateHostConfig = originalEvaluate
	})
	deploymentBundleBuilderImage = "ghcr.io/helmrdotdev/bundle-builder@sha256:" + strings.Repeat("a", 64)
	buildContextTempDir = t.TempDir()
	source := t.TempDir()
	for name, body := range map[string]string{
		"package.json":   `{"name":"fixture","private":true}`,
		"build/setup.sh": "#!/bin/sh\n",
		".dockerignore":  "build\n",
	} {
		if err := os.MkdirAll(filepath.Dir(filepath.Join(source, name)), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(source, name), []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	live, err := filepath.EvalSymlinks(source)
	if err != nil {
		t.Fatal(err)
	}
	t.Setenv("NPM_TOKEN", "value-that-must-not-be-written")
	evaluations := 0
	evaluateHostConfig = func(_ context.Context, project string, _ io.Writer) (hostconfig.Document, error) {
		evaluations++
		if resolved, _ := filepath.EvalSymlinks(project); resolved != live {
			t.Fatalf("config evaluated in %q, want the live project %q", project, live)
		}
		document := resolvedDocument(
			hostconfig.Step{Kind: "copy", Source: "build/setup.sh", Destination: "/opt/setup.sh"},
			hostconfig.Step{Kind: "run", Argv: []string{"/bin/sh", "/opt/setup.sh"}},
		)
		document.Build.InstallCommand = "./prepare.sh"
		document.Build.Secrets = []string{"NPM_TOKEN"}
		return document, nil
	}
	stopped := errors.New("stop after install request")
	var targets []string
	runDockerBuildx = func(_ context.Context, _ *cobra.Command, request dockerBuildxRequest) error {
		targets = append(targets, request.Target)
		dockerfile, err := os.ReadFile(request.Dockerfile)
		if err != nil {
			t.Fatal(err)
		}
		ignore, err := os.ReadFile(request.Dockerfile + ".dockerignore")
		if err != nil || !strings.HasPrefix(string(ignore), "#") || strings.Contains(string(ignore), "build") {
			t.Fatalf("graph %s has no source-preserving ignore file: %q %v", request.Target, ignore, err)
		}
		switch request.Target {
		case "environment":
			if request.ContextDirectory == live || len(request.SecretIDs) != 0 ||
				request.BuildContexts["helmr-builder"] != "docker-image://"+deploymentBundleBuilderImage {
				t.Fatalf("environment request = %+v", request)
			}
			if _, err := os.Stat(filepath.Join(request.ContextDirectory, "build", "setup.sh")); err != nil {
				t.Fatalf("captured context lacks the copy source: %v", err)
			}
			if !strings.Contains(string(dockerfile), `COPY ["/build/setup.sh","/opt/setup.sh"]`+"\n"+`RUN ["/bin/sh","/opt/setup.sh"]`) {
				t.Fatalf("environment graph:\n%s", dockerfile)
			}
			writeTestLayout(t, request.Output)
			return nil
		case "installed-tree":
			environment := request.BuildContexts["helmr_environment"]
			if !strings.HasPrefix(environment, "oci-layout://") || !strings.Contains(environment, "@sha256:") {
				t.Fatalf("install does not start from the materialized environment: %q", environment)
			}
			if strings.Contains(string(dockerfile), "/opt/setup.sh") {
				t.Fatalf("install graph repeats preparation steps:\n%s", dockerfile)
			}
			if !strings.Contains(string(dockerfile), `"/bin/bash","-euo","pipefail","-c","./prepare.sh"`) ||
				len(request.SecretIDs) != 1 || request.SecretIDs[0] != "NPM_TOKEN" ||
				strings.Contains(string(dockerfile), "value-that-must-not-be-written") {
				t.Fatalf("install settings did not come from the config: %+v\n%s", request, dockerfile)
			}
			return stopped
		}
		t.Fatalf("unexpected graph %q", request.Target)
		return nil
	}
	err = buildDeploymentBundleAt(t.Context(), &cobra.Command{}, source, filepath.Join(t.TempDir(), "bundle"), false)
	if !errors.Is(err, stopped) {
		t.Fatalf("build: %v", err)
	}
	if evaluations != 1 || strings.Join(targets, ",") != "environment,installed-tree" {
		t.Fatalf("evaluations = %d, graphs = %v", evaluations, targets)
	}

	// No builder steps: the pinned builder is the environment and nothing is materialized.
	evaluateHostConfig = func(context.Context, string, io.Writer) (hostconfig.Document, error) { return resolvedDocument(), nil }
	runDockerBuildx = func(_ context.Context, _ *cobra.Command, request dockerBuildxRequest) error {
		if request.Target != "installed-tree" || request.BuildContexts["helmr_environment"] != "docker-image://"+deploymentBundleBuilderImage {
			t.Fatalf("unprepared build request = %+v", request)
		}
		return stopped
	}
	if err := buildDeploymentBundleAt(t.Context(), &cobra.Command{}, source, filepath.Join(t.TempDir(), "bundle"), false); !errors.Is(err, stopped) {
		t.Fatalf("build: %v", err)
	}

	// A copy source that exists on the host but is excluded from the capture stops the build before Docker.
	evaluateHostConfig = func(context.Context, string, io.Writer) (hostconfig.Document, error) {
		return resolvedDocument(hostconfig.Step{Kind: "copy", Source: "host-only/tool", Destination: "/opt/tool"}), nil
	}
	runDockerBuildx = func(context.Context, *cobra.Command, dockerBuildxRequest) error {
		t.Fatal("an unresolvable builder reached BuildKit")
		return nil
	}
	if err := os.MkdirAll(filepath.Join(source, "host-only", "tool"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(source, ".helmrignore"), []byte("host-only\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	err = buildDeploymentBundleAt(t.Context(), &cobra.Command{}, source, filepath.Join(t.TempDir(), "bundle"), false)
	if err == nil || !strings.Contains(err.Error(), "not in the captured project source") {
		t.Fatalf("host-only copy source error = %v", err)
	}
}

// writeTestLayout produces the smallest valid linux/amd64 OCI layout.
func writeTestLayout(t *testing.T, directory string) {
	t.Helper()
	manifest := []byte(`{"schemaVersion":2}`)
	digest := fmt.Sprintf("sha256:%x", sha256.Sum256(manifest))
	for name, body := range map[string][]byte{
		"oci-layout": []byte(`{"imageLayoutVersion":"1.0.0"}`),
		"index.json": []byte(fmt.Sprintf(`{"schemaVersion":2,"manifests":[{"mediaType":"application/vnd.oci.image.manifest.v1+json","digest":%q,"size":%d,"platform":{"os":"linux","architecture":"amd64"}}]}`, digest, len(manifest))),
		filepath.Join("blobs", "sha256", strings.TrimPrefix(digest, "sha256:")): manifest,
	} {
		path := filepath.Join(directory, name)
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, body, 0o644); err != nil {
			t.Fatal(err)
		}
	}
}
