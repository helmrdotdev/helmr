package dispatch

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"time"

	"github.com/helmrdotdev/helmr/internal/db"
	"github.com/jackc/pgx/v5/pgtype"
)

const (
	DefaultQueueReconcileInterval                = 5 * time.Second
	DefaultQueueReconcileOrgLimit                = int32(500)
	DefaultQueueReconcileRunLimit                = int32(100)
	DefaultQueueReconcileConsecutiveFailureLimit = 3
	queueReconcileUnlockTimeout                  = 5 * time.Second
)

type QueueReconcilerStore interface {
	ListOrganizationIDsPage(context.Context, db.ListOrganizationIDsPageParams) ([]pgtype.UUID, error)
}

type QueueEnqueuer interface {
	ReconcileOrgQueue(context.Context, pgtype.UUID, int32) (QueueReconcileStats, error)
}

type QueueReconcileLock interface {
	TryLock(ctx context.Context) (QueueReconcileLockGuard, bool, error)
}

type QueueReconcileLockGuard interface {
	Store(fallback QueueReconcilerStore) QueueReconcilerStore
	Unlock(ctx context.Context) error
}

type QueueReconciler struct {
	store        QueueReconcilerStore
	enqueuer     QueueEnqueuer
	lock         QueueReconcileLock
	every        time.Duration
	orgLimit     int32
	runLimit     int32
	failureLimit int
	log          *slog.Logger
}

type QueueReconcilerOption func(*QueueReconciler)

func WithQueueReconcileInterval(every time.Duration) QueueReconcilerOption {
	return func(reconciler *QueueReconciler) {
		reconciler.every = every
	}
}

func WithQueueReconcileLimits(orgLimit int32, runLimit int32) QueueReconcilerOption {
	return func(reconciler *QueueReconciler) {
		reconciler.orgLimit = orgLimit
		reconciler.runLimit = runLimit
	}
}

func WithQueueReconcileConsecutiveFailureLimit(limit int) QueueReconcilerOption {
	return func(reconciler *QueueReconciler) {
		reconciler.failureLimit = limit
	}
}

func WithQueueReconcileLogger(log *slog.Logger) QueueReconcilerOption {
	return func(reconciler *QueueReconciler) {
		reconciler.log = log
	}
}

func WithQueueReconcileLock(lock QueueReconcileLock) QueueReconcilerOption {
	return func(reconciler *QueueReconciler) {
		reconciler.lock = lock
	}
}

func NewQueueReconciler(store QueueReconcilerStore, enqueuer QueueEnqueuer, opts ...QueueReconcilerOption) (*QueueReconciler, error) {
	if store == nil {
		return nil, errors.New("queue reconciler store is required")
	}
	if enqueuer == nil {
		return nil, errors.New("queue reconciler enqueuer is required")
	}
	reconciler := &QueueReconciler{
		store:        store,
		enqueuer:     enqueuer,
		every:        DefaultQueueReconcileInterval,
		orgLimit:     DefaultQueueReconcileOrgLimit,
		runLimit:     DefaultQueueReconcileRunLimit,
		failureLimit: DefaultQueueReconcileConsecutiveFailureLimit,
		log:          slog.Default(),
	}
	for _, opt := range opts {
		opt(reconciler)
	}
	if reconciler.every <= 0 {
		return nil, errors.New("queue reconcile interval must be positive")
	}
	if reconciler.orgLimit <= 0 {
		return nil, errors.New("queue reconcile org limit must be positive")
	}
	if reconciler.runLimit <= 0 {
		return nil, errors.New("queue reconcile run limit must be positive")
	}
	if reconciler.failureLimit <= 0 {
		return nil, errors.New("queue reconcile consecutive failure limit must be positive")
	}
	if reconciler.log == nil {
		reconciler.log = slog.Default()
	}
	return reconciler, nil
}

func (r *QueueReconciler) Run(ctx context.Context) error {
	timer := time.NewTimer(0)
	defer timer.Stop()
	consecutiveFailures := 0
	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-timer.C:
		}
		if err := r.ReconcileOnce(ctx); err != nil {
			if ctx.Err() != nil {
				return ctx.Err()
			}
			consecutiveFailures++
			r.log.Warn("queue reconcile failed", "error", err, "consecutive_failures", consecutiveFailures)
			if consecutiveFailures >= r.failureLimit {
				return fmt.Errorf("queue reconcile failed %d consecutive times: %w", consecutiveFailures, err)
			}
		} else {
			consecutiveFailures = 0
		}
		timer.Reset(r.every)
	}
}

func (r *QueueReconciler) ReconcileOnce(ctx context.Context) error {
	var guard QueueReconcileLockGuard
	store := r.store
	if r.lock != nil {
		var locked bool
		var err error
		guard, locked, err = r.lock.TryLock(ctx)
		if err != nil {
			return err
		}
		if !locked {
			r.log.Debug("queue reconcile lock is held by another instance")
			return nil
		}
		store = guard.Store(r.store)
		defer func() {
			unlockCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), queueReconcileUnlockTimeout)
			defer cancel()
			if err := guard.Unlock(unlockCtx); err != nil {
				r.log.Warn("release queue reconcile lock failed", "error", err)
			}
		}()
	}
	var problems []error
	var afterID pgtype.UUID
	for {
		orgIDs, err := store.ListOrganizationIDsPage(ctx, db.ListOrganizationIDsPageParams{
			AfterID:  afterID,
			RowLimit: r.orgLimit,
		})
		if err != nil {
			return err
		}
		for _, orgID := range orgIDs {
			stats, err := r.enqueuer.ReconcileOrgQueue(ctx, orgID, r.runLimit)
			if err != nil {
				problems = append(problems, err)
			}
			if stats.Scanned > 0 || stats.Failed > 0 {
				r.log.Info("queue reconcile org", "org_id", orgID, "scanned", stats.Scanned, "enqueued", stats.Enqueued, "skipped", stats.Skipped, "failed", stats.Failed)
			}
		}
		if len(orgIDs) < int(r.orgLimit) {
			break
		}
		afterID = orgIDs[len(orgIDs)-1]
	}
	return errors.Join(problems...)
}
