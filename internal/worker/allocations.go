package worker

import (
	"context"
	"errors"
	"log/slog"
	"sync"
	"time"

	"github.com/helmrdotdev/helmr/internal/ids"
	"github.com/helmrdotdev/helmr/internal/workerapi"
)

type AllocationDiscovery interface {
	ListAllocations(context.Context, *workerapi.AllocationIdentity) (workerapi.AllocationListResponse, error)
}

// AllocationOwner retains physical custody across Run errors. Finished means
// local closure and its durable stop observation have both completed.
type AllocationOwner interface {
	Run(context.Context) error
	Finished() bool
}

// AllocationConsumer bounds retained custody separately from physical capacity.
// The reservation ledger remains the authority for VM and quarantine resources.
type AllocationConsumer struct {
	client  AllocationDiscovery
	create  func(workerapi.AllocationIdentity, func(context.Context) error) (AllocationOwner, error)
	limit   int
	poll    time.Duration
	mu      sync.Mutex
	entries map[workerapi.AllocationIdentity]AllocationOwner
	pending []workerapi.AllocationIdentity
	after   *workerapi.AllocationIdentity
}

func NewAllocationConsumer(client AllocationDiscovery, limit int, poll time.Duration, create func(workerapi.AllocationIdentity, func(context.Context) error) (AllocationOwner, error)) (*AllocationConsumer, error) {
	if client == nil || create == nil || limit <= 0 || poll <= 0 {
		return nil, errors.New("allocation discovery, owner factory, positive custody bound and polling interval are required")
	}
	return &AllocationConsumer{client: client, create: create, limit: limit, poll: poll, entries: make(map[workerapi.AllocationIdentity]AllocationOwner)}, nil
}

// RunDiscovery is drain-eligible and must be joined before exclusive finalization.
// It continues scanning when every owner or pending hint slot is occupied.
func (c *AllocationConsumer) RunDiscovery(ctx context.Context) error {
	for ctx.Err() == nil {
		if err := c.discoverPage(ctx); err != nil && ctx.Err() == nil {
			slog.Warn("discover physical allocations", "error", err)
		}
		if err := waitAllocation(ctx, c.poll); err != nil {
			return err
		}
	}
	return ctx.Err()
}

func (c *AllocationConsumer) discoverPage(ctx context.Context) error {
	// Serialize one bounded primary-CP read with retirement. A stale page read
	// before a stop ACK is applied while its owner still exists, never after it
	// retires. Physical work and renewal do not hold this mutex.
	c.mu.Lock()
	defer c.mu.Unlock()
	requestCtx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	page, err := c.client.ListAllocations(requestCtx, c.after)
	if err != nil {
		return err
	}
	if len(page.Allocations) == 0 {
		c.after = nil
		return nil
	}
	previous := c.after
	for _, identity := range page.Allocations {
		if (identity.Kind != "computer" && identity.Kind != "preparation") || identity.Epoch <= 0 || ids.Validate(identity.EnvironmentID) != nil || ids.Validate(identity.OwnerID) != nil || ids.Validate(identity.InstanceID) != nil || (previous != nil && !allocationAfter(identity, *previous)) {
			c.after = nil
			return errors.New("invalid or unordered physical allocation page")
		}
		previous = &identity
	}
	for _, identity := range page.Allocations {
		if c.entries[identity] != nil || len(c.pending) >= c.limit {
			continue
		}
		duplicate := false
		for _, hint := range c.pending {
			if hint == identity {
				duplicate = true
				break
			}
		}
		if !duplicate {
			c.pending = append(c.pending, identity)
		}
	}
	last := page.Allocations[len(page.Allocations)-1]
	c.after = &last
	return nil
}

func allocationAfter(a, b workerapi.AllocationIdentity) bool {
	if a.Kind != b.Kind {
		return a.Kind > b.Kind
	}
	if a.EnvironmentID != b.EnvironmentID {
		return a.EnvironmentID > b.EnvironmentID
	}
	if a.OwnerID != b.OwnerID {
		return a.OwnerID > b.OwnerID
	}
	return a.Epoch > b.Epoch
}

func (c *AllocationConsumer) Claim(ctx context.Context, admission ConsumerAdmission) (Work, bool, error) {
	if admission == nil {
		return nil, false, errors.New("allocation startup admission is required")
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if err := ctx.Err(); err != nil {
		return nil, false, err
	}
	if len(c.entries) >= c.limit || len(c.pending) == 0 {
		return nil, false, nil
	}
	identity := c.pending[0]
	// Construction is effect-free; custody is installed before returning work.
	owner, err := c.create(identity, admission.AdmitAllocatedStart)
	if err != nil {
		return nil, false, err
	}
	if owner == nil {
		return nil, false, errors.New("allocation factory returned no physical owner")
	}
	c.pending = c.pending[1:]
	c.entries[identity] = owner
	return func(ctx context.Context) error {
		delay := c.poll
		for {
			err := owner.Run(ctx)
			if owner.Finished() {
				if err != nil {
					slog.Warn("physical allocation failed before completed cleanup", "kind", identity.Kind, "owner", identity.OwnerID, "epoch", identity.Epoch, "error", err)
				}
				c.mu.Lock()
				delete(c.entries, identity)
				c.mu.Unlock()
				return nil
			}
			if ctx.Err() != nil {
				return ctx.Err()
			}
			var fatal FatalWorkError
			if errors.As(err, &fatal) && fatal.FatalWorker() {
				return err
			}
			if err != nil {
				slog.Warn("physical allocation retains custody", "kind", identity.Kind, "owner", identity.OwnerID, "epoch", identity.Epoch, "error", err)
			}
			if err := waitAllocation(ctx, delay); err != nil {
				// One final cancellation-aware invocation lets the owner join cleanup.
				cleanupErr := owner.Run(ctx)
				return errors.Join(err, cleanupErr)
			}
			if delay < 30*time.Second {
				delay = min(delay*2, 30*time.Second)
			}
		}
	}, true, nil
}

func waitAllocation(ctx context.Context, delay time.Duration) error {
	timer := time.NewTimer(delay)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-timer.C:
		return nil
	}
}
