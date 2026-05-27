package executor

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/helmrdotdev/helmr/internal/api"
	"github.com/helmrdotdev/helmr/internal/archive"
	"github.com/helmrdotdev/helmr/internal/builder"
	"github.com/helmrdotdev/helmr/internal/cas"
	"github.com/helmrdotdev/helmr/internal/checkout"
	bundlev0 "github.com/helmrdotdev/helmr/internal/proto/bundle/v0"
	"github.com/helmrdotdev/helmr/internal/task"
)

var ErrRunnerRequired = errors.New("runtime runner is required")
var ErrBuilderRequired = errors.New("build engine is required")
var ErrDetached = errors.New("runtime detached after checkpoint")

func DefaultWorkDir() string {
	return filepath.Join(os.TempDir(), "helmr-worker")
}

type Executor struct {
	WorkDir    string
	GitPath    string
	CAS        cas.Store
	Builder    builder.Engine
	Runner     Runner
	Waitpoints WaitHandler
}

type Runner interface {
	Run(context.Context, Request) (Result, error)
}

type Request struct {
	Lease            api.WorkerRunLease
	Run              ResolvedRun
	Artifact         builder.Artifact
	DeploymentSource builder.Source
	Workspace        checkout.WorkspaceArtifact
	WaitHandler      WaitHandler
}

type Result struct {
	ExitCode int32
	Output   json.RawMessage
	Detached bool
}

type WaitHandler interface {
	Wait(context.Context, WaitRequest) error
}

type WaitRequest struct {
	Lease          api.WorkerRunLease
	CorrelationID  string
	Kind           api.WorkerWaitpointKind
	Request        json.RawMessage
	DisplayText    string
	TimeoutSeconds *int32
	Policy         string
	ActiveDuration time.Duration
	Checkpointer   Checkpointer
}

type Checkpointer interface {
	CreateCheckpoint(context.Context, CheckpointRequest) (api.WorkerCheckpointManifest, error)
}

type CheckpointRequest struct {
	RunID        string
	WaitpointID  string
	CheckpointID string
}

func (e Executor) Execute(ctx context.Context, claim api.WorkerRunLease, run api.WorkerRun) api.WorkerReleaseResult {
	resolved, err := Resolve(run)
	if err != nil {
		return failedResult(err)
	}
	if resolved.Restore != nil {
		return e.runRuntime(ctx, claim, resolved, builder.Artifact{}, builder.Source{}, checkout.WorkspaceArtifact{})
	}
	buildEngine := e.Builder
	if buildEngine == nil {
		return failedResult(ErrBuilderRequired)
	}
	deploymentSource, cleanupDeploymentSource, err := e.materializeSourceArtifact(ctx, resolved.DeploymentSource, "deployment")
	if err != nil {
		return failedResult(err)
	}
	defer cleanupDeploymentSource()

	bundle, err := e.loadTaskBundle(ctx, resolved.DeploymentTask.BundleDigest)
	if err != nil {
		return failedResult(err)
	}
	resolved.Bundle = bundle
	if err := validateDeploymentTaskMetadata(resolved, bundle); err != nil {
		return failedResult(err)
	}
	buildSecrets, err := builder.BuildSecretValues(resolved.Bundle, resolved.Secrets)
	if err != nil {
		return failedResult(err)
	}
	artifact, err := buildEngine.Build(ctx, builder.Request{
		RunID:        resolved.RunID,
		TaskID:       resolved.TaskID,
		CacheScope:   taskBuildCacheScope(resolved),
		Payload:      resolved.Payload,
		BuildSecrets: buildSecrets,
		Bundle:       resolved.Bundle,
		Source:       deploymentSource,
		MaxDuration:  resolved.MaxDuration,
	})
	if err != nil {
		return failedResult(fmt.Errorf("build run: %w", err))
	}
	workspaceWorktree, cleanupWorkspace, err := e.materializeSource(ctx, resolved.Workspace, run.WorkspaceCheckoutToken, "workspace")
	if err != nil {
		return failedResult(err)
	}
	defer cleanupWorkspace()
	workspaceArtifact, cleanupWorkspaceArtifact, err := checkout.CreateWorkspaceArtifact(workspaceWorktree, e.tempDir())
	if err != nil {
		return failedResult(err)
	}
	defer cleanupWorkspaceArtifact()
	return e.runRuntime(ctx, claim, resolved, artifact, deploymentSource, workspaceArtifact)
}

func taskBuildCacheScope(resolved ResolvedRun) string {
	return buildCacheScope(resolved.DeploymentSource.Digest, resolved.TaskID)
}

func validateDeploymentTaskMetadata(resolved ResolvedRun, bundle *bundlev0.Bundle) error {
	if bundle == nil || bundle.Task == nil {
		return errors.New("compiled task bundle is missing task metadata")
	}
	if want := strings.TrimSpace(resolved.DeploymentTask.FilePath); want != "" && strings.TrimSpace(bundle.Task.ModulePath) != want {
		return fmt.Errorf("deployment task %s file_path %q does not match compiled module_path %q", resolved.TaskID, want, bundle.Task.ModulePath)
	}
	return nil
}

func (e Executor) loadTaskBundle(ctx context.Context, digest string) (*bundlev0.Bundle, error) {
	if e.CAS == nil {
		return nil, errors.New("task bundle CAS is required")
	}
	digest = strings.TrimSpace(digest)
	if digest == "" {
		return nil, errors.New("deployment task bundle_digest is required")
	}
	body, err := e.CAS.Get(ctx, digest)
	if err != nil {
		return nil, fmt.Errorf("get task bundle artifact: %w", err)
	}
	content, readErr := io.ReadAll(body)
	closeErr := body.Close()
	if readErr != nil {
		return nil, fmt.Errorf("read task bundle artifact: %w", readErr)
	}
	if closeErr != nil {
		return nil, fmt.Errorf("close task bundle artifact: %w", closeErr)
	}
	return task.DecodeBundle(content)
}

func buildCacheScope(repository string, taskID string) string {
	repository = strings.TrimSpace(repository)
	taskID = strings.TrimSpace(taskID)
	if repository == "" {
		return taskID
	}
	if taskID == "" {
		return repository
	}
	return repository + "/" + taskID
}

func (e Executor) runRuntime(ctx context.Context, claim api.WorkerRunLease, resolved ResolvedRun, artifact builder.Artifact, deploymentSource builder.Source, workspace checkout.WorkspaceArtifact) api.WorkerReleaseResult {
	runner := e.Runner
	if runner == nil {
		return failedResult(ErrRunnerRequired)
	}
	result, err := runner.Run(ctx, Request{
		Lease:            claim,
		Run:              resolved,
		Artifact:         artifact,
		DeploymentSource: deploymentSource,
		Workspace:        workspace,
		WaitHandler:      e.Waitpoints,
	})
	if err != nil {
		return failedResult(fmt.Errorf("run artifact: %w", err))
	}
	if result.Detached {
		return api.WorkerReleaseResult{Kind: "detached"}
	}
	release := api.WorkerReleaseResult{Kind: "completed", ExitCode: &result.ExitCode}
	if result.ExitCode == 0 && len(result.Output) > 0 {
		release.Output = append(json.RawMessage(nil), result.Output...)
	}
	return release
}

func (e Executor) tempDir() string {
	if e.WorkDir != "" {
		return e.WorkDir
	}
	return DefaultWorkDir()
}

func (e Executor) materializeSource(ctx context.Context, source api.GitHubSource, token *api.WorkerCheckoutToken, label string) (checkout.Worktree, func(), error) {
	workDir := e.WorkDir
	if workDir == "" {
		workDir = DefaultWorkDir()
	}
	if err := os.MkdirAll(workDir, 0o755); err != nil {
		return checkout.Worktree{}, func() {}, fmt.Errorf("create worker work dir: %w", err)
	}
	destination, err := os.MkdirTemp(workDir, label+"-")
	if err != nil {
		return checkout.Worktree{}, func() {}, fmt.Errorf("create run checkout dir: %w", err)
	}
	cleanup := func() { _ = os.RemoveAll(destination) }

	options := []checkout.Option{}
	if e.GitPath != "" {
		options = append(options, checkout.WithGitPath(e.GitPath))
	}
	if token != nil && token.Token != "" {
		options = append(options, checkout.WithTokenProvider(func(context.Context, api.GitHubSource) (string, error) {
			return token.Token, nil
		}))
	}
	worktree, err := checkout.Clone(ctx, source, destination, options...)
	if err != nil {
		cleanup()
		return checkout.Worktree{}, func() {}, fmt.Errorf("materialize %s github source: %w", label, err)
	}
	return worktree, cleanup, nil
}

func (e Executor) materializeSourceArtifact(ctx context.Context, artifact api.DeploymentSourceArtifact, label string) (builder.Source, func(), error) {
	if e.CAS == nil {
		return builder.Source{}, func() {}, errors.New("source artifact CAS is required")
	}
	workDir := e.WorkDir
	if workDir == "" {
		workDir = DefaultWorkDir()
	}
	if err := os.MkdirAll(workDir, 0o755); err != nil {
		return builder.Source{}, func() {}, fmt.Errorf("create worker work dir: %w", err)
	}
	destination, err := os.MkdirTemp(workDir, label+"-artifact-")
	if err != nil {
		return builder.Source{}, func() {}, fmt.Errorf("create source artifact dir: %w", err)
	}
	cleanup := func() { _ = os.RemoveAll(destination) }
	body, err := e.CAS.Get(ctx, strings.TrimSpace(artifact.Digest))
	if err != nil {
		cleanup()
		return builder.Source{}, func() {}, fmt.Errorf("get %s source artifact: %w", label, err)
	}
	extractErr := archive.ExtractTar(body, destination)
	closeErr := body.Close()
	if extractErr != nil {
		cleanup()
		return builder.Source{}, func() {}, fmt.Errorf("extract %s source artifact: %w", label, extractErr)
	}
	if closeErr != nil {
		cleanup()
		return builder.Source{}, func() {}, fmt.Errorf("close %s source artifact: %w", label, closeErr)
	}
	return builder.Source{CheckoutRoot: destination, ProjectRoot: destination, SHA: strings.TrimSpace(artifact.Digest)}, cleanup, nil
}

func failedResult(err error) api.WorkerReleaseResult {
	message := err.Error()
	result := api.WorkerReleaseResult{Kind: "failed", Error: &message}
	var maxDuration MaxDurationError
	if errors.As(err, &maxDuration) {
		failureKind := "max_duration"
		limitSeconds := int32(maxDuration.Limit / time.Second)
		result.FailureKind = &failureKind
		result.LimitSeconds = &limitSeconds
	}
	var parseErr task.ParseError
	if errors.As(err, &parseErr) {
		failureKind := parseErr.FailureKind()
		result.FailureKind = &failureKind
	}
	return result
}
