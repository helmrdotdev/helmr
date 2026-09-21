package dispatch

import (
	"context"
	"errors"
	"log/slog"
	"time"

	"github.com/helmrdotdev/helmr/internal/db"
)

const (
	defaultRunLeaseRecoveryInterval = 5 * time.Second
	defaultRunLeaseRecoveryTimeout  = 15 * time.Second
	defaultRunLeaseRecoveryLimit    = int32(100)
	runLeaseRecoveryUnlockTimeout   = 5 * time.Second
)

type RunLeaseRecoverer interface {
	RecoverExpiredRuntimeReservations(context.Context, int32) (int, error)
	RecoverRunExecutionLeases(context.Context, int32) (int, error)
	RecoverExpiredRunResumes(context.Context, int32) ([]db.RecoverExpiredRunResumesRow, error)
}

type RunLeaseRecoveryLock interface {
	TryLock(context.Context) (RunLeaseRecoveryLockGuard, bool, error)
}

type RunLeaseRecoveryLockGuard interface {
	Unlock(context.Context) error
}

type RunLeaseReconciler struct {
	recoverer RunLeaseRecoverer
	lock      RunLeaseRecoveryLock
	interval  time.Duration
	timeout   time.Duration
	limit     int32
	log       *slog.Logger
}

func NewRunLeaseReconciler(recoverer RunLeaseRecoverer, lock RunLeaseRecoveryLock, log *slog.Logger) (*RunLeaseReconciler, error) {
	if recoverer == nil || lock == nil {
		return nil, errors.New("Run lease recoverer and lock are required")
	}
	if log == nil {
		log = slog.Default()
	}
	return &RunLeaseReconciler{
		recoverer: recoverer,
		lock:      lock,
		interval:  defaultRunLeaseRecoveryInterval,
		timeout:   defaultRunLeaseRecoveryTimeout,
		limit:     defaultRunLeaseRecoveryLimit,
		log:       log,
	}, nil
}

func (r *RunLeaseReconciler) Run(ctx context.Context) error {
	for {
		cycle, cancel := context.WithTimeout(ctx, r.timeout)
		err := r.reconcile(cycle)
		cancel()
		if ctx.Err() != nil {
			return ctx.Err()
		}
		delay := r.interval
		if err != nil {
			r.log.Warn("Run lease recovery failed", "error", err)
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

func (r *RunLeaseReconciler) reconcile(ctx context.Context) error {
	guard, locked, err := r.lock.TryLock(ctx)
	if err != nil || !locked {
		return err
	}
	defer func() {
		unlock, cancel := context.WithTimeout(context.WithoutCancel(ctx), runLeaseRecoveryUnlockTimeout)
		defer cancel()
		if err := guard.Unlock(unlock); err != nil {
			r.log.Warn("release Run lease recovery lock failed", "error", err)
		}
	}()
	// Bound each independent recovery lane so backlog cannot starve another.
	executionLimit := (r.limit + 2) / 3
	resumeLimit := (r.limit + 1) / 3
	runtimeLimit := r.limit / 3
	errorsByLane := make(chan error, 3)
	go func() {
		_, err := r.recoverer.RecoverRunExecutionLeases(ctx, executionLimit)
		errorsByLane <- err
	}()
	go func() {
		_, err := r.recoverer.RecoverExpiredRunResumes(ctx, resumeLimit)
		errorsByLane <- err
	}()
	go func() {
		_, err := r.recoverer.RecoverExpiredRuntimeReservations(ctx, runtimeLimit)
		errorsByLane <- err
	}()
	return errors.Join(<-errorsByLane, <-errorsByLane, <-errorsByLane)
}
