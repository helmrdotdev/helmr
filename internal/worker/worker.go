package worker

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/helmrdotdev/helmr/internal/api"
	"github.com/helmrdotdev/helmr/internal/client"
)

type ControlClient interface {
	LeaseDeploymentBuild(ctx context.Context, capabilities api.WorkerCapabilities) (api.WorkerDeploymentBuildLeaseResponse, error)
	CompleteDeploymentBuild(ctx context.Context, lease api.WorkerDeploymentBuildLease, result api.WorkerDeploymentBuildResult) (api.WorkerDeploymentBuildResponse, error)
	ClaimWorkspaceMaterialization(ctx context.Context, capabilities api.WorkerCapabilities) (api.WorkerWorkspaceMaterializationClaimResponse, error)
	RenewWorkspaceMaterialization(ctx context.Context, request api.WorkerWorkspaceMaterializationRenewRequest) (api.WorkspaceMaterializationResponse, error)
	MarkWorkspaceMaterializationRunning(ctx context.Context, request api.WorkerWorkspaceMaterializationRunningRequest) (api.WorkspaceMaterializationResponse, error)
	StopWorkspaceMaterialization(ctx context.Context, request api.WorkerWorkspaceMaterializationStopRequest) (api.WorkspaceMaterializationResponse, error)
	FailWorkspaceMaterialization(ctx context.Context, request api.WorkerWorkspaceMaterializationFailRequest) (api.WorkspaceMaterializationResponse, error)
	ClaimWorkspaceMaterializationOperation(ctx context.Context, request api.WorkerWorkspaceOperationClaimRequest) (api.WorkerWorkspaceOperationClaimResponse, error)
	StartWorkspaceMaterializationOperation(ctx context.Context, request api.WorkerWorkspaceOperationStartRequest) (api.WorkspaceOperationResponse, error)
	CompleteWorkspaceMaterializationOperation(ctx context.Context, request api.WorkerWorkspaceOperationCompleteRequest) (api.WorkspaceOperationResponse, error)
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
	LeaseRun(ctx context.Context, capabilities api.WorkerCapabilities) (api.WorkerRunLeaseResponse, error)
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
	RunWorkspaceMaterialization(ctx context.Context, materialization api.WorkerWorkspaceMaterialization, client api.WorkerWorkspaceMaterializerControlClient) error
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
	client            ControlClient
	executor          Executor
	deploymentBuilder DeploymentBuilder
	materializer      Materializer
	capabilities      api.WorkerCapabilities
	pollEvery         time.Duration
	renewEvery        time.Duration
	renewWait         time.Duration
	releaseWait       time.Duration
	log               *slog.Logger
	materializationMu sync.Mutex
	materializationWG sync.WaitGroup
	materializing     bool
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
		client:       client,
		executor:     executor,
		capabilities: capabilities,
		pollEvery:    2 * time.Second,
		renewEvery:   10 * time.Second,
		renewWait:    5 * time.Second,
		releaseWait:  30 * time.Second,
		log:          slog.Default(),
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
	if runner.log == nil {
		runner.log = slog.Default()
	}
	return runner, nil
}

func (r *Runner) Run(ctx context.Context) error {
	timer := time.NewTimer(0)
	defer timer.Stop()

	for {
		select {
		case <-ctx.Done():
			r.waitForMaterializations()
			return ctx.Err()
		case <-timer.C:
		}

		worked, err := r.RunOnce(ctx)
		if err != nil {
			r.log.Error("worker iteration failed", "error", err)
		}

		delay := time.Duration(0)
		if !worked {
			delay = r.pollEvery
		}
		timer.Reset(delay)
	}
}

func (r *Runner) tryReserveMaterialization() bool {
	r.materializationMu.Lock()
	defer r.materializationMu.Unlock()
	if r.materializing {
		return false
	}
	r.materializing = true
	return true
}

func (r *Runner) releaseMaterializationReservation() {
	r.materializationMu.Lock()
	defer r.materializationMu.Unlock()
	r.materializing = false
}

func (r *Runner) waitForMaterializations() {
	r.materializationWG.Wait()
}

func (r *Runner) RunOnce(ctx context.Context) (bool, error) {
	if r.deploymentBuilder != nil {
		leased, err := r.client.LeaseDeploymentBuild(ctx, r.capabilities)
		if err != nil {
			return false, fmt.Errorf("lease deployment build: %w", err)
		}
		if leased.Lease != nil && leased.Deployment != nil {
			lease := *leased.Lease
			deployment := *leased.Deployment
			r.log.Info("worker leased deployment build", "deployment_id", lease.DeploymentID)
			result := r.deploymentBuilder.BuildDeployment(ctx, lease, deployment)
			response, err := r.client.CompleteDeploymentBuild(ctx, lease, result)
			if err != nil {
				return true, fmt.Errorf("complete deployment build %s: %w", lease.DeploymentID, err)
			}
			if strings.TrimSpace(response.Status) != "deployed" {
				r.log.Warn("worker completed deployment build with non-deployed status", "deployment_id", lease.DeploymentID, "status", response.Status)
			} else {
				r.log.Info("worker completed deployment build", "deployment_id", lease.DeploymentID, "status", response.Status)
			}
			return true, nil
		}
	}
	if r.materializer != nil && r.tryReserveMaterialization() {
		claimed, err := r.client.ClaimWorkspaceMaterialization(ctx, r.capabilities)
		if err != nil {
			r.releaseMaterializationReservation()
			return false, fmt.Errorf("claim workspace materialization: %w", err)
		}
		if claimed.Materialization != nil {
			materialization := *claimed.Materialization
			r.log.Info("worker claimed workspace materialization", "workspace_id", materialization.WorkspaceID, "materialization_id", materialization.ID)
			r.materializationWG.Add(1)
			go func() {
				defer r.materializationWG.Done()
				defer r.releaseMaterializationReservation()
				if err := r.materializer.RunWorkspaceMaterialization(ctx, materialization, r.client); err != nil && ctx.Err() == nil {
					r.log.Error("workspace materialization failed", "workspace_id", materialization.WorkspaceID, "materialization_id", materialization.ID, "error", err)
				}
			}()
			return true, nil
		}
		r.releaseMaterializationReservation()
	}
	leased, err := r.client.LeaseRun(ctx, r.capabilities)
	if err != nil {
		return false, fmt.Errorf("lease run: %w", err)
	}
	if leased.Lease == nil || leased.Run == nil {
		return false, nil
	}

	lease := *leased.Lease
	run := *leased.Run
	r.log.Info("worker leased run", "run_id", lease.RunID, "task_id", run.TaskID)

	if err := validateLeaseRequirements(r.capabilities, lease, run); err != nil {
		message := "worker rejected run requirements: " + err.Error()
		if releaseErr := r.release(lease, api.WorkerReleaseResult{Kind: "failed", Error: &message}); releaseErr != nil && !isStaleLease(releaseErr) {
			return true, fmt.Errorf("release rejected run %s: %w", lease.RunID, releaseErr)
		}
		r.log.Warn("worker rejected run requirements", "run_id", lease.RunID, "error", err)
		return true, nil
	}

	if _, err := r.client.StartRun(ctx, lease); err != nil {
		if ctx.Err() != nil {
			message := "worker shutdown before execution started"
			if releaseErr := r.release(lease, api.WorkerReleaseResult{Kind: "cancelled", Error: &message}); releaseErr != nil && !isStaleLease(releaseErr) {
				return true, fmt.Errorf("release shutdown cancellation for run %s: %w", lease.RunID, releaseErr)
			}
			return true, nil
		}
		if isStaleLease(err) {
			return true, nil
		}
		return true, fmt.Errorf("start run %s: %w", lease.RunID, err)
	}

	leaseState := newRunLeaseState(lease)
	execCtx, cancelExec := context.WithCancel(ctx)
	defer cancelExec()
	renewDone := make(chan *renewError, 1)
	go func() {
		renewDone <- r.renewUntilDone(execCtx, leaseState)
	}()

	resultDone := make(chan api.WorkerReleaseResult, 1)
	go func() {
		resultDone <- r.executor.Execute(execCtx, leaseState, run)
	}()

	var result api.WorkerReleaseResult
	var renewErr *renewError
	renewObserved := false
	select {
	case result = <-resultDone:
	case err := <-renewDone:
		renewErr = err
		renewObserved = true
		cancelExec()
		if err != nil {
			r.log.Warn("worker lease renew failed; cancelling execution", "run_id", lease.RunID, "error", err)
			result = <-resultDone
			switch err.kind {
			case renewTimeout:
				if result.Kind == "" {
					message := fmt.Sprintf("worker lease renew timed out: %v", err.err)
					result = api.WorkerReleaseResult{Kind: "failed", Error: &message}
				}
			case renewStale:
				return true, nil
			default:
				return true, fmt.Errorf("renew run %s: %w", lease.RunID, err.err)
			}
		} else {
			result = <-resultDone
		}
	}
	if renewObserved && renewErr != nil {
		r.log.Warn("worker lease renew failed after execution", "run_id", lease.RunID, "error", renewErr)
		switch renewErr.kind {
		case renewTimeout:
			if result.Kind == "" {
				message := fmt.Sprintf("worker lease renew failed: %v", renewErr.err)
				result = api.WorkerReleaseResult{Kind: "failed", Error: &message}
			}
		case renewStale:
			return true, nil
		default:
			return true, fmt.Errorf("renew run %s: %w", lease.RunID, renewErr.err)
		}
	}
	if result.Kind == "detached" {
		cancelExec()
		if !renewObserved {
			renewErr = <-renewDone
			if renewErr != nil {
				r.log.Warn("worker lease renew stopped after detach", "run_id", lease.RunID, "error", renewErr)
			}
		}
		r.log.Info("worker detached run after checkpoint", "run_id", lease.RunID)
		return true, nil
	}

	if err := r.release(leaseState.CurrentWorkerRunLease(), result); err != nil {
		cancelExec()
		if !renewObserved {
			<-renewDone
		}
		return true, fmt.Errorf("release run %s: %w", lease.RunID, err)
	}
	cancelExec()
	if !renewObserved {
		renewErr = <-renewDone
		if renewErr != nil {
			r.log.Warn("worker lease renew stopped after release", "run_id", lease.RunID, "error", renewErr)
		}
	}
	r.log.Info("worker released run", "run_id", lease.RunID, "result", result.Kind)
	return true, nil
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
