package dispatch

import (
	"bytes"
	"context"
	"errors"
	"log/slog"
	"strings"
	"testing"
	"time"
)

type recordingInstanceReconciliationGuard struct{ unlocked int }

func (g *recordingInstanceReconciliationGuard) Unlock() error { g.unlocked++; return nil }

func testInstanceReconciler(
	guard *recordingInstanceReconciliationGuard,
	locked bool,
	reconcile func(context.Context, int32) (int, error),
	log *slog.Logger,
) *InstanceReconciler {
	if log == nil {
		log = slog.New(slog.DiscardHandler)
	}
	return &InstanceReconciler{
		tryLock: func(context.Context) (instanceReconciliationGuard, bool, error) {
			if !locked {
				return nil, false, nil
			}
			return guard, true, nil
		},
		reconcile: reconcile,
		interval:  defaultInstanceReconciliationInterval,
		timeout:   defaultInstanceReconciliationTimeout,
		limit:     defaultInstanceReconciliationLimit,
		log:       log,
	}
}

func TestInstanceReconcilerBoundsReconciliationAndReleasesLock(t *testing.T) {
	reconcileErr := errors.New("instance recovery failed")
	var limit int32
	guard := &recordingInstanceReconciliationGuard{}
	reconciler := testInstanceReconciler(guard, true, func(_ context.Context, bound int32) (int, error) {
		limit = bound
		return 0, reconcileErr
	}, nil)
	if err := reconciler.reconcileCycle(t.Context()); !errors.Is(err, reconcileErr) {
		t.Fatalf("reconciliation error: %v", err)
	}
	// The instance lane keeps its share of the former combined batch of 100.
	if limit != 50 {
		t.Fatalf("bound: %d", limit)
	}
	if guard.unlocked != 1 {
		t.Fatal("reconciliation lock retained")
	}
}

func TestInstanceReconcilerSkipsCycleWhileAnotherDispatcherHoldsLock(t *testing.T) {
	reconciler := testInstanceReconciler(nil, false, func(context.Context, int32) (int, error) {
		t.Fatal("reconciliation ran without the singleton lock")
		return 0, nil
	}, nil)
	if err := reconciler.reconcileCycle(t.Context()); err != nil {
		t.Fatal(err)
	}
}

func TestInstanceReconcilerLogsFailureAndStopsOnCancellation(t *testing.T) {
	var logs bytes.Buffer
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	guard := &recordingInstanceReconciliationGuard{}
	calls := 0
	reconciler := testInstanceReconciler(guard, true, func(context.Context, int32) (int, error) {
		calls++
		if calls == 1 {
			return 0, errors.New("instance recovery failed")
		}
		cancel()
		return 0, context.Canceled
	}, slog.New(slog.NewTextHandler(&logs, nil)))
	done := make(chan error, 1)
	go func() { done <- reconciler.Run(ctx) }()
	select {
	case err := <-done:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("Run returned %v", err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("Run did not stop after cancellation")
	}
	if calls != 2 || guard.unlocked != 2 {
		t.Fatalf("cycles=%d unlocks=%d", calls, guard.unlocked)
	}
	if !strings.Contains(logs.String(), "Computer instance reconciliation failed") {
		t.Fatalf("missing failure log: %s", logs.String())
	}
}
