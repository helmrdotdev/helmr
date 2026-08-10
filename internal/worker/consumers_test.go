package worker

import (
	"context"
	"encoding/json"
	"errors"
	"sync/atomic"
	"testing"
	"time"

	"github.com/helmrdotdev/helmr/internal/capacity"
	"github.com/helmrdotdev/helmr/internal/deployment"
	"github.com/helmrdotdev/helmr/internal/vm"
	"github.com/helmrdotdev/helmr/internal/workerapi"
)

func testWorkerDeploymentBuild() workerapi.DeploymentBuild {
	return workerapi.DeploymentBuild{
		ID:             "deployment-1",
		NodeVersion:    "24.16.0",
		BuildContract:  deployment.ProgramBuildContract,
		ImageCacheMode: "prefer",
		Runtime: workerapi.CASObject{
			Digest:    "sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa",
			MediaType: deployment.RuntimeArtifactMediaType,
			SizeBytes: 1,
		},
		Manager: workerapi.ManagerPin{
			Name:    "npm",
			Version: "11.5.0",
			Artifact: workerapi.CASObject{
				Digest:    "sha256:bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb",
				MediaType: deployment.ManagerTreeMediaType,
				SizeBytes: 1,
			},
		},
		Toolchain: workerapi.CASObject{
			Digest:    "sha256:cccccccccccccccccccccccccccccccccccccccccccccccccccccccccccccccc",
			MediaType: deployment.ToolchainMediaType,
			SizeBytes: 1,
		},
	}
}

type consumerBuildPolicy struct{}

func (consumerBuildPolicy) Digest() (string, error) {
	return "sha256:cccccccccccccccccccccccccccccccccccccccccccccccccccccccccccccccc", nil
}

func (consumerBuildPolicy) Node(string) (deployment.VersionDomain, []string, error) {
	return deployment.VersionDomain{Major: 24, Minimum: "24.3.0"},
		[]string{deployment.NodeNoStripTypes},
		nil
}

func (consumerBuildPolicy) Manager(
	deployment.PackageManager,
) (deployment.ManagerPolicy, error) {
	return deployment.ManagerPolicy{}, nil
}

func (consumerBuildPolicy) DeniesDigest(string) bool   { return false }
func (consumerBuildPolicy) DeniesSelector(string) bool { return false }

type consumerPlatformAcquirer struct{}

func (consumerPlatformAcquirer) Acquire(
	context.Context,
	workerapi.PlatformAcquisition,
) (workerapi.PlatformAcquisitionCandidates, error) {
	return workerapi.PlatformAcquisitionCandidates{}, errors.New("unexpected platform acquisition")
}

type consumerTestClient struct {
	ControlPlaneClient
	runWork         []workerapi.RunLeaseWork
	buildLease      workerapi.DeploymentBuildLease
	deployment      workerapi.DeploymentBuild
	discoveryCalls  atomic.Int32
	buildStartCalls atomic.Int32
	buildComplete   atomic.Int32
	buildDelivery   atomic.Int32
	buildReject     atomic.Int32
	deliveryReason  atomic.Value
	rejectReason    atomic.Value
}

func (c *consumerTestClient) DiscoverRunLeases(context.Context) (workerapi.RunLeaseDiscoveryResponse, error) {
	c.discoveryCalls.Add(1)
	return workerapi.RunLeaseDiscoveryResponse{Items: c.runWork}, nil
}

func (c *consumerTestClient) LeaseDeploymentBuild(context.Context) (workerapi.DeploymentBuildLeaseResponse, error) {
	return workerapi.DeploymentBuildLeaseResponse{Lease: &c.buildLease, Deployment: &c.deployment}, nil
}

func (c *consumerTestClient) StartDeploymentBuild(context.Context, workerapi.DeploymentBuildLease) (workerapi.DeploymentBuildStartResponse, error) {
	c.buildStartCalls.Add(1)
	return workerapi.DeploymentBuildStartResponse{Lease: c.buildLease}, nil
}

func (c *consumerTestClient) CompleteDeploymentBuild(context.Context, workerapi.DeploymentBuildLease, json.RawMessage) (workerapi.DeploymentBuildResponse, error) {
	c.buildComplete.Add(1)
	return workerapi.DeploymentBuildResponse{Status: "deployed"}, nil
}

func (c *consumerTestClient) ReportDeploymentBuildDeliveryFailure(_ context.Context, request workerapi.DeploymentBuildDeliveryFailureRequest) (workerapi.DeploymentBuildResponse, error) {
	c.buildDelivery.Add(1)
	c.deliveryReason.Store(request.ReasonCode)
	return workerapi.DeploymentBuildResponse{Status: "building"}, nil
}

func (c *consumerTestClient) RejectDeploymentBuild(_ context.Context, request workerapi.DeploymentBuildRejectRequest) error {
	c.buildReject.Add(1)
	c.rejectReason.Store(request.ReasonCode)
	return nil
}

type detachedTestExecutor struct{ calls atomic.Int32 }

func (e *detachedTestExecutor) ExecuteRunLease(context.Context, workerapi.RunLeaseWork) error {
	e.calls.Add(1)
	return nil
}

type successfulTestBuilder struct{ calls atomic.Int32 }

func (b *successfulTestBuilder) Build(context.Context, workerapi.DeploymentBuildLease, workerapi.DeploymentBuild, deployment.ImageOperationRevocations) (json.RawMessage, error) {
	b.calls.Add(1)
	return json.RawMessage(`{"error":{"message":"test","reasonCode":"verification_failed"},"formatVersion":0,"outcome":"failed"}`), nil
}

type deliveryFailureTestBuilder struct{}

func (*deliveryFailureTestBuilder) Build(context.Context, workerapi.DeploymentBuildLease, workerapi.DeploymentBuild, deployment.ImageOperationRevocations) (json.RawMessage, error) {
	return nil, deliveryFailureTestError{}
}

type deliveryFailureTestError struct{}

func (deliveryFailureTestError) Error() string {
	return "verifier infrastructure failed"
}

func (deliveryFailureTestError) DeploymentBuildDeliveryFailureReason() workerapi.DeploymentBuildDeliveryFailureReason {
	return workerapi.DeploymentBuildDeliveryProgramVerifierFailed
}

type unclassifiedFailureTestBuilder struct{}

func (*unclassifiedFailureTestBuilder) Build(context.Context, workerapi.DeploymentBuildLease, workerapi.DeploymentBuild, deployment.ImageOperationRevocations) (json.RawMessage, error) {
	return nil, errors.New("unclassified infrastructure failure")
}

type cleanupUnprovenTestBuilder struct {
	owner vm.Owner
}

func (b *cleanupUnprovenTestBuilder) Build(context.Context, workerapi.DeploymentBuildLease, workerapi.DeploymentBuild, deployment.ImageOperationRevocations) (json.RawMessage, error) {
	return nil, vm.NewGuestError(&vm.CleanupUnprovenError{
		Owner: b.owner,
		Cause: errors.New("guest process absence could not be proven"),
	})
}

type fatalBuildTestBuilder struct{}

func (*fatalBuildTestBuilder) Build(context.Context, workerapi.DeploymentBuildLease, workerapi.DeploymentBuild, deployment.ImageOperationRevocations) (json.RawMessage, error) {
	return nil, fatalBuildTestError{}
}

type fatalBuildTestError struct{}

func (fatalBuildTestError) Error() string     { return "build service failed" }
func (fatalBuildTestError) FatalWorker() bool { return true }

type canceledBuildTestBuilder struct{}

func (*canceledBuildTestBuilder) Build(context.Context, workerapi.DeploymentBuildLease, workerapi.DeploymentBuild, deployment.ImageOperationRevocations) (json.RawMessage, error) {
	return nil, context.Canceled
}

func TestRunConsumerExecutesDiscoveredLeaseInsideRegisteredWork(t *testing.T) {
	capabilities := testCapabilities()
	client := &consumerTestClient{
		runWork: []workerapi.RunLeaseWork{{LeaseID: "lease-1", LeaseSequence: 1}},
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
	if got := executor.calls.Load(); got != 0 {
		t.Fatalf("executor calls before registered work = %d", got)
	}
	if err := work(context.Background()); err != nil {
		t.Fatal(err)
	}
	if got := executor.calls.Load(); got != 1 {
		t.Fatalf("executor calls = %d", got)
	}
}

func TestRunConsumerDoesNotRediscoverActiveTuple(t *testing.T) {
	capabilities := testCapabilities()
	client := &consumerTestClient{
		runWork: []workerapi.RunLeaseWork{{LeaseID: "lease-1", LeaseSequence: 1}},
	}
	executor := &detachedTestExecutor{}
	runner, err := NewRunner(client, executor, capabilities, WithCapacity(testCapacity(t, capabilities)))
	if err != nil {
		t.Fatal(err)
	}
	consumer := NewRunConsumer(runner)
	first, ok, err := consumer.Claim(context.Background())
	if err != nil || !ok || first == nil {
		t.Fatalf("first claim = (%v, %v, %v)", first, ok, err)
	}
	second, ok, err := consumer.Claim(context.Background())
	if err != nil || ok || second != nil {
		t.Fatalf("duplicate claim = (%v, %v, %v)", second, ok, err)
	}
	if err := first(context.Background()); err != nil {
		t.Fatal(err)
	}
	third, ok, err := consumer.Claim(context.Background())
	if err != nil || !ok || third == nil {
		t.Fatalf("claim after completion = (%v, %v, %v)", third, ok, err)
	}
}

func TestBuildConsumerStartsLeaseInsideRegisteredWork(t *testing.T) {
	capabilities := testCapabilities()
	capabilities.MaxBuildExecutors = 1
	capabilities.SupportsBuild = true
	client := &consumerTestClient{
		buildLease: workerapi.DeploymentBuildLease{
			ID: "build-lease-1", WorkerEpoch: 1, DeploymentID: "deployment-1", ExpiresAt: time.Now().Add(time.Minute),
			RequestedBuildExecutors: 1, RequestedCPUMillis: 3000, RequestedMemoryBytes: 4 << 30,
			RequestedGuestEphemeralDiskBytes: 32 << 30,
		},
		deployment: testWorkerDeploymentBuild(),
	}
	executor := &detachedTestExecutor{}
	builder := &successfulTestBuilder{}
	runner, err := NewRunner(
		client,
		executor,
		capabilities,
		WithCapacity(testCapacity(t, capabilities)),
		WithBuildPolicy(consumerBuildPolicy{}),
		WithPlatformAcquirer(consumerPlatformAcquirer{}),
		WithBuildExecutor(builder),
	)
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
	capabilities.VMGuestEphemeralDiskBytes = 32 << 30
	capabilities.GuestEphemeralDiskBytes = 32 << 30
	client := &consumerTestClient{
		buildLease: workerapi.DeploymentBuildLease{
			ID: "build-lease-1", WorkerEpoch: 1, DeploymentID: "deployment-1", ExpiresAt: time.Now().Add(time.Minute),
			RequestedBuildExecutors: 1, RequestedCPUMillis: 3000, RequestedMemoryBytes: 4 << 30,
			RequestedGuestEphemeralDiskBytes: 32 << 30,
		},
		deployment: testWorkerDeploymentBuild(),
	}
	runner, err := NewRunner(
		client,
		&detachedTestExecutor{},
		capabilities,
		WithCapacity(testCapacity(t, capabilities)),
		WithBuildPolicy(consumerBuildPolicy{}),
		WithPlatformAcquirer(consumerPlatformAcquirer{}),
		WithBuildExecutor(&successfulTestBuilder{}),
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
	validLease := workerapi.DeploymentBuildLease{
		ID: "build-lease-1", WorkerEpoch: 1, DeploymentID: "deployment-1", ExpiresAt: time.Now().Add(time.Minute),
		RequestedBuildExecutors: 1, RequestedCPUMillis: 3000, RequestedMemoryBytes: 4 << 30,
		RequestedGuestEphemeralDiskBytes: 32 << 30,
	}
	tests := []struct {
		name        string
		reason      string
		mutate      func(*workerapi.Capabilities, *workerapi.DeploymentBuildLease, *workerapi.DeploymentBuild)
		withBuilder bool
	}{
		{
			name: "requirements unsupported", reason: "requirements_unsupported", withBuilder: true,
			mutate: func(capabilities *workerapi.Capabilities, _ *workerapi.DeploymentBuildLease, _ *workerapi.DeploymentBuild) {
				capabilities.VMMilliCPU = 1999
			},
		},
		{
			name: "runtime architecture mismatch", reason: "requirements_unsupported", withBuilder: true,
			mutate: func(capabilities *workerapi.Capabilities, _ *workerapi.DeploymentBuildLease, _ *workerapi.DeploymentBuild) {
				capabilities.Runtime.Arch = "aarch64"
			},
		},
		{
			name: "malformed runtime descriptor", reason: "requirements_unsupported", withBuilder: true,
			mutate: func(_ *workerapi.Capabilities, _ *workerapi.DeploymentBuildLease, deployment *workerapi.DeploymentBuild) {
				deployment.Runtime.SizeBytes = 0
			},
		},
		{
			name: "unregistered toolchain", reason: "requirements_unsupported", withBuilder: true,
			mutate: func(_ *workerapi.Capabilities, _ *workerapi.DeploymentBuildLease, deployment *workerapi.DeploymentBuild) {
				deployment.Toolchain.Digest = "invalid"
			},
		},
		{name: "builder unavailable", reason: "builder_unavailable"},
		{
			name: "lease deadline too short", reason: "lease_deadline_too_short", withBuilder: true,
			mutate: func(_ *workerapi.Capabilities, lease *workerapi.DeploymentBuildLease, _ *workerapi.DeploymentBuild) {
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
			options := []Option{
				WithCapacity(testCapacity(t, capabilities)),
				WithBuildPolicy(consumerBuildPolicy{}),
				WithPlatformAcquirer(consumerPlatformAcquirer{}),
			}
			if tt.withBuilder {
				options = append(options, WithBuildExecutor(&successfulTestBuilder{}))
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
	capabilities.VMGuestEphemeralDiskBytes = 32 << 30
	capabilities.GuestEphemeralDiskBytes = 34 << 30
	capabilities.MaxBuildExecutors = 1
	capabilities.SupportsBuild = true
	resources := testCapacity(t, capabilities)
	runtimeKey := capacity.Key{Kind: "runtime", Epoch: 1, ID: "runtime-1"}
	created, err := resources.Reserve(runtimeKey, capacity.Vector{
		CPUMillis: 1500, MemoryBytes: 1536 << 20,
		GuestEphemeralDiskBytes: 1536 << 20, VMSlots: 1,
	})
	if err != nil || !created {
		t.Fatalf("reserve runtime = (%t, %v)", created, err)
	}
	client := &consumerTestClient{
		buildLease: workerapi.DeploymentBuildLease{
			ID: "build-lease-1", WorkerEpoch: 1, DeploymentID: "deployment-1", ExpiresAt: time.Now().Add(time.Minute),
			RequestedBuildExecutors: 1, RequestedCPUMillis: 3000, RequestedMemoryBytes: 4 << 30,
			RequestedGuestEphemeralDiskBytes: 32 << 30,
		},
		deployment: testWorkerDeploymentBuild(),
	}
	runner, err := NewRunner(
		client,
		&detachedTestExecutor{},
		capabilities,
		WithCapacity(resources),
		WithBuildPolicy(consumerBuildPolicy{}),
		WithPlatformAcquirer(consumerPlatformAcquirer{}),
		WithBuildExecutor(&successfulTestBuilder{}),
	)
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
	runner, err := NewRunner(client, &detachedTestExecutor{}, capabilities, WithCapacity(testCapacity(t, capabilities)), WithBuildExecutor(&deliveryFailureTestBuilder{}))
	if err != nil {
		t.Fatal(err)
	}
	lease := workerapi.DeploymentBuildLease{DeploymentID: "deployment-1"}
	if err := runner.executeStartedBuild(context.Background(), lease, workerapi.DeploymentBuild{}); err != nil {
		t.Fatal(err)
	}
	if got := client.buildDelivery.Load(); got != 1 {
		t.Fatalf("delivery failure reports = %d", got)
	}
	if got := client.buildComplete.Load(); got != 0 {
		t.Fatalf("completion calls = %d", got)
	}
	if got := client.deliveryReason.Load(); got != workerapi.DeploymentBuildDeliveryProgramVerifierFailed {
		t.Fatalf("delivery reason = %v", got)
	}
}

func TestUnclassifiedBuildFailureWaitsForLeaseExpiry(t *testing.T) {
	client := &consumerTestClient{}
	capabilities := testCapabilities()
	runner, err := NewRunner(client, &detachedTestExecutor{}, capabilities, WithCapacity(testCapacity(t, capabilities)), WithBuildExecutor(&unclassifiedFailureTestBuilder{}))
	if err != nil {
		t.Fatal(err)
	}
	err = runner.executeStartedBuild(context.Background(), workerapi.DeploymentBuildLease{DeploymentID: "deployment-1"}, workerapi.DeploymentBuild{})
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
	lease := workerapi.DeploymentBuildLease{
		ID: "build-lease-1", WorkerEpoch: 1, DeploymentID: "deployment-1", ExpiresAt: time.Now().Add(time.Minute),
		RequestedBuildExecutors: 1, RequestedCPUMillis: 3000, RequestedMemoryBytes: 4 << 30,
		RequestedGuestEphemeralDiskBytes: 32 << 30,
	}
	client := &consumerTestClient{
		buildLease: lease,
		deployment: testWorkerDeploymentBuild(),
	}
	resources := testCapacity(t, capabilities)
	builder := &cleanupUnprovenTestBuilder{
		owner: vm.Owner{Kind: vm.OwnerBuild, ID: "019c10d5-a6f7-7af1-8f5f-000000000701"},
	}
	runner, err := NewRunner(
		client,
		&detachedTestExecutor{},
		capabilities,
		WithCapacity(resources),
		WithBuildPolicy(consumerBuildPolicy{}),
		WithPlatformAcquirer(consumerPlatformAcquirer{}),
		WithBuildExecutor(builder),
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
	lease := workerapi.DeploymentBuildLease{
		ID: "build-lease-1", WorkerEpoch: 1, DeploymentID: "deployment-1", ExpiresAt: time.Now().Add(time.Minute),
		RequestedBuildExecutors: 1, RequestedCPUMillis: 3000, RequestedMemoryBytes: 4 << 30,
		RequestedGuestEphemeralDiskBytes: 32 << 30,
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
		WithBuildPolicy(consumerBuildPolicy{}),
		WithPlatformAcquirer(consumerPlatformAcquirer{}),
		WithBuildExecutor(&fatalBuildTestBuilder{}),
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
	lease := workerapi.DeploymentBuildLease{
		ID: "build-lease-1", WorkerEpoch: 1, DeploymentID: "deployment-1", ExpiresAt: time.Now().Add(time.Minute),
		RequestedBuildExecutors: 1, RequestedCPUMillis: 3000, RequestedMemoryBytes: 4 << 30,
		RequestedGuestEphemeralDiskBytes: 32 << 30,
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
		WithBuildPolicy(consumerBuildPolicy{}),
		WithPlatformAcquirer(consumerPlatformAcquirer{}),
		WithBuildExecutor(&canceledBuildTestBuilder{}),
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
