package dispatcher

import (
	"context"
	"errors"
	"log/slog"
	"time"

	"github.com/helmrdotdev/helmr/internal/db"
	"github.com/helmrdotdev/helmr/internal/dispatch/queuewriter"
	"github.com/jackc/pgx/v5/pgtype"
)

const (
	DefaultDispatchReconcileInterval = 5 * time.Second
	DefaultDispatchReconcileOrgLimit = int32(500)
	DefaultDispatchReconcileRunLimit = int32(100)
)

type DispatchReconcilerStore interface {
	ListOrganizationIDsPage(context.Context, db.ListOrganizationIDsPageParams) ([]pgtype.UUID, error)
}

type DispatchReconcilerEnqueuer interface {
	ReconcileOrg(context.Context, pgtype.UUID, int32) (queuewriter.ReconcileStats, error)
}

type DispatchReconciler struct {
	store       DispatchReconcilerStore
	queuewriter DispatchReconcilerEnqueuer
	every       time.Duration
	orgLimit    int32
	runLimit    int32
	log         *slog.Logger
}

type DispatchReconcilerOption func(*DispatchReconciler)

func WithDispatchReconcileInterval(every time.Duration) DispatchReconcilerOption {
	return func(reconciler *DispatchReconciler) {
		reconciler.every = every
	}
}

func WithDispatchReconcileLimits(orgLimit int32, runLimit int32) DispatchReconcilerOption {
	return func(reconciler *DispatchReconciler) {
		reconciler.orgLimit = orgLimit
		reconciler.runLimit = runLimit
	}
}

func WithDispatchReconcileLogger(log *slog.Logger) DispatchReconcilerOption {
	return func(reconciler *DispatchReconciler) {
		reconciler.log = log
	}
}

func NewDispatchReconciler(store DispatchReconcilerStore, runEnqueuer DispatchReconcilerEnqueuer, opts ...DispatchReconcilerOption) (*DispatchReconciler, error) {
	if store == nil {
		return nil, errors.New("dispatch reconciler store is required")
	}
	if runEnqueuer == nil {
		return nil, errors.New("dispatch reconciler queuewriter is required")
	}
	reconciler := &DispatchReconciler{
		store:       store,
		queuewriter: runEnqueuer,
		every:       DefaultDispatchReconcileInterval,
		orgLimit:    DefaultDispatchReconcileOrgLimit,
		runLimit:    DefaultDispatchReconcileRunLimit,
		log:         slog.Default(),
	}
	for _, opt := range opts {
		opt(reconciler)
	}
	if reconciler.every <= 0 {
		return nil, errors.New("dispatch reconcile interval must be positive")
	}
	if reconciler.orgLimit <= 0 {
		return nil, errors.New("dispatch reconcile org limit must be positive")
	}
	if reconciler.runLimit <= 0 {
		return nil, errors.New("dispatch reconcile run limit must be positive")
	}
	if reconciler.log == nil {
		reconciler.log = slog.Default()
	}
	return reconciler, nil
}

func (r *DispatchReconciler) Run(ctx context.Context) error {
	timer := time.NewTimer(0)
	defer timer.Stop()
	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-timer.C:
		}
		if err := r.ReconcileOnce(ctx); err != nil {
			r.log.Warn("dispatch reconcile failed", "error", err)
		}
		timer.Reset(r.every)
	}
}

func (r *DispatchReconciler) ReconcileOnce(ctx context.Context) error {
	var problems []error
	var afterID pgtype.UUID
	for {
		orgIDs, err := r.store.ListOrganizationIDsPage(ctx, db.ListOrganizationIDsPageParams{
			AfterID:  afterID,
			RowLimit: r.orgLimit,
		})
		if err != nil {
			return err
		}
		for _, orgID := range orgIDs {
			stats, err := r.queuewriter.ReconcileOrg(ctx, orgID, r.runLimit)
			if err != nil {
				problems = append(problems, err)
			}
			if stats.Scanned > 0 || stats.Failed > 0 {
				r.log.Info("dispatch reconcile org", "org_id", orgID, "scanned", stats.Scanned, "enqueued", stats.Enqueued, "skipped", stats.Skipped, "failed", stats.Failed)
			}
		}
		if len(orgIDs) < int(r.orgLimit) {
			break
		}
		afterID = orgIDs[len(orgIDs)-1]
	}
	return errors.Join(problems...)
}
