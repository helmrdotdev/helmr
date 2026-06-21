package worker

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"strings"
	"testing"
	"time"

	"github.com/helmrdotdev/helmr/internal/api"
	"github.com/helmrdotdev/helmr/internal/client"
	"github.com/helmrdotdev/helmr/internal/compute"
)

type executorFunc func(ctx context.Context, leases api.WorkerRunLeaseProvider, run api.WorkerRun) api.WorkerReleaseResult

func (f executorFunc) Execute(ctx context.Context, leases api.WorkerRunLeaseProvider, run api.WorkerRun) api.WorkerReleaseResult {
	return f(ctx, leases, run)
}

type deploymentBuilderFunc func(ctx context.Context, lease api.WorkerDeploymentBuildLease, deployment api.WorkerDeploymentBuild) api.WorkerDeploymentBuildResult

func (f deploymentBuilderFunc) BuildDeployment(ctx context.Context, lease api.WorkerDeploymentBuildLease, deployment api.WorkerDeploymentBuild) api.WorkerDeploymentBuildResult {
	return f(ctx, lease, deployment)
}

type materializerFunc func(ctx context.Context, materialization api.WorkerWorkspaceMaterialization, client api.WorkerWorkspaceMaterializerControlClient) error

func (f materializerFunc) RunWorkspaceMaterialization(ctx context.Context, materialization api.WorkerWorkspaceMaterialization, client api.WorkerWorkspaceMaterializerControlClient) error {
	return f(ctx, materialization, client)
}

func TestRunOnceNoClaim(t *testing.T) {
	client := &fakeClient{}
	runner := newTestRunner(t, client, executorFunc(func(context.Context, api.WorkerRunLeaseProvider, api.WorkerRun) api.WorkerReleaseResult {
		t.Fatal("executor should not run")
		return api.WorkerReleaseResult{}
	}))

	worked, err := runner.RunOnce(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if worked {
		t.Fatal("expected no work")
	}
}

func TestRunOnceStartsWorkspaceMaterializationWithoutRunLease(t *testing.T) {
	called := make(chan api.WorkerWorkspaceMaterialization, 1)
	client := &fakeClient{
		materializationClaim: api.WorkerWorkspaceMaterializationClaimResponse{
			Materialization: &api.WorkerWorkspaceMaterialization{
				ID:                "mat-1",
				OrgID:             "org-1",
				ProjectID:         "project-1",
				EnvironmentID:     "env-1",
				WorkspaceID:       "workspace-1",
				ReservationToken:  "reservation-token",
				FencingGeneration: 1,
				ExpiresAt:         time.Now().Add(time.Minute),
			},
		},
	}
	runner := newTestRunner(t, client, executorFunc(func(context.Context, api.WorkerRunLeaseProvider, api.WorkerRun) api.WorkerReleaseResult {
		t.Fatal("executor should not run for materialization-only work")
		return api.WorkerReleaseResult{}
	}))
	runner.materializer = materializerFunc(func(_ context.Context, materialization api.WorkerWorkspaceMaterialization, _ api.WorkerWorkspaceMaterializerControlClient) error {
		called <- materialization
		return nil
	})
	worked, err := runner.RunOnce(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if !worked {
		t.Fatal("RunOnce worked = false")
	}
	select {
	case materialization := <-called:
		if materialization.ID != "mat-1" {
			t.Fatalf("materialization id = %q", materialization.ID)
		}
	case <-time.After(time.Second):
		t.Fatal("materializer was not called")
	}
	if client.materializationClaims != 1 {
		t.Fatalf("materialization claims = %d", client.materializationClaims)
	}
}

func TestRunOnceClaimsWorkspaceMaterializationsWhileAnotherIsActive(t *testing.T) {
	block := make(chan struct{})
	started := make(chan string, 2)
	client := &fakeClient{
		materializationClaimQueue: []api.WorkerWorkspaceMaterializationClaimResponse{
			{
				Materialization: &api.WorkerWorkspaceMaterialization{
					ID:                "mat-1",
					OrgID:             "org-1",
					WorkspaceID:       "workspace-1",
					ReservationToken:  "reservation-token-1",
					FencingGeneration: 1,
					ExpiresAt:         time.Now().Add(time.Minute),
				},
			},
			{
				Materialization: &api.WorkerWorkspaceMaterialization{
					ID:                "mat-2",
					OrgID:             "org-1",
					WorkspaceID:       "workspace-2",
					ReservationToken:  "reservation-token-2",
					FencingGeneration: 1,
					ExpiresAt:         time.Now().Add(time.Minute),
				},
			},
		},
	}
	runner := newTestRunner(t, client, executorFunc(func(context.Context, api.WorkerRunLeaseProvider, api.WorkerRun) api.WorkerReleaseResult {
		t.Fatal("executor should not run")
		return api.WorkerReleaseResult{}
	}))
	runner.materializer = materializerFunc(func(_ context.Context, materialization api.WorkerWorkspaceMaterialization, _ api.WorkerWorkspaceMaterializerControlClient) error {
		started <- materialization.ID
		<-block
		return nil
	})

	worked, err := runner.RunOnce(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if !worked {
		t.Fatal("first RunOnce worked = false")
	}
	select {
	case id := <-started:
		if id != "mat-1" {
			t.Fatalf("first materialization id = %q", id)
		}
	case <-time.After(time.Second):
		t.Fatal("materializer did not start")
	}
	worked, err = runner.RunOnce(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if !worked {
		t.Fatal("second RunOnce worked = false")
	}
	select {
	case id := <-started:
		if id != "mat-2" {
			t.Fatalf("second materialization id = %q", id)
		}
	case <-time.After(time.Second):
		t.Fatal("second materializer did not start")
	}
	if client.materializationClaims != 2 {
		t.Fatalf("materialization claims = %d, want 2", client.materializationClaims)
	}
	close(block)
	runner.waitForMaterializations()
}

func TestRunWaitsForWorkspaceMaterializationOnShutdown(t *testing.T) {
	started := make(chan struct{}, 1)
	release := make(chan struct{})
	client := &fakeClient{
		materializationClaimQueue: []api.WorkerWorkspaceMaterializationClaimResponse{
			{
				Materialization: &api.WorkerWorkspaceMaterialization{
					ID:                "mat-1",
					OrgID:             "org-1",
					WorkspaceID:       "workspace-1",
					ReservationToken:  "reservation-token",
					FencingGeneration: 1,
					ExpiresAt:         time.Now().Add(time.Minute),
				},
			},
		},
	}
	runner := newTestRunner(t, client, executorFunc(func(context.Context, api.WorkerRunLeaseProvider, api.WorkerRun) api.WorkerReleaseResult {
		t.Fatal("executor should not run")
		return api.WorkerReleaseResult{}
	}))
	runner.materializer = materializerFunc(func(ctx context.Context, _ api.WorkerWorkspaceMaterialization, _ api.WorkerWorkspaceMaterializerControlClient) error {
		started <- struct{}{}
		<-ctx.Done()
		<-release
		return nil
	})

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- runner.Run(ctx) }()
	select {
	case <-started:
	case <-time.After(time.Second):
		t.Fatal("materializer did not start")
	}
	cancel()
	select {
	case err := <-done:
		t.Fatalf("runner returned before materializer stopped: %v", err)
	case <-time.After(50 * time.Millisecond):
	}
	close(release)
	select {
	case err := <-done:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("runner err = %v, want context canceled", err)
		}
	case <-time.After(time.Second):
		t.Fatal("runner did not return after materializer stopped")
	}
}

func TestRunOnceStartsExecutesRenewsAndReleases(t *testing.T) {
	claim := api.WorkerRunLease{
		ID:                "00000000-0000-0000-0000-000000000001",
		RunID:             "00000000-0000-0000-0000-000000000002",
		WorkerInstanceID:  "worker-1",
		AttemptNumber:     1,
		DispatchMessageID: "message-1",
		DispatchLeaseID:   "lease-1",
		ExpiresAt:         time.Now().Add(time.Minute),
	}
	client := &fakeClient{
		claimResponse: api.WorkerRunLeaseResponse{
			Lease: &claim,
			Run:   &api.WorkerRun{ID: claim.RunID, RunLeaseID: claim.ID, TaskID: "deploy"},
		},
		renewResponse: api.WorkerRenewResponse{Lease: claim},
	}
	executed := false
	releaseResult := api.WorkerReleaseResult{}
	renewed := make(chan struct{}, 1)
	executor := executorFunc(func(ctx context.Context, leases api.WorkerRunLeaseProvider, run api.WorkerRun) api.WorkerReleaseResult {
		got := leases.CurrentWorkerRunLease()
		if got.ID != claim.ID || run.TaskID != "deploy" {
			t.Fatalf("unexpected execution input claim=%+v run=%+v", got, run)
		}
		executed = true
		timer := time.NewTimer(time.Second)
		defer timer.Stop()
		select {
		case <-renewed:
		case <-timer.C:
			return api.WorkerReleaseResult{Kind: "failed", Error: new("timed out waiting for lease renewal")}
		}
		select {
		case <-ctx.Done():
			t.Fatal("context cancelled before executor returned")
		default:
		}
		exitCode := int32(0)
		return api.WorkerReleaseResult{Kind: "completed", ExitCode: &exitCode}
	})
	runner := newTestRunner(t, client, executor)
	runner.renewEvery = time.Millisecond
	client.renewed = renewed

	worked, err := runner.RunOnce(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if !worked || !executed {
		t.Fatalf("worked=%v executed=%v", worked, executed)
	}
	if client.starts != 1 || client.renews == 0 || client.releases != 1 {
		t.Fatalf("starts=%d renews=%d releases=%d", client.starts, client.renews, client.releases)
	}
	releaseResult = client.releaseResult
	if releaseResult.Kind != "completed" {
		t.Fatalf("release result = %+v", releaseResult)
	}
}

func TestRunOnceUsesRenewedLeaseForExecutionContextAndRelease(t *testing.T) {
	claim := api.WorkerRunLease{
		ID:                "00000000-0000-0000-0000-000000000001",
		RunID:             "00000000-0000-0000-0000-000000000002",
		WorkerInstanceID:  "worker-1",
		AttemptNumber:     1,
		DispatchMessageID: "message-1",
		DispatchLeaseID:   "lease-1",
		ExpiresAt:         time.Now().Add(time.Minute),
	}
	renewedClaim := claim
	renewedClaim.DispatchLeaseID = "lease-2"
	renewedClaim.ExpiresAt = time.Now().Add(2 * time.Minute)
	renewed := make(chan struct{}, 1)
	client := &fakeClient{
		claimResponse: api.WorkerRunLeaseResponse{
			Lease: &claim,
			Run:   &api.WorkerRun{ID: claim.RunID, RunLeaseID: claim.ID, TaskID: "deploy"},
		},
		renewResponse: api.WorkerRenewResponse{Lease: renewedClaim},
		renewed:       renewed,
	}
	executor := executorFunc(func(ctx context.Context, leases api.WorkerRunLeaseProvider, _ api.WorkerRun) api.WorkerReleaseResult {
		got := leases.CurrentWorkerRunLease()
		if got.DispatchLeaseID != "lease-1" {
			t.Fatalf("initial executor lease = %+v", got)
		}
		timer := time.NewTimer(time.Second)
		defer timer.Stop()
		select {
		case <-renewed:
		case <-timer.C:
			return api.WorkerReleaseResult{Kind: "failed", Error: new("timed out waiting for lease renewal")}
		}
		current := leases.CurrentWorkerRunLease()
		if current.DispatchLeaseID != "lease-2" {
			t.Fatalf("current executor lease = %+v", current)
		}
		exitCode := int32(0)
		return api.WorkerReleaseResult{Kind: "completed", ExitCode: &exitCode}
	})
	runner := newTestRunner(t, client, executor)
	runner.renewEvery = time.Millisecond

	worked, err := runner.RunOnce(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if !worked {
		t.Fatal("expected work")
	}
	if client.releaseLease.DispatchLeaseID != "lease-2" {
		t.Fatalf("release lease = %+v", client.releaseLease)
	}
}

func TestRunOnceReturnsReleaseError(t *testing.T) {
	claim := api.WorkerRunLease{
		ID:                "00000000-0000-0000-0000-000000000001",
		RunID:             "00000000-0000-0000-0000-000000000002",
		WorkerInstanceID:  "worker-1",
		AttemptNumber:     1,
		DispatchMessageID: "message-1",
		DispatchLeaseID:   "lease-1",
	}
	client := &fakeClient{
		claimResponse: api.WorkerRunLeaseResponse{
			Lease: &claim,
			Run:   &api.WorkerRun{ID: claim.RunID, RunLeaseID: claim.ID, TaskID: "deploy"},
		},
		releaseErr: errors.New("release failed"),
	}
	runner := newTestRunner(t, client, executorFunc(func(context.Context, api.WorkerRunLeaseProvider, api.WorkerRun) api.WorkerReleaseResult {
		exitCode := int32(0)
		return api.WorkerReleaseResult{Kind: "completed", ExitCode: &exitCode}
	}))
	runner.releaseWait = time.Millisecond

	worked, err := runner.RunOnce(context.Background())
	if err == nil {
		t.Fatal("expected error")
	}
	if !worked {
		t.Fatal("expected work")
	}
}

func TestRunOnceCancelsExecutionWhenRenewIsStale(t *testing.T) {
	claim := api.WorkerRunLease{
		ID:                "00000000-0000-0000-0000-000000000001",
		RunID:             "00000000-0000-0000-0000-000000000002",
		WorkerInstanceID:  "worker-1",
		AttemptNumber:     1,
		DispatchMessageID: "message-1",
		DispatchLeaseID:   "lease-1",
	}
	client := &fakeClient{
		claimResponse: api.WorkerRunLeaseResponse{
			Lease: &claim,
			Run:   &api.WorkerRun{ID: claim.RunID, RunLeaseID: claim.ID, TaskID: "deploy"},
		},
		renewErr: &client.HTTPError{StatusCode: 409, Status: "409 Conflict", Message: "worker run lease is stale"},
	}
	runner := newTestRunner(t, client, executorFunc(func(ctx context.Context, _ api.WorkerRunLeaseProvider, _ api.WorkerRun) api.WorkerReleaseResult {
		<-ctx.Done()
		message := "cancelled"
		return api.WorkerReleaseResult{Kind: "failed", Error: &message}
	}))
	runner.renewEvery = time.Millisecond

	worked, err := runner.RunOnce(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if !worked {
		t.Fatal("expected work")
	}
	if client.releases != 0 {
		t.Fatalf("releases = %d", client.releases)
	}
}

func TestRunOnceReturnsTransientRenewErrorWithoutRelease(t *testing.T) {
	claim := api.WorkerRunLease{
		ID:                "00000000-0000-0000-0000-000000000001",
		RunID:             "00000000-0000-0000-0000-000000000002",
		WorkerInstanceID:  "worker-1",
		AttemptNumber:     1,
		DispatchMessageID: "message-1",
		DispatchLeaseID:   "lease-1",
	}
	client := &fakeClient{
		claimResponse: api.WorkerRunLeaseResponse{
			Lease: &claim,
			Run:   &api.WorkerRun{ID: claim.RunID, RunLeaseID: claim.ID, TaskID: "deploy"},
		},
		renewErr: errors.New("control plane unavailable"),
	}
	runner := newTestRunner(t, client, executorFunc(func(ctx context.Context, _ api.WorkerRunLeaseProvider, _ api.WorkerRun) api.WorkerReleaseResult {
		<-ctx.Done()
		message := "cancelled"
		return api.WorkerReleaseResult{Kind: "failed", Error: &message}
	}))
	runner.renewEvery = time.Millisecond
	runner.renewWait = 10 * time.Millisecond

	worked, err := runner.RunOnce(context.Background())
	if err == nil || !strings.Contains(err.Error(), "control plane unavailable") {
		t.Fatalf("err = %v", err)
	}
	if !worked {
		t.Fatal("expected work")
	}
	if client.releases != 0 {
		t.Fatalf("releases = %d", client.releases)
	}
}

func TestRunOnceTimesOutHungRenewAndReleases(t *testing.T) {
	claim := api.WorkerRunLease{
		ID:                "00000000-0000-0000-0000-000000000001",
		RunID:             "00000000-0000-0000-0000-000000000002",
		WorkerInstanceID:  "worker-1",
		AttemptNumber:     1,
		DispatchMessageID: "message-1",
		DispatchLeaseID:   "lease-1",
	}
	client := &fakeClient{
		claimResponse: api.WorkerRunLeaseResponse{
			Lease: &claim,
			Run:   &api.WorkerRun{ID: claim.RunID, RunLeaseID: claim.ID, TaskID: "deploy"},
		},
		renewWaitForCancel: true,
	}
	runner := newTestRunner(t, client, executorFunc(func(ctx context.Context, _ api.WorkerRunLeaseProvider, _ api.WorkerRun) api.WorkerReleaseResult {
		<-ctx.Done()
		message := "cancelled"
		return api.WorkerReleaseResult{Kind: "cancelled", Error: &message}
	}))
	runner.renewEvery = time.Millisecond
	runner.renewWait = time.Millisecond

	worked, err := runner.RunOnce(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if !worked || client.releases != 1 {
		t.Fatalf("worked=%v releases=%d", worked, client.releases)
	}
	if client.releaseResult.Kind != "cancelled" {
		t.Fatalf("release result = %+v", client.releaseResult)
	}
}

func TestRunOnceReleasesShutdownBeforeStart(t *testing.T) {
	claim := api.WorkerRunLease{
		ID:                "00000000-0000-0000-0000-000000000001",
		RunID:             "00000000-0000-0000-0000-000000000002",
		WorkerInstanceID:  "worker-1",
		AttemptNumber:     1,
		DispatchMessageID: "message-1",
		DispatchLeaseID:   "lease-1",
	}
	client := &fakeClient{
		claimResponse: api.WorkerRunLeaseResponse{
			Lease: &claim,
			Run:   &api.WorkerRun{ID: claim.RunID, RunLeaseID: claim.ID, TaskID: "deploy"},
		},
		startErr: context.Canceled,
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	runner := newTestRunner(t, client, executorFunc(func(context.Context, api.WorkerRunLeaseProvider, api.WorkerRun) api.WorkerReleaseResult {
		t.Fatal("executor should not run")
		return api.WorkerReleaseResult{}
	}))

	worked, err := runner.RunOnce(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if !worked || client.releases != 1 {
		t.Fatalf("worked=%v releases=%d", worked, client.releases)
	}
	if client.releaseResult.Kind != "cancelled" {
		t.Fatalf("release result = %+v", client.releaseResult)
	}
}

func TestRunOnceReleasesWithFreshContextAfterCancellation(t *testing.T) {
	claim := api.WorkerRunLease{
		ID:                "00000000-0000-0000-0000-000000000001",
		RunID:             "00000000-0000-0000-0000-000000000002",
		WorkerInstanceID:  "worker-1",
		AttemptNumber:     1,
		DispatchMessageID: "message-1",
		DispatchLeaseID:   "lease-1",
	}
	client := &fakeClient{
		claimResponse: api.WorkerRunLeaseResponse{
			Lease: &claim,
			Run:   &api.WorkerRun{ID: claim.RunID, RunLeaseID: claim.ID, TaskID: "deploy"},
		},
	}
	ctx, cancel := context.WithCancel(context.Background())
	runner := newTestRunner(t, client, executorFunc(func(context.Context, api.WorkerRunLeaseProvider, api.WorkerRun) api.WorkerReleaseResult {
		cancel()
		message := "shutdown"
		return api.WorkerReleaseResult{Kind: "cancelled", Error: &message}
	}))

	worked, err := runner.RunOnce(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if !worked || client.releases != 1 {
		t.Fatalf("worked=%v releases=%d", worked, client.releases)
	}
	if client.releaseCtxErr != nil {
		t.Fatalf("release context was cancelled: %v", client.releaseCtxErr)
	}
}

func TestRunOnceSkipsReleaseAfterCheckpointDetach(t *testing.T) {
	claim := api.WorkerRunLease{
		ID:                "00000000-0000-0000-0000-000000000001",
		RunID:             "00000000-0000-0000-0000-000000000002",
		WorkerInstanceID:  "worker-1",
		AttemptNumber:     1,
		DispatchMessageID: "message-1",
		DispatchLeaseID:   "lease-1",
	}
	client := &fakeClient{
		claimResponse: api.WorkerRunLeaseResponse{
			Lease: &claim,
			Run:   &api.WorkerRun{ID: claim.RunID, RunLeaseID: claim.ID, TaskID: "deploy"},
		},
	}
	runner := newTestRunner(t, client, executorFunc(func(context.Context, api.WorkerRunLeaseProvider, api.WorkerRun) api.WorkerReleaseResult {
		return api.WorkerReleaseResult{Kind: "detached"}
	}))

	worked, err := runner.RunOnce(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if !worked || client.releases != 0 {
		t.Fatalf("worked=%v releases=%d", worked, client.releases)
	}
}

func TestRunOnceLogsFailedDeploymentBuildStatus(t *testing.T) {
	lease := api.WorkerDeploymentBuildLease{
		ID:               "lease-1",
		DeploymentID:     "deployment-1",
		WorkerInstanceID: "worker-1",
		ExpiresAt:        time.Now().Add(time.Minute),
	}
	client := &fakeClient{
		deploymentClaimResponse: api.WorkerDeploymentBuildLeaseResponse{
			Lease:      &lease,
			Deployment: &api.WorkerDeploymentBuild{ID: lease.DeploymentID},
		},
		deploymentResponseStatus: "failed",
	}
	var logs strings.Builder
	runner, err := NewRunner(
		client,
		executorFunc(func(context.Context, api.WorkerRunLeaseProvider, api.WorkerRun) api.WorkerReleaseResult {
			t.Fatal("executor should not run while a deployment build is available")
			return api.WorkerReleaseResult{}
		}),
		testCapabilities(),
		WithPollEvery(time.Millisecond),
		WithRenewEvery(time.Hour),
		WithLogger(slog.New(slog.NewTextHandler(&logs, nil))),
		WithDeploymentBuilder(deploymentBuilderFunc(func(context.Context, api.WorkerDeploymentBuildLease, api.WorkerDeploymentBuild) api.WorkerDeploymentBuildResult {
			message := "build failed"
			return api.WorkerDeploymentBuildResult{Error: &message}
		})),
	)
	if err != nil {
		t.Fatal(err)
	}

	worked, err := runner.RunOnce(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if !worked || client.deploymentCompletions != 1 {
		t.Fatalf("worked=%v deploymentCompletions=%d", worked, client.deploymentCompletions)
	}
	output := logs.String()
	if !strings.Contains(output, `level=WARN`) || !strings.Contains(output, `status=failed`) {
		t.Fatalf("logs = %s", output)
	}
}

func newTestRunner(t *testing.T, client ControlClient, executor Executor) *Runner {
	t.Helper()
	runner, err := NewRunner(
		client,
		executor,
		testCapabilities(),
		WithPollEvery(time.Millisecond),
		WithRenewEvery(time.Hour),
		WithLogger(slog.New(slog.NewTextHandler(io.Discard, nil))),
	)
	if err != nil {
		t.Fatal(err)
	}
	return runner
}

type fakeClient struct {
	deploymentClaimResponse   api.WorkerDeploymentBuildLeaseResponse
	deploymentResult          api.WorkerDeploymentBuildResult
	deploymentCompletions     int
	deploymentResponseStatus  string
	claimResponse             api.WorkerRunLeaseResponse
	renewResponse             api.WorkerRenewResponse
	renewErr                  error
	startErr                  error
	releaseErr                error
	releaseCtxErr             error
	renewed                   chan<- struct{}
	renewWaitForCancel        bool
	starts                    int
	renews                    int
	releases                  int
	releaseLease              api.WorkerRunLease
	releaseResult             api.WorkerReleaseResult
	materializationClaim      api.WorkerWorkspaceMaterializationClaimResponse
	materializationClaimQueue []api.WorkerWorkspaceMaterializationClaimResponse
	materializationClaims     int
	materializationStops      int
	materializationFailures   int
	operationClaim            api.WorkerWorkspaceOperationClaimResponse
	operationClaims           int
	operationComplete         api.WorkerWorkspaceOperationCompleteRequest
}

func (f *fakeClient) LeaseDeploymentBuild(context.Context, api.WorkerCapabilities) (api.WorkerDeploymentBuildLeaseResponse, error) {
	return f.deploymentClaimResponse, nil
}

func (f *fakeClient) CompleteDeploymentBuild(_ context.Context, _ api.WorkerDeploymentBuildLease, result api.WorkerDeploymentBuildResult) (api.WorkerDeploymentBuildResponse, error) {
	f.deploymentCompletions++
	f.deploymentResult = result
	status := f.deploymentResponseStatus
	if status == "" {
		status = "deployed"
	}
	return api.WorkerDeploymentBuildResponse{Status: status}, nil
}

func (f *fakeClient) ClaimWorkspaceMaterialization(context.Context, api.WorkerCapabilities) (api.WorkerWorkspaceMaterializationClaimResponse, error) {
	f.materializationClaims++
	if len(f.materializationClaimQueue) > 0 {
		response := f.materializationClaimQueue[0]
		f.materializationClaimQueue = f.materializationClaimQueue[1:]
		return response, nil
	}
	return f.materializationClaim, nil
}

func (f *fakeClient) RenewWorkspaceMaterialization(context.Context, api.WorkerWorkspaceMaterializationRenewRequest) (api.WorkspaceMaterializationResponse, error) {
	return api.WorkspaceMaterializationResponse{State: "running"}, nil
}

func (f *fakeClient) MarkWorkspaceMaterializationRunning(context.Context, api.WorkerWorkspaceMaterializationRunningRequest) (api.WorkspaceMaterializationResponse, error) {
	return api.WorkspaceMaterializationResponse{State: "running"}, nil
}

func (f *fakeClient) StopWorkspaceMaterialization(context.Context, api.WorkerWorkspaceMaterializationStopRequest) (api.WorkspaceMaterializationResponse, error) {
	f.materializationStops++
	return api.WorkspaceMaterializationResponse{State: "stopped"}, nil
}

func (f *fakeClient) FailWorkspaceMaterialization(context.Context, api.WorkerWorkspaceMaterializationFailRequest) (api.WorkspaceMaterializationResponse, error) {
	f.materializationFailures++
	return api.WorkspaceMaterializationResponse{State: "failed"}, nil
}

func (f *fakeClient) ClaimWorkspaceMaterializationOperation(context.Context, api.WorkerWorkspaceOperationClaimRequest) (api.WorkerWorkspaceOperationClaimResponse, error) {
	f.operationClaims++
	return f.operationClaim, nil
}

func (f *fakeClient) StartWorkspaceMaterializationOperation(context.Context, api.WorkerWorkspaceOperationStartRequest) (api.WorkspaceOperationResponse, error) {
	return api.WorkspaceOperationResponse{State: "running"}, nil
}

func (f *fakeClient) CompleteWorkspaceMaterializationOperation(_ context.Context, request api.WorkerWorkspaceOperationCompleteRequest) (api.WorkspaceOperationResponse, error) {
	f.operationComplete = request
	return api.WorkspaceOperationResponse{ID: request.OperationID, State: "completed"}, nil
}

func (f *fakeClient) MarkWorkspaceExecStarted(context.Context, api.WorkerWorkspaceExecStartedRequest) (api.WorkspaceExecEnvelope, error) {
	return api.WorkspaceExecEnvelope{}, nil
}

func (f *fakeClient) AppendWorkspaceExecOutput(context.Context, api.WorkerWorkspaceExecOutputRequest) (api.ListWorkspaceExecStreamChunksResponse, error) {
	return api.ListWorkspaceExecStreamChunksResponse{}, nil
}

func (f *fakeClient) ListWorkspaceExecInput(context.Context, api.WorkerWorkspaceExecInputRequest) (api.WorkerWorkspaceExecInputResponse, error) {
	return api.WorkerWorkspaceExecInputResponse{}, nil
}

func (f *fakeClient) AdvanceWorkspaceExecInputDelivered(context.Context, api.WorkerWorkspaceExecInputDeliveredRequest) (api.WorkspaceExecEnvelope, error) {
	return api.WorkspaceExecEnvelope{}, nil
}

func (f *fakeClient) MarkWorkspaceExecExited(context.Context, api.WorkerWorkspaceExecExitedRequest) (api.WorkspaceExecEnvelope, error) {
	return api.WorkspaceExecEnvelope{}, nil
}

func (f *fakeClient) MarkWorkspacePtyOpened(context.Context, api.WorkerWorkspacePtyOpenedRequest) (api.WorkspacePtyEnvelope, error) {
	return api.WorkspacePtyEnvelope{}, nil
}

func (f *fakeClient) AppendWorkspacePtyOutput(context.Context, api.WorkerWorkspacePtyOutputRequest) (api.ListWorkspacePtyStreamChunksResponse, error) {
	return api.ListWorkspacePtyStreamChunksResponse{}, nil
}

func (f *fakeClient) ListWorkspacePtyInput(context.Context, api.WorkerWorkspacePtyInputRequest) (api.WorkerWorkspacePtyInputResponse, error) {
	return api.WorkerWorkspacePtyInputResponse{}, nil
}

func (f *fakeClient) AdvanceWorkspacePtyInputDelivered(context.Context, api.WorkerWorkspacePtyInputDeliveredRequest) (api.WorkspacePtyEnvelope, error) {
	return api.WorkspacePtyEnvelope{}, nil
}

func (f *fakeClient) MarkWorkspacePtyResizeApplied(context.Context, api.WorkerWorkspacePtyResizeAppliedRequest) (api.WorkspacePtyEnvelope, error) {
	return api.WorkspacePtyEnvelope{}, nil
}

func (f *fakeClient) MarkWorkspacePtyClosed(context.Context, api.WorkerWorkspacePtyClosedRequest) (api.WorkspacePtyEnvelope, error) {
	return api.WorkspacePtyEnvelope{}, nil
}

func (f *fakeClient) LeaseRun(context.Context, api.WorkerCapabilities) (api.WorkerRunLeaseResponse, error) {
	if f.claimResponse.Lease != nil && f.claimResponse.Lease.ProtocolVersion == "" {
		f.claimResponse.Lease.ProtocolVersion = api.CurrentWorkerProtocolVersion
	}
	if f.claimResponse.Run != nil {
		if f.claimResponse.Run.WorkerProtocolVersion == "" {
			f.claimResponse.Run.WorkerProtocolVersion = api.CurrentWorkerProtocolVersion
		}
		if err := f.claimResponse.Run.Requirements.Validate(); err != nil {
			f.claimResponse.Run.Requirements = testRequirements()
		}
	}
	return f.claimResponse, nil
}

func testCapabilities() api.WorkerCapabilities {
	return api.WorkerCapabilities{
		ProtocolVersion:         api.CurrentWorkerProtocolVersion,
		RuntimeID:               "sha256:runtime",
		RuntimeArch:             "arm64",
		RuntimeABI:              "helmr.firecracker.snapshot.v0",
		KernelDigest:            "sha256:kernel",
		InitramfsDigest:         "sha256:initramfs",
		RootfsDigest:            "sha256:rootfs",
		CNIProfile:              "helmr/v0",
		MaxVCPUs:                2,
		MaxMemoryMiB:            2048,
		MaxDiskMiB:              20480,
		ExecutionSlotsAvailable: 1,
		Network: api.WorkerNetworkCapabilities{
			Internet:      true,
			BlockInternet: true,
			DenyCIDRs:     true,
		},
	}
}

func testRequirements() compute.RunRuntimeRequirements {
	return compute.RunRuntimeRequirements{
		Resources: compute.ResourceVector{
			MilliCPU:  1000,
			MemoryMiB: 512,
			DiskMiB:   1024,
			Slots:     1,
		},
		Runtime: compute.RuntimeSelector{
			ID:              "sha256:runtime",
			Arch:            "arm64",
			ABI:             "helmr.firecracker.snapshot.v0",
			KernelDigest:    "sha256:kernel",
			InitramfsDigest: "sha256:initramfs",
			RootfsDigest:    "sha256:rootfs",
			CNIProfile:      "helmr/v0",
		},
		Network: compute.DefaultNetworkPolicy(),
	}
}

func (f *fakeClient) StartRun(context.Context, api.WorkerRunLease) (api.WorkerStartResponse, error) {
	f.starts++
	if f.startErr != nil {
		return api.WorkerStartResponse{}, f.startErr
	}
	return api.WorkerStartResponse{Status: "running"}, nil
}

func (f *fakeClient) RenewRun(ctx context.Context, _ api.WorkerRunLease) (api.WorkerRenewResponse, error) {
	f.renews++
	if f.renewed != nil {
		select {
		case f.renewed <- struct{}{}:
		default:
		}
	}
	if f.renewWaitForCancel {
		<-ctx.Done()
		return api.WorkerRenewResponse{}, ctx.Err()
	}
	if f.renewErr != nil {
		return api.WorkerRenewResponse{}, f.renewErr
	}
	if f.renewResponse.Lease.ID == "" {
		return api.WorkerRenewResponse{}, nil
	}
	return f.renewResponse, nil
}

func (f *fakeClient) ReleaseRun(ctx context.Context, lease api.WorkerRunLease, result api.WorkerReleaseResult) (api.WorkerReleaseResponse, error) {
	f.releases++
	f.releaseCtxErr = ctx.Err()
	f.releaseLease = lease
	f.releaseResult = result
	if f.releaseErr != nil {
		return api.WorkerReleaseResponse{}, f.releaseErr
	}
	return api.WorkerReleaseResponse{Status: "succeeded"}, nil
}
