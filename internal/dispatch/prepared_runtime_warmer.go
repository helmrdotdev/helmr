package dispatch

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"time"
)

const (
	DefaultRuntimePrepareInterval                = 5 * time.Second
	DefaultRuntimePrepareLimit                   = int32(20)
	DefaultRuntimePrepareConsecutiveFailureLimit = 3
	preparedRuntimeWarmUnlockTimeout             = 5 * time.Second
)

type RuntimePreparerStore interface {
	ReconcilePreparedRuntimeSupply(context.Context, int32, int32) ([]PreparedRuntimeWake, error)
}

type RuntimePrepareLock interface {
	TryLock(ctx context.Context) (RuntimePrepareLockGuard, bool, error)
}

type RuntimePrepareLockGuard interface {
	Unlock(ctx context.Context) error
}

type RuntimePreparer struct {
	store        RuntimePreparerStore
	lock         RuntimePrepareLock
	every        time.Duration
	targetCount  int32
	limit        int32
	failureLimit int
	log          *slog.Logger
	wakes        WorkerWakePublisher
}

type RuntimePreparerOption func(*RuntimePreparer)

func WithRuntimePrepareInterval(every time.Duration) RuntimePreparerOption {
	return func(p *RuntimePreparer) { p.every = every }
}

func WithRuntimePrepareTarget(target int32) RuntimePreparerOption {
	return func(p *RuntimePreparer) { p.targetCount = target }
}

func WithRuntimePrepareLimit(limit int32) RuntimePreparerOption {
	return func(p *RuntimePreparer) { p.limit = limit }
}

func WithRuntimePrepareConsecutiveFailureLimit(limit int) RuntimePreparerOption {
	return func(p *RuntimePreparer) { p.failureLimit = limit }
}

func WithRuntimePrepareLogger(log *slog.Logger) RuntimePreparerOption {
	return func(p *RuntimePreparer) { p.log = log }
}

func WithRuntimePrepareLock(lock RuntimePrepareLock) RuntimePreparerOption {
	return func(p *RuntimePreparer) { p.lock = lock }
}

func WithRuntimePrepareWakePublisher(wakes WorkerWakePublisher) RuntimePreparerOption {
	return func(p *RuntimePreparer) { p.wakes = wakes }
}

func NewRuntimePreparer(store RuntimePreparerStore, opts ...RuntimePreparerOption) (*RuntimePreparer, error) {
	if store == nil {
		return nil, errors.New("prepared runtime store is required")
	}
	p := &RuntimePreparer{store: store, every: DefaultRuntimePrepareInterval,
		limit: DefaultRuntimePrepareLimit, failureLimit: DefaultRuntimePrepareConsecutiveFailureLimit,
		log: slog.Default()}
	for _, opt := range opts {
		opt(p)
	}
	if p.every <= 0 || p.limit <= 0 || p.failureLimit <= 0 || p.targetCount < 0 {
		return nil, errors.New("prepared runtime configuration is invalid")
	}
	return p, nil
}

func (p *RuntimePreparer) Run(ctx context.Context) error {
	timer := time.NewTimer(0)
	defer timer.Stop()
	consecutiveFailures := 0
	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-timer.C:
			if err := p.Reconcile(ctx); err != nil && !errors.Is(err, context.Canceled) {
				consecutiveFailures++
				if consecutiveFailures >= p.failureLimit {
					return fmt.Errorf("reconcile prepared runtime intent after %d consecutive failures: %w", consecutiveFailures, err)
				}
				p.log.Warn("prepared runtime reconciliation retry", "failure_count", consecutiveFailures, "error", err)
				timer.Reset(p.every * time.Duration(consecutiveFailures+1))
				continue
			}
			consecutiveFailures = 0
			timer.Reset(p.every)
		}
	}
}

func (p *RuntimePreparer) Reconcile(ctx context.Context) error {
	if p.targetCount == 0 {
		return nil
	}
	store := p.store
	if p.lock == nil {
		created, err := store.ReconcilePreparedRuntimeSupply(ctx, p.targetCount, p.limit)
		if len(created) > 0 {
			p.log.Info("prepared runtime supply reconciled", "created", len(created), "target", p.targetCount)
		}
		return errors.Join(err, p.publishPreparedRuntimeWakes(ctx, created))
	}
	guard, locked, err := p.lock.TryLock(ctx)
	if err != nil || !locked {
		return err
	}
	created, reconcileErr := p.store.ReconcilePreparedRuntimeSupply(ctx, p.targetCount, p.limit)
	if len(created) > 0 {
		p.log.Info("prepared runtime supply reconciled", "created", len(created), "target", p.targetCount)
	}
	wakeErr := p.publishPreparedRuntimeWakes(ctx, created)
	unlockCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), preparedRuntimeWarmUnlockTimeout)
	defer cancel()
	return errors.Join(reconcileErr, wakeErr, guard.Unlock(unlockCtx))
}

func (p *RuntimePreparer) publishPreparedRuntimeWakes(ctx context.Context, created []PreparedRuntimeWake) error {
	if p.wakes == nil {
		return nil
	}
	var problems []error
	for _, wake := range created {
		if err := p.wakes.PublishWorkerWake(ctx, WorkerWake{Domain: "runtime", WorkerID: wake.WorkerInstanceID,
			WorkerEpoch: wake.WorkerEpoch, RuntimeID: wake.RuntimeInstanceID, AuthorityID: wake.RuntimeInstanceID}); err != nil {
			problems = append(problems, fmt.Errorf("publish prepared runtime wake: %w", err))
		}
	}
	return errors.Join(problems...)
}
