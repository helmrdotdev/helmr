package computerhost

import (
	"context"
	"sync"
	"time"
)

// allocationWake broadcasts a hint. Waiters snapshot before their durable read,
// so a notification during that read remains observable when they wait. The
// protected operation, never the hint, supplies authority and work identity.
type allocationWake struct {
	mu      sync.Mutex
	changed chan struct{}
}

func (w *allocationWake) snapshot() <-chan struct{} {
	if w == nil {
		return nil
	}
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.changed == nil {
		w.changed = make(chan struct{})
	}
	return w.changed
}
func (w *allocationWake) notify() {
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.changed != nil {
		close(w.changed)
	}
	w.changed = make(chan struct{})
}
func waitAllocationWake(ctx context.Context, wake <-chan struct{}, delay time.Duration) error {
	timer := time.NewTimer(delay)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return context.Cause(ctx)
	case <-wake:
		return nil
	case <-timer.C:
		return nil
	}
}

func (o *ComputerAllocationOwner) Wake()    { o.workWake.notify(); o.WakeSaves() }
func (o *PreparationAllocationOwner) Wake() { o.workWake.notify() }

func (o *BootSeedAllocationOwner) Wake() { o.workWake.notify() }
