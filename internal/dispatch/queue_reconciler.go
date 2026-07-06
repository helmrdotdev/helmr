package dispatch

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"time"

	"github.com/helmrdotdev/helmr/internal/db"
)

const (
	DefaultQueueReconcileInterval                = 5 * time.Second
	DefaultQueueReconcileScopeLimit              = int32(500)
	DefaultQueueReconcileRunLimit                = int32(100)
	DefaultQueueReconcileConsecutiveFailureLimit = 3
	queueReconcileUnlockTimeout                  = 5 * time.Second
)

type QueueReconcilerStore interface {
	ListQueuedRunCandidateScopes(context.Context, db.ListQueuedRunCandidateScopesParams) ([]db.ListQueuedRunCandidateScopesRow, error)
}

type QueueEnqueuer interface {
	ReconcileQueueScope(context.Context, QueueScope, int32) (QueueReconcileStats, error)
}

type QueueReconcileLock interface {
	TryLock(ctx context.Context) (QueueReconcileLockGuard, bool, error)
}

type QueueReconcileLockGuard interface {
	Store(fallback QueueReconcilerStore) QueueReconcilerStore
	Unlock(ctx context.Context) error
}

type QueueReconciler struct {
	store         QueueReconcilerStore
	enqueuer      QueueEnqueuer
	lock          QueueReconcileLock
	selector      QueueScopeSelector
	workerGroupID string
	every         time.Duration
	scopeLimit    int32
	runLimit      int32
	failureLimit  int
	log           *slog.Logger
}

type QueueReconcilerOption func(*QueueReconciler)

func WithQueueReconcileInterval(every time.Duration) QueueReconcilerOption {
	return func(reconciler *QueueReconciler) {
		reconciler.every = every
	}
}

func WithQueueReconcileLimits(scopeLimit int32, runLimit int32) QueueReconcilerOption {
	return func(reconciler *QueueReconciler) {
		reconciler.scopeLimit = scopeLimit
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

func WithQueueReconcileScopeSelector(selector QueueScopeSelector) QueueReconcilerOption {
	return func(reconciler *QueueReconciler) {
		reconciler.selector = selector
	}
}

func WithQueueReconcileWorkerGroupID(workerGroupID string) QueueReconcilerOption {
	return func(reconciler *QueueReconciler) {
		reconciler.workerGroupID = strings.TrimSpace(workerGroupID)
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
		selector:     RoundRobinQueueScopeSelector{},
		every:        DefaultQueueReconcileInterval,
		scopeLimit:   DefaultQueueReconcileScopeLimit,
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
	if reconciler.scopeLimit <= 0 {
		return nil, errors.New("queue reconcile scope limit must be positive")
	}
	if reconciler.runLimit <= 0 {
		return nil, errors.New("queue reconcile run limit must be positive")
	}
	if reconciler.failureLimit <= 0 {
		return nil, errors.New("queue reconcile consecutive failure limit must be positive")
	}
	if reconciler.workerGroupID == "" {
		return nil, errors.New("queue reconcile worker group id is required")
	}
	if reconciler.log == nil {
		reconciler.log = slog.Default()
	}
	if reconciler.selector == nil {
		return nil, errors.New("queue reconcile scope selector is required")
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
	scanSeed := time.Now().UTC().Format(time.RFC3339Nano)
	var afterSortKey string
	var afterRow db.ListQueuedRunCandidateScopesRow
	for {
		rows, err := store.ListQueuedRunCandidateScopes(ctx, db.ListQueuedRunCandidateScopesParams{
			WorkerGroupID:      r.workerGroupID,
			AfterSortKey:       afterSortKey,
			AfterOrgID:         afterRow.OrgID,
			AfterWorkerGroupID: afterRow.WorkerGroupID,
			AfterProjectID:     afterRow.ProjectID,
			AfterEnvironmentID: afterRow.EnvironmentID,
			AfterQueueClass:    afterRow.QueueClass,
			AfterQueueName:     afterRow.QueueName,
			RowLimit:           r.scopeLimit,
			ScanSeed:           scanSeed,
		})
		if err != nil {
			return err
		}
		scopes := make([]QueueScope, 0, len(rows))
		for _, row := range rows {
			scopes = append(scopes, QueueScope{
				OrgID:         row.OrgID,
				WorkerGroupID: row.WorkerGroupID,
				ProjectID:     row.ProjectID,
				EnvironmentID: row.EnvironmentID,
				QueueClass:    row.QueueClass,
				QueueName:     row.QueueName,
			})
		}
		for _, scope := range r.selector.Order(scopes) {
			stats, err := r.enqueuer.ReconcileQueueScope(ctx, scope, r.runLimit)
			if err != nil {
				problems = append(problems, err)
			}
			if stats.Scanned > 0 || stats.Failed > 0 {
				r.log.Info("queue reconcile scope", "org_id", scope.OrgID, "worker_group_id", scope.WorkerGroupID, "project_id", scope.ProjectID, "environment_id", scope.EnvironmentID, "queue_name", scope.QueueName, "scanned", stats.Scanned, "enqueued", stats.Enqueued, "skipped", stats.Skipped, "failed", stats.Failed)
			}
		}
		if len(rows) < int(r.scopeLimit) {
			break
		}
		last := rows[len(rows)-1]
		afterSortKey = last.SortKey
		afterRow = last
	}
	return errors.Join(problems...)
}
