package executor

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/helmrdotdev/helmr/internal/api"
	"github.com/helmrdotdev/helmr/internal/archive"
	"github.com/helmrdotdev/helmr/internal/builder"
	"github.com/helmrdotdev/helmr/internal/cas"
	"github.com/helmrdotdev/helmr/internal/proto/bundle/v0"
	"github.com/helmrdotdev/helmr/internal/sha256sum"
	"google.golang.org/protobuf/proto"
)

func TestExecutorBuildsMaterializedSources(t *testing.T) {
	workDir := t.TempDir()

	builder := &fakeBuilder{artifact: builder.Artifact{ImageTarPath: "/rootfs.ext4", ConfigPath: "/config.json"}}
	runner := &fakeRunner{exitCode: 0, output: json.RawMessage(`{"ok":true}`)}
	store := deploymentSourceCAS(t)
	result := Executor{
		WorkDir: workDir,
		GitPath: fakeGit(t),
		CAS:     store,
		Builder: builder,
		Runner:  runner,
	}.Execute(context.Background(), api.WorkerRunLease{}, validRun())

	if result.Kind != "completed" || result.ExitCode == nil || *result.ExitCode != 0 {
		t.Fatalf("result = %+v", result)
	}
	if string(result.Output) != `{"ok":true}` {
		t.Fatalf("output = %s", result.Output)
	}
	if builder.request.RunID != "run-1" || builder.request.TaskID != "deploy" {
		t.Fatalf("build request = %+v", builder.request)
	}
	if builder.request.CacheScope != validDeploymentSource().Digest+"/deploy" {
		t.Fatalf("build cache scope = %q", builder.request.CacheScope)
	}
	if builder.request.Bundle == nil || builder.request.Bundle.Image == nil {
		t.Fatalf("build bundle = %+v", builder.request.Bundle)
	}
	if builder.request.Source.SHA != validDeploymentSource().Digest || builder.request.Source.ProjectRoot == "" {
		t.Fatalf("build source = %+v", builder.request.Source)
	}
	if runner.request.Artifact.ImageTarPath != "/rootfs.ext4" {
		t.Fatalf("runtime request = %+v", runner.request)
	}
	if runner.request.DeploymentSource.ProjectRoot == "" || runner.request.Workspace.Path == "" || runner.request.Workspace.Digest == "" {
		t.Fatalf("runtime inputs = task:%+v workspace:%+v", runner.request.DeploymentSource, runner.request.Workspace)
	}
	if runner.request.Workspace.MediaType == "" || runner.request.Workspace.Encoding != "tar" {
		t.Fatalf("workspace artifact = %+v", runner.request.Workspace)
	}
	object, err := store.Stat(context.Background(), runner.request.Workspace.Digest)
	if err != nil {
		t.Fatalf("workspace artifact was not published to CAS: %+v", runner.request.Workspace)
	}
	if object.SizeBytes != runner.request.Workspace.SizeBytes || object.MediaType != runner.request.Workspace.MediaType {
		t.Fatalf("workspace CAS object = %+v, workspace = %+v", object, runner.request.Workspace)
	}
	entries, err := os.ReadDir(workDir)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 0 {
		t.Fatalf("checkout dir was not cleaned up: %+v", entries)
	}
}

func TestExecutorPassesResolvedSecretsToBuilder(t *testing.T) {
	t.Setenv("FAKE_GIT_LOG", filepath.Join(t.TempDir(), "git.log"))
	build := &fakeBuilder{artifact: builder.Artifact{ImageTarPath: "/rootfs.ext4"}}
	run := validRun()
	run.Secrets = api.ResolvedSecrets{"TOKEN": []byte("secret-value")}
	result := Executor{
		WorkDir: t.TempDir(),
		GitPath: fakeGit(t),
		CAS:     deploymentSourceCASWithBundle(t, secretBundle()),
		Builder: build,
		Runner:  &fakeRunner{},
	}.Execute(context.Background(), api.WorkerRunLease{}, run)
	if result.Kind != "completed" {
		t.Fatalf("result = %+v", result)
	}
	if string(build.request.BuildSecrets["TOKEN"]) != "secret-value" {
		t.Fatalf("build secrets = %+v", build.request.BuildSecrets)
	}
}

func TestExecutorLoadsDeploymentTaskBundleFromCAS(t *testing.T) {
	t.Setenv("FAKE_GIT_LOG", filepath.Join(t.TempDir(), "git.log"))
	run := validRun()

	result := Executor{
		WorkDir: t.TempDir(),
		GitPath: fakeGit(t),
		CAS:     deploymentSourceCAS(t),
		Builder: &fakeBuilder{artifact: builder.Artifact{ImageTarPath: "/rootfs.ext4"}},
		Runner:  &fakeRunner{},
	}.Execute(context.Background(), api.WorkerRunLease{}, run)

	if result.Kind != "completed" {
		t.Fatalf("result = %+v", result)
	}
	if run.DeploymentTask.BundleDigest != validTaskBundleDigest() {
		t.Fatalf("run bundle digest = %q", run.DeploymentTask.BundleDigest)
	}
}

func TestExecutorMaterializesDeploymentSourceArtifactFromCAS(t *testing.T) {
	sourceRoot := t.TempDir()
	if err := os.MkdirAll(filepath.Join(sourceRoot, "src"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(sourceRoot, "src", "task.ts"), []byte("task"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(sourceRoot, ".git"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(sourceRoot, ".git", "config"), []byte("git"), 0o644); err != nil {
		t.Fatal(err)
	}
	tarArchive, cleanup, err := archive.CreateTar(sourceRoot, t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer cleanup()
	content, err := os.ReadFile(tarArchive.Path)
	if err != nil {
		t.Fatal(err)
	}

	t.Setenv("FAKE_GIT_LOG", filepath.Join(t.TempDir(), "git.log"))
	build := &fakeBuilder{artifact: builder.Artifact{ImageTarPath: "/rootfs.ext4"}}
	run := validRun()
	run.DeploymentSource = api.DeploymentSourceArtifact{Digest: tarArchive.Digest}

	result := Executor{
		WorkDir: t.TempDir(),
		GitPath: fakeGit(t),
		CAS:     deploymentSourceCASWithObjects(t, map[string][]byte{tarArchive.Digest: content}),
		Builder: build,
		Runner:  &fakeRunner{},
	}.Execute(context.Background(), api.WorkerRunLease{}, run)

	if result.Kind != "completed" {
		t.Fatalf("result = %+v", result)
	}
	if build.request.CacheScope != tarArchive.Digest+"/deploy" {
		t.Fatalf("cache scope = %q", build.request.CacheScope)
	}
}

func TestExecutorRestoresWithoutCheckoutOrBuild(t *testing.T) {
	runner := &fakeRunner{}
	run := validRun()
	run.Restore = &api.WorkerRestore{
		CheckpointID: "checkpoint-1",
		Waitpoint: api.WorkerRestoreWaitpoint{
			ID:         "waitpoint-1",
			RunWaitID:  "run-wait-1",
			ResumeKind: "completed",
		},
	}
	result := Executor{
		WorkDir: t.TempDir(),
		GitPath: "/missing/git",
		Runner:  runner,
	}.Execute(context.Background(), api.WorkerRunLease{}, run)

	if result.Kind != "completed" {
		t.Fatalf("result = %+v", result)
	}
	if runner.request.Run.Restore == nil || runner.request.Run.Restore.CheckpointID != "checkpoint-1" {
		t.Fatalf("runtime request = %+v", runner.request)
	}
}

func TestExecutorReturnsBuildBoundaryError(t *testing.T) {
	t.Setenv("FAKE_GIT_LOG", filepath.Join(t.TempDir(), "git.log"))

	result := Executor{WorkDir: t.TempDir(), GitPath: fakeGit(t)}.Execute(context.Background(), api.WorkerRunLease{}, validRun())
	if result.Kind != "failed" || result.Error == nil || !strings.Contains(*result.Error, ErrBuilderRequired.Error()) {
		t.Fatalf("result = %+v", result)
	}
}

func TestExecutorReturnsRuntimeBoundaryError(t *testing.T) {
	t.Setenv("FAKE_GIT_LOG", filepath.Join(t.TempDir(), "git.log"))

	result := Executor{
		WorkDir: t.TempDir(),
		GitPath: fakeGit(t),
		CAS:     deploymentSourceCAS(t),
		Builder: &fakeBuilder{artifact: builder.Artifact{ImageTarPath: "/rootfs.ext4"}},
	}.Execute(context.Background(), api.WorkerRunLease{}, validRun())
	if result.Kind != "failed" || result.Error == nil || !strings.Contains(*result.Error, ErrRunnerRequired.Error()) {
		t.Fatalf("result = %+v", result)
	}
}

func TestExecutorRequiresTaskBundle(t *testing.T) {
	t.Setenv("FAKE_GIT_LOG", filepath.Join(t.TempDir(), "git.log"))

	run := validRun()
	run.DeploymentTask.BundleDigest = ""
	result := Executor{
		WorkDir: t.TempDir(),
		GitPath: fakeGit(t),
		CAS:     deploymentSourceCAS(t),
		Builder: &fakeBuilder{artifact: builder.Artifact{ImageTarPath: "/rootfs.ext4"}},
	}.Execute(context.Background(), api.WorkerRunLease{}, run)
	if result.Kind != "failed" || result.Error == nil || !strings.Contains(*result.Error, "bundle_digest is required") {
		t.Fatalf("result = %+v", result)
	}
}

type fakeBuilder struct {
	request  builder.Request
	artifact builder.Artifact
	err      error
}

func (b *fakeBuilder) Build(_ context.Context, request builder.Request) (builder.Artifact, error) {
	b.request = request
	if b.err != nil {
		return builder.Artifact{}, b.err
	}
	return b.artifact, nil
}

type fakeRunner struct {
	request  Request
	exitCode int32
	output   json.RawMessage
	err      error
}

func (r *fakeRunner) Run(_ context.Context, request Request) (Result, error) {
	r.request = request
	if r.err != nil {
		return Result{}, r.err
	}
	return Result{ExitCode: r.exitCode, Output: r.output}, nil
}

type artifactCAS struct {
	objects    map[string][]byte
	mediaTypes map[string]string
}

func (f *artifactCAS) Put(_ context.Context, mediaType string, body io.Reader) (cas.Object, error) {
	content, err := io.ReadAll(body)
	if err != nil {
		return cas.Object{}, err
	}
	digest := sha256sum.DigestBytes(content)
	f.objects[digest] = append([]byte(nil), content...)
	f.mediaTypes[digest] = mediaType
	return cas.Object{Digest: digest, SizeBytes: int64(len(content)), MediaType: mediaType}, nil
}

func (f *artifactCAS) Stage(context.Context, string) (cas.Stage, error) {
	return nil, errors.New("not implemented")
}

func (f *artifactCAS) Stat(_ context.Context, digest string) (cas.Object, error) {
	content, ok := f.objects[digest]
	if !ok {
		return cas.Object{}, errors.New("object not found")
	}
	return cas.Object{Digest: digest, SizeBytes: int64(len(content)), MediaType: f.mediaTypes[digest]}, nil
}

func (f *artifactCAS) Get(_ context.Context, digest string) (io.ReadCloser, error) {
	content, ok := f.objects[digest]
	if !ok {
		return nil, errors.New("object not found")
	}
	return io.NopCloser(bytes.NewReader(content)), nil
}

func (f *artifactCAS) Delete(context.Context, string) error {
	return nil
}

func deploymentSourceCAS(t *testing.T) *artifactCAS {
	t.Helper()
	return deploymentSourceCASWithBundle(t, testBundle())
}

func deploymentSourceCASWithBundle(t *testing.T, bundle *bundlev0.Bundle) *artifactCAS {
	t.Helper()
	root := t.TempDir()
	if err := os.WriteFile(filepath.Join(root, "task.ts"), []byte("export default {}"), 0o644); err != nil {
		t.Fatal(err)
	}
	tarArchive, cleanup, err := archive.CreateTar(root, t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer cleanup()
	content, err := os.ReadFile(tarArchive.Path)
	if err != nil {
		t.Fatal(err)
	}
	return deploymentSourceCASWithObjects(t, map[string][]byte{validDeploymentSource().Digest: content, validTaskBundleDigest(): marshalBundle(t, bundle)})
}

func deploymentSourceCASWithObjects(t *testing.T, objects map[string][]byte) *artifactCAS {
	t.Helper()
	if _, ok := objects[validTaskBundleDigest()]; !ok {
		objects[validTaskBundleDigest()] = marshalBundle(t, testBundle())
	}
	mediaTypes := make(map[string]string, len(objects))
	for digest := range objects {
		mediaTypes[digest] = cas.DeploymentSourceArtifactMediaType
	}
	mediaTypes[validTaskBundleDigest()] = "application/vnd.helmr.task-bundle.v0+proto"
	return &artifactCAS{objects: objects, mediaTypes: mediaTypes}
}

func marshalBundle(t *testing.T, bundle *bundlev0.Bundle) []byte {
	t.Helper()
	body, err := proto.Marshal(bundle)
	if err != nil {
		t.Fatal(err)
	}
	return body
}

func testBundle() *bundlev0.Bundle {
	return &bundlev0.Bundle{
		Task: &bundlev0.TaskSpec{ModulePath: "src/task.ts"},
		Image: &bundlev0.ImageSpec{
			Steps: []*bundlev0.ImageStep{{
				Kind: &bundlev0.ImageStep_From{From: &bundlev0.From{Ref: "debian:trixie-slim"}},
			}},
		},
	}
}

func secretBundle() *bundlev0.Bundle {
	bundle := testBundle()
	bundle.Image = &bundlev0.ImageSpec{
		Steps: []*bundlev0.ImageStep{{
			Kind: &bundlev0.ImageStep_Run{Run: &bundlev0.Run{
				Argv: []string{"true"},
				SecretMounts: []*bundlev0.SecretMountBinding{{
					Dst:       "/run/secrets/TOKEN",
					SecretRef: &bundlev0.SecretRef{Name: "TOKEN"},
				}},
			}},
		}},
	}
	return bundle
}

func fakeGit(t *testing.T) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "git")
	script := `#!/bin/sh
set -eu
printf '%s\n' "$*" >> "$FAKE_GIT_LOG"
if [ -n "${FAKE_EXPECT_AUTH:-}" ] && [ "${GIT_CONFIG_VALUE_0:-}" != "${FAKE_EXPECT_AUTH}" ]; then
	echo "missing installation auth" >&2
	exit 98
fi

root=""
if [ "${1:-}" = "init" ]; then
	root="${3:-}"
	mkdir -p "$root/.git"
	exit 0
fi

if [ "${1:-}" = "-C" ]; then
	root="$2"
	shift 2
fi

case "${1:-}" in
	remote|fetch)
		exit 0
		;;
	checkout)
		mkdir -p "$root"
		printf '%s\n' "${FAKE_EXPECT_SHA}" > "$root/.git/HEAD_VALUE"
		exit 0
		;;
	rev-parse)
		cat "$root/.git/HEAD_VALUE"
		exit 0
		;;
	*)
		echo "unexpected git command: $*" >&2
		exit 99
		;;
esac
`
	if err := os.WriteFile(path, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	return path
}
