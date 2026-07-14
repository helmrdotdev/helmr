package executor

import (
	"context"
	"crypto/sha256"
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
	"github.com/helmrdotdev/helmr/internal/proto/bundle/v0"
	"github.com/helmrdotdev/helmr/internal/sha256sum"
	"github.com/helmrdotdev/helmr/internal/task"
	"github.com/helmrdotdev/helmr/internal/workspace"
)

var ErrRunnerRequired = errors.New("runtime runner is required")
var ErrBuilderRequired = errors.New("build engine is required")
var ErrDetached = errors.New("runtime detached after checkpoint")

func DefaultWorkDir() string {
	return filepath.Join(os.TempDir(), "helmr-worker")
}

type Executor struct {
	WorkDir  string
	GitPath  string
	CAS      cas.Store
	Builder  builder.Engine
	Runner   Runner
	RunWaits WaitHandler
}

type Runner interface {
	Run(context.Context, Request) (Result, error)
}

type Request struct {
	Leases           api.WorkerRunLeaseProvider
	Run              ResolvedRun
	Artifact         builder.Artifact
	DeploymentSource builder.Source
	Workspace        workspace.WorkspaceArtifact
	WaitHandler      WaitHandler
}

type Result struct {
	ExitCode       int32
	Output         json.RawMessage
	Detached       bool
	ActiveDuration time.Duration
	Workspace      *workspace.WorkspaceArtifact
}

type WaitHandler interface {
	Wait(context.Context, WaitRequest) error
}

type RunWaitAppender interface {
	AddRunWait(context.Context, WaitRequest) (api.WorkerCreateRunWaitResponse, error)
}

type WaitRequest struct {
	Leases             api.WorkerRunLeaseProvider
	Lease              api.WorkerRunLease
	CorrelationID      string
	Kind               api.WorkerRunWaitKind
	Params             json.RawMessage
	Metadata           json.RawMessage
	Tags               []string
	TimeoutSeconds     *int32
	IdleTimeoutSeconds *int32
	ActiveDuration     time.Duration
	Workspace          api.WorkerWorkspace
	Checkpointer       Checkpointer
	Resume             func(context.Context, WaitResumeDecision) error
}

type WaitResumeDecision struct {
	Kind string
	Data json.RawMessage
}

type Checkpointer interface {
	CreateCheckpoint(context.Context, CheckpointRequest) (CheckpointResult, error)
}

type CheckpointRequest struct {
	RunID            string
	RunWaitID        string
	CheckpointID     string
	CaptureWorkspace bool
}

type CheckpointResult struct {
	Manifest         api.WorkerCheckpointManifest
	WorkspaceCapture *workspace.WorkspaceArtifact
}

func (e Executor) Execute(ctx context.Context, leases api.WorkerRunLeaseProvider, run api.WorkerRun) api.WorkerReleaseResult {
	if leases == nil {
		return failedResult(errors.New("worker run lease provider is required"))
	}
	resolved, err := Resolve(run)
	if err != nil {
		return failedResult(err)
	}
	if resolved.Restore != nil {
		return e.runRuntime(ctx, leases, resolved, builder.Artifact{}, builder.Source{}, workspace.WorkspaceArtifact{})
	}
	if strings.TrimSpace(resolved.Workspace.WorkspaceMountID) == "" {
		return failedResult(errors.New("workspace mount id is required for worker run execution"))
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
	resolved.Workspace.MountPath = workspaceMountPath(bundle)
	if err := validateDeploymentTaskMetadata(resolved, bundle); err != nil {
		return failedResult(err)
	}

	workspaceArtifact, cleanupWorkspaceArtifact, err := e.runtimeWorkspaceArtifact(ctx, resolved.Workspace, true)
	if err != nil {
		return failedResult(err)
	}
	defer cleanupWorkspaceArtifact()
	return e.runRuntime(ctx, leases, resolved, builder.Artifact{}, deploymentSource, workspaceArtifact)
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

func (e Executor) runRuntime(ctx context.Context, leases api.WorkerRunLeaseProvider, resolved ResolvedRun, artifact builder.Artifact, deploymentSource builder.Source, ws workspace.WorkspaceArtifact) api.WorkerReleaseResult {
	runner := e.Runner
	if runner == nil {
		return failedResult(ErrRunnerRequired)
	}
	result, err := runner.Run(ctx, Request{
		Leases:           leases,
		Run:              resolved,
		Artifact:         artifact,
		DeploymentSource: deploymentSource,
		Workspace:        ws,
		WaitHandler:      e.RunWaits,
	})
	if err != nil {
		return failedResult(fmt.Errorf("run artifact: %w", err))
	}
	if result.Detached {
		return api.WorkerReleaseResult{Kind: "detached"}
	}
	release := api.WorkerReleaseResult{Kind: "completed", ExitCode: &result.ExitCode, ActiveDurationMs: result.ActiveDuration.Milliseconds()}
	if result.ExitCode == 0 && len(result.Output) > 0 {
		release.Output = append(json.RawMessage(nil), result.Output...)
	}
	if result.ExitCode == 0 {
		workspaceCommit, err := workerWorkspaceCommit(resolved.Workspace, result.Workspace)
		if err != nil {
			return failedResult(err)
		}
		release.Workspace = workspaceCommit
	}
	return release
}

func workerWorkspaceCommit(base api.WorkerWorkspace, artifact *workspace.WorkspaceArtifact) (*api.WorkerWorkspace, error) {
	if strings.TrimSpace(base.ID) == "" {
		return nil, nil
	}
	if artifact == nil {
		return nil, errors.New("successful session run did not publish a workspace artifact")
	}
	if strings.TrimSpace(base.WriteLeaseID) == "" {
		return nil, errors.New("workspace write lease is required")
	}
	if strings.TrimSpace(base.WriteFencingToken) == "" {
		return nil, errors.New("workspace write fencing token is required")
	}
	mountPath := strings.TrimSpace(base.MountPath)
	if mountPath == "" {
		mountPath = "/workspace"
	}
	return &api.WorkerWorkspace{
		ID:                strings.TrimSpace(base.ID),
		WorkspaceMountID:  strings.TrimSpace(base.WorkspaceMountID),
		FencingGeneration: base.FencingGeneration,
		WriteLeaseID:      strings.TrimSpace(base.WriteLeaseID),
		WriteFencingToken: strings.TrimSpace(base.WriteFencingToken),
		BaseVersionID:     strings.TrimSpace(base.BaseVersionID),
		MountPath:         mountPath,
		Artifact: &api.WorkerWorkspaceArtifact{
			Digest:     artifact.Digest,
			MediaType:  artifact.MediaType,
			Encoding:   artifact.Encoding,
			SizeBytes:  artifact.SizeBytes,
			EntryCount: int32(artifact.EntryCount),
		},
	}, nil
}

func (e Executor) materializeWorkspaceArtifact(ctx context.Context, base api.WorkerWorkspace) (workspace.WorkspaceArtifact, func(), error) {
	if base.Artifact == nil || strings.TrimSpace(base.Artifact.Digest) == "" {
		artifact, cleanup, err := workspace.CreateEmptyWorkspaceArtifact(e.tempDir())
		if err != nil {
			return workspace.WorkspaceArtifact{}, func() {}, err
		}
		if err := e.publishWorkspaceArtifact(ctx, artifact); err != nil {
			cleanup()
			return workspace.WorkspaceArtifact{}, func() {}, err
		}
		return artifact, cleanup, nil
	}
	if e.CAS == nil {
		return workspace.WorkspaceArtifact{}, func() {}, errors.New("workspace artifact CAS is required")
	}
	if err := os.MkdirAll(e.tempDir(), 0o755); err != nil {
		return workspace.WorkspaceArtifact{}, func() {}, fmt.Errorf("create worker work dir: %w", err)
	}
	file, err := os.CreateTemp(e.tempDir(), "workspace-base-*.tar")
	if err != nil {
		return workspace.WorkspaceArtifact{}, func() {}, fmt.Errorf("create workspace artifact file: %w", err)
	}
	path := file.Name()
	cleanup := func() { _ = os.Remove(path) }
	body, err := e.CAS.Get(ctx, strings.TrimSpace(base.Artifact.Digest))
	if err != nil {
		_ = file.Close()
		cleanup()
		return workspace.WorkspaceArtifact{}, func() {}, fmt.Errorf("get workspace artifact: %w", err)
	}
	hash := sha256.New()
	size, copyErr := io.Copy(io.MultiWriter(file, hash), body)
	bodyCloseErr := body.Close()
	closeErr := file.Close()
	if copyErr != nil {
		cleanup()
		return workspace.WorkspaceArtifact{}, func() {}, fmt.Errorf("copy workspace artifact: %w", copyErr)
	}
	if bodyCloseErr != nil {
		cleanup()
		return workspace.WorkspaceArtifact{}, func() {}, fmt.Errorf("close workspace artifact body: %w", bodyCloseErr)
	}
	if closeErr != nil {
		cleanup()
		return workspace.WorkspaceArtifact{}, func() {}, fmt.Errorf("close workspace artifact file: %w", closeErr)
	}
	if digest := sha256sum.DigestHash(hash); digest != strings.TrimSpace(base.Artifact.Digest) {
		cleanup()
		return workspace.WorkspaceArtifact{}, func() {}, fmt.Errorf("workspace artifact digest mismatch: got %s, want %s", digest, base.Artifact.Digest)
	}
	if size != base.Artifact.SizeBytes {
		cleanup()
		return workspace.WorkspaceArtifact{}, func() {}, fmt.Errorf("workspace artifact size mismatch: got %d, want %d", size, base.Artifact.SizeBytes)
	}
	return workspace.WorkspaceArtifact{
		Path:       path,
		Digest:     strings.TrimSpace(base.Artifact.Digest),
		MediaType:  strings.TrimSpace(base.Artifact.MediaType),
		Encoding:   strings.TrimSpace(base.Artifact.Encoding),
		SizeBytes:  base.Artifact.SizeBytes,
		EntryCount: int(base.Artifact.EntryCount),
	}, cleanup, nil
}

func (e Executor) runtimeWorkspaceArtifact(ctx context.Context, base api.WorkerWorkspace, materializedRun bool) (workspace.WorkspaceArtifact, func(), error) {
	if materializedRun {
		artifact, err := workspaceArtifactMetadata(base)
		return artifact, func() {}, err
	}
	return e.materializeWorkspaceArtifact(ctx, base)
}

func workspaceArtifactMetadata(base api.WorkerWorkspace) (workspace.WorkspaceArtifact, error) {
	if base.Artifact == nil {
		return workspace.WorkspaceArtifact{}, errors.New("materialized workspace run requires workspace artifact metadata")
	}
	artifact := base.Artifact
	digest := strings.TrimSpace(artifact.Digest)
	if digest == "" {
		return workspace.WorkspaceArtifact{}, errors.New("materialized workspace run requires workspace artifact digest")
	}
	mediaType := strings.TrimSpace(artifact.MediaType)
	if mediaType != workspace.ArtifactMediaType {
		return workspace.WorkspaceArtifact{}, fmt.Errorf("unsupported workspace artifact media_type %q", artifact.MediaType)
	}
	encoding := strings.TrimSpace(artifact.Encoding)
	if encoding != workspace.ArtifactEncoding {
		return workspace.WorkspaceArtifact{}, fmt.Errorf("unsupported workspace artifact encoding %q", artifact.Encoding)
	}
	if artifact.SizeBytes <= 0 {
		return workspace.WorkspaceArtifact{}, errors.New("materialized workspace run requires workspace artifact size_bytes")
	}
	if artifact.SizeBytes > workspace.MaxArtifactArchiveBytes {
		return workspace.WorkspaceArtifact{}, fmt.Errorf("workspace artifact size_bytes %d exceeds max %d", artifact.SizeBytes, workspace.MaxArtifactArchiveBytes)
	}
	if artifact.EntryCount > workspace.MaxArtifactEntries {
		return workspace.WorkspaceArtifact{}, fmt.Errorf("workspace artifact entry_count %d exceeds max %d", artifact.EntryCount, workspace.MaxArtifactEntries)
	}
	return workspace.WorkspaceArtifact{
		Digest:     digest,
		MediaType:  mediaType,
		Encoding:   encoding,
		SizeBytes:  artifact.SizeBytes,
		EntryCount: int(artifact.EntryCount),
	}, nil
}

func (e Executor) publishWorkspaceArtifact(ctx context.Context, artifact workspace.WorkspaceArtifact) error {
	if e.CAS == nil {
		return errors.New("workspace artifact CAS is required")
	}
	body, err := os.Open(artifact.Path)
	if err != nil {
		return fmt.Errorf("open workspace artifact: %w", err)
	}
	object, putErr := e.CAS.Put(ctx, artifact.MediaType, body)
	closeErr := body.Close()
	if putErr != nil {
		return fmt.Errorf("put workspace artifact: %w", putErr)
	}
	if closeErr != nil {
		return fmt.Errorf("close workspace artifact: %w", closeErr)
	}
	if object.Digest != artifact.Digest {
		return fmt.Errorf("workspace artifact digest mismatch: got %s, want %s", object.Digest, artifact.Digest)
	}
	if object.SizeBytes != artifact.SizeBytes {
		return fmt.Errorf("workspace artifact size mismatch: got %d, want %d", object.SizeBytes, artifact.SizeBytes)
	}
	if strings.TrimSpace(object.MediaType) != strings.TrimSpace(artifact.MediaType) {
		return fmt.Errorf("workspace artifact media_type mismatch: got %q, want %q", object.MediaType, artifact.MediaType)
	}
	return nil
}

func (e Executor) tempDir() string {
	if e.WorkDir != "" {
		return e.WorkDir
	}
	return DefaultWorkDir()
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
