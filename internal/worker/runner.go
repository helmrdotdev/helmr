package worker

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"time"

	"github.com/helmrdotdev/helmr/internal/api"
	"github.com/helmrdotdev/helmr/internal/capacity"
	"github.com/helmrdotdev/helmr/internal/client"
	"github.com/helmrdotdev/helmr/internal/deployment"
)

const defaultDeploymentBuildCompletionGrace = 30 * time.Second

type ControlClient interface {
	DiscoverRunLeases(ctx context.Context) (api.WorkerRunLeaseDiscoveryResponse, error)
	LeaseDeploymentBuild(ctx context.Context) (api.WorkerDeploymentBuildLeaseResponse, error)
	StartDeploymentBuild(ctx context.Context, lease api.WorkerDeploymentBuildLease) (api.WorkerDeploymentBuildStartResponse, error)
	RenewDeploymentBuild(ctx context.Context, lease api.WorkerDeploymentBuildLease) (api.WorkerDeploymentBuildRenewResponse, error)
	RejectDeploymentBuild(ctx context.Context, request api.WorkerDeploymentBuildRejectRequest) error
	ReportDeploymentBuildDeliveryFailure(ctx context.Context, request api.WorkerDeploymentBuildDeliveryFailureRequest) (api.WorkerDeploymentBuildResponse, error)
	CompleteDeploymentBuild(ctx context.Context, lease api.WorkerDeploymentBuildLease, result json.RawMessage) (api.WorkerDeploymentBuildResponse, error)
	ClaimWorkspaceMount(ctx context.Context, capabilities api.WorkerCapabilities) (api.WorkerWorkspaceMountClaimResponse, error)
	RenewWorkspaceMount(ctx context.Context, request api.WorkerWorkspaceMountRenewRequest) (api.WorkspaceMountResponse, error)
	MarkWorkspaceMountMounted(ctx context.Context, request api.WorkerWorkspaceMountMountedRequest) (api.WorkspaceMountResponse, error)
	CaptureWorkspaceMount(ctx context.Context, request api.WorkerWorkspaceMountCaptureRequest) (api.WorkerWorkspaceMountCaptureResponse, error)
	StopWorkspaceMount(ctx context.Context, request api.WorkerWorkspaceMountStopRequest) (api.WorkspaceMountResponse, error)
	FailWorkspaceMount(ctx context.Context, request api.WorkerWorkspaceMountFailRequest) (api.WorkspaceMountResponse, error)
	ClaimWorkspaceExec(ctx context.Context, request api.WorkerWorkspaceExecClaimRequest) (api.WorkerWorkspaceExecClaimResponse, error)
	CompleteWorkspaceExec(ctx context.Context, request api.WorkerWorkspaceExecCompleteRequest) (api.WorkspaceMountResponse, error)
}

type RunLeaseExecutor interface {
	ExecuteRunLease(context.Context, api.WorkerRunLeaseWork) error
}

type BuildExecutor interface {
	Build(ctx context.Context, lease api.WorkerDeploymentBuildLease, deployment api.WorkerDeploymentBuild) (json.RawMessage, error)
}

type BuildPolicy interface {
	Resolve(string, string, string) (deployment.BuildTarget, error)
}

type Materializer interface {
	RunWorkspaceMount(ctx context.Context, mount api.WorkerWorkspaceMount, client api.WorkerWorkspaceMaterializerControlClient) error
}

type Runner struct {
	client                         ControlClient
	runLeaseExecutor               RunLeaseExecutor
	buildExecutor                  BuildExecutor
	buildPolicy                    BuildPolicy
	materializer                   Materializer
	capabilities                   api.WorkerCapabilities
	resources                      *capacity.Ledger
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

func WithBuildExecutor(executor BuildExecutor) Option {
	return func(runner *Runner) {
		runner.buildExecutor = executor
	}
}

func WithBuildPolicy(policy BuildPolicy) Option {
	return func(runner *Runner) {
		runner.buildPolicy = policy
	}
}

func WithMaterializer(materializer Materializer) Option {
	return func(runner *Runner) {
		runner.materializer = materializer
	}
}

func WithCapacity(resources *capacity.Ledger) Option {
	return func(runner *Runner) {
		runner.resources = resources
	}
}

func NewRunner(client ControlClient, executor RunLeaseExecutor, capabilities api.WorkerCapabilities, opts ...Option) (*Runner, error) {
	if client == nil {
		return nil, errors.New("worker client is required")
	}
	if executor == nil {
		return nil, errors.New("worker executor is required")
	}
	runner := &Runner{
		client:                         client,
		runLeaseExecutor:               executor,
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
	if runner.resources == nil {
		return nil, errors.New("worker capacity ledger is required")
	}
	if capabilities.SupportsBuild && runner.buildPolicy == nil {
		return nil, errors.New("build worker policy is required")
	}
	if runner.log == nil {
		runner.log = slog.Default()
	}
	return runner, nil
}

func isStaleLease(err error) bool {
	return client.IsStatus(err, http.StatusConflict)
}
