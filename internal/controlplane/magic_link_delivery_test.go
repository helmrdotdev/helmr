package controlplane

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

func TestMagicLinkDeliveryBoundsWorkersAndQueue(t *testing.T) {
	delivery := newMagicLinkDelivery(discardMagicLinkLog(), 2, 2, time.Second)
	ctx, cancel := context.WithCancel(t.Context())
	done := make(chan error, 1)
	go func() { done <- delivery.Run(ctx) }()

	started := make(chan struct{}, 4)
	release := make(chan struct{}, 4)
	completed := make(chan struct{}, 4)
	var active atomic.Int32
	var maximum atomic.Int32
	job := func(id string) magicLinkDeliveryJob {
		return magicLinkDeliveryJob{
			id: id, purpose: "login",
			deliver: func(ctx context.Context) error {
				current := active.Add(1)
				defer active.Add(-1)
				for {
					observed := maximum.Load()
					if current <= observed || maximum.CompareAndSwap(observed, current) {
						break
					}
				}
				started <- struct{}{}
				select {
				case <-ctx.Done():
					return ctx.Err()
				case <-release:
					completed <- struct{}{}
					return nil
				}
			},
			fail: func(context.Context) error { return nil },
		}
	}
	if !delivery.enqueue(job("one")) || !delivery.enqueue(job("two")) {
		t.Fatal("initial jobs were rejected")
	}
	waitMagicLinkSignals(t, started, 2)
	if !delivery.enqueue(job("three")) || !delivery.enqueue(job("four")) {
		t.Fatal("buffered jobs were rejected")
	}
	if delivery.enqueue(job("five")) {
		t.Fatal("job beyond the fixed queue was accepted")
	}
	for range 4 {
		release <- struct{}{}
	}
	waitMagicLinkSignals(t, completed, 4)
	if got := maximum.Load(); got != 2 {
		t.Fatalf("maximum active deliveries = %d, want 2", got)
	}
	cancel()
	if runErr := <-done; !errors.Is(runErr, context.Canceled) {
		t.Fatalf("Run error = %v, want cancellation", runErr)
	}
}

func TestMagicLinkDeliveryShutdownFailsActiveAndQueuedJobs(t *testing.T) {
	delivery := newMagicLinkDelivery(discardMagicLinkLog(), 1, 2, time.Second)
	ctx, cancel := context.WithCancel(t.Context())
	done := make(chan error, 1)
	go func() { done <- delivery.Run(ctx) }()

	started := make(chan struct{}, 1)
	failed := make(map[string]int)
	var failedMu sync.Mutex
	job := func(id string) magicLinkDeliveryJob {
		return magicLinkDeliveryJob{
			id: id, purpose: "login",
			deliver: func(ctx context.Context) error {
				started <- struct{}{}
				<-ctx.Done()
				return ctx.Err()
			},
			fail: func(context.Context) error {
				failedMu.Lock()
				failed[id]++
				failedMu.Unlock()
				return nil
			},
		}
	}
	if !delivery.enqueue(job("active")) {
		t.Fatal("active job was rejected")
	}
	waitMagicLinkSignals(t, started, 1)
	if !delivery.enqueue(job("queued-one")) || !delivery.enqueue(job("queued-two")) {
		t.Fatal("queued jobs were rejected")
	}
	cancel()
	if runErr := <-done; !errors.Is(runErr, context.Canceled) {
		t.Fatalf("Run error = %v, want cancellation", runErr)
	}
	if delivery.enqueue(job("after-stop")) {
		t.Fatal("delivery accepted work after shutdown")
	}
	failedMu.Lock()
	defer failedMu.Unlock()
	for _, id := range []string{"active", "queued-one", "queued-two"} {
		if failed[id] != 1 {
			t.Fatalf("failure marks for %s = %d, want 1", id, failed[id])
		}
	}
}

func TestMagicLinkDeliveryShutdownFailureMarkIsIdempotent(t *testing.T) {
	delivery := newMagicLinkDelivery(discardMagicLinkLog(), 1, 1, time.Second)
	ctx, cancel := context.WithCancel(t.Context())
	done := make(chan error, 1)
	go func() { done <- delivery.Run(ctx) }()
	started := make(chan struct{}, 1)
	if !delivery.enqueue(magicLinkDeliveryJob{
		id: "already-finalized", purpose: "login",
		deliver: func(ctx context.Context) error {
			started <- struct{}{}
			<-ctx.Done()
			return ctx.Err()
		},
		fail: func(context.Context) error { return nil },
	}) {
		t.Fatal("job was rejected")
	}
	waitMagicLinkSignals(t, started, 1)
	cancel()
	if runErr := <-done; !errors.Is(runErr, context.Canceled) {
		t.Fatalf("Run error = %v, want cancellation", runErr)
	}
}

func TestMagicLinkDeliverySurfacesShutdownFailureMarkError(t *testing.T) {
	delivery := newMagicLinkDelivery(discardMagicLinkLog(), 1, 1, time.Second)
	ctx, cancel := context.WithCancel(t.Context())
	done := make(chan error, 1)
	go func() { done <- delivery.Run(ctx) }()
	started := make(chan struct{}, 1)
	want := errors.New("database unavailable")
	if !delivery.enqueue(magicLinkDeliveryJob{
		id: "failed-cleanup", purpose: "login",
		deliver: func(ctx context.Context) error {
			started <- struct{}{}
			<-ctx.Done()
			return ctx.Err()
		},
		fail: func(context.Context) error { return want },
	}) {
		t.Fatal("job was rejected")
	}
	waitMagicLinkSignals(t, started, 1)
	cancel()
	if runErr := <-done; !errors.Is(runErr, want) || errors.Is(runErr, context.Canceled) {
		t.Fatalf("Run error = %v, want surfaced cleanup failure", runErr)
	}
}

func waitMagicLinkSignals(t *testing.T, signals <-chan struct{}, count int) {
	t.Helper()
	for range count {
		select {
		case <-signals:
		case <-time.After(time.Second):
			t.Fatal("timed out waiting for magic link delivery")
		}
	}
}

func discardMagicLinkLog() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}
