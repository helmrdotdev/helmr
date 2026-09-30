package main

import (
	"context"
	"errors"
	"strings"
	"sync/atomic"
	"testing"
)

func blockingRunner(name string, stopped *atomic.Int32, result func(context.Context) error) dispatcherRunner {
	return dispatcherRunner{name: name, run: func(ctx context.Context) error {
		<-ctx.Done()
		stopped.Add(1)
		return result(ctx)
	}}
}

func returnNil(context.Context) error            { return nil }
func returnContextErr(ctx context.Context) error { return ctx.Err() }

func TestSuperviseRunnersReportsNilExitUnderLiveParent(t *testing.T) {
	var stopped atomic.Int32
	err := superviseRunners(t.Context(), []dispatcherRunner{
		blockingRunner("peer returning nil", &stopped, returnNil),
		{name: "exiting runner", run: returnNil},
		blockingRunner("peer returning canceled", &stopped, returnContextErr),
	})
	if !errors.Is(err, errRunnerStoppedUnexpectedly) {
		t.Fatalf("superviseRunners() = %v, want unexpected exit", err)
	}
	if !strings.Contains(err.Error(), "exiting runner") {
		t.Fatalf("superviseRunners() = %v, want runner name", err)
	}
	if strings.Contains(err.Error(), "peer") {
		t.Fatalf("superviseRunners() = %v, cancelled peers must not be reported", err)
	}
	if got := stopped.Load(); got != 2 {
		t.Fatalf("joined peers = %d, want 2", got)
	}
}

func TestSuperviseRunnersReportsCanceledExitUnderLiveParent(t *testing.T) {
	var stopped atomic.Int32
	err := superviseRunners(t.Context(), []dispatcherRunner{
		{name: "canceled runner", run: func(context.Context) error { return context.Canceled }},
		blockingRunner("peer", &stopped, returnContextErr),
	})
	if !errors.Is(err, errRunnerStoppedUnexpectedly) || errors.Is(err, context.Canceled) {
		t.Fatalf("superviseRunners() = %v, want unexpected exit that is not a cancellation", err)
	}
	if !strings.Contains(err.Error(), "canceled runner") {
		t.Fatalf("superviseRunners() = %v, want runner name", err)
	}
	if got := stopped.Load(); got != 1 {
		t.Fatalf("joined peers = %d, want 1", got)
	}
}

func TestSuperviseRunnersShutsDownCleanlyOnParentCancel(t *testing.T) {
	ctx, cancel := context.WithCancel(t.Context())
	var stopped atomic.Int32
	runners := []dispatcherRunner{
		blockingRunner("returns nil", &stopped, returnNil),
		blockingRunner("returns canceled", &stopped, returnContextErr),
	}
	errc := make(chan error, 1)
	go func() { errc <- superviseRunners(ctx, runners) }()
	cancel()
	if err := <-errc; err != nil {
		t.Fatalf("superviseRunners() = %v, want clean shutdown", err)
	}
	if got := stopped.Load(); got != 2 {
		t.Fatalf("joined runners = %d, want 2", got)
	}
}
