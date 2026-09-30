package run

import (
	"bytes"
	"context"
	"errors"
	"log/slog"
	"strings"
	"testing"
	"time"
)

type recordingLeaseRecoveryGuard struct{ unlocked int }

func (g *recordingLeaseRecoveryGuard) Unlock() error { g.unlocked++; return nil }

func testLeaseReconciler(
	guard *recordingLeaseRecoveryGuard,
	locked bool,
	recoverLeases func(context.Context, int32) (int, error),
	log *slog.Logger,
) *LeaseReconciler {
	if log == nil {
		log = slog.New(slog.DiscardHandler)
	}
	return &LeaseReconciler{
		tryLock: func(context.Context) (leaseRecoveryGuard, bool, error) {
			if !locked {
				return nil, false, nil
			}
			return guard, true, nil
		},
		recoverLeases: recoverLeases,
		interval:      defaultLeaseRecoveryInterval,
		timeout:       defaultLeaseRecoveryTimeout,
		limit:         defaultLeaseRecoveryLimit,
		log:           log,
	}
}

func TestLeaseReconcilerBoundsRecoveryAndReleasesLock(t *testing.T) {
	recoveryErr := errors.New("execution recovery failed")
	var limit int32
	guard := &recordingLeaseRecoveryGuard{}
	reconciler := testLeaseReconciler(guard, true, func(_ context.Context, bound int32) (int, error) {
		limit = bound
		return 0, recoveryErr
	}, nil)
	if err := reconciler.reconcile(t.Context()); !errors.Is(err, recoveryErr) {
		t.Fatalf("recovery error: %v", err)
	}
	// The execution lane keeps its share of the former combined batch of 100.
	if limit != 50 {
		t.Fatalf("bound: %d", limit)
	}
	if guard.unlocked != 1 {
		t.Fatal("recovery lock retained")
	}
}

func TestLeaseReconcilerSkipsCycleWhileAnotherDispatcherHoldsLock(t *testing.T) {
	reconciler := testLeaseReconciler(nil, false, func(context.Context, int32) (int, error) {
		t.Fatal("recovery ran without the singleton lock")
		return 0, nil
	}, nil)
	if err := reconciler.reconcile(t.Context()); err != nil {
		t.Fatal(err)
	}
}

func TestLeaseReconcilerLogsFailureAndStopsOnCancellation(t *testing.T) {
	var logs bytes.Buffer
	log := slog.New(slog.NewTextHandler(&logs, nil))
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	guard := &recordingLeaseRecoveryGuard{}
	calls := 0
	reconciler := testLeaseReconciler(guard, true, func(context.Context, int32) (int, error) {
		calls++
		if calls == 1 {
			return 0, errors.New("execution recovery failed")
		}
		cancel()
		return 0, context.Canceled
	}, log)
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
	if !strings.Contains(logs.String(), "Run lease recovery failed") {
		t.Fatalf("missing failure log: %s", logs.String())
	}
}
