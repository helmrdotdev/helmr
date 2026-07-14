package dispatch

import (
	"context"
	"errors"
	"log/slog"
	"time"

	"github.com/helmrdotdev/helmr/internal/db"
)

const (
	DefaultQueueReconcileInterval                = 5 * time.Second
	DefaultBuildQueueReconcileInterval           = 10 * time.Second
	DefaultQueueReconcileScopeLimit              = int32(500)
	DefaultQueueReconcileRunLimit                = int32(100)
	DefaultBuildQueueReconcileRegionLimit        = int32(32)
	DefaultBuildQueueReconcileCandidateLimit     = int32(8)
	DefaultQueueReconcileConsecutiveFailureLimit = 3
	defaultRunQueueQueryTimeout                  = 15 * time.Second
	defaultBuildQueueQueryTimeout                = 30 * time.Second
	defaultRunQueueFailureBackoff                = 5 * time.Second
	defaultBuildQueueFailureBackoff              = 30 * time.Second
	queueReconcileUnlockTimeout                  = 5 * time.Second
)

type QueueReconcilerStore interface {
	ListQueuedRunCandidateScopes(context.Context, db.ListQueuedRunCandidateScopesParams) ([]db.ListQueuedRunCandidateScopesRow, error)
}

type RunQueueEnqueuer interface {
	ReconcileQueueScope(context.Context, QueueScope, int32) (QueueReconcileStats, error)
}

type BuildQueueEnqueuer interface {
	ReconcileBuildReady(context.Context, int32, int32) (QueueReconcileStats, error)
}

type QueueReconcileLock interface {
	TryLock(ctx context.Context) (QueueReconcileLockGuard, bool, error)
}

type QueueReconcileLockGuard interface {
	Store(fallback QueueReconcilerStore) QueueReconcilerStore
	Unlock(ctx context.Context) error
}

type QueueReconciler struct {
	store               QueueReconcilerStore
	runEnqueuer         RunQueueEnqueuer
	buildEnqueuer       BuildQueueEnqueuer
	runLock             QueueReconcileLock
	buildLock           QueueReconcileLock
	selector            QueueScopeSelector
	runEvery            time.Duration
	buildEvery          time.Duration
	runQueryTimeout     time.Duration
	buildQueryTimeout   time.Duration
	runFailureBackoff   time.Duration
	buildFailureBackoff time.Duration
	scopeLimit          int32
	runLimit            int32
	buildRegionLimit    int32
	buildCandidateLimit int32
	failureLimit        int
	metrics             reconcileMetrics
	log                 *slog.Logger
}

type QueueReconcilerOption func(*QueueReconciler)

func WithQueueReconcileInterval(every time.Duration) QueueReconcilerOption {
	return func(reconciler *QueueReconciler) {
		reconciler.runEvery = every
		reconciler.buildEvery = every
	}
}

func WithQueueReconcileIntervals(runEvery, buildEvery time.Duration) QueueReconcilerOption {
	return func(reconciler *QueueReconciler) {
		reconciler.runEvery = runEvery
		reconciler.buildEvery = buildEvery
	}
}

func WithQueueReconcileLimits(scopeLimit int32, runLimit int32) QueueReconcilerOption {
	return func(reconciler *QueueReconciler) {
		reconciler.scopeLimit = scopeLimit
		reconciler.runLimit = runLimit
	}
}

func WithBuildQueueReconcileLimits(regionLimit int32, candidateLimit int32) QueueReconcilerOption {
	return func(reconciler *QueueReconciler) {
		reconciler.buildRegionLimit = regionLimit
		reconciler.buildCandidateLimit = candidateLimit
	}
}

func WithQueueReconcileQueryTimeouts(runTimeout, buildTimeout time.Duration) QueueReconcilerOption {
	return func(reconciler *QueueReconciler) {
		reconciler.runQueryTimeout = runTimeout
		reconciler.buildQueryTimeout = buildTimeout
	}
}

func WithQueueReconcileFailureBackoffs(runBackoff, buildBackoff time.Duration) QueueReconcilerOption {
	return func(reconciler *QueueReconciler) {
		reconciler.runFailureBackoff = runBackoff
		reconciler.buildFailureBackoff = buildBackoff
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
		reconciler.runLock = lock
	}
}

func WithBuildQueueReconcileLock(lock QueueReconcileLock) QueueReconcilerOption {
	return func(reconciler *QueueReconciler) {
		reconciler.buildLock = lock
	}
}

func WithQueueReconcileScopeSelector(selector QueueScopeSelector) QueueReconcilerOption {
	return func(reconciler *QueueReconciler) {
		reconciler.selector = selector
	}
}

func NewQueueReconciler(store QueueReconcilerStore, runEnqueuer RunQueueEnqueuer, buildEnqueuer BuildQueueEnqueuer, opts ...QueueReconcilerOption) (*QueueReconciler, error) {
	if store == nil {
		return nil, errors.New("queue reconciler store is required")
	}
	if runEnqueuer == nil || buildEnqueuer == nil {
		return nil, errors.New("run and build queue reconcilers are required")
	}
	reconciler := &QueueReconciler{
		store: store, runEnqueuer: runEnqueuer, buildEnqueuer: buildEnqueuer,
		selector: RoundRobinQueueScopeSelector{},
		runEvery: DefaultQueueReconcileInterval, buildEvery: DefaultBuildQueueReconcileInterval,
		runQueryTimeout: defaultRunQueueQueryTimeout, buildQueryTimeout: defaultBuildQueueQueryTimeout,
		runFailureBackoff: defaultRunQueueFailureBackoff, buildFailureBackoff: defaultBuildQueueFailureBackoff,
		scopeLimit: DefaultQueueReconcileScopeLimit, runLimit: DefaultQueueReconcileRunLimit,
		buildRegionLimit: DefaultBuildQueueReconcileRegionLimit, buildCandidateLimit: DefaultBuildQueueReconcileCandidateLimit,
		failureLimit: DefaultQueueReconcileConsecutiveFailureLimit,
		metrics:      newReconcileMetrics(), log: slog.Default(),
	}
	for _, opt := range opts {
		opt(reconciler)
	}
	if reconciler.runEvery <= 0 || reconciler.buildEvery <= 0 {
		return nil, errors.New("run and build queue reconcile intervals must be positive")
	}
	if reconciler.runQueryTimeout <= 0 || reconciler.buildQueryTimeout <= 0 {
		return nil, errors.New("run and build queue query timeouts must be positive")
	}
	if reconciler.runFailureBackoff <= 0 || reconciler.buildFailureBackoff <= 0 {
		return nil, errors.New("run and build queue failure backoffs must be positive")
	}
	if reconciler.scopeLimit <= 0 {
		return nil, errors.New("queue reconcile scope limit must be positive")
	}
	if reconciler.runLimit <= 0 {
		return nil, errors.New("queue reconcile run limit must be positive")
	}
	if reconciler.buildRegionLimit <= 0 || reconciler.buildCandidateLimit <= 0 {
		return nil, errors.New("build queue reconcile limits must be positive")
	}
	if reconciler.failureLimit <= 0 {
		return nil, errors.New("queue reconcile consecutive failure limit must be positive")
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
	runCtx, cancel := context.WithCancel(ctx)
	defer cancel()
	errC := make(chan error, 2)
	go func() { errC <- r.runDomainLoop(runCtx, "run", r.runEvery, r.runFailureBackoff, r.ReconcileRunsOnce) }()
	go func() {
		errC <- r.runDomainLoop(runCtx, "build", r.buildEvery, r.buildFailureBackoff, r.ReconcileBuildsOnce)
	}()
	var firstErr error
	for i := range 2 {
		err := <-errC
		if firstErr == nil && err != nil && !errors.Is(err, context.Canceled) {
			firstErr = err
		}
		if i == 0 {
			cancel()
		}
	}
	if firstErr != nil {
		return firstErr
	}
	return ctx.Err()
}

func (r *QueueReconciler) runDomainLoop(ctx context.Context, domain string, every, failureBackoff time.Duration, reconcile func(context.Context) error) error {
	consecutiveFailures := 0
	for {
		started := time.Now()
		err := reconcile(ctx)
		delay := every
		if err != nil {
			if ctx.Err() != nil {
				return ctx.Err()
			}
			consecutiveFailures++
			delay = failureBackoff
			r.metrics.observe(ctx, "ready_queue", domain, "failure", time.Since(started))
			if consecutiveFailures%r.failureLimit == 0 {
				r.log.Error("queue reconcile repeatedly failing", "domain", domain, "duration_ms", time.Since(started).Milliseconds(), "error", err, "consecutive_failures", consecutiveFailures)
			} else {
				r.log.Warn("queue reconcile failed", "domain", domain, "duration_ms", time.Since(started).Milliseconds(), "error", err, "consecutive_failures", consecutiveFailures)
			}
		} else {
			consecutiveFailures = 0
			r.metrics.observe(ctx, "ready_queue", domain, "success", time.Since(started))
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

func (r *QueueReconciler) ReconcileOnce(ctx context.Context) error {
	return errors.Join(r.ReconcileRunsOnce(ctx), r.ReconcileBuildsOnce(ctx))
}

func (r *QueueReconciler) ReconcileRunsOnce(ctx context.Context) error {
	var guard QueueReconcileLockGuard
	store := r.store
	if r.runLock != nil {
		var locked bool
		var err error
		lockCtx, cancel := context.WithTimeout(ctx, r.runQueryTimeout)
		guard, locked, err = r.runLock.TryLock(lockCtx)
		cancel()
		if err != nil {
			return err
		}
		if !locked {
			r.log.Debug("queue reconcile lock is held by another instance")
			return nil
		}
		store = guard.Store(r.store)
		defer r.unlockQueueReconcile(ctx, "run", guard)
	}
	var problems []error
	scanSeed := time.Now().UTC().Format(time.RFC3339Nano)
	var afterSortKey string
	var afterRow db.ListQueuedRunCandidateScopesRow
	for {
		queryCtx, cancel := context.WithTimeout(ctx, r.runQueryTimeout)
		rows, err := store.ListQueuedRunCandidateScopes(queryCtx, db.ListQueuedRunCandidateScopesParams{
			AfterSortKey:       afterSortKey,
			AfterOrgID:         afterRow.OrgID,
			AfterProjectID:     afterRow.ProjectID,
			AfterEnvironmentID: afterRow.EnvironmentID,
			AfterRegionID:      afterRow.RegionID,
			AfterQueueClass:    afterRow.QueueClass,
			AfterQueueName:     afterRow.QueueName,
			RowLimit:           r.scopeLimit,
			ScanSeed:           scanSeed,
		})
		cancel()
		if err != nil {
			return err
		}
		scopes := make([]QueueScope, 0, len(rows))
		for _, row := range rows {
			scopes = append(scopes, QueueScope{
				OrgID:         row.OrgID,
				RegionID:      row.RegionID,
				ProjectID:     row.ProjectID,
				EnvironmentID: row.EnvironmentID,
				QueueClass:    row.QueueClass,
				QueueName:     row.QueueName,
			})
		}
		for _, scope := range r.selector.Order(scopes) {
			queryCtx, cancel := context.WithTimeout(ctx, r.runQueryTimeout)
			stats, err := r.runEnqueuer.ReconcileQueueScope(queryCtx, scope, r.runLimit)
			cancel()
			if err != nil {
				problems = append(problems, err)
			}
			if stats.Scanned > 0 || stats.Failed > 0 {
				r.log.Info("queue reconcile scope", "org_id", scope.OrgID, "region_id", scope.RegionID, "project_id", scope.ProjectID, "environment_id", scope.EnvironmentID, "queue_name", scope.QueueName, "scanned", stats.Scanned, "enqueued", stats.Enqueued, "skipped", stats.Skipped, "failed", stats.Failed)
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

func (r *QueueReconciler) ReconcileBuildsOnce(ctx context.Context) error {
	var guard QueueReconcileLockGuard
	if r.buildLock != nil {
		lockCtx, cancel := context.WithTimeout(ctx, r.buildQueryTimeout)
		var locked bool
		var err error
		guard, locked, err = r.buildLock.TryLock(lockCtx)
		cancel()
		if err != nil {
			return err
		}
		if !locked {
			r.log.Debug("queue reconcile lock is held by another instance", "domain", "build")
			return nil
		}
		defer r.unlockQueueReconcile(ctx, "build", guard)
	}
	queryCtx, cancel := context.WithTimeout(ctx, r.buildQueryTimeout)
	buildStats, err := r.buildEnqueuer.ReconcileBuildReady(queryCtx, r.buildRegionLimit, r.buildCandidateLimit)
	cancel()
	if err != nil {
		return err
	}
	if buildStats.Scanned > 0 || buildStats.Failed > 0 {
		r.log.Info("queue reconcile builds", "scanned", buildStats.Scanned, "enqueued", buildStats.Enqueued, "failed", buildStats.Failed)
	}
	return nil
}

func (r *QueueReconciler) unlockQueueReconcile(ctx context.Context, domain string, guard QueueReconcileLockGuard) {
	unlockCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), queueReconcileUnlockTimeout)
	defer cancel()
	if err := guard.Unlock(unlockCtx); err != nil {
		r.log.Warn("release queue reconcile lock failed", "domain", domain, "error", err)
	}
}
