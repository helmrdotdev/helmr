package dispatch

import (
	"context"
	"errors"
	"log/slog"
	"time"

	"github.com/helmrdotdev/helmr/internal/pglock"
)

const (
	instanceReconciliationLockName        = "helmr.dispatcher.computer_instance_reconciliation"
	defaultInstanceReconciliationInterval = 5 * time.Second
	defaultInstanceReconciliationTimeout  = 15 * time.Second
	defaultInstanceReconciliationLimit    = int32(50)
)

// InstanceReconciler runs ReconcileComputerInstances in bounded cycles. A
// session-level advisory lock keeps one reconciler active across dispatchers;
// each candidate is still relocked in its own transaction.
type InstanceReconciler struct {
	tryLock   func(context.Context) (instanceReconciliationGuard, bool, error)
	reconcile func(context.Context, int32) (int, error)
	interval  time.Duration
	timeout   time.Duration
	limit     int32
	log       *slog.Logger
}

type instanceReconciliationGuard interface {
	Unlock() error
}

// NewInstanceReconciler holds its singleton lock on one connection from the
// authority's pool; the reconciliation it runs takes other connections from
// that pool.
func NewInstanceReconciler(authority *Authority, log *slog.Logger) (*InstanceReconciler, error) {
	if authority == nil {
		return nil, errors.New("computer instance reconciliation authority is required")
	}
	if log == nil {
		log = slog.Default()
	}
	key := pglock.Key(instanceReconciliationLockName)
	return &InstanceReconciler{
		tryLock: func(ctx context.Context) (instanceReconciliationGuard, bool, error) {
			guard, locked, err := pglock.TryAcquire(ctx, authority.pool, key)
			if err != nil || !locked {
				return nil, locked, err
			}
			return guard, true, nil
		},
		reconcile: authority.ReconcileComputerInstances,
		interval:  defaultInstanceReconciliationInterval,
		timeout:   defaultInstanceReconciliationTimeout,
		limit:     defaultInstanceReconciliationLimit,
		log:       log,
	}, nil
}

func (r *InstanceReconciler) Run(ctx context.Context) error {
	for {
		cycle, cancel := context.WithTimeout(ctx, r.timeout)
		err := r.reconcileCycle(cycle)
		cancel()
		if ctx.Err() != nil {
			return ctx.Err()
		}
		delay := r.interval
		if err != nil {
			r.log.Warn("Computer instance reconciliation failed", "error", err)
			delay = time.Second
		}
		timer := time.NewTimer(delay)
		select {
		case <-ctx.Done():
			timer.Stop()
			return ctx.Err()
		case <-timer.C:
		}
	}
}

func (r *InstanceReconciler) reconcileCycle(ctx context.Context) error {
	guard, locked, err := r.tryLock(ctx)
	if err != nil || !locked {
		return err
	}
	defer func() {
		if err := guard.Unlock(); err != nil {
			r.log.Warn("release Computer instance reconciliation lock failed", "error", err)
		}
	}()
	_, err = r.reconcile(ctx, r.limit)
	return err
}
