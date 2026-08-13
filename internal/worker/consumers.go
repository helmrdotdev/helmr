package worker

import (
	"context"
	"errors"
	"fmt"
	"sync"

	"github.com/helmrdotdev/helmr/internal/workerapi"
)

type runConsumer struct {
	runner *Runner
	mu     sync.Mutex
	active map[workerapi.RunLeaseWork]struct{}
}

type workspaceConsumer struct{ runner *Runner }

type fatalWorkerError struct{ err error }

func (err *fatalWorkerError) Error() string     { return err.err.Error() }
func (err *fatalWorkerError) Unwrap() error     { return err.err }
func (err *fatalWorkerError) FatalWorker() bool { return true }

func NewRunConsumer(runner *Runner) Consumer {
	return &runConsumer{runner: runner, active: make(map[workerapi.RunLeaseWork]struct{})}
}

func NewWorkspaceConsumer(runner *Runner) Consumer { return workspaceConsumer{runner: runner} }

func (c *runConsumer) Claim(ctx context.Context) (Work, bool, error) {
	discovered, err := c.runner.client.DiscoverRunLeases(ctx)
	if err != nil {
		return nil, false, fmt.Errorf("discover run leases: %w", err)
	}
	c.mu.Lock()
	var selected workerapi.RunLeaseWork
	for _, work := range discovered.Items {
		if work.LeaseID == "" || work.LeaseSequence <= 0 {
			c.mu.Unlock()
			return nil, false, errors.New("discovered run lease identity is invalid")
		}
		if _, running := c.active[work]; running {
			continue
		}
		selected = work
		c.active[work] = struct{}{}
		break
	}
	c.mu.Unlock()
	if selected.LeaseID == "" {
		return nil, false, nil
	}
	return func(workCtx context.Context) error {
		defer func() {
			c.mu.Lock()
			delete(c.active, selected)
			c.mu.Unlock()
		}()
		if err := c.runner.runLeaseExecutor.ExecuteRunLease(workCtx, selected); err != nil {
			if isStaleLease(err) {
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
