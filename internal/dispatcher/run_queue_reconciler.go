package dispatcher

import (
	"context"
	"errors"
	"log/slog"
	"time"

	"github.com/helmrdotdev/helmr/internal/db"
	"github.com/helmrdotdev/helmr/internal/runqueue/publisher"
	"github.com/jackc/pgx/v5/pgtype"
)

const (
	DefaultRunQueueReconcileInterval = 5 * time.Second
	DefaultRunQueueReconcileOrgLimit = int32(500)
	DefaultRunQueueReconcileRunLimit = int32(100)
	runQueueReconcileUnlockTimeout   = 5 * time.Second
)

type RunQueueReconcilerStore interface {
	ListOrganizationIDsPage(context.Context, db.ListOrganizationIDsPageParams) ([]pgtype.UUID, error)
}

type RunQueuePublisher interface {
	ReconcileOrg(context.Context, pgtype.UUID, int32) (publisher.ReconcileStats, error)
}

type RunQueueReconcileLock interface {
	TryLock(ctx context.Context) (RunQueueReconcileLockGuard, bool, error)
}

type RunQueueReconcileLockGuard interface {
	Store(fallback RunQueueReconcilerStore) RunQueueReconcilerStore
	Unlock(ctx context.Context) error
}

type RunQueueReconciler struct {
	store     RunQueueReconcilerStore
	publisher RunQueuePublisher
	lock      RunQueueReconcileLock
	every     time.Duration
	orgLimit  int32
	runLimit  int32
	log       *slog.Logger
}

type RunQueueReconcilerOption func(*RunQueueReconciler)

func WithRunQueueReconcileInterval(every time.Duration) RunQueueReconcilerOption {
	return func(reconciler *RunQueueReconciler) {
		reconciler.every = every
	}
}

func WithRunQueueReconcileLimits(orgLimit int32, runLimit int32) RunQueueReconcilerOption {
	return func(reconciler *RunQueueReconciler) {
		reconciler.orgLimit = orgLimit
		reconciler.runLimit = runLimit
	}
}

func WithRunQueueReconcileLogger(log *slog.Logger) RunQueueReconcilerOption {
	return func(reconciler *RunQueueReconciler) {
		reconciler.log = log
	}
}

func WithRunQueueReconcileLock(lock RunQueueReconcileLock) RunQueueReconcilerOption {
	return func(reconciler *RunQueueReconciler) {
		reconciler.lock = lock
	}
}

func NewRunQueueReconciler(store RunQueueReconcilerStore, runPublisher RunQueuePublisher, opts ...RunQueueReconcilerOption) (*RunQueueReconciler, error) {
	if store == nil {
		return nil, errors.New("run queue reconciler store is required")
	}
	if runPublisher == nil {
		return nil, errors.New("run queue reconciler publisher is required")
	}
	reconciler := &RunQueueReconciler{
		store:     store,
		publisher: runPublisher,
		every:     DefaultRunQueueReconcileInterval,
		orgLimit:  DefaultRunQueueReconcileOrgLimit,
		runLimit:  DefaultRunQueueReconcileRunLimit,
		log:       slog.Default(),
	}
	for _, opt := range opts {
		opt(reconciler)
	}
	if reconciler.every <= 0 {
		return nil, errors.New("run queue reconcile interval must be positive")
	}
	if reconciler.orgLimit <= 0 {
		return nil, errors.New("run queue reconcile org limit must be positive")
	}
	if reconciler.runLimit <= 0 {
		return nil, errors.New("run queue reconcile run limit must be positive")
	}
	if reconciler.log == nil {
		reconciler.log = slog.Default()
	}
	return reconciler, nil
}

func (r *RunQueueReconciler) Run(ctx context.Context) error {
	timer := time.NewTimer(0)
	defer timer.Stop()
	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-timer.C:
		}
		if err := r.ReconcileOnce(ctx); err != nil {
			r.log.Warn("run queue reconcile failed", "error", err)
		}
		timer.Reset(r.every)
	}
}

func (r *RunQueueReconciler) ReconcileOnce(ctx context.Context) error {
	var guard RunQueueReconcileLockGuard
	store := r.store
	if r.lock != nil {
		var locked bool
		var err error
		guard, locked, err = r.lock.TryLock(ctx)
		if err != nil {
			return err
		}
		if !locked {
			r.log.Debug("run queue reconcile lock is held by another instance")
			return nil
		}
		store = guard.Store(r.store)
		defer func() {
			unlockCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), runQueueReconcileUnlockTimeout)
			defer cancel()
			if err := guard.Unlock(unlockCtx); err != nil {
				r.log.Warn("release run queue reconcile lock failed", "error", err)
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
			stats, err := r.publisher.ReconcileOrg(ctx, orgID, r.runLimit)
			if err != nil {
				problems = append(problems, err)
			}
			if stats.Scanned > 0 || stats.Failed > 0 {
				r.log.Info("run queue reconcile org", "org_id", orgID, "scanned", stats.Scanned, "enqueued", stats.Enqueued, "skipped", stats.Skipped, "failed", stats.Failed)
			}
		}
		if len(orgIDs) < int(r.orgLimit) {
			break
		}
		afterID = orgIDs[len(orgIDs)-1]
	}
	return errors.Join(problems...)
}
