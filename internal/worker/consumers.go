package worker

import (
	"context"
	"errors"
	"fmt"
	"maps"
	"sync"

	"github.com/helmrdotdev/helmr/internal/httpclient"
	"github.com/helmrdotdev/helmr/internal/workerapi"
)

type runConsumer struct {
	runner              *Runner
	mu                  sync.Mutex
	active              map[workerapi.RunLeaseWork]struct{}
	stale               map[workerapi.RunLeaseWork]struct{}
	discovery           *runDiscovery
	discoveryGeneration uint64
}

type workspaceConsumer struct{ runner *Runner }

type fatalWorkerError struct{ err error }

func (err *fatalWorkerError) Error() string     { return err.err.Error() }
func (err *fatalWorkerError) Unwrap() error     { return err.err }
func (err *fatalWorkerError) FatalWorker() bool { return true }

func NewRunConsumer(runner *Runner) Consumer {
	return &runConsumer{
		runner: runner,
		active: make(map[workerapi.RunLeaseWork]struct{}),
		stale:  make(map[workerapi.RunLeaseWork]struct{}),
	}
}

func NewWorkspaceConsumer(runner *Runner) Consumer { return workspaceConsumer{runner: runner} }

func (c *runConsumer) acquireRunLease(ctx context.Context) (workerapi.RunLeaseWork, error) {
	c.mu.Lock()
	if err := ctx.Err(); err != nil {
		c.mu.Unlock()
		return workerapi.RunLeaseWork{}, err
	}
	batch := c.discovery
	if batch != nil {
		c.mu.Unlock()
		select {
		case <-ctx.Done():
			return workerapi.RunLeaseWork{}, ctx.Err()
		case <-batch.done:
			c.mu.Lock()
			defer c.mu.Unlock()
			if batch.err != nil {
				return workerapi.RunLeaseWork{}, batch.err
			}
			return c.selectRunLeaseLocked(ctx, batch)
		}
	}
	c.discoveryGeneration++
	batch = &runDiscovery{
		done: make(chan struct{}), generation: c.discoveryGeneration,
		excluded: maps.Clone(c.active),
	}
	c.discovery = batch
	c.mu.Unlock()

	response, err := c.runner.client.DiscoverRunLeases(ctx)
	c.mu.Lock()
	defer c.mu.Unlock()
	if err == nil {
		visible := make(map[workerapi.RunLeaseWork]struct{}, len(response.Items))
		for _, work := range response.Items {
			if work.LeaseID == "" || work.LeaseSequence <= 0 {
				err = errors.New("discovered run lease identity is invalid")
				break
			}
			visible[work] = struct{}{}
		}
		if err == nil {
			for work := range c.stale {
				if _, stillVisible := visible[work]; !stillVisible {
					delete(c.stale, work)
				}
			}
			batch.items = response.Items
		}
	}
	if err != nil {
		batch.err = fmt.Errorf("discover run leases: %w", err)
	}
	c.discovery = nil
	close(batch.done)
	if batch.err != nil {
		return workerapi.RunLeaseWork{}, batch.err
	}
	return c.selectRunLeaseLocked(ctx, batch)
}

type runDiscovery struct {
	done       chan struct{}
	generation uint64
	items      []workerapi.RunLeaseWork
	next       int
	excluded   map[workerapi.RunLeaseWork]struct{}
	err        error
}

// The caller holds c.mu so publication and the initiator's reservation stay atomic.
func (c *runConsumer) selectRunLeaseLocked(ctx context.Context, batch *runDiscovery) (workerapi.RunLeaseWork, error) {
	if err := ctx.Err(); err != nil {
		return workerapi.RunLeaseWork{}, err
	}
	// A delayed caller must not replay an observation superseded by a new discovery.
	if batch.generation != c.discoveryGeneration {
		return workerapi.RunLeaseWork{}, nil
	}
	for batch.next < len(batch.items) {
		work := batch.items[batch.next]
		batch.next++
		// Retirement during discovery must not make its old snapshot eligible again.
		if _, excluded := batch.excluded[work]; excluded {
			continue
		}
		if _, running := c.active[work]; running {
			continue
		}
		if _, rejected := c.stale[work]; rejected {
			continue
		}
		batch.excluded[work] = struct{}{}
		c.active[work] = struct{}{}
		return work, nil
	}
	return workerapi.RunLeaseWork{}, nil
}

func (c *runConsumer) Claim(ctx context.Context) (Work, bool, error) {
	selected, err := c.acquireRunLease(ctx)
	if err != nil {
		return nil, false, err
	}
	if selected.LeaseID == "" {
		return nil, false, nil
	}
	return func(workCtx context.Context) error {
		stale := false
		defer func() {
			c.mu.Lock()
			delete(c.active, selected)
			if stale {
				c.stale[selected] = struct{}{}
			}
			c.mu.Unlock()
		}()
		if err := c.runner.runLeaseExecutor.ExecuteRunLease(workCtx, selected); err != nil {
			if isStaleLease(err) {
				if c.runner.log != nil {
					attributes := []any{
						"run_lease_id", selected.LeaseID,
						"lease_sequence", selected.LeaseSequence,
					}
					var responseError *httpclient.Error
					if errors.As(err, &responseError) {
						attributes = append(attributes, "code", responseError.Code)
						if len(responseError.Details) > 0 {
							attributes = append(attributes, "details", string(responseError.Details))
						}
					}
					c.runner.log.Warn("run lease execution lost its authority", attributes...)
				}
				stale = true
				return nil
			}
			return fmt.Errorf("execute run lease %s/%d: %w", selected.LeaseID, selected.LeaseSequence, err)
		}
		return nil
	}, true, nil
}

func (c workspaceConsumer) Claim(ctx context.Context) (Work, bool, error) {
	r := c.runner
	if r.materializer == nil {
		return nil, false, nil
	}
	claimed, err := r.client.ClaimWorkspaceMount(ctx, r.capabilities)
	if err != nil {
		return nil, false, fmt.Errorf("claim workspace mount: %w", err)
	}
	if claimed.Mount == nil {
		return nil, false, nil
	}
	mount := *claimed.Mount
	return func(workCtx context.Context) error {
		return r.materializer.RunWorkspaceMount(workCtx, mount, r.client)
	}, true, nil
}
