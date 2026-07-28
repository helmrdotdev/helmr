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
	"github.com/helmrdotdev/helmr/internal/capacity"
	"github.com/helmrdotdev/helmr/internal/compute"
	"github.com/helmrdotdev/helmr/internal/deployment"
	"github.com/helmrdotdev/helmr/internal/vm"
)

type runConsumer struct {
	runner *Runner
	mu     sync.Mutex
	active map[api.WorkerRunLeaseWork]struct{}
}
type platformAcquisitionConsumer struct{ runner *Runner }
type buildConsumer struct{ runner *Runner }
type workspaceConsumer struct{ runner *Runner }

type fatalWorkerError struct {
	err error
}

func (err *fatalWorkerError) Error() string     { return err.err.Error() }
func (err *fatalWorkerError) Unwrap() error     { return err.err }
func (err *fatalWorkerError) FatalWorker() bool { return true }

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

func NewRunConsumer(runner *Runner) Consumer {
	return &runConsumer{runner: runner, active: make(map[api.WorkerRunLeaseWork]struct{})}
}
func NewPlatformAcquisitionConsumer(runner *Runner) Consumer {
	return platformAcquisitionConsumer{runner: runner}
}
func NewBuildConsumer(runner *Runner) Consumer { return buildConsumer{runner: runner} }
func NewWorkspaceConsumer(runner *Runner) Consumer {
	return workspaceConsumer{runner: runner}
}

func (c *runConsumer) Claim(ctx context.Context) (Work, bool, error) {
	discovered, err := c.runner.client.DiscoverRunLeases(ctx)
	if err != nil {
		return nil, false, fmt.Errorf("discover Run Leases: %w", err)
	}
	c.mu.Lock()
	var selected api.WorkerRunLeaseWork
	for _, work := range discovered.Items {
		if work.LeaseID == "" || work.LeaseSequence <= 0 {
			c.mu.Unlock()
			return nil, false, errors.New("discovered Run Lease identity is invalid")
		}
		if _, running := c.active[work]; running {
			continue
		}
		selected = work
		c.active[work] = struct{}{}
		break
	}
	c.mu.Unlock()
	if selected.LeaseID == "" {
		return nil, false, nil
	}
	return func(workCtx context.Context) error {
		defer func() {
			c.mu.Lock()
			delete(c.active, selected)
			c.mu.Unlock()
		}()
		if err := c.runner.runLeaseExecutor.ExecuteRunLease(workCtx, selected); err != nil {
			if isStaleLease(err) {
				return nil
			}
			return fmt.Errorf(
				"execute Run Lease %s/%d: %w",
				selected.LeaseID,
				selected.LeaseSequence,
				err,
			)
		}
		return nil
	}, true, nil
}

func (c platformAcquisitionConsumer) Claim(ctx context.Context) (Work, bool, error) {
	r := c.runner
	next, err := r.client.NextPlatformAcquisition(ctx)
	if err != nil {
		return nil, false, fmt.Errorf("read Platform acquisition: %w", err)
	}
	if next.Acquisition == nil {
		return nil, false, nil
	}
	acquisition := *next.Acquisition
	policyDigest, err := r.buildPolicy.Digest()
	if err != nil {
		return nil, true, &fatalWorkerError{err: fmt.Errorf("read build policy digest: %w", err)}
	}
	if acquisition.BuildPolicyDigest != policyDigest {
		return nil, true, &fatalWorkerError{err: errors.New("Control and Worker build policies differ")}
	}
	if r.platformAcquirer == nil {
		return nil, true, &fatalWorkerError{err: errors.New("Platform acquirer is not configured")}
	}
	return func(workCtx context.Context) error {
		candidates, err := r.platformAcquirer.Acquire(workCtx, acquisition)
		if err != nil {
			var deterministic interface {
				PlatformAcquisitionFailureReason() api.WorkerPlatformAcquisitionFailureReason
			}
			if !errors.As(err, &deterministic) {
				return fmt.Errorf("acquire Platform Artifacts for Deployment %s: %w", acquisition.DeploymentID, err)
			}
			raw, _ := json.Marshal(map[string]string{"message": err.Error()})
			_, reportErr := r.client.FailPlatformAcquisition(
				workCtx,
				api.WorkerPlatformAcquisitionFailRequest{
					Acquisition: acquisition,
					Reason:      deterministic.PlatformAcquisitionFailureReason(),
					Error:       raw,
				},
			)
			if isStaleLease(reportErr) {
				return nil
			}
			return reportErr
		}
		_, err = r.client.CompletePlatformAcquisition(
			workCtx,
			api.WorkerPlatformAcquisitionCompleteRequest{
				Acquisition: acquisition,
				Candidates:  candidates,
			},
		)
		if isStaleLease(err) {
			return nil
		}
		return err
	}, true, nil
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
	if err := validateBuildEnvelope(r.capabilities, deployment); err != nil {
		return nil, true, r.rejectBuild(ctx, lease, "requirements_unsupported", err)
	}
	if err := validateBuildLeaseShape(r.capabilities, lease); err != nil {
		return nil, true, r.rejectBuild(ctx, lease, "requirements_unsupported", err)
	}
	if r.buildExecutor == nil {
		err := errors.New("build executor is not configured")
		return nil, true, r.rejectBuild(ctx, lease, "builder_unavailable", err)
	}
	if !lease.ExpiresAt.Add(-r.deploymentBuildCompletionGrace).After(time.Now()) {
		err := errors.New("deployment build lease does not have enough time remaining")
		return nil, true, r.rejectBuild(ctx, lease, "lease_deadline_too_short", err)
	}
	resourceKey := capacity.Key{Kind: "build", Epoch: lease.WorkerEpoch, ID: lease.ID}
	created, err := r.resources.Reserve(resourceKey, capacity.Vector{
		CPUMillis:         lease.RequestedCPUMillis,
		MemoryBytes:       lease.RequestedMemoryBytes,
		WorkloadDiskBytes: lease.RequestedWorkloadDiskBytes,
		ScratchBytes:      lease.RequestedScratchBytes,
		BuildSlots:        int64(lease.RequestedBuildExecutors),
	})
	if err != nil {
		return nil, true, r.rejectBuild(ctx, lease, "local_capacity_exceeded", err)
	}
	if !created {
		return nil, true, errors.New("deployment build is already reserved locally")
	}
	return func(workCtx context.Context) error {
		releaseCapacity := true
		defer func() {
			if !releaseCapacity {
				return
			}
			if err := r.resources.Release(resourceKey); err != nil {
				r.log.Error("release deployment build capacity", "deployment_id", lease.DeploymentID, "error", err)
			}
		}()
		started, err := r.client.StartDeploymentBuild(workCtx, lease)
		if err != nil {
			return fmt.Errorf("start deployment build %s: %w", lease.DeploymentID, err)
		}
		err = r.executeStartedBuild(workCtx, started.Lease, deployment)
		var cleanupUnproven *vm.CleanupUnprovenError
		if errors.As(err, &cleanupUnproven) {
			releaseCapacity = false
			return &fatalWorkerError{err: err}
		}
		var fatal FatalWorkError
		if errors.As(err, &fatal) && fatal.FatalWorker() {
			releaseCapacity = false
		}
		return err
	}, true, nil
}

func validateBuildEnvelope(
	capabilities api.WorkerCapabilities,
	build api.WorkerDeploymentBuild,
) error {
	if capabilities.RuntimeArch != string(deployment.ArchitectureX8664) {
		return fmt.Errorf("build worker architecture %q is unsupported", capabilities.RuntimeArch)
	}
	if build.BuildContractVersion != deployment.ProgramBuildContractVersion {
		return fmt.Errorf(
			"deployment build contract version %q is unsupported",
			build.BuildContractVersion,
		)
	}
	if !capabilities.SupportsBuild {
		return errors.New("worker is not certified for builds")
	}
	manager := deployment.PackageManager{
		Integrity: build.Manager.Integrity,
		Name:      deployment.PackageManagerName(build.Manager.Name),
		Version:   build.Manager.Version,
	}
	if err := deployment.ValidatePackageManager(manager); err != nil {
		return fmt.Errorf("deployment Manager selector: %w", err)
	}
	for name, object := range map[string]api.CASObject{
		"runtime":   build.Runtime,
		"Manager":   build.Manager.Artifact,
		"toolchain": build.Toolchain,
	} {
		if _, err := deployment.SHA256DigestBytes(object.Digest); err != nil {
			return fmt.Errorf("deployment %s digest: %w", name, err)
		}
		if object.SizeBytes < 1 {
			return fmt.Errorf("deployment %s size is invalid", name)
		}
	}
	if build.Runtime.MediaType != deployment.RuntimeArtifactMediaType ||
		build.Manager.Artifact.MediaType != deployment.ManagerTreeMediaType ||
		build.Toolchain.MediaType != deployment.ToolchainMediaType {
		return errors.New("deployment Platform Artifact media type is invalid")
	}
	return nil
}

func validateBuildLeaseShape(capabilities api.WorkerCapabilities, lease api.WorkerDeploymentBuildLease) error {
	envelope := compute.BuildEnvelopeResources()
	guest := compute.BuildGuestResources()
	if lease.RequestedCPUMillis != envelope.MilliCPU ||
		lease.RequestedMemoryBytes != envelope.MemoryMiB<<20 ||
		lease.RequestedWorkloadDiskBytes != 0 ||
		lease.RequestedScratchBytes != envelope.DiskMiB<<20 ||
		lease.RequestedBuildExecutors != 1 {
		return errors.New("build lease does not match the fixed build envelope")
	}
	if capabilities.MaxBuildExecutors != 1 {
		return errors.New("build worker must expose exactly one build executor")
	}
	if capabilities.VMMilliCPU < guest.MilliCPU ||
		capabilities.VMMemoryMiB < guest.MemoryMiB ||
		capabilities.VMMaxScratchBytes < guest.DiskMiB<<20 {
		return errors.New("worker VM shape cannot host the fixed build guest")
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
	type buildOutcome struct {
		result json.RawMessage
		err    error
	}
	resultDone := make(chan buildOutcome, 1)
	go func() {
		result, err := r.buildExecutor.Build(buildCtx, lease, deployment)
		resultDone <- buildOutcome{result: result, err: err}
	}()
	var outcome buildOutcome
	select {
	case outcome = <-resultDone:
		cancelBuild()
		<-renewDone
	case renewErr := <-renewDone:
		if renewErr == nil {
			outcome = <-resultDone
			break
		}
		cancelBuild()
		outcome = <-resultDone
		if isStaleLease(renewErr) {
			return nil
		}
		if outcome.err == nil {
			outcome.err = fmt.Errorf("renew deployment build lease: %w", renewErr)
		}
	}
	if outcome.err != nil {
		var cleanupUnproven *vm.CleanupUnprovenError
		if errors.As(outcome.err, &cleanupUnproven) {
			return outcome.err
		}
		var deliveryFailure interface {
			DeploymentBuildDeliveryFailureReason() api.WorkerDeploymentBuildDeliveryFailureReason
		}
		if !errors.As(outcome.err, &deliveryFailure) {
			return fmt.Errorf("build deployment %s: %w", lease.DeploymentID, outcome.err)
		}
		reportCtx, cancelReport := context.WithTimeout(context.WithoutCancel(ctx), r.releaseWait)
		defer cancelReport()
		response, err := r.client.ReportDeploymentBuildDeliveryFailure(reportCtx, api.WorkerDeploymentBuildDeliveryFailureRequest{
			Lease:      leaseState.current(),
			ReasonCode: deliveryFailure.DeploymentBuildDeliveryFailureReason(),
		})
		if err != nil {
			if isStaleLease(err) {
				return nil
			}
			return fmt.Errorf("report deployment build delivery failure %s: %w", lease.DeploymentID, err)
		}
		if strings.TrimSpace(response.Status) != "building" &&
			strings.TrimSpace(response.Status) != "deployed" &&
			strings.TrimSpace(response.Status) != "failed" {
			r.log.Warn("worker reported deployment build delivery failure with unexpected status", "deployment_id", lease.DeploymentID, "status", response.Status)
		}
		return nil
	}
	completeCtx, cancelComplete := context.WithTimeout(context.WithoutCancel(ctx), r.releaseWait)
	defer cancelComplete()
	response, err := r.client.CompleteDeploymentBuild(completeCtx, leaseState.current(), outcome.result)
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
