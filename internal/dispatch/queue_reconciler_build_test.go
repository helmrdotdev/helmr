package dispatch

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"sync/atomic"
	"testing"
	"time"

	"github.com/helmrdotdev/helmr/internal/db"
)

type buildReconcileStoreFake struct{}

func (buildReconcileStoreFake) ListQueuedRunCandidateScopes(context.Context, db.ListQueuedRunCandidateScopesParams) ([]db.ListQueuedRunCandidateScopesRow, error) {
	return nil, nil
}

type buildReconcileEnqueuerFake struct{ regions, candidates int32 }

func (*buildReconcileEnqueuerFake) ReconcileQueueScope(context.Context, QueueScope, int32) (QueueReconcileStats, error) {
	return QueueReconcileStats{}, nil
}
func (f *buildReconcileEnqueuerFake) ReconcileBuildReady(_ context.Context, regions, candidates int32) (QueueReconcileStats, error) {
	f.regions, f.candidates = regions, candidates
	return QueueReconcileStats{Scanned: 1, Enqueued: 1}, nil
}

func TestQueueReconcilerReconstructsBuildReadyIndex(t *testing.T) {
	enqueuer := &buildReconcileEnqueuerFake{}
	reconciler, err := NewQueueReconciler(buildReconcileStoreFake{}, enqueuer, enqueuer,
		WithBuildQueueReconcileLimits(11, 7))
	if err != nil {
		t.Fatal(err)
	}
	if err := reconciler.ReconcileOnce(context.Background()); err != nil {
		t.Fatal(err)
	}
	if enqueuer.regions != 11 || enqueuer.candidates != 7 {
		t.Fatalf("build reconstruction limits = %d/%d, want 11/7", enqueuer.regions, enqueuer.candidates)
	}
}

type isolatedQueueStoreFake struct{}

func (isolatedQueueStoreFake) ListQueuedRunCandidateScopes(context.Context, db.ListQueuedRunCandidateScopesParams) ([]db.ListQueuedRunCandidateScopesRow, error) {
	return []db.ListQueuedRunCandidateScopesRow{{QueueName: "run"}}, nil
}

type countingRunEnqueuer struct {
	calls atomic.Int64
	wake  chan struct{}
}

func (f *countingRunEnqueuer) ReconcileQueueScope(context.Context, QueueScope, int32) (QueueReconcileStats, error) {
	f.calls.Add(1)
	select {
	case f.wake <- struct{}{}:
	default:
	}
	return QueueReconcileStats{}, nil
}

type blockingBuildEnqueuer struct {
	started chan struct{}
}

func (f blockingBuildEnqueuer) ReconcileBuildReady(ctx context.Context, _, _ int32) (QueueReconcileStats, error) {
	select {
	case f.started <- struct{}{}:
	default:
	}
	<-ctx.Done()
	return QueueReconcileStats{}, ctx.Err()
}

func TestQueueReconcilerBlockedBuildQueryDoesNotStarveRunReconciliation(t *testing.T) {
	runEnqueuer := &countingRunEnqueuer{wake: make(chan struct{}, 8)}
	buildStarted := make(chan struct{}, 1)
	reconciler, err := NewQueueReconciler(
		isolatedQueueStoreFake{}, runEnqueuer, blockingBuildEnqueuer{started: buildStarted},
		WithQueueReconcileIntervals(5*time.Millisecond, time.Hour),
		WithQueueReconcileQueryTimeouts(100*time.Millisecond, time.Second),
		WithQueueReconcileConsecutiveFailureLimit(100),
	)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- reconciler.Run(ctx) }()

	select {
	case <-buildStarted:
	case <-time.After(time.Second):
		cancel()
		t.Fatal("build reconciliation did not start")
	}
	for runEnqueuer.calls.Load() < 3 {
		select {
		case <-runEnqueuer.wake:
		case <-time.After(time.Second):
			cancel()
			t.Fatalf("run reconciliation stalled while build query was blocked; calls=%d", runEnqueuer.calls.Load())
		}
	}
	cancel()
	select {
	case err := <-done:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("Run() error = %v, want context cancellation", err)
		}
	case <-time.After(time.Second):
		t.Fatal("queue reconciler did not stop")
	}
}

type failingBuildEnqueuer struct {
	calls atomic.Int64
	wake  chan struct{}
}

func (f *failingBuildEnqueuer) ReconcileBuildReady(context.Context, int32, int32) (QueueReconcileStats, error) {
	f.calls.Add(1)
	select {
	case f.wake <- struct{}{}:
	default:
	}
	return QueueReconcileStats{}, errors.New("build database unavailable")
}

func TestQueueReconcilerRepeatedBuildFailuresRemainDomainLocal(t *testing.T) {
	runEnqueuer := &countingRunEnqueuer{wake: make(chan struct{}, 32)}
	buildEnqueuer := &failingBuildEnqueuer{wake: make(chan struct{}, 32)}
	reconciler, err := NewQueueReconciler(
		isolatedQueueStoreFake{}, runEnqueuer, buildEnqueuer,
		WithQueueReconcileIntervals(5*time.Millisecond, 5*time.Millisecond),
		WithQueueReconcileFailureBackoffs(5*time.Millisecond, 5*time.Millisecond),
		WithQueueReconcileConsecutiveFailureLimit(3),
		WithQueueReconcileLogger(slog.New(slog.NewTextHandler(io.Discard, nil))),
	)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- reconciler.Run(ctx) }()

	for buildEnqueuer.calls.Load() < 5 {
		select {
		case <-buildEnqueuer.wake:
		case <-time.After(time.Second):
			cancel()
			t.Fatalf("build reconciliation stopped after its failure threshold; calls=%d", buildEnqueuer.calls.Load())
		}
	}
	runCalls := runEnqueuer.calls.Load()
	for runEnqueuer.calls.Load() <= runCalls {
		select {
		case <-runEnqueuer.wake:
		case <-time.After(time.Second):
			cancel()
			t.Fatalf("run reconciliation stopped after repeated build failures; calls=%d", runEnqueuer.calls.Load())
		}
	}
	cancel()
	select {
	case err := <-done:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("Run() error = %v, want context cancellation", err)
		}
	case <-time.After(time.Second):
		t.Fatal("queue reconciler did not stop")
	}
}
