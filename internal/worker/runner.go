package worker

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"time"

	"github.com/helmrdotdev/helmr/internal/capacity"
	"github.com/helmrdotdev/helmr/internal/deployment"
	"github.com/helmrdotdev/helmr/internal/httpclient"
	"github.com/helmrdotdev/helmr/internal/workerapi"
)

const defaultDeploymentBuildCompletionGrace = 30 * time.Second

type ControlPlaneClient interface {
	DiscoverRunLeases(ctx context.Context) (workerapi.RunLeaseDiscoveryResponse, error)
	NextPlatformAcquisition(ctx context.Context) (workerapi.PlatformAcquisitionResponse, error)
	CompletePlatformAcquisition(ctx context.Context, request workerapi.PlatformAcquisitionCompleteRequest) (workerapi.PlatformAcquisitionResult, error)
	FailPlatformAcquisition(ctx context.Context, request workerapi.PlatformAcquisitionFailRequest) (workerapi.PlatformAcquisitionResult, error)
	LeaseDeploymentBuild(ctx context.Context) (workerapi.DeploymentBuildLeaseResponse, error)
	StartDeploymentBuild(ctx context.Context, lease workerapi.DeploymentBuildLease) (workerapi.DeploymentBuildStartResponse, error)
	RenewDeploymentBuild(ctx context.Context, lease workerapi.DeploymentBuildLease) (workerapi.DeploymentBuildRenewResponse, error)
	RejectDeploymentBuild(ctx context.Context, request workerapi.DeploymentBuildRejectRequest) error
	ReportDeploymentBuildDeliveryFailure(ctx context.Context, request workerapi.DeploymentBuildDeliveryFailureRequest) (workerapi.DeploymentBuildResponse, error)
	CompleteDeploymentBuild(ctx context.Context, lease workerapi.DeploymentBuildLease, result json.RawMessage) (workerapi.DeploymentBuildResponse, error)
	ClaimWorkspaceMount(ctx context.Context, capabilities workerapi.Capabilities) (workerapi.WorkspaceMountClaimResponse, error)
	RenewWorkspaceMount(ctx context.Context, request workerapi.WorkspaceMountRenewRequest) (workerapi.WorkspaceMountResponse, error)
	MarkWorkspaceMountMounted(ctx context.Context, request workerapi.WorkspaceMountMountedRequest) (workerapi.WorkspaceMountResponse, error)
	CaptureWorkspaceMount(ctx context.Context, request workerapi.WorkspaceMountCaptureRequest) (workerapi.WorkspaceMountCaptureResponse, error)
	StopWorkspaceMount(ctx context.Context, request workerapi.WorkspaceMountStopRequest) (workerapi.WorkspaceMountResponse, error)
	FailWorkspaceMount(ctx context.Context, request workerapi.WorkspaceMountFailRequest) (workerapi.WorkspaceMountResponse, error)
	ClaimWorkspaceExec(ctx context.Context, request workerapi.WorkspaceExecClaimRequest) (workerapi.WorkspaceExecClaimResponse, error)
	CompleteWorkspaceExec(ctx context.Context, request workerapi.WorkspaceExecCompleteRequest) (workerapi.WorkspaceMountResponse, error)
}

type RunLeaseExecutor interface {
	ExecuteRunLease(context.Context, workerapi.RunLeaseWork) error
}

type BuildExecutor interface {
	Build(
		ctx context.Context,
		lease workerapi.DeploymentBuildLease,
		deployment workerapi.DeploymentBuild,
		revocations deployment.ImageOperationRevocations,
	) (json.RawMessage, error)
}

type PlatformAcquirer interface {
	Acquire(context.Context, workerapi.PlatformAcquisition) (workerapi.PlatformAcquisitionCandidates, error)
}

type BuildPolicy interface {
	Digest() (string, error)
	Node(string) (deployment.VersionDomain, []string, error)
	Manager(deployment.PackageManager) (deployment.ManagerPolicy, error)
	DeniesDigest(string) bool
	DeniesSelector(string) bool
}

type Materializer interface {
	RunWorkspaceMount(ctx context.Context, mount workerapi.WorkspaceMount, client workerapi.WorkspaceMaterializerControlPlaneClient) error
}

type Runner struct {
	client                         ControlPlaneClient
	runLeaseExecutor               RunLeaseExecutor
	platformAcquirer               PlatformAcquirer
	buildExecutor                  BuildExecutor
	buildPolicy                    BuildPolicy
	materializer                   Materializer
	capabilities                   workerapi.Capabilities
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

func WithPlatformAcquirer(acquirer PlatformAcquirer) Option {
	return func(runner *Runner) {
		runner.platformAcquirer = acquirer
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

func NewRunner(client ControlPlaneClient, executor RunLeaseExecutor, capabilities workerapi.Capabilities, opts ...Option) (*Runner, error) {
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
	if capabilities.SupportsBuild && runner.platformAcquirer == nil {
		return nil, errors.New("build worker platform acquirer is required")
	}
	if runner.log == nil {
		runner.log = slog.Default()
	}
	return runner, nil
}

func isStaleLease(err error) bool {
	return httpclient.IsStatus(err, http.StatusConflict)
}
