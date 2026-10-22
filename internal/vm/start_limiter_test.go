package vm

import (
	"context"
	"errors"
	"sync/atomic"
	"testing"
)

type limitedStartBackend struct {
	started chan string
	release chan struct{}
	active  atomic.Int32
	peak    atomic.Int32
}

func (c *limitedStartBackend) start(ctx context.Context, kind string) (CheckpointableMachine, error) {
	active := c.active.Add(1)
	defer c.active.Add(-1)
	for {
		peak := c.peak.Load()
		if active <= peak || c.peak.CompareAndSwap(peak, active) {
			break
		}
	}
	c.started <- kind
	select {
	case <-c.release:
		return nil, nil
	case <-ctx.Done():
		return nil, ctx.Err()
	}
}

func (c *limitedStartBackend) Restore(ctx context.Context, _ RestoreRequest) (CheckpointableMachine, error) {
	return c.start(ctx, "restore")
}
func (c *limitedStartBackend) Materialize(ctx context.Context, _ MaterializeRequest) (CheckpointableMachine, error) {
	return c.start(ctx, "materialize")
}
func (c *limitedStartBackend) PrepareBootSeed(ctx context.Context, _ MaterializeRequest) (BootSeedCapture, error) {
	_, err := c.start(ctx, "boot_seed")
	return nil, err
}
func (c *limitedStartBackend) PrepareComputer(ctx context.Context, _ MaterializeRequest) (PreparedComputer, error) {
	_, err := c.start(ctx, "prepared_computer")
	return nil, err
}
func (*limitedStartBackend) BootSeedMatches(MaterializeRequest, []byte) (bool, error) {
	return false, nil
}
func (*limitedStartBackend) Cleanup(context.Context, Owner) error { return nil }

func TestStartLimiterSharesOneHostWideBudgetAcrossStartKinds(t *testing.T) {
	backend := &limitedStartBackend{started: make(chan string, 4), release: make(chan struct{}, 4)}
	limiter, err := NewStartLimiter(backend, 1)
	if err != nil {
		t.Fatal(err)
	}
	done := make(chan error, 4)
	go func() { _, err := limiter.Restore(context.Background(), RestoreRequest{}); done <- err }()
	go func() { _, err := limiter.Materialize(context.Background(), MaterializeRequest{}); done <- err }()
	go func() { _, err := limiter.PrepareBootSeed(context.Background(), MaterializeRequest{}); done <- err }()
	go func() { _, err := limiter.PrepareComputer(context.Background(), MaterializeRequest{}); done <- err }()
	for range 4 {
		<-backend.started
		if got := backend.active.Load(); got != 1 {
			t.Fatalf("active starts = %d, want 1", got)
		}
		backend.release <- struct{}{}
	}
	for range 4 {
		if err := <-done; err != nil {
			t.Fatal(err)
		}
	}
	if got := backend.peak.Load(); got != 1 {
		t.Fatalf("peak starts = %d, want 1", got)
	}
}

func TestStartLimiterCanceledPreparedStartDoesNotReachBackend(t *testing.T) {
	backend := &limitedStartBackend{started: make(chan string, 1), release: make(chan struct{})}
	limiter, err := NewStartLimiter(backend, 1)
	if err != nil {
		t.Fatal(err)
	}
	done := make(chan error, 1)
	go func() { _, err := limiter.Restore(t.Context(), RestoreRequest{}); done <- err }()
	<-backend.started
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	if _, err := limiter.PrepareComputer(ctx, MaterializeRequest{}); !errors.Is(err, context.Canceled) {
		t.Fatal(err)
	}
	select {
	case kind := <-backend.started:
		t.Fatalf("canceled start reached backend: %s", kind)
	default:
	}
	close(backend.release)
	if err := <-done; err != nil {
		t.Fatal(err)
	}
}
