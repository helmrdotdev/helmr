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
)

func TestRunOnceNoClaim(t *testing.T) {
	client := &fakeClient{}
	runner := newTestRunner(t, client, ExecutorFunc(func(context.Context, api.WorkerClaim, api.WorkerRun) api.WorkerReleaseResult {
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

func TestRunOnceStartsExecutesRenewsAndReleases(t *testing.T) {
	claim := api.WorkerClaim{
		ID:        "00000000-0000-0000-0000-000000000001",
		RunID:     "00000000-0000-0000-0000-000000000002",
		WorkerID:  "worker-1",
		ExpiresAt: time.Now().Add(time.Minute),
	}
	client := &fakeClient{
		claimResponse: api.WorkerClaimResponse{
			Claim: &claim,
			Run:   &api.WorkerRun{ID: claim.RunID, TaskID: "deploy"},
		},
		renewResponse: api.WorkerRenewResponse{Claim: claim},
	}
	executed := false
	releaseResult := api.WorkerReleaseResult{}
	releaseDone := make(chan struct{})
	executor := ExecutorFunc(func(ctx context.Context, got api.WorkerClaim, run api.WorkerRun) api.WorkerReleaseResult {
		if got.ID != claim.ID || run.TaskID != "deploy" {
			t.Fatalf("unexpected execution input claim=%+v run=%+v", got, run)
		}
		executed = true
		<-releaseDone
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
	go func() {
		time.Sleep(5 * time.Millisecond)
		close(releaseDone)
	}()

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

func TestRunOnceReturnsReleaseError(t *testing.T) {
	claim := api.WorkerClaim{
		ID:       "00000000-0000-0000-0000-000000000001",
		RunID:    "00000000-0000-0000-0000-000000000002",
		WorkerID: "worker-1",
	}
	client := &fakeClient{
		claimResponse: api.WorkerClaimResponse{
			Claim: &claim,
			Run:   &api.WorkerRun{ID: claim.RunID, TaskID: "deploy"},
		},
		releaseErr: errors.New("release failed"),
	}
	runner := newTestRunner(t, client, ExecutorFunc(func(context.Context, api.WorkerClaim, api.WorkerRun) api.WorkerReleaseResult {
		exitCode := int32(0)
		return api.WorkerReleaseResult{Kind: "completed", ExitCode: &exitCode}
	}))

	worked, err := runner.RunOnce(context.Background())
	if err == nil {
		t.Fatal("expected error")
	}
	if !worked {
		t.Fatal("expected work")
	}
}

func TestRunOnceCancelsExecutionWhenRenewIsStale(t *testing.T) {
	claim := api.WorkerClaim{
		ID:       "00000000-0000-0000-0000-000000000001",
		RunID:    "00000000-0000-0000-0000-000000000002",
		WorkerID: "worker-1",
	}
	client := &fakeClient{
		claimResponse: api.WorkerClaimResponse{
			Claim: &claim,
			Run:   &api.WorkerRun{ID: claim.RunID, TaskID: "deploy"},
		},
		renewErr: &client.HTTPError{StatusCode: 409, Status: "409 Conflict", Message: "worker claim is stale"},
	}
	runner := newTestRunner(t, client, ExecutorFunc(func(ctx context.Context, _ api.WorkerClaim, _ api.WorkerRun) api.WorkerReleaseResult {
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
	claim := api.WorkerClaim{
		ID:       "00000000-0000-0000-0000-000000000001",
		RunID:    "00000000-0000-0000-0000-000000000002",
		WorkerID: "worker-1",
	}
	client := &fakeClient{
		claimResponse: api.WorkerClaimResponse{
			Claim: &claim,
			Run:   &api.WorkerRun{ID: claim.RunID, TaskID: "deploy"},
		},
		renewErr: errors.New("control plane unavailable"),
	}
	runner := newTestRunner(t, client, ExecutorFunc(func(ctx context.Context, _ api.WorkerClaim, _ api.WorkerRun) api.WorkerReleaseResult {
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
	claim := api.WorkerClaim{
		ID:       "00000000-0000-0000-0000-000000000001",
		RunID:    "00000000-0000-0000-0000-000000000002",
		WorkerID: "worker-1",
	}
	client := &fakeClient{
		claimResponse: api.WorkerClaimResponse{
			Claim: &claim,
			Run:   &api.WorkerRun{ID: claim.RunID, TaskID: "deploy"},
		},
		renewWaitForCancel: true,
	}
	runner := newTestRunner(t, client, ExecutorFunc(func(ctx context.Context, _ api.WorkerClaim, _ api.WorkerRun) api.WorkerReleaseResult {
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
	claim := api.WorkerClaim{
		ID:       "00000000-0000-0000-0000-000000000001",
		RunID:    "00000000-0000-0000-0000-000000000002",
		WorkerID: "worker-1",
	}
	client := &fakeClient{
		claimResponse: api.WorkerClaimResponse{
			Claim: &claim,
			Run:   &api.WorkerRun{ID: claim.RunID, TaskID: "deploy"},
		},
		startErr: context.Canceled,
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	runner := newTestRunner(t, client, ExecutorFunc(func(context.Context, api.WorkerClaim, api.WorkerRun) api.WorkerReleaseResult {
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
	claim := api.WorkerClaim{
		ID:       "00000000-0000-0000-0000-000000000001",
		RunID:    "00000000-0000-0000-0000-000000000002",
		WorkerID: "worker-1",
	}
	client := &fakeClient{
		claimResponse: api.WorkerClaimResponse{
			Claim: &claim,
			Run:   &api.WorkerRun{ID: claim.RunID, TaskID: "deploy"},
		},
	}
	ctx, cancel := context.WithCancel(context.Background())
	runner := newTestRunner(t, client, ExecutorFunc(func(context.Context, api.WorkerClaim, api.WorkerRun) api.WorkerReleaseResult {
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
	claim := api.WorkerClaim{
		ID:       "00000000-0000-0000-0000-000000000001",
		RunID:    "00000000-0000-0000-0000-000000000002",
		WorkerID: "worker-1",
	}
	client := &fakeClient{
		claimResponse: api.WorkerClaimResponse{
			Claim: &claim,
			Run:   &api.WorkerRun{ID: claim.RunID, TaskID: "deploy"},
		},
	}
	runner := newTestRunner(t, client, ExecutorFunc(func(context.Context, api.WorkerClaim, api.WorkerRun) api.WorkerReleaseResult {
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

func newTestRunner(t *testing.T, client ControlClient, executor Executor) *Runner {
	t.Helper()
	runner, err := NewRunner(
		client,
		executor,
		"worker-1",
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
	claimResponse      api.WorkerClaimResponse
	renewResponse      api.WorkerRenewResponse
	renewErr           error
	startErr           error
	releaseErr         error
	releaseCtxErr      error
	renewWaitForCancel bool
	starts             int
	renews             int
	releases           int
	releaseResult      api.WorkerReleaseResult
}

func (f *fakeClient) ClaimRun(context.Context, api.WorkerCapabilities) (api.WorkerClaimResponse, error) {
	return f.claimResponse, nil
}

func testCapabilities() api.WorkerCapabilities {
	return api.WorkerCapabilities{
		RuntimeArch:    "arm64",
		RuntimeABI:     "helmr.firecracker.snapshot.v0",
		KernelDigest:   "sha256:kernel",
		RootfsDigest:   "sha256:rootfs",
		CNIProfile:     "helmr/v1",
		MaxVCPUs:       2,
		MaxMemoryMiB:   2048,
		SlotsAvailable: 1,
	}
}

func (f *fakeClient) StartRun(context.Context, api.WorkerClaim) (api.WorkerStartResponse, error) {
	f.starts++
	if f.startErr != nil {
		return api.WorkerStartResponse{}, f.startErr
	}
	return api.WorkerStartResponse{Status: "running"}, nil
}

func (f *fakeClient) RenewRun(ctx context.Context, _ api.WorkerClaim) (api.WorkerRenewResponse, error) {
	f.renews++
	if f.renewWaitForCancel {
		<-ctx.Done()
		return api.WorkerRenewResponse{}, ctx.Err()
	}
	if f.renewErr != nil {
		return api.WorkerRenewResponse{}, f.renewErr
	}
	if f.renewResponse.Claim.ID == "" {
		return api.WorkerRenewResponse{}, nil
	}
	return f.renewResponse, nil
}

func (f *fakeClient) ReleaseRun(ctx context.Context, _ api.WorkerClaim, result api.WorkerReleaseResult) (api.WorkerReleaseResponse, error) {
	f.releases++
	f.releaseCtxErr = ctx.Err()
	f.releaseResult = result
	if f.releaseErr != nil {
		return api.WorkerReleaseResponse{}, f.releaseErr
	}
	return api.WorkerReleaseResponse{Status: "succeeded"}, nil
}
