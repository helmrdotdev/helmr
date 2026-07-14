package worker

import (
	"context"
	"sync/atomic"
	"testing"
	"time"

	"github.com/helmrdotdev/helmr/internal/api"
)

type consumerTestClient struct {
	ControlClient
	runLease        api.WorkerRunLease
	run             api.WorkerRun
	buildLease      api.WorkerDeploymentBuildLease
	deployment      api.WorkerDeploymentBuild
	runStartCalls   atomic.Int32
	buildStartCalls atomic.Int32
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
	return api.WorkerDeploymentBuildResponse{Status: "deployed"}, nil
}

type detachedTestExecutor struct{ calls atomic.Int32 }

func (e *detachedTestExecutor) Execute(context.Context, api.WorkerRunLeaseProvider, api.WorkerRun) api.WorkerReleaseResult {
	e.calls.Add(1)
	return api.WorkerReleaseResult{Kind: "detached"}
}

type successfulTestBuilder struct{ calls atomic.Int32 }

func (b *successfulTestBuilder) BuildDeployment(context.Context, api.WorkerDeploymentBuildLease, api.WorkerDeploymentBuild) api.WorkerDeploymentBuildResult {
	b.calls.Add(1)
	return api.WorkerDeploymentBuildResult{}
}

func TestRunConsumerStartsLeaseInsideRegisteredWork(t *testing.T) {
	capabilities := testCapabilities()
	client := &consumerTestClient{
		runLease: api.WorkerRunLease{RunID: "run-1", ProtocolVersion: capabilities.ProtocolVersion},
		run:      api.WorkerRun{WorkerProtocolVersion: capabilities.ProtocolVersion, Requirements: testRequirements()},
	}
	executor := &detachedTestExecutor{}
	runner, err := NewRunner(client, executor, capabilities)
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
	client := &consumerTestClient{
		buildLease: api.WorkerDeploymentBuildLease{
			DeploymentID: "deployment-1", ExpiresAt: time.Now().Add(time.Minute),
			RequestedBuildExecutors: 1, RequestedCPUMillis: 1000, RequestedMemoryBytes: 512 << 20,
			RequestedWorkloadDiskBytes: 1024 << 20, RequestedScratchBytes: 1024 << 20,
		},
		deployment: api.WorkerDeploymentBuild{ID: "deployment-1"},
	}
	executor := &detachedTestExecutor{}
	builder := &successfulTestBuilder{}
	runner, err := NewRunner(client, executor, capabilities, WithDeploymentBuilder(builder))
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
