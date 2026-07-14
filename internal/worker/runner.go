package worker

import (
	"context"
	"errors"
	"log/slog"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/helmrdotdev/helmr/internal/api"
	"github.com/helmrdotdev/helmr/internal/client"
)

const defaultDeploymentBuildCompletionGrace = 30 * time.Second

type ControlClient interface {
	LeaseDeploymentBuild(ctx context.Context) (api.WorkerDeploymentBuildLeaseResponse, error)
	StartDeploymentBuild(ctx context.Context, lease api.WorkerDeploymentBuildLease) (api.WorkerDeploymentBuildStartResponse, error)
	RenewDeploymentBuild(ctx context.Context, lease api.WorkerDeploymentBuildLease) (api.WorkerDeploymentBuildRenewResponse, error)
	RejectDeploymentBuild(ctx context.Context, request api.WorkerDeploymentBuildRejectRequest) error
	CompleteDeploymentBuild(ctx context.Context, lease api.WorkerDeploymentBuildLease, result api.WorkerDeploymentBuildResult) (api.WorkerDeploymentBuildResponse, error)
	ClaimWorkspaceMount(ctx context.Context, capabilities api.WorkerCapabilities) (api.WorkerWorkspaceMountClaimResponse, error)
	RenewWorkspaceMount(ctx context.Context, request api.WorkerWorkspaceMountRenewRequest) (api.WorkspaceMountResponse, error)
	MarkWorkspaceMountMounted(ctx context.Context, request api.WorkerWorkspaceMountMountedRequest) (api.WorkspaceMountResponse, error)
	CaptureWorkspaceMount(ctx context.Context, request api.WorkerWorkspaceMountCaptureRequest) (api.WorkerWorkspaceMountCaptureResponse, error)
	StopWorkspaceMount(ctx context.Context, request api.WorkerWorkspaceMountStopRequest) (api.WorkspaceMountResponse, error)
	FailWorkspaceMount(ctx context.Context, request api.WorkerWorkspaceMountFailRequest) (api.WorkspaceMountResponse, error)
	ClaimWorkspaceOperation(ctx context.Context, request api.WorkerWorkspaceOperationClaimRequest) (api.WorkerWorkspaceOperationClaimResponse, error)
	StartWorkspaceOperation(ctx context.Context, request api.WorkerWorkspaceOperationStartRequest) (api.WorkspaceOperationResponse, error)
	CompleteWorkspaceOperation(ctx context.Context, request api.WorkerWorkspaceOperationCompleteRequest) (api.WorkspaceOperationResponse, error)
	MarkWorkspaceExecStarted(ctx context.Context, request api.WorkerWorkspaceExecStartedRequest) (api.WorkspaceExecEnvelope, error)
	AppendWorkspaceExecOutput(ctx context.Context, request api.WorkerWorkspaceExecOutputRequest) (api.ListWorkspaceExecStreamChunksResponse, error)
	ListWorkspaceExecInput(ctx context.Context, request api.WorkerWorkspaceExecInputRequest) (api.WorkerWorkspaceExecInputResponse, error)
	AdvanceWorkspaceExecInputDelivered(ctx context.Context, request api.WorkerWorkspaceExecInputDeliveredRequest) (api.WorkspaceExecEnvelope, error)
	MarkWorkspaceExecExited(ctx context.Context, request api.WorkerWorkspaceExecExitedRequest) (api.WorkspaceExecEnvelope, error)
	MarkWorkspacePtyOpened(ctx context.Context, request api.WorkerWorkspacePtyOpenedRequest) (api.WorkspacePtyEnvelope, error)
	AppendWorkspacePtyOutput(ctx context.Context, request api.WorkerWorkspacePtyOutputRequest) (api.ListWorkspacePtyStreamChunksResponse, error)
	ListWorkspacePtyInput(ctx context.Context, request api.WorkerWorkspacePtyInputRequest) (api.WorkerWorkspacePtyInputResponse, error)
	AdvanceWorkspacePtyInputDelivered(ctx context.Context, request api.WorkerWorkspacePtyInputDeliveredRequest) (api.WorkspacePtyEnvelope, error)
	MarkWorkspacePtyResizeApplied(ctx context.Context, request api.WorkerWorkspacePtyResizeAppliedRequest) (api.WorkspacePtyEnvelope, error)
	MarkWorkspacePtyClosed(ctx context.Context, request api.WorkerWorkspacePtyClosedRequest) (api.WorkspacePtyEnvelope, error)
	LeaseRun(ctx context.Context) (api.WorkerRunLeaseResponse, error)
	RejectRun(ctx context.Context, request api.WorkerRejectRunRequest) error
	StartRun(ctx context.Context, lease api.WorkerRunLease) (api.WorkerStartResponse, error)
	RenewRun(ctx context.Context, lease api.WorkerRunLease) (api.WorkerRenewResponse, error)
	ReleaseRun(ctx context.Context, lease api.WorkerRunLease, result api.WorkerReleaseResult) (api.WorkerReleaseResponse, error)
}

type Executor interface {
	Execute(ctx context.Context, leases api.WorkerRunLeaseProvider, run api.WorkerRun) api.WorkerReleaseResult
}

type DeploymentBuilder interface {
	BuildDeployment(ctx context.Context, lease api.WorkerDeploymentBuildLease, deployment api.WorkerDeploymentBuild) api.WorkerDeploymentBuildResult
}

type Materializer interface {
	RunWorkspaceMount(ctx context.Context, mount api.WorkerWorkspaceMount, client api.WorkerWorkspaceMaterializerControlClient) error
}

type runLeaseState struct {
	mu    sync.RWMutex
	lease api.WorkerRunLease
}

func newRunLeaseState(lease api.WorkerRunLease) *runLeaseState {
	return &runLeaseState{lease: lease}
}

func (s *runLeaseState) CurrentWorkerRunLease() api.WorkerRunLease {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.lease
}

func (s *runLeaseState) set(lease api.WorkerRunLease) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.lease = lease
}

type Runner struct {
	client                         ControlClient
	executor                       Executor
	deploymentBuilder              DeploymentBuilder
	materializer                   Materializer
	capabilities                   api.WorkerCapabilities
	pollEvery                      time.Duration
	renewEvery                     time.Duration
	renewWait                      time.Duration
	releaseWait                    time.Duration
	deploymentBuildCompletionGrace time.Duration
	log                            *slog.Logger
}

type Option func(*Runner)

func WithPollEvery(duration time.Duration) Option {
	return func(runner *Runner) {
		runner.pollEvery = duration
	}
}

func WithRenewEvery(duration time.Duration) Option {
	return func(runner *Runner) {
		runner.renewEvery = duration
	}
}

func WithLogger(log *slog.Logger) Option {
	return func(runner *Runner) {
		runner.log = log
	}
}

func WithDeploymentBuilder(builder DeploymentBuilder) Option {
	return func(runner *Runner) {
		runner.deploymentBuilder = builder
	}
}

func WithMaterializer(materializer Materializer) Option {
	return func(runner *Runner) {
		runner.materializer = materializer
	}
}

func NewRunner(client ControlClient, executor Executor, capabilities api.WorkerCapabilities, opts ...Option) (*Runner, error) {
	if client == nil {
		return nil, errors.New("worker client is required")
	}
	if executor == nil {
		return nil, errors.New("worker executor is required")
	}
	runner := &Runner{
		client:                         client,
		executor:                       executor,
		capabilities:                   capabilities,
		pollEvery:                      2 * time.Second,
		renewEvery:                     10 * time.Second,
		renewWait:                      5 * time.Second,
		releaseWait:                    30 * time.Second,
		deploymentBuildCompletionGrace: defaultDeploymentBuildCompletionGrace,
		log:                            slog.Default(),
	}
	for _, opt := range opts {
		opt(runner)
	}
	if runner.pollEvery <= 0 {
		return nil, errors.New("worker poll interval must be positive")
	}
	if runner.renewEvery <= 0 {
		return nil, errors.New("worker renew interval must be positive")
	}
	if runner.renewWait <= 0 {
		return nil, errors.New("worker renew timeout must be positive")
	}
	if runner.renewWait >= runner.renewEvery {
		return nil, errors.New("worker renew timeout must be less than renew interval")
	}
	if runner.releaseWait <= 0 {
		return nil, errors.New("worker release timeout must be positive")
	}
	if runner.deploymentBuildCompletionGrace <= 0 {
		return nil, errors.New("worker deployment build completion grace must be positive")
	}
	if runner.log == nil {
		runner.log = slog.Default()
	}
	return runner, nil
}

func (r *Runner) release(lease api.WorkerRunLease, result api.WorkerReleaseResult) error {
	releaseCtx, cancelRelease := context.WithTimeout(context.Background(), r.releaseWait)
	defer cancelRelease()
	retryEvery := r.renewEvery / 2
	if retryEvery <= 0 || retryEvery > time.Second {
		retryEvery = time.Second
	}
	var lastErr error
	for {
		if _, err := r.client.ReleaseRun(releaseCtx, lease, result); err == nil {
			return nil
		} else {
			lastErr = err
			if isStaleLease(err) {
				return err
			}
		}
		timer := time.NewTimer(retryEvery)
		select {
		case <-releaseCtx.Done():
			timer.Stop()
			if lastErr != nil {
				return lastErr
			}
			return releaseCtx.Err()
		case <-timer.C:
		}
	}
}

type renewErrorKind int

const (
	renewFailed renewErrorKind = iota
	renewStale
	renewTimeout
)

type renewError struct {
	kind renewErrorKind
	err  error
}

func (e *renewError) Error() string {
	return e.err.Error()
}

func (e *renewError) Unwrap() error {
	return e.err
}

func (r *Runner) renewUntilDone(ctx context.Context, leaseState *runLeaseState) *renewError {
	ticker := time.NewTicker(r.renewEvery)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return nil
		case <-ticker.C:
			renewCtx, cancelRenew := context.WithTimeout(ctx, r.renewWait)
			renewed, err := r.client.RenewRun(renewCtx, leaseState.CurrentWorkerRunLease())
			timedOut := errors.Is(renewCtx.Err(), context.DeadlineExceeded)
			cancelRenew()
			if err != nil {
				if ctx.Err() != nil {
					return nil
				}
				if timedOut || errors.Is(err, context.DeadlineExceeded) {
					return &renewError{kind: renewTimeout, err: err}
				}
				if isStaleLease(err) {
					return &renewError{kind: renewStale, err: err}
				}
				return &renewError{kind: renewFailed, err: err}
			}
			if strings.TrimSpace(renewed.Lease.ID) == "" {
				return &renewError{kind: renewFailed, err: errors.New("renew run response did not include a lease")}
			}
			leaseState.set(renewed.Lease)
		}
	}
}

func isStaleLease(err error) bool {
	return client.IsStatus(err, http.StatusConflict)
}
