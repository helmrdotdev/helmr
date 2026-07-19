package worker

import (
	"context"
	"errors"
	"sync/atomic"
	"testing"
	"time"

	"github.com/helmrdotdev/helmr/internal/api"
	"github.com/helmrdotdev/helmr/internal/capacity"
	"github.com/helmrdotdev/helmr/internal/vm"
)

func testWorkerDeploymentBuild() api.WorkerDeploymentBuild {
	return api.WorkerDeploymentBuild{
		ID:                      "deployment-1",
		MaterializerVersion:     "helmr.dependencies.v0",
		StandardToolchainDigest: "sha256:bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb",
		Runtime: api.WorkerRuntimeDescriptor{
			Architecture:      "aarch64",
			Digest:            "sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa",
			FormatVersion:     0,
			MediaType:         "application/vnd.helmr.runtime.v0+squashfs",
			RuntimeAPIVersion: "helmr.runtime.v0",
			SizeBytes:         1,
		},
	}
}

type consumerTestClient struct {
	ControlClient
	runLease        api.WorkerRunLease
	run             api.WorkerRun
	buildLease      api.WorkerDeploymentBuildLease
	deployment      api.WorkerDeploymentBuild
	runStartCalls   atomic.Int32
	buildStartCalls atomic.Int32
	buildComplete   atomic.Int32
	buildDelivery   atomic.Int32
	buildReject     atomic.Int32
	deliveryReason  atomic.Value
	rejectReason    atomic.Value
}

func (c *consumerTestClient) LeaseRun(context.Context) (api.WorkerRunLeaseResponse, error) {
	return api.WorkerRunLeaseResponse{Lease: &c.runLease, Run: &c.run}, nil
}

func (c *consumerTestClient) StartRun(context.Context, api.WorkerRunLease) (api.WorkerStartResponse, error) {
	c.runStartCalls.Add(1)
	return api.WorkerStartResponse{Lease: c.runLease}, nil
}

func (c *consumerTestClient) LeaseDeploymentBuild(context.Context) (api.WorkerDeploymentBuildLeaseResponse, error) {
	return api.WorkerDeploymentBuildLeaseResponse{Lease: &c.buildLease, Deployment: &c.deployment}, nil
}

func (c *consumerTestClient) StartDeploymentBuild(context.Context, api.WorkerDeploymentBuildLease) (api.WorkerDeploymentBuildStartResponse, error) {
	c.buildStartCalls.Add(1)
	return api.WorkerDeploymentBuildStartResponse{Lease: c.buildLease}, nil
}

func (c *consumerTestClient) CompleteDeploymentBuild(context.Context, api.WorkerDeploymentBuildLease, api.WorkerDeploymentBuildResult) (api.WorkerDeploymentBuildResponse, error) {
	c.buildComplete.Add(1)
	return api.WorkerDeploymentBuildResponse{Status: "deployed"}, nil
}

func (c *consumerTestClient) ReportDeploymentBuildDeliveryFailure(_ context.Context, request api.WorkerDeploymentBuildDeliveryFailureRequest) (api.WorkerDeploymentBuildResponse, error) {
	c.buildDelivery.Add(1)
	c.deliveryReason.Store(request.ReasonCode)
	return api.WorkerDeploymentBuildResponse{Status: "building"}, nil
}

func (c *consumerTestClient) RejectDeploymentBuild(_ context.Context, request api.WorkerDeploymentBuildRejectRequest) error {
	c.buildReject.Add(1)
	c.rejectReason.Store(request.ReasonCode)
	return nil
}

type detachedTestExecutor struct{ calls atomic.Int32 }

func (e *detachedTestExecutor) Execute(context.Context, api.WorkerRunLeaseProvider, api.WorkerRun) api.WorkerReleaseResult {
	e.calls.Add(1)
	return api.WorkerReleaseResult{Kind: "detached"}
}

type successfulTestBuilder struct{ calls atomic.Int32 }

func (b *successfulTestBuilder) BuildDeployment(context.Context, api.WorkerDeploymentBuildLease, api.WorkerDeploymentBuild) (api.WorkerDeploymentBuildResult, error) {
	b.calls.Add(1)
	return api.WorkerDeploymentBuildResult{}, nil
}

type deliveryFailureTestBuilder struct{}

func (*deliveryFailureTestBuilder) BuildDeployment(context.Context, api.WorkerDeploymentBuildLease, api.WorkerDeploymentBuild) (api.WorkerDeploymentBuildResult, error) {
	return api.WorkerDeploymentBuildResult{}, deliveryFailureTestError{}
}

type deliveryFailureTestError struct{}

func (deliveryFailureTestError) Error() string {
	return "verifier infrastructure failed"
}

func (deliveryFailureTestError) DeploymentBuildDeliveryFailureReason() api.WorkerDeploymentBuildDeliveryFailureReason {
	return api.WorkerDeploymentBuildDeliveryProgramVerifierFailed
}

type unclassifiedFailureTestBuilder struct{}

func (*unclassifiedFailureTestBuilder) BuildDeployment(context.Context, api.WorkerDeploymentBuildLease, api.WorkerDeploymentBuild) (api.WorkerDeploymentBuildResult, error) {
	return api.WorkerDeploymentBuildResult{}, errors.New("unclassified infrastructure failure")
}

type cleanupUnprovenTestBuilder struct {
	owner vm.Owner
}

func (b *cleanupUnprovenTestBuilder) BuildDeployment(context.Context, api.WorkerDeploymentBuildLease, api.WorkerDeploymentBuild) (api.WorkerDeploymentBuildResult, error) {
	return api.WorkerDeploymentBuildResult{}, vm.NewGuestError(&vm.CleanupUnprovenError{
		Owner: b.owner,
		Cause: errors.New("guest process absence could not be proven"),
	})
}

type fatalBuildTestBuilder struct{}

func (*fatalBuildTestBuilder) BuildDeployment(context.Context, api.WorkerDeploymentBuildLease, api.WorkerDeploymentBuild) (api.WorkerDeploymentBuildResult, error) {
	return api.WorkerDeploymentBuildResult{}, fatalBuildTestError{}
}

type fatalBuildTestError struct{}

func (fatalBuildTestError) Error() string     { return "build service failed" }
func (fatalBuildTestError) FatalWorker() bool { return true }

type canceledBuildTestBuilder struct{}

func (*canceledBuildTestBuilder) BuildDeployment(context.Context, api.WorkerDeploymentBuildLease, api.WorkerDeploymentBuild) (api.WorkerDeploymentBuildResult, error) {
	return api.WorkerDeploymentBuildResult{}, context.Canceled
}

func TestRunConsumerStartsLeaseInsideRegisteredWork(t *testing.T) {
	capabilities := testCapabilities()
	client := &consumerTestClient{
		runLease: api.WorkerRunLease{RunID: "run-1", ProtocolVersion: capabilities.ProtocolVersion},
		run:      api.WorkerRun{WorkerProtocolVersion: capabilities.ProtocolVersion, Requirements: testRequirements()},
	}
	executor := &detachedTestExecutor{}
	runner, err := NewRunner(client, executor, capabilities, WithCapacity(testCapacity(t, capabilities)))
	if err != nil {
		t.Fatal(err)
	}
	claimCtx, cancelClaim := context.WithCancel(context.Background())
	work, ok, err := NewRunConsumer(runner).Claim(claimCtx)
	if err != nil || !ok || work == nil {
		t.Fatalf("claim = (%v, %v, %v)", work, ok, err)
	}
	cancelClaim()
	if got := client.runStartCalls.Load(); got != 0 {
		t.Fatalf("start calls before registered work = %d", got)
	}
	if err := work(context.Background()); err != nil {
		t.Fatal(err)
	}
	if got := client.runStartCalls.Load(); got != 1 {
		t.Fatalf("start calls = %d", got)
	}
	if got := executor.calls.Load(); got != 1 {
		t.Fatalf("executor calls = %d", got)
	}
}

func TestBuildConsumerStartsLeaseInsideRegisteredWork(t *testing.T) {
	capabilities := testCapabilities()
	capabilities.MaxBuildExecutors = 1
	capabilities.SupportsBuild = true
	client := &consumerTestClient{
		buildLease: api.WorkerDeploymentBuildLease{
			ID: "build-lease-1", WorkerEpoch: 1, DeploymentID: "deployment-1", ExpiresAt: time.Now().Add(time.Minute),
			RequestedBuildExecutors: 1, RequestedCPUMillis: 3000, RequestedMemoryBytes: 4 << 30,
			RequestedWorkloadDiskBytes: 0, RequestedScratchBytes: 32 << 30,
		},
		deployment: testWorkerDeploymentBuild(),
	}
	executor := &detachedTestExecutor{}
	builder := &successfulTestBuilder{}
	runner, err := NewRunner(client, executor, capabilities, WithCapacity(testCapacity(t, capabilities)), WithDeploymentBuilder(builder))
	if err != nil {
		t.Fatal(err)
	}
	claimCtx, cancelClaim := context.WithCancel(context.Background())
	work, ok, err := NewBuildConsumer(runner).Claim(claimCtx)
	if err != nil || !ok || work == nil {
		t.Fatalf("claim = (%v, %v, %v)", work, ok, err)
	}
	cancelClaim()
	if got := client.buildStartCalls.Load(); got != 0 {
		t.Fatalf("start calls before registered work = %d", got)
	}
	if err := work(context.Background()); err != nil {
		t.Fatal(err)
	}
	if got := client.buildStartCalls.Load(); got != 1 {
		t.Fatalf("start calls = %d", got)
	}
	if got := builder.calls.Load(); got != 1 {
		t.Fatalf("builder calls = %d", got)
	}
}

func TestDefaultBuildEnvelopeFitsDefaultBuildWorker(t *testing.T) {
	capabilities := testCapabilities()
	capabilities.MaxBuildExecutors = 1
	capabilities.SupportsBuild = true
	capabilities.VMMaxScratchBytes = 20 << 30
	capabilities.ScratchBytes = 32 << 30
	client := &consumerTestClient{
		buildLease: api.WorkerDeploymentBuildLease{
			ID: "build-lease-1", WorkerEpoch: 1, DeploymentID: "deployment-1", ExpiresAt: time.Now().Add(time.Minute),
			RequestedBuildExecutors: 1, RequestedCPUMillis: 3000, RequestedMemoryBytes: 4 << 30,
			RequestedWorkloadDiskBytes: 0, RequestedScratchBytes: 32 << 30,
		},
		deployment: testWorkerDeploymentBuild(),
	}
	runner, err := NewRunner(
		client,
		&detachedTestExecutor{},
		capabilities,
		WithCapacity(testCapacity(t, capabilities)),
		WithDeploymentBuilder(&successfulTestBuilder{}),
	)
	if err != nil {
		t.Fatal(err)
	}
	work, ok, err := NewBuildConsumer(runner).Claim(context.Background())
	if err != nil || !ok || work == nil {
		t.Fatalf("default build claim = (%v, %v, %v)", work, ok, err)
	}
}

func TestBuildPreStartRejectionsReturnSuccessfulNilWork(t *testing.T) {
	validLease := api.WorkerDeploymentBuildLease{
		ID: "build-lease-1", WorkerEpoch: 1, DeploymentID: "deployment-1", ExpiresAt: time.Now().Add(time.Minute),
		RequestedBuildExecutors: 1, RequestedCPUMillis: 3000, RequestedMemoryBytes: 4 << 30,
		RequestedWorkloadDiskBytes: 0, RequestedScratchBytes: 32 << 30,
	}
	tests := []struct {
		name        string
		reason      string
		mutate      func(*api.WorkerCapabilities, *api.WorkerDeploymentBuildLease, *api.WorkerDeploymentBuild)
		withBuilder bool
	}{
		{
			name: "requirements unsupported", reason: "requirements_unsupported", withBuilder: true,
			mutate: func(capabilities *api.WorkerCapabilities, _ *api.WorkerDeploymentBuildLease, _ *api.WorkerDeploymentBuild) {
				capabilities.VMMilliCPU = 1999
			},
		},
		{
			name: "runtime architecture mismatch", reason: "requirements_unsupported", withBuilder: true,
			mutate: func(_ *api.WorkerCapabilities, _ *api.WorkerDeploymentBuildLease, deployment *api.WorkerDeploymentBuild) {
				deployment.Runtime.Architecture = "x86_64"
			},
		},
		{
			name: "malformed runtime descriptor", reason: "requirements_unsupported", withBuilder: true,
			mutate: func(_ *api.WorkerCapabilities, _ *api.WorkerDeploymentBuildLease, deployment *api.WorkerDeploymentBuild) {
				deployment.Runtime.SizeBytes = 0
			},
		},
		{name: "builder unavailable", reason: "builder_unavailable"},
		{
			name: "lease deadline too short", reason: "lease_deadline_too_short", withBuilder: true,
			mutate: func(_ *api.WorkerCapabilities, lease *api.WorkerDeploymentBuildLease, _ *api.WorkerDeploymentBuild) {
				lease.ExpiresAt = time.Now()
			},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			capabilities := testCapabilities()
			capabilities.MaxBuildExecutors = 1
			capabilities.SupportsBuild = true
			lease := validLease
			deployment := testWorkerDeploymentBuild()
			if tt.mutate != nil {
				tt.mutate(&capabilities, &lease, &deployment)
			}
			client := &consumerTestClient{
				buildLease: lease,
				deployment: deployment,
			}
			options := []Option{WithCapacity(testCapacity(t, capabilities))}
			if tt.withBuilder {
				options = append(options, WithDeploymentBuilder(&successfulTestBuilder{}))
			}
			runner, err := NewRunner(client, &detachedTestExecutor{}, capabilities, options...)
			if err != nil {
				t.Fatal(err)
			}
			work, ok, err := NewBuildConsumer(runner).Claim(context.Background())
			if err != nil || !ok || work != nil {
				t.Fatalf("rejected claim = (%v, %v, %v)", work, ok, err)
			}
			if got := client.rejectReason.Load(); got != tt.reason {
				t.Fatalf("rejection reason = %v, want %q", got, tt.reason)
			}
			if got := client.buildStartCalls.Load(); got != 0 {
				t.Fatalf("start calls = %d, want 0", got)
			}
		})
	}
}

func TestBuildAdmissionIncludesRuntimeOccupancy(t *testing.T) {
	capabilities := testCapabilities()
	capabilities.MaxVCPUs = 3
	capabilities.MaxMemoryMiB = 4096
	capabilities.MaxDiskMiB = 2048
	capabilities.VMMaxScratchBytes = 20 << 30
	capabilities.ScratchBytes = 34 << 30
	capabilities.MaxBuildExecutors = 1
	capabilities.SupportsBuild = true
	resources := testCapacity(t, capabilities)
	runtimeKey := capacity.Key{Kind: "runtime", Epoch: 1, ID: "runtime-1"}
	created, err := resources.Reserve(runtimeKey, capacity.Vector{
		CPUMillis: 1500, MemoryBytes: 1536 << 20,
		WorkloadDiskBytes: 1536 << 20, ScratchBytes: 1536 << 20, VMSlots: 1,
	})
	if err != nil || !created {
		t.Fatalf("reserve runtime = (%t, %v)", created, err)
	}
	client := &consumerTestClient{
		buildLease: api.WorkerDeploymentBuildLease{
			ID: "build-lease-1", WorkerEpoch: 1, DeploymentID: "deployment-1", ExpiresAt: time.Now().Add(time.Minute),
			RequestedBuildExecutors: 1, RequestedCPUMillis: 3000, RequestedMemoryBytes: 4 << 30,
			RequestedWorkloadDiskBytes: 0, RequestedScratchBytes: 32 << 30,
		},
		deployment: testWorkerDeploymentBuild(),
	}
	runner, err := NewRunner(client, &detachedTestExecutor{}, capabilities, WithCapacity(resources), WithDeploymentBuilder(&successfulTestBuilder{}))
	if err != nil {
		t.Fatal(err)
	}
	work, ok, err := NewBuildConsumer(runner).Claim(context.Background())
	if err != nil || !ok || work != nil {
		t.Fatalf("capacity-blocked claim = (%v, %v, %v)", work, ok, err)
	}
	if got := client.rejectReason.Load(); got != "local_capacity_exceeded" {
		t.Fatalf("rejection reason = %v", got)
	}
	if err := resources.Release(runtimeKey); err != nil {
		t.Fatal(err)
	}
	work, ok, err = NewBuildConsumer(runner).Claim(context.Background())
	if err != nil || !ok || work == nil {
		t.Fatalf("claim after runtime release = (%v, %v, %v)", work, ok, err)
	}
}

func TestVerifierInfrastructureFailureUsesDeliveryFailureBoundary(t *testing.T) {
	client := &consumerTestClient{}
	capabilities := testCapabilities()
	runner, err := NewRunner(client, &detachedTestExecutor{}, capabilities, WithCapacity(testCapacity(t, capabilities)), WithDeploymentBuilder(&deliveryFailureTestBuilder{}))
	if err != nil {
		t.Fatal(err)
	}
	lease := api.WorkerDeploymentBuildLease{DeploymentID: "deployment-1"}
	if err := runner.executeStartedBuild(context.Background(), lease, api.WorkerDeploymentBuild{}); err != nil {
		t.Fatal(err)
	}
	if got := client.buildDelivery.Load(); got != 1 {
		t.Fatalf("delivery failure reports = %d", got)
	}
	if got := client.buildComplete.Load(); got != 0 {
		t.Fatalf("completion calls = %d", got)
	}
	if got := client.deliveryReason.Load(); got != api.WorkerDeploymentBuildDeliveryProgramVerifierFailed {
		t.Fatalf("delivery reason = %v", got)
	}
}

func TestUnclassifiedBuildFailureWaitsForLeaseExpiry(t *testing.T) {
	client := &consumerTestClient{}
	capabilities := testCapabilities()
	runner, err := NewRunner(client, &detachedTestExecutor{}, capabilities, WithCapacity(testCapacity(t, capabilities)), WithDeploymentBuilder(&unclassifiedFailureTestBuilder{}))
	if err != nil {
		t.Fatal(err)
	}
	err = runner.executeStartedBuild(context.Background(), api.WorkerDeploymentBuildLease{DeploymentID: "deployment-1"}, api.WorkerDeploymentBuild{})
	if err == nil {
		t.Fatal("unclassified builder error must be returned")
	}
	if got := client.buildDelivery.Load(); got != 0 {
		t.Fatalf("delivery failure reports = %d", got)
	}
	if got := client.buildComplete.Load(); got != 0 {
		t.Fatalf("completion calls = %d", got)
	}
}

func TestBuildCleanupAmbiguityRetainsReservationAndTerminatesWorker(t *testing.T) {
	capabilities := testCapabilities()
	capabilities.MaxBuildExecutors = 1
	capabilities.SupportsBuild = true
	lease := api.WorkerDeploymentBuildLease{
		ID: "build-lease-1", WorkerEpoch: 1, DeploymentID: "deployment-1", ExpiresAt: time.Now().Add(time.Minute),
		RequestedBuildExecutors: 1, RequestedCPUMillis: 3000, RequestedMemoryBytes: 4 << 30,
		RequestedWorkloadDiskBytes: 0, RequestedScratchBytes: 32 << 30,
	}
	client := &consumerTestClient{
		buildLease: lease,
		deployment: testWorkerDeploymentBuild(),
	}
	resources := testCapacity(t, capabilities)
	builder := &cleanupUnprovenTestBuilder{
		owner: vm.Owner{Kind: vm.OwnerBuild, ID: "00000000-0000-0000-0000-000000000701"},
	}
	runner, err := NewRunner(
		client,
		&detachedTestExecutor{},
		capabilities,
		WithCapacity(resources),
		WithDeploymentBuilder(builder),
	)
	if err != nil {
		t.Fatal(err)
	}
	work, ok, err := NewBuildConsumer(runner).Claim(context.Background())
	if err != nil || !ok || work == nil {
		t.Fatalf("claim = (%v, %v, %v)", work, ok, err)
	}
	err = work(context.Background())
	var fatal FatalWorkError
	if !errors.As(err, &fatal) || !fatal.FatalWorker() {
		t.Fatalf("work error = %v, want fatal worker error", err)
	}
	key := capacity.Key{Kind: "build", Epoch: lease.WorkerEpoch, ID: lease.ID}
	if _, ok := resources.Snapshot().Reservations[key]; !ok {
		t.Fatal("build reservation was released without exact cleanup proof")
	}
	if got := client.buildDelivery.Load(); got != 0 {
		t.Fatalf("delivery failure reports = %d, want 0", got)
	}
	if got := client.buildComplete.Load(); got != 0 {
		t.Fatalf("completion calls = %d, want 0", got)
	}
}

func TestBuildServiceFailureRetainsReservationAndTerminatesWorker(t *testing.T) {
	capabilities := testCapabilities()
	capabilities.MaxBuildExecutors = 1
	capabilities.SupportsBuild = true
	lease := api.WorkerDeploymentBuildLease{
		ID: "build-lease-1", WorkerEpoch: 1, DeploymentID: "deployment-1", ExpiresAt: time.Now().Add(time.Minute),
		RequestedBuildExecutors: 1, RequestedCPUMillis: 3000, RequestedMemoryBytes: 4 << 30,
		RequestedWorkloadDiskBytes: 0, RequestedScratchBytes: 32 << 30,
	}
	client := &consumerTestClient{
		buildLease: lease,
		deployment: testWorkerDeploymentBuild(),
	}
	resources := testCapacity(t, capabilities)
	runner, err := NewRunner(
		client,
		&detachedTestExecutor{},
		capabilities,
		WithCapacity(resources),
		WithDeploymentBuilder(&fatalBuildTestBuilder{}),
	)
	if err != nil {
		t.Fatal(err)
	}
	work, ok, err := NewBuildConsumer(runner).Claim(context.Background())
	if err != nil || !ok || work == nil {
		t.Fatalf("claim = (%v, %v, %v)", work, ok, err)
	}
	err = work(context.Background())
	var fatal FatalWorkError
	if !errors.As(err, &fatal) || !fatal.FatalWorker() {
		t.Fatalf("work error = %v, want fatal worker error", err)
	}
	key := capacity.Key{Kind: "build", Epoch: lease.WorkerEpoch, ID: lease.ID}
	if _, ok := resources.Snapshot().Reservations[key]; !ok {
		t.Fatal("build reservation was released after build service failure")
	}
	if got := client.buildDelivery.Load(); got != 0 {
		t.Fatalf("delivery failure reports = %d, want 0", got)
	}
}

func TestCanceledBuildReleasesReservation(t *testing.T) {
	capabilities := testCapabilities()
	capabilities.MaxBuildExecutors = 1
	capabilities.SupportsBuild = true
	lease := api.WorkerDeploymentBuildLease{
		ID: "build-lease-1", WorkerEpoch: 1, DeploymentID: "deployment-1", ExpiresAt: time.Now().Add(time.Minute),
		RequestedBuildExecutors: 1, RequestedCPUMillis: 3000, RequestedMemoryBytes: 4 << 30,
		RequestedWorkloadDiskBytes: 0, RequestedScratchBytes: 32 << 30,
	}
	client := &consumerTestClient{
		buildLease: lease,
		deployment: testWorkerDeploymentBuild(),
	}
	resources := testCapacity(t, capabilities)
	runner, err := NewRunner(
		client,
		&detachedTestExecutor{},
		capabilities,
		WithCapacity(resources),
		WithDeploymentBuilder(&canceledBuildTestBuilder{}),
	)
	if err != nil {
		t.Fatal(err)
	}
	work, ok, err := NewBuildConsumer(runner).Claim(context.Background())
	if err != nil || !ok || work == nil {
		t.Fatalf("claim = (%v, %v, %v)", work, ok, err)
	}
	if err := work(context.Background()); !errors.Is(err, context.Canceled) {
		t.Fatalf("work error = %v, want context canceled", err)
	}
	key := capacity.Key{Kind: "build", Epoch: lease.WorkerEpoch, ID: lease.ID}
	if _, ok := resources.Snapshot().Reservations[key]; ok {
		t.Fatal("canceled build reservation was retained")
	}
}
