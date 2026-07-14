package dispatch

import (
	"context"
	"errors"
	"sync/atomic"
	"testing"
	"time"

	"github.com/helmrdotdev/helmr/internal/db"

	"github.com/jackc/pgx/v5/pgtype"
)

type isolationQueue struct{}

func (isolationQueue) Enqueue(context.Context, Message) (EnqueueResult, error) {
	return EnqueueResult{}, nil
}
func (isolationQueue) ReadyRegions(context.Context, WorkKind, int64) ([]string, error) {
	return nil, nil
}
func (isolationQueue) SelectReady(context.Context, ReadySelection) ([]Message, error) {
	return nil, nil
}
func (isolationQueue) RemoveReady(context.Context, WorkKind, string, string) error { return nil }

type countingRunPlacementDiscovery struct {
	calls atomic.Int64
	wake  chan struct{}
}

func (f *countingRunPlacementDiscovery) ListQueuedRunCandidateScopes(context.Context, db.ListQueuedRunCandidateScopesParams) ([]db.ListQueuedRunCandidateScopesRow, error) {
	f.calls.Add(1)
	select {
	case f.wake <- struct{}{}:
	default:
	}
	return nil, nil
}
func (*countingRunPlacementDiscovery) ListQueuedRunDispatchCandidatesForScope(context.Context, db.ListQueuedRunDispatchCandidatesForScopeParams) ([]db.ListQueuedRunDispatchCandidatesForScopeRow, error) {
	return nil, nil
}

type blockingBuildPlacementDiscovery struct {
	started chan struct{}
}

func (f blockingBuildPlacementDiscovery) ListQueuedDeploymentBuildRegions(ctx context.Context, _ int32) ([]string, error) {
	select {
	case f.started <- struct{}{}:
	default:
	}
	<-ctx.Done()
	return nil, ctx.Err()
}
func (blockingBuildPlacementDiscovery) ListQueuedDeploymentBuildCandidates(context.Context, db.ListQueuedDeploymentBuildCandidatesParams) ([]db.ListQueuedDeploymentBuildCandidatesRow, error) {
	return nil, nil
}

type isolationRunAuthority struct{}

func (isolationRunAuthority) PlaceReadyRun(context.Context, ReadyRunCandidate, pgtype.Timestamptz) (ReadyRunPlacement, error) {
	return ReadyRunPlacement{}, nil
}

type isolationBuildAuthority struct{}

func (isolationBuildAuthority) PlaceReadyBuild(context.Context, ReadyBuildCandidate, pgtype.Timestamptz) (db.LeaseQueuedDeploymentBuildRow, error) {
	return db.LeaseQueuedDeploymentBuildRow{}, nil
}

type isolationWakePublisher struct{}

func (isolationWakePublisher) PublishWorkerWake(context.Context, WorkerWake) error { return nil }

func TestPlacementReconcilerBlockedBuildDatabaseWorkDoesNotStarveRunPlacement(t *testing.T) {
	runDiscovery := &countingRunPlacementDiscovery{wake: make(chan struct{}, 8)}
	buildStarted := make(chan struct{}, 1)
	reconciler, err := NewPlacementReconciler(
		runDiscovery, isolationRunAuthority{},
		blockingBuildPlacementDiscovery{started: buildStarted}, isolationBuildAuthority{},
		isolationQueue{}, isolationWakePublisher{}, nil,
	)
	if err != nil {
		t.Fatal(err)
	}
	reconciler.runPolicy.interval = 5 * time.Millisecond
	reconciler.runPolicy.failureBackoff = 5 * time.Millisecond
	reconciler.runPolicy.timeout = 100 * time.Millisecond
	reconciler.buildPolicy.interval = time.Hour
	reconciler.buildPolicy.failureBackoff = time.Hour
	reconciler.buildPolicy.timeout = time.Second

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- reconciler.Run(ctx) }()
	select {
	case <-buildStarted:
	case <-time.After(time.Second):
		cancel()
		t.Fatal("build placement database work did not start")
	}
	for runDiscovery.calls.Load() < 3 {
		select {
		case <-runDiscovery.wake:
		case <-time.After(time.Second):
			cancel()
			t.Fatalf("run placement stalled while build database work was blocked; calls=%d", runDiscovery.calls.Load())
		}
	}
	cancel()
	select {
	case err := <-done:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("Run() error = %v, want context cancellation", err)
		}
	case <-time.After(time.Second):
		t.Fatal("placement reconciler did not stop")
	}
}
