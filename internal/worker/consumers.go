package worker

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"sync"
	"time"

	"github.com/helmrdotdev/helmr/internal/api"
)

type runConsumer struct{ runner *Runner }
type buildConsumer struct{ runner *Runner }
type workspaceConsumer struct{ runner *Runner }

type buildLeaseState struct {
	mu    sync.RWMutex
	lease api.WorkerDeploymentBuildLease
}

func (s *buildLeaseState) current() api.WorkerDeploymentBuildLease {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.lease
}

func (s *buildLeaseState) set(lease api.WorkerDeploymentBuildLease) {
	s.mu.Lock()
	s.lease = lease
	s.mu.Unlock()
}

func NewRunConsumer(runner *Runner) Consumer   { return runConsumer{runner: runner} }
func NewBuildConsumer(runner *Runner) Consumer { return buildConsumer{runner: runner} }
func NewWorkspaceConsumer(runner *Runner) Consumer {
	return workspaceConsumer{runner: runner}
}

func (c runConsumer) Claim(ctx context.Context) (Work, bool, error) {
	r := c.runner
	leased, err := r.client.LeaseRun(ctx)
	if err != nil {
		return nil, false, fmt.Errorf("lease run: %w", err)
	}
	if leased.Lease == nil || leased.Run == nil {
		return nil, false, nil
	}
	lease, run := *leased.Lease, *leased.Run
	if err := validateLeaseRequirements(r.capabilities, lease, run); err != nil {
		payload, _ := json.Marshal(map[string]string{"message": err.Error()})
		if rejectErr := r.client.RejectRun(ctx, api.WorkerRejectRunRequest{Lease: lease, ReasonCode: "requirements_unsupported", Error: payload}); rejectErr != nil && !isStaleLease(rejectErr) {
			return nil, true, rejectErr
		}
		return func(context.Context) error { return nil }, true, nil
	}
	return func(workCtx context.Context) error {
		started, err := r.client.StartRun(workCtx, lease)
		if err != nil {
			if isStaleLease(err) {
				return nil
			}
			return fmt.Errorf("start run %s: %w", lease.RunID, err)
		}
		return r.executeStartedRun(workCtx, started.Lease, run)
	}, true, nil
}

func (r *Runner) executeStartedRun(ctx context.Context, lease api.WorkerRunLease, run api.WorkerRun) error {
	leaseState := newRunLeaseState(lease)
	execCtx, cancelExec := context.WithCancel(ctx)
	defer cancelExec()
	renewDone := make(chan *renewError, 1)
	go func() { renewDone <- r.renewUntilDone(execCtx, leaseState) }()
	resultDone := make(chan api.WorkerReleaseResult, 1)
	go func() { resultDone <- r.executor.Execute(execCtx, leaseState, run) }()
	var result api.WorkerReleaseResult
	var renewErr *renewError
	renewObserved := false
	select {
	case result = <-resultDone:
	case renewErr = <-renewDone:
		renewObserved = true
		cancelExec()
		if renewErr != nil {
			result = <-resultDone
			if renewErr.kind == renewStale {
				return nil
			}
			if renewErr.kind == renewTimeout && result.Kind == "" {
				message := fmt.Sprintf("worker lease renew timed out: %v", renewErr.err)
				result = api.WorkerReleaseResult{Kind: "failed", Error: &message}
			}
			if renewErr.kind == renewFailed {
				return fmt.Errorf("renew run %s: %w", lease.RunID, renewErr.err)
			}
		} else {
			result = <-resultDone
		}
	}
	if result.Kind == "detached" {
		cancelExec()
		if !renewObserved {
			<-renewDone
		}
		return nil
	}
	if err := r.release(leaseState.CurrentWorkerRunLease(), result); err != nil {
		cancelExec()
		if !renewObserved {
			<-renewDone
		}
		return fmt.Errorf("release run %s: %w", lease.RunID, err)
	}
	cancelExec()
	if !renewObserved {
		<-renewDone
	}
	return nil
}

func (c buildConsumer) Claim(ctx context.Context) (Work, bool, error) {
	r := c.runner
	leased, err := r.client.LeaseDeploymentBuild(ctx)
	if err != nil {
		return nil, false, fmt.Errorf("lease deployment build: %w", err)
	}
	if leased.Lease == nil || leased.Deployment == nil {
		return nil, false, nil
	}
	lease, deployment := *leased.Lease, *leased.Deployment
	if err := validateBuildLeaseShape(r.capabilities, lease); err != nil {
		return nil, true, r.rejectBuild(ctx, lease, "requirements_unsupported", err)
	}
	if r.deploymentBuilder == nil {
		err := errors.New("deployment builder is not configured")
		return nil, true, r.rejectBuild(ctx, lease, "builder_unavailable", err)
	}
	if !lease.ExpiresAt.Add(-r.deploymentBuildCompletionGrace).After(time.Now()) {
		err := errors.New("deployment build lease does not have enough time remaining")
		return nil, true, r.rejectBuild(ctx, lease, "lease_deadline_too_short", err)
	}
	return func(workCtx context.Context) error {
		started, err := r.client.StartDeploymentBuild(workCtx, lease)
		if err != nil {
			return fmt.Errorf("start deployment build %s: %w", lease.DeploymentID, err)
		}
		return r.executeStartedBuild(workCtx, started.Lease, deployment)
	}, true, nil
}

func validateBuildLeaseShape(capabilities api.WorkerCapabilities, lease api.WorkerDeploymentBuildLease) error {
	if lease.RequestedBuildExecutors <= 0 || lease.RequestedCPUMillis <= 0 || lease.RequestedMemoryBytes <= 0 || lease.RequestedWorkloadDiskBytes < 0 || lease.RequestedScratchBytes < 0 {
		return errors.New("build lease resource vector is invalid")
	}
	if lease.RequestedBuildExecutors > capabilities.MaxBuildExecutors {
		return errors.New("build executor count exceeds worker capacity")
	}
	executors := int64(lease.RequestedBuildExecutors)
	perExecutorWorkload := (lease.RequestedWorkloadDiskBytes + executors - 1) / executors
	perExecutorScratch := (lease.RequestedScratchBytes + executors - 1) / executors
	perExecutorCPU := (lease.RequestedCPUMillis + executors - 1) / executors
	perExecutorMemory := (lease.RequestedMemoryBytes + executors - 1) / executors
	if perExecutorCPU > capabilities.VMMilliCPU || perExecutorMemory > capabilities.VMMemoryMiB*1024*1024 || perExecutorWorkload > capabilities.VMMaxDiskMiB*1024*1024 || perExecutorScratch > capabilities.VMMaxScratchBytes {
		return fmt.Errorf("build per-executor disk shape exceeds worker VM shape")
	}
	return nil
}

func (r *Runner) rejectBuild(ctx context.Context, lease api.WorkerDeploymentBuildLease, reason string, cause error) error {
	payload, _ := json.Marshal(map[string]string{"message": cause.Error()})
	if err := r.client.RejectDeploymentBuild(ctx, api.WorkerDeploymentBuildRejectRequest{Lease: lease, ReasonCode: reason, Error: payload}); err != nil && !isStaleLease(err) {
		return err
	}
	return nil
}

func (r *Runner) executeStartedBuild(ctx context.Context, lease api.WorkerDeploymentBuildLease, deployment api.WorkerDeploymentBuild) error {
	buildCtx, cancelBuild := context.WithCancel(ctx)
	defer cancelBuild()
	leaseState := &buildLeaseState{lease: lease}
	renewDone := make(chan error, 1)
	go func() { renewDone <- r.renewBuildUntilDone(buildCtx, leaseState) }()
	resultDone := make(chan api.WorkerDeploymentBuildResult, 1)
	go func() { resultDone <- r.deploymentBuilder.BuildDeployment(buildCtx, lease, deployment) }()
	var result api.WorkerDeploymentBuildResult
	select {
	case result = <-resultDone:
		cancelBuild()
		<-renewDone
	case renewErr := <-renewDone:
		if renewErr == nil {
			result = <-resultDone
			break
		}
		cancelBuild()
		result = <-resultDone
		if isStaleLease(renewErr) {
			return nil
		}
		if result.Error == nil {
			message := "deployment build lease renewal failed: " + renewErr.Error()
			result.Error = &message
		}
	}
	completeCtx, cancelComplete := context.WithTimeout(context.WithoutCancel(ctx), r.releaseWait)
	defer cancelComplete()
	response, err := r.client.CompleteDeploymentBuild(completeCtx, leaseState.current(), result)
	if err != nil {
		return fmt.Errorf("complete deployment build %s: %w", lease.DeploymentID, err)
	}
	if strings.TrimSpace(response.Status) != "deployed" {
		r.log.Warn("worker completed deployment build with non-deployed status", "deployment_id", lease.DeploymentID, "status", response.Status)
	}
	return nil
}

func (r *Runner) renewBuildUntilDone(ctx context.Context, state *buildLeaseState) error {
	ticker := time.NewTicker(r.renewEvery)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return nil
		case <-ticker.C:
		}
		renewCtx, cancelRenew := context.WithTimeout(ctx, r.renewWait)
		response, err := r.client.RenewDeploymentBuild(renewCtx, state.current())
		cancelRenew()
		if err != nil {
			if ctx.Err() != nil {
				return nil
			}
			return err
		}
		if strings.TrimSpace(response.Lease.ID) == "" {
			return errors.New("renew deployment build response did not include a lease")
		}
		state.set(response.Lease)
	}
}

func (c workspaceConsumer) Claim(ctx context.Context) (Work, bool, error) {
	r := c.runner
	if r.materializer == nil {
		return nil, false, nil
	}
	claimed, err := r.client.ClaimWorkspaceMount(ctx, r.capabilities)
	if err != nil {
		return nil, false, fmt.Errorf("claim workspace mount: %w", err)
	}
	if claimed.Mount == nil {
		return nil, false, nil
	}
	mount := *claimed.Mount
	return func(workCtx context.Context) error { return r.materializer.RunWorkspaceMount(workCtx, mount, r.client) }, true, nil
}
