package vm

import (
	"context"
	"sync/atomic"
	"testing"
)

type limitedStartConnector struct {
	started chan string
	release chan struct{}
	active  atomic.Int32
	peak    atomic.Int32
}

func (c *limitedStartConnector) start(ctx context.Context, kind string) (Session, error) {
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

func (c *limitedStartConnector) Connect(ctx context.Context, _ ConnectRequest) (Session, error) {
	return c.start(ctx, "connect")
}
func (c *limitedStartConnector) Restore(ctx context.Context, _ RestoreRequest) (Session, error) {
	return c.start(ctx, "restore")
}
func (c *limitedStartConnector) Materialize(ctx context.Context, _ MaterializeRequest) (Session, error) {
	return c.start(ctx, "materialize")
}
func (*limitedStartConnector) CleanupRuntime(context.Context, string) error { return nil }

func TestStartLimiterSharesOneHostWideBudgetAcrossStartKinds(t *testing.T) {
	connector := &limitedStartConnector{started: make(chan string, 3), release: make(chan struct{}, 3)}
	limiter, err := NewStartLimiter(connector, 1)
	if err != nil {
		t.Fatal(err)
	}
	done := make(chan error, 3)
	go func() { _, err := limiter.Connect(context.Background(), ConnectRequest{}); done <- err }()
	go func() { _, err := limiter.Restore(context.Background(), RestoreRequest{}); done <- err }()
	go func() { _, err := limiter.Materialize(context.Background(), MaterializeRequest{}); done <- err }()
	for range 3 {
		<-connector.started
		if got := connector.active.Load(); got != 1 {
			t.Fatalf("active starts = %d, want 1", got)
		}
		connector.release <- struct{}{}
	}
	for range 3 {
		if err := <-done; err != nil {
			t.Fatal(err)
		}
	}
	if got := connector.peak.Load(); got != 1 {
		t.Fatalf("peak starts = %d, want 1", got)
	}
}
