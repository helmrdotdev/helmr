package controlplane

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"sync/atomic"
	"testing"
	"time"
)

func TestEmailCodeDeliveryBoundsWorkersAndQueue(t *testing.T) {
	delivery := newEmailCodeDelivery(discardEmailCodeLog(), 2, 2)
	ctx, cancel := context.WithCancel(t.Context())
	done := make(chan error, 1)
	go func() { done <- delivery.Run(ctx) }()

	started := make(chan struct{}, 4)
	release := make(chan struct{}, 4)
	completed := make(chan struct{}, 4)
	var active atomic.Int32
	var maximum atomic.Int32
	job := func() emailCodeDeliveryJob {
		return emailCodeDeliveryJob{
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
		}
	}
	for range 2 {
		if !delivery.enqueue(job()) {
			t.Fatal("initial jobs were rejected")
		}
	}
	waitEmailCodeSignals(t, started, 2)
	for range 2 {
		if !delivery.enqueue(job()) {
			t.Fatal("buffered jobs were rejected")
		}
	}
	if delivery.enqueue(job()) {
		t.Fatal("job beyond the fixed queue was accepted")
	}
	for range 4 {
		release <- struct{}{}
	}
	waitEmailCodeSignals(t, completed, 4)
	if got := maximum.Load(); got != 2 {
		t.Fatalf("maximum active deliveries = %d, want 2", got)
	}
	cancel()
	if runErr := <-done; !errors.Is(runErr, context.Canceled) {
		t.Fatalf("Run error = %v, want cancellation", runErr)
	}
}

func TestEmailCodeDeliveryShutdownCancelsSenderAndJoinsWorkers(t *testing.T) {
	delivery := newEmailCodeDelivery(discardEmailCodeLog(), 1, 2)
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	done := make(chan error, 1)
	go func() { done <- delivery.Run(ctx) }()
	started := make(chan struct{}, 1)
	cancelled := make(chan struct{}, 1)
	release := make(chan struct{})
	defer close(release)
	active := emailCodeDeliveryJob{deliver: func(ctx context.Context) error {
		started <- struct{}{}
		<-ctx.Done()
		cancelled <- struct{}{}
		<-release
		return ctx.Err()
	}}
	if !delivery.enqueue(active) {
		t.Fatal("active job was rejected")
	}
	waitEmailCodeSignals(t, started, 1)
	var queuedCalled atomic.Bool
	queued := emailCodeDeliveryJob{deliver: func(context.Context) error { queuedCalled.Store(true); return nil }}
	for range 2 {
		if !delivery.enqueue(queued) {
			t.Fatal("queued jobs were rejected")
		}
	}
	cancel()
	waitEmailCodeSignals(t, cancelled, 1)
	select {
	case <-done:
		t.Fatal("Run returned before its sender stopped")
	default:
	}
	release <- struct{}{}
	select {
	case err := <-done:
		if !errors.Is(err, context.Canceled) {
			t.Fatal(err)
		}
	case <-time.After(time.Second):
		t.Fatal("Run did not join workers")
	}
	if queuedCalled.Load() {
		t.Fatal("queued sender ran after cancellation")
	}
	if delivery.enqueue(queued) {
		t.Fatal("delivery accepted work after shutdown")
	}
}

func waitEmailCodeSignals(t *testing.T, signals <-chan struct{}, count int) {
	t.Helper()
	for range count {
		select {
		case <-signals:
		case <-time.After(time.Second):
			t.Fatal("timed out waiting for magic link delivery")
		}
	}
}

func discardEmailCodeLog() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}
