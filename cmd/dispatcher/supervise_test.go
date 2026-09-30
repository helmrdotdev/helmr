package main

import (
	"context"
	"errors"
	"strings"
	"sync"
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

func TestSuperviseRunnersWrapsRunnerErrorUnderLiveParent(t *testing.T) {
	failure := errors.New("database unavailable")
	var stopped atomic.Int32
	err := superviseRunners(t.Context(), []dispatcherRunner{
		{name: "failing runner", run: func(context.Context) error { return failure }},
		blockingRunner("peer", &stopped, returnContextErr),
	})
	if !errors.Is(err, failure) {
		t.Fatalf("superviseRunners() = %v, want wrapped runner error", err)
	}
	if !strings.Contains(err.Error(), "failing runner") {
		t.Fatalf("superviseRunners() = %v, want runner name", err)
	}
	if errors.Is(err, errRunnerStoppedUnexpectedly) {
		t.Fatalf("superviseRunners() = %v, a runner error is not an unexpected clean exit", err)
	}
	if got := stopped.Load(); got != 1 {
		t.Fatalf("joined peers = %d, want 1", got)
	}
}

func TestSuperviseRunnersJoinsSimultaneousFailures(t *testing.T) {
	firstFailure := errors.New("first failure")
	secondFailure := errors.New("second failure")
	var ready sync.WaitGroup
	ready.Add(2)
	failTogether := func(failure error) func(context.Context) error {
		return func(context.Context) error {
			ready.Done()
			ready.Wait()
			return failure
		}
	}
	var stopped atomic.Int32
	err := superviseRunners(t.Context(), []dispatcherRunner{
		{name: "first runner", run: failTogether(firstFailure)},
		{name: "second runner", run: failTogether(secondFailure)},
		blockingRunner("peer", &stopped, returnContextErr),
	})
	if !errors.Is(err, firstFailure) || !errors.Is(err, secondFailure) {
		t.Fatalf("superviseRunners() = %v, want both failures", err)
	}
	for _, name := range []string{"first runner", "second runner"} {
		if !strings.Contains(err.Error(), name) {
			t.Fatalf("superviseRunners() = %v, want runner name %q", err, name)
		}
	}
	if strings.Contains(err.Error(), "peer") {
		t.Fatalf("superviseRunners() = %v, cancelled peer must not be reported", err)
	}
	if got := stopped.Load(); got != 1 {
		t.Fatalf("joined peers = %d, want 1", got)
	}
}

func TestSuperviseRunnersReportsRunnerErrorDuringParentCancel(t *testing.T) {
	ctx, cancel := context.WithCancel(t.Context())
	failure := errors.New("flush failed")
	var stopped atomic.Int32
	runners := []dispatcherRunner{
		blockingRunner("failing on shutdown", &stopped, func(context.Context) error { return failure }),
		blockingRunner("returns canceled", &stopped, returnContextErr),
	}
	errc := make(chan error, 1)
	go func() { errc <- superviseRunners(ctx, runners) }()
	cancel()
	err := <-errc
	if !errors.Is(err, failure) {
		t.Fatalf("superviseRunners() = %v, want shutdown failure", err)
	}
	if !strings.Contains(err.Error(), "failing on shutdown") {
		t.Fatalf("superviseRunners() = %v, want runner name", err)
	}
	if strings.Contains(err.Error(), "returns canceled") {
		t.Fatalf("superviseRunners() = %v, clean shutdown must not be reported", err)
	}
	if got := stopped.Load(); got != 2 {
		t.Fatalf("joined runners = %d, want 2", got)
	}
}
