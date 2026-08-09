package dispatch

import (
	"context"
	"errors"
	"log/slog"
	"time"

	"github.com/helmrdotdev/helmr/internal/db"
)

const (
	defaultRunResumeRecoveryInterval = 5 * time.Second
	defaultRunResumeRecoveryTimeout  = 15 * time.Second
	defaultRunResumeRecoveryLimit    = int32(100)
	runResumeRecoveryUnlockTimeout   = 5 * time.Second
)

type RunResumeRecoverer interface {
	RecoverExpiredRunResumes(context.Context, int32) ([]db.RecoverExpiredRunResumesRow, error)
}

type RunResumeRecoveryLock interface {
	TryLock(context.Context) (RunResumeRecoveryLockGuard, bool, error)
}

type RunResumeRecoveryLockGuard interface {
	Unlock(context.Context) error
}

type RunResumeReconciler struct {
	recoverer RunResumeRecoverer
	lock      RunResumeRecoveryLock
	interval  time.Duration
	timeout   time.Duration
	limit     int32
	log       *slog.Logger
}

func NewRunResumeReconciler(recoverer RunResumeRecoverer, lock RunResumeRecoveryLock, log *slog.Logger) (*RunResumeReconciler, error) {
	if recoverer == nil || lock == nil {
		return nil, errors.New("run resume recoverer and lock are required")
	}
	if log == nil {
		log = slog.Default()
	}
	return &RunResumeReconciler{
		recoverer: recoverer,
		lock:      lock,
		interval:  defaultRunResumeRecoveryInterval,
		timeout:   defaultRunResumeRecoveryTimeout,
		limit:     defaultRunResumeRecoveryLimit,
		log:       log,
	}, nil
}

func (r *RunResumeReconciler) Run(ctx context.Context) error {
	for {
		cycle, cancel := context.WithTimeout(ctx, r.timeout)
		err := r.reconcile(cycle)
		cancel()
		if ctx.Err() != nil {
			return ctx.Err()
		}
		delay := r.interval
		if err != nil {
			r.log.Warn("run resume recovery failed", "error", err)
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

func (r *RunResumeReconciler) reconcile(ctx context.Context) error {
	guard, locked, err := r.lock.TryLock(ctx)
	if err != nil || !locked {
		return err
	}
	defer func() {
		unlock, cancel := context.WithTimeout(context.WithoutCancel(ctx), runResumeRecoveryUnlockTimeout)
		defer cancel()
		if err := guard.Unlock(unlock); err != nil {
			r.log.Warn("release run resume recovery lock failed", "error", err)
		}
	}()
	_, err = r.recoverer.RecoverExpiredRunResumes(ctx, r.limit)
	return err
}
