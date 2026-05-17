package executor

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/helmrdotdev/helmr/internal/api"
	"github.com/helmrdotdev/helmr/internal/builder"
	"github.com/helmrdotdev/helmr/internal/cas"
	bundlev0 "github.com/helmrdotdev/helmr/internal/gen/helmr/bundle/v0"
	"github.com/helmrdotdev/helmr/internal/sourcetar"
)

func TestExecutorBuildsMaterializedSources(t *testing.T) {
	workDir := t.TempDir()
	logPath := filepath.Join(t.TempDir(), "git.log")
	t.Setenv("FAKE_GIT_LOG", logPath)
	t.Setenv("FAKE_EXPECT_SHA", validSource().SHA)

	builder := &fakeBuilder{artifact: builder.Artifact{ImageTarPath: "/rootfs.ext4", ConfigPath: "/config.json"}}
	runner := &fakeRunner{exitCode: 0, output: json.RawMessage(`{"ok":true}`)}
	result := Executor{
		WorkDir:  workDir,
		GitPath:  fakeGit(t),
		CAS:      taskSourceCAS(t),
		Builder:  builder,
		Runner:   runner,
		Compiler: fakeCompiler{},
	}.Execute(context.Background(), api.WorkerClaim{}, validRun())

	if result.Kind != "completed" || result.ExitCode == nil || *result.ExitCode != 0 {
		t.Fatalf("result = %+v", result)
	}
	if string(result.Output) != `{"ok":true}` {
		t.Fatalf("output = %s", result.Output)
	}
	if builder.request.RunID != "run-1" || builder.request.TaskID != "deploy" {
		t.Fatalf("build request = %+v", builder.request)
	}
	if builder.request.CacheScope != validTaskSource().Digest+"/deploy" {
		t.Fatalf("build cache scope = %q", builder.request.CacheScope)
	}
	if builder.request.Bundle == nil || builder.request.Bundle.Image == nil {
		t.Fatalf("build bundle = %+v", builder.request.Bundle)
	}
	if builder.request.Source.SHA != validTaskSource().Digest || builder.request.Source.ProjectRoot == "" {
		t.Fatalf("build source = %+v", builder.request.Source)
	}
	if runner.request.Artifact.ImageTarPath != "/rootfs.ext4" {
		t.Fatalf("runtime request = %+v", runner.request)
	}
	if runner.request.TaskSource.ProjectRoot == "" || runner.request.WorkspaceSource.ProjectRoot == "" {
		t.Fatalf("runtime sources = task:%+v workspace:%+v", runner.request.TaskSource, runner.request.WorkspaceSource)
	}
	if runner.request.TaskSource.ProjectRoot == runner.request.WorkspaceSource.ProjectRoot {
		t.Fatalf("task and workspace sources should be separately materialized: %s", runner.request.TaskSource.ProjectRoot)
	}
	log := readFile(t, logPath)
	if !strings.Contains(log, "fetch --depth=1 origin "+validSource().SHA) {
		t.Fatalf("git log = %s", log)
	}
	entries, err := os.ReadDir(workDir)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 0 {
		t.Fatalf("checkout dir was not cleaned up: %+v", entries)
	}
}

func TestExecutorUsesWorkspaceCheckoutToken(t *testing.T) {
	token := "secret-token"
	logPath := filepath.Join(t.TempDir(), "git.log")
	t.Setenv("FAKE_GIT_LOG", logPath)
	t.Setenv("FAKE_EXPECT_SHA", validSource().SHA)
	t.Setenv("FAKE_EXPECT_AUTH", "AUTHORIZATION: basic "+base64.StdEncoding.EncodeToString([]byte("x-access-token:"+token)))

	run := validRun()
	run.WorkspaceCheckoutToken = &api.WorkerCheckoutToken{Token: token}
	result := Executor{
		WorkDir:  t.TempDir(),
		GitPath:  fakeGit(t),
		CAS:      taskSourceCAS(t),
		Builder:  &fakeBuilder{artifact: builder.Artifact{ImageTarPath: "/rootfs.ext4"}},
		Runner:   &fakeRunner{},
		Compiler: fakeCompiler{},
	}.Execute(context.Background(), api.WorkerClaim{}, run)

	if result.Kind != "completed" {
		t.Fatalf("result = %+v", result)
	}
	if !strings.Contains(readFile(t, logPath), "fetch --depth=1 origin "+validSource().SHA) {
		t.Fatal("git was not invoked")
	}
}

func TestExecutorPassesResolvedSecretsToBuilder(t *testing.T) {
	t.Setenv("FAKE_GIT_LOG", filepath.Join(t.TempDir(), "git.log"))
	t.Setenv("FAKE_EXPECT_SHA", validSource().SHA)
	build := &fakeBuilder{artifact: builder.Artifact{ImageTarPath: "/rootfs.ext4"}}
	run := validRun()
	run.Secrets = api.ResolvedSecrets{"TOKEN": []byte("secret-value")}
	result := Executor{
		WorkDir:  t.TempDir(),
		GitPath:  fakeGit(t),
		CAS:      taskSourceCAS(t),
		Builder:  build,
		Runner:   &fakeRunner{},
		Compiler: secretCompiler{},
	}.Execute(context.Background(), api.WorkerClaim{}, run)
	if result.Kind != "completed" {
		t.Fatalf("result = %+v", result)
	}
	if string(build.request.BuildSecrets["TOKEN"]) != "secret-value" {
		t.Fatalf("build secrets = %+v", build.request.BuildSecrets)
	}
}

func TestExecutorPassesRunSourceToCompiler(t *testing.T) {
	t.Setenv("FAKE_GIT_LOG", filepath.Join(t.TempDir(), "git.log"))
	t.Setenv("FAKE_EXPECT_SHA", validSource().SHA)
	compiler := &capturingCompiler{}
	run := validRun()

	result := Executor{
		WorkDir:  t.TempDir(),
		GitPath:  fakeGit(t),
		CAS:      taskSourceCAS(t),
		Builder:  &fakeBuilder{artifact: builder.Artifact{ImageTarPath: "/rootfs.ext4"}},
		Runner:   &fakeRunner{},
		Compiler: compiler,
	}.Execute(context.Background(), api.WorkerClaim{}, run)

	if result.Kind != "completed" {
		t.Fatalf("result = %+v", result)
	}
	if compiler.request.TaskID != "deploy" || compiler.request.Source.ProjectRoot == "" {
		t.Fatalf("compiler request = %+v", compiler.request)
	}
}

func TestExecutorMaterializesTaskSourceArtifactFromCAS(t *testing.T) {
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
	archive, cleanup, err := sourcetar.CreateTar(sourceRoot, t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer cleanup()
	content, err := os.ReadFile(archive.Path)
	if err != nil {
		t.Fatal(err)
	}

	t.Setenv("FAKE_GIT_LOG", filepath.Join(t.TempDir(), "git.log"))
	t.Setenv("FAKE_EXPECT_SHA", validSource().SHA)
	compiler := &capturingCompiler{expectedFile: filepath.Join("src", "task.ts"), absentFile: filepath.Join(".git", "config")}
	build := &fakeBuilder{artifact: builder.Artifact{ImageTarPath: "/rootfs.ext4"}}
	run := validRun()
	run.TaskSource = api.TaskSourceArtifact{Digest: archive.Digest}

	result := Executor{
		WorkDir:  t.TempDir(),
		GitPath:  fakeGit(t),
		CAS:      &artifactCAS{objects: map[string][]byte{archive.Digest: content}},
		Builder:  build,
		Runner:   &fakeRunner{},
		Compiler: compiler,
	}.Execute(context.Background(), api.WorkerClaim{}, run)

	if result.Kind != "completed" {
		t.Fatalf("result = %+v", result)
	}
	if compiler.request.Source.SHA != archive.Digest {
		t.Fatalf("compiler source = %+v", compiler.request.Source)
	}
	if build.request.CacheScope != archive.Digest+"/deploy" {
		t.Fatalf("cache scope = %q", build.request.CacheScope)
	}
}

func TestExecutorRestoresWithoutCheckoutOrBuild(t *testing.T) {
	runner := &fakeRunner{}
	run := validRun()
	run.Restore = &api.WorkerRestore{
		CheckpointID: "checkpoint-1",
		Waitpoint: api.WorkerRestoreWaitpoint{
			ID:             "waitpoint-1",
			ResolutionKind: "approved",
		},
	}
	result := Executor{
		WorkDir: t.TempDir(),
		GitPath: "/missing/git",
		Runner:  runner,
	}.Execute(context.Background(), api.WorkerClaim{}, run)

	if result.Kind != "completed" {
		t.Fatalf("result = %+v", result)
	}
	if runner.request.Run.Restore == nil || runner.request.Run.Restore.CheckpointID != "checkpoint-1" {
		t.Fatalf("runtime request = %+v", runner.request)
	}
}

func TestExecutorReturnsBuildBoundaryError(t *testing.T) {
	t.Setenv("FAKE_GIT_LOG", filepath.Join(t.TempDir(), "git.log"))
	t.Setenv("FAKE_EXPECT_SHA", validSource().SHA)

	result := Executor{WorkDir: t.TempDir(), GitPath: fakeGit(t)}.Execute(context.Background(), api.WorkerClaim{}, validRun())
	if result.Kind != "failed" || result.Error == nil || !strings.Contains(*result.Error, ErrBuilderRequired.Error()) {
		t.Fatalf("result = %+v", result)
	}
}

func TestExecutorReturnsRuntimeBoundaryError(t *testing.T) {
	t.Setenv("FAKE_GIT_LOG", filepath.Join(t.TempDir(), "git.log"))
	t.Setenv("FAKE_EXPECT_SHA", validSource().SHA)

	result := Executor{
		WorkDir:  t.TempDir(),
		GitPath:  fakeGit(t),
		CAS:      taskSourceCAS(t),
		Builder:  &fakeBuilder{artifact: builder.Artifact{ImageTarPath: "/rootfs.ext4"}},
		Compiler: fakeCompiler{},
	}.Execute(context.Background(), api.WorkerClaim{}, validRun())
	if result.Kind != "failed" || result.Error == nil || !strings.Contains(*result.Error, ErrRunnerRequired.Error()) {
		t.Fatalf("result = %+v", result)
	}
}

func TestExecutorRequiresCompiler(t *testing.T) {
	t.Setenv("FAKE_GIT_LOG", filepath.Join(t.TempDir(), "git.log"))
	t.Setenv("FAKE_EXPECT_SHA", validSource().SHA)

	result := Executor{
		WorkDir: t.TempDir(),
		GitPath: fakeGit(t),
		CAS:     taskSourceCAS(t),
		Builder: &fakeBuilder{artifact: builder.Artifact{ImageTarPath: "/rootfs.ext4"}},
	}.Execute(context.Background(), api.WorkerClaim{}, validRun())
	if result.Kind != "failed" || result.Error == nil || !strings.Contains(*result.Error, ErrCompilerRequired.Error()) {
		t.Fatalf("result = %+v", result)
	}
}

func TestExecutorReturnsWorkspaceCheckoutErrors(t *testing.T) {
	run := validRun()
	run.Workspace.SHA = "bad"

	result := Executor{
		WorkDir:  t.TempDir(),
		GitPath:  fakeGit(t),
		CAS:      taskSourceCAS(t),
		Builder:  &fakeBuilder{},
		Compiler: fakeCompiler{},
	}.Execute(context.Background(), api.WorkerClaim{}, run)
	if result.Kind != "failed" || result.Error == nil {
		t.Fatalf("result = %+v", result)
	}
	if !strings.Contains(*result.Error, "source.sha") {
		t.Fatalf("result = %+v", result)
	}
}

type fakeBuilder struct {
	request  builder.Request
	artifact builder.Artifact
	err      error
}

type fakeCompiler struct{}

type secretCompiler struct{}

type capturingCompiler struct {
	request      CompileRequest
	expectedFile string
	absentFile   string
}

func (fakeCompiler) Compile(_ context.Context, _ CompileRequest) (*bundlev0.Bundle, error) {
	return &bundlev0.Bundle{
		Task: &bundlev0.TaskSpec{ModulePath: "src/task.ts"},
		Image: &bundlev0.ImageSpec{
			Steps: []*bundlev0.ImageStep{{
				Kind: &bundlev0.ImageStep_From{From: &bundlev0.From{Ref: "debian:trixie-slim"}},
			}},
		},
	}, nil
}

func (p *capturingCompiler) Compile(ctx context.Context, request CompileRequest) (*bundlev0.Bundle, error) {
	p.request = request
	if p.expectedFile != "" {
		if _, err := os.Stat(filepath.Join(request.Source.ProjectRoot, p.expectedFile)); err != nil {
			return nil, err
		}
	}
	if p.absentFile != "" {
		if _, err := os.Stat(filepath.Join(request.Source.ProjectRoot, p.absentFile)); err == nil {
			return nil, errors.New("unexpected materialized source file")
		} else if !os.IsNotExist(err) {
			return nil, err
		}
	}
	return fakeCompiler{}.Compile(ctx, request)
}

func (secretCompiler) Compile(_ context.Context, _ CompileRequest) (*bundlev0.Bundle, error) {
	return &bundlev0.Bundle{
		Task: &bundlev0.TaskSpec{ModulePath: "src/task.ts"},
		Image: &bundlev0.ImageSpec{
			Steps: []*bundlev0.ImageStep{{
				Kind: &bundlev0.ImageStep_Run{Run: &bundlev0.Run{
					Argv: []string{"true"},
					SecretMounts: []*bundlev0.SecretMountBinding{{
						Dst:       "/run/secrets/TOKEN",
						SecretRef: &bundlev0.SecretRef{Name: "TOKEN"},
					}},
				}},
			}},
		},
	}, nil
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
	objects map[string][]byte
}

func (f *artifactCAS) Put(context.Context, string, io.Reader) (cas.Object, error) {
	return cas.Object{}, nil
}

func (f *artifactCAS) Stat(context.Context, string) (cas.Object, error) {
	return cas.Object{}, nil
}

func (f *artifactCAS) Get(_ context.Context, digest string) (io.ReadCloser, error) {
	return io.NopCloser(bytes.NewReader(f.objects[digest])), nil
}

func (f *artifactCAS) Delete(context.Context, string) error {
	return nil
}

func taskSourceCAS(t *testing.T) *artifactCAS {
	t.Helper()
	root := t.TempDir()
	if err := os.WriteFile(filepath.Join(root, "task.ts"), []byte("export default {}"), 0o644); err != nil {
		t.Fatal(err)
	}
	archive, cleanup, err := sourcetar.CreateTar(root, t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer cleanup()
	content, err := os.ReadFile(archive.Path)
	if err != nil {
		t.Fatal(err)
	}
	return &artifactCAS{objects: map[string][]byte{validTaskSource().Digest: content}}
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

func readFile(t *testing.T, path string) string {
	t.Helper()
	content, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return string(content)
}
