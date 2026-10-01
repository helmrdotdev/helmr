package vm

import (
	"context"
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
func (*limitedStartBackend) Cleanup(context.Context, Owner) error { return nil }

func TestStartLimiterSharesOneHostWideBudgetAcrossStartKinds(t *testing.T) {
	backend := &limitedStartBackend{started: make(chan string, 2), release: make(chan struct{}, 2)}
	limiter, err := NewStartLimiter(backend, 1)
	if err != nil {
		t.Fatal(err)
	}
	done := make(chan error, 2)
	go func() { _, err := limiter.Restore(context.Background(), RestoreRequest{}); done <- err }()
	go func() { _, err := limiter.Materialize(context.Background(), MaterializeRequest{}); done <- err }()
	for range 2 {
		<-backend.started
		if got := backend.active.Load(); got != 1 {
			t.Fatalf("active starts = %d, want 1", got)
		}
		backend.release <- struct{}{}
	}
	for range 2 {
		if err := <-done; err != nil {
			t.Fatal(err)
		}
	}
	if got := backend.peak.Load(); got != 1 {
		t.Fatalf("peak starts = %d, want 1", got)
	}
}
