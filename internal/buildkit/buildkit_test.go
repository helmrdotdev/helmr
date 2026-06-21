package buildkit

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/helmrdotdev/helmr/internal/builder"
	"github.com/helmrdotdev/helmr/internal/proto/bundle/v0"
	bkclient "github.com/moby/buildkit/client"
	"github.com/moby/buildkit/client/llb"
)

func TestBuildKitE2E(t *testing.T) {
	if os.Getenv("HELMR_BUILDKIT_E2E") != "1" {
		t.Skip("set HELMR_BUILDKIT_E2E=1 to run against buildkitd")
	}
	ctx := context.Background()
	buildkit, closeBuildKit, err := Open(ctx, Config{
		Addr:           os.Getenv("HELMR_WORKER_BUILDKIT_ADDR"),
		OutputRoot:     t.TempDir(),
		CacheNamespace: "e2e",
	})
	if err != nil {
		t.Fatal(err)
	}
	defer func() {
		if err := closeBuildKit(); err != nil {
			t.Fatal(err)
		}
	}()

	sourceRoot := t.TempDir()
	artifact, err := buildkit.Build(ctx, builder.Request{
		RunID:      "buildkit-e2e",
		TaskID:     "build",
		CacheScope: "helmrdotdev/helmr/buildkit-e2e",
		Bundle: &bundlev0.Bundle{Image: image(
			from("busybox:1.36.1"),
			run("sh", "-c", "printf 'hello from helmr buildkit\\n' > /hello.txt"),
		)},
		Source: builder.Source{ProjectRoot: sourceRoot, SHA: strings.Repeat("b", 40)},
	})
	if err != nil {
		t.Fatal(err)
	}
	if artifact.ImageTarPath == "" || artifact.ConfigPath == "" || artifact.ManifestPath == "" {
		t.Fatalf("artifact = %+v", artifact)
	}
	if artifact.RootPath == "" {
		t.Fatalf("artifact root path is empty: %+v", artifact)
	}
	if info, err := os.Stat(artifact.ImageTarPath); err != nil {
		t.Fatal(err)
	} else if info.Size() == 0 {
		t.Fatalf("image tar is empty: %s", artifact.ImageTarPath)
	}
}

func TestBuilderExportsOCIImageTar(t *testing.T) {
	sourceRoot := t.TempDir()
	if err := os.WriteFile(filepath.Join(sourceRoot, "package.json"), []byte("{}"), 0o644); err != nil {
		t.Fatal(err)
	}
	solver := &fakeBuildKitSolver{}
	artifact, err := New(solver, t.TempDir()).Build(context.Background(), builder.Request{
		RunID:  "run/1",
		TaskID: "task:deploy",
		Bundle: &bundlev0.Bundle{Image: image(
			from("debian:trixie-slim"),
			copySourceFile("/app/package.json", "package.json"),
		)},
		Source: builder.Source{ProjectRoot: sourceRoot, SHA: strings.Repeat("a", 40)},
	})
	if err != nil {
		t.Fatal(err)
	}
	if content := readFile(t, artifact.ImageTarPath); content != "oci-image" {
		t.Fatalf("image tar = %q", content)
	}
	if artifact.ConfigPath == "" || artifact.ManifestPath == "" {
		t.Fatalf("artifact = %+v", artifact)
	}
	if len(solver.opt.Exports) != 1 || solver.opt.Exports[0].Type != bkclient.ExporterOCI {
		t.Fatalf("exports = %+v", solver.opt.Exports)
	}
	configJSON := solver.opt.Exports[0].Attrs["containerimage.config"]
	var config imageConfig
	if err := json.Unmarshal([]byte(configJSON), &config); err != nil {
		t.Fatalf("containerimage.config = %q: %v", configJSON, err)
	}
	if config.Config.User != "" || config.Config.WorkingDir != defaultRuntimeWorkdir {
		t.Fatalf("config = %+v", config.Config)
	}
	if strings.Contains(configJSON, `"User"`) {
		t.Fatalf("default config should omit User: %s", configJSON)
	}
	if _, ok := solver.opt.LocalMounts["source_file_1"]; !ok {
		t.Fatalf("local mounts = %+v", solver.opt.LocalMounts)
	}
	var sidecar imageConfig
	if err := json.Unmarshal([]byte(readFile(t, artifact.ConfigPath)), &sidecar); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(sidecar, config) {
		t.Fatalf("sidecar config = %+v, exporter config = %+v", sidecar, config)
	}
}

func TestConfigDefaultsToWorkerLocalSocket(t *testing.T) {
	if got := (Config{}).addr(); got != defaultBuildKitAddr {
		t.Fatalf("addr = %q", got)
	}
}

func TestConfigRejectsDockerEndpoint(t *testing.T) {
	_, err := (Config{Addr: "unix:///var/run/docker.sock"}).endpoint()
	if err == nil || !strings.Contains(err.Error(), "not a Docker endpoint") {
		t.Fatalf("err = %v", err)
	}
}

func TestPlanImageRejectsHardExcludedSource(t *testing.T) {
	sourceRoot := t.TempDir()
	if err := os.Mkdir(filepath.Join(sourceRoot, ".git"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(sourceRoot, ".git", "config"), []byte("[core]\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	_, err := planImage(image(
		from("debian:trixie-slim"),
		copySourceFile("/app/config", ".git/config"),
	), nil, sourceRoot, defaultPlatform, defaultCacheNS)
	if err == nil || !strings.Contains(err.Error(), "hard-excluded") {
		t.Fatalf("err = %v", err)
	}
}

func TestPlanImageDefaultsRuntimeConfig(t *testing.T) {
	plan, err := planImage(image(from("debian:trixie-slim")), nil, t.TempDir(), defaultPlatform, defaultCacheNS)
	if err != nil {
		t.Fatal(err)
	}
	if plan.Config.Config.User != "" {
		t.Fatalf("user = %q", plan.Config.Config.User)
	}
	if plan.Config.Config.WorkingDir != defaultRuntimeWorkdir {
		t.Fatalf("workdir = %q", plan.Config.Config.WorkingDir)
	}
}

func TestPlanImagePreservesExplicitRuntimeConfig(t *testing.T) {
	plan, err := planImage(image(
		from("debian:trixie-slim"),
		workdir("/app"),
		workdir("service"),
		user("agent"),
	), nil, t.TempDir(), defaultPlatform, defaultCacheNS)
	if err != nil {
		t.Fatal(err)
	}
	if plan.Config.Config.User != "agent" {
		t.Fatalf("user = %q", plan.Config.Config.User)
	}
	if plan.Config.Config.WorkingDir != "/app/service" {
		t.Fatalf("workdir = %q", plan.Config.Config.WorkingDir)
	}
}

func TestPlanImageRejectsEscapingSource(t *testing.T) {
	_, err := planImage(image(
		from("debian:trixie-slim"),
		copySourceFile("/app/passwd", "../passwd"),
	), nil, t.TempDir(), defaultPlatform, defaultCacheNS)
	if err == nil || !strings.Contains(err.Error(), "escapes root") {
		t.Fatalf("err = %v", err)
	}
}

func TestPlanImageRejectsParentComponentSource(t *testing.T) {
	sourceRoot := t.TempDir()
	if err := os.MkdirAll(filepath.Join(sourceRoot, "b"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(sourceRoot, "b", "package.json"), []byte("{}"), 0o644); err != nil {
		t.Fatal(err)
	}
	_, err := planImage(image(
		from("debian:trixie-slim"),
		copySourceFile("/app/package.json", "a/../b/package.json"),
	), nil, sourceRoot, defaultPlatform, defaultCacheNS)
	if err == nil || !strings.Contains(err.Error(), "escapes root") {
		t.Fatalf("err = %v", err)
	}
}

func TestCacheIDUsesNamespace(t *testing.T) {
	planner := imagePlanner{cacheNS: "org_1"}
	if got := planner.cacheID("npm/cache"); got != "org_1/npm_cache" {
		t.Fatalf("cache ID = %q", got)
	}
}

func TestRequestCacheNamespaceUsesScope(t *testing.T) {
	b := New(&fakeBuildKitSolver{}, t.TempDir(), "tenant")
	got := b.requestCacheNamespace(builder.Request{TaskID: "deploy", CacheScope: "helmrdotdev/helmr/deploy"})
	if got != "tenant/helmrdotdev/helmr/deploy" {
		t.Fatalf("cache namespace = %q", got)
	}
}

func TestRunRejectsSecretAndCacheMountTogether(t *testing.T) {
	_, err := planImage(image(
		from("debian:trixie-slim"),
		&bundlev0.ImageStep{Kind: &bundlev0.ImageStep_Run{Run: &bundlev0.Run{
			Argv: []string{"true"},
			CacheMounts: []*bundlev0.CacheMountBinding{{
				Dst:     "/root/.cache",
				CacheId: "deps",
			}},
			SecretMounts: []*bundlev0.SecretMountBinding{{
				Dst:       "/run/secrets/token",
				SecretRef: &bundlev0.SecretRef{Name: "token"},
			}},
		}}},
	), nil, t.TempDir(), defaultPlatform, defaultCacheNS)
	if err == nil || !strings.Contains(err.Error(), "cannot combine secret mounts") {
		t.Fatalf("err = %v", err)
	}
}

func TestBuilderRejectsMissingBuildSecret(t *testing.T) {
	sourceRoot := t.TempDir()
	solver := &fakeBuildKitSolver{}
	_, err := New(solver, t.TempDir()).Build(context.Background(), builder.Request{
		RunID:  "run-1",
		TaskID: "deploy",
		Bundle: &bundlev0.Bundle{Image: image(
			from("debian:trixie-slim"),
			runWithSecret("token"),
		)},
		Source: builder.Source{ProjectRoot: sourceRoot},
	})
	if err == nil || !strings.Contains(err.Error(), `build secret "token"`) {
		t.Fatalf("err = %v", err)
	}
	if solver.opt.Exports != nil {
		t.Fatalf("solver was called: %+v", solver.opt)
	}
}

func TestBuilderRedactsBuildSecretFromSolveError(t *testing.T) {
	sourceRoot := t.TempDir()
	leaked := "long-secret-value"
	short := "1234567"
	solver := &fakeBuildKitSolver{err: errors.New("build failed with " + leaked + " but not " + short)}
	_, err := New(solver, t.TempDir()).Build(context.Background(), builder.Request{
		RunID:  "run-1",
		TaskID: "deploy",
		Bundle: &bundlev0.Bundle{Image: image(
			from("debian:trixie-slim"),
			runWithSecret("token"),
		)},
		Source: builder.Source{ProjectRoot: sourceRoot},
		BuildSecrets: map[string][]byte{
			"token": []byte(leaked),
			"short": []byte(short),
		},
	})
	if err == nil {
		t.Fatal("expected solve error")
	}
	if strings.Contains(err.Error(), leaked) {
		t.Fatalf("error leaked build secret: %v", err)
	}
	if !strings.Contains(err.Error(), "***") {
		t.Fatalf("error was not redacted: %v", err)
	}
	if strings.Contains(err.Error(), short) {
		t.Fatalf("error leaked short build secret: %v", err)
	}
}

type fakeBuildKitSolver struct {
	opt bkclient.SolveOpt
	def *llb.Definition
	err error
}

func (s *fakeBuildKitSolver) Solve(_ context.Context, def *llb.Definition, opt bkclient.SolveOpt, _ chan *bkclient.SolveStatus) (*bkclient.SolveResponse, error) {
	s.def = def
	s.opt = opt
	if s.err != nil {
		return nil, s.err
	}
	if len(opt.Exports) != 1 || opt.Exports[0].Output == nil {
		return nil, nil
	}
	out, err := opt.Exports[0].Output(nil)
	if err != nil {
		return nil, err
	}
	if _, err := io.WriteString(out, "oci-image"); err != nil {
		return nil, err
	}
	return &bkclient.SolveResponse{}, out.Close()
}

func image(steps ...*bundlev0.ImageStep) *bundlev0.ImageSpec {
	return &bundlev0.ImageSpec{
		Platform: &bundlev0.Platform{Os: "linux", Architecture: "amd64"},
		Steps:    steps,
	}
}

func from(ref string) *bundlev0.ImageStep {
	return &bundlev0.ImageStep{Kind: &bundlev0.ImageStep_From{From: &bundlev0.From{Ref: ref}}}
}

func copySourceFile(dst, path string) *bundlev0.ImageStep {
	return &bundlev0.ImageStep{Kind: &bundlev0.ImageStep_CopySourceFile{CopySourceFile: &bundlev0.CopySourceFile{
		Dst:    dst,
		SrcRef: &bundlev0.SourceFileRef{Path: path},
	}}}
}

func run(argv ...string) *bundlev0.ImageStep {
	return &bundlev0.ImageStep{Kind: &bundlev0.ImageStep_Run{Run: &bundlev0.Run{
		Argv: argv,
	}}}
}

func workdir(path string) *bundlev0.ImageStep {
	return &bundlev0.ImageStep{Kind: &bundlev0.ImageStep_Workdir{Workdir: &bundlev0.Workdir{Path: path}}}
}

func user(name string) *bundlev0.ImageStep {
	return &bundlev0.ImageStep{Kind: &bundlev0.ImageStep_User{User: &bundlev0.User{Name: name}}}
}

func runWithSecret(name string) *bundlev0.ImageStep {
	return &bundlev0.ImageStep{Kind: &bundlev0.ImageStep_Run{Run: &bundlev0.Run{
		Argv: []string{"true"},
		SecretMounts: []*bundlev0.SecretMountBinding{{
			Dst:       "/run/secrets/" + name,
			SecretRef: &bundlev0.SecretRef{Name: name},
		}},
	}}}
}

func readFile(t *testing.T, path string) string {
	t.Helper()
	content, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return string(content)
}
