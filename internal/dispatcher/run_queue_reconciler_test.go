package dispatcher

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"testing"

	"github.com/helmrdotdev/helmr/internal/db"
	"github.com/helmrdotdev/helmr/internal/ids"
	"github.com/helmrdotdev/helmr/internal/runqueue/publisher"
	"github.com/jackc/pgx/v5/pgtype"
)

func TestRunQueueReconcilerReconcilesOrganizations(t *testing.T) {
	ctx := context.Background()
	orgA := ids.ToPG(ids.New())
	orgB := ids.ToPG(ids.New())
	store := &fakeRunQueueReconcilerStore{orgIDs: []pgtype.UUID{orgA, orgB}}
	runPublisher := &fakeRunQueuePublisher{
		stats: map[pgtype.UUID]publisher.ReconcileStats{
			orgA: {Scanned: 2, Enqueued: 2},
			orgB: {Scanned: 1, Failed: 1},
		},
		errs: map[pgtype.UUID]error{orgB: errors.New("redis unavailable")},
	}
	reconciler, err := NewRunQueueReconciler(store, runPublisher, WithRunQueueReconcileLogger(slog.New(slog.NewTextHandler(io.Discard, nil))))
	if err != nil {
		t.Fatal(err)
	}

	if err := reconciler.ReconcileOnce(ctx); err == nil {
		t.Fatal("reconcile error = nil")
	}
	if len(runPublisher.orgIDs) != 2 || runPublisher.orgIDs[0] != orgA || runPublisher.orgIDs[1] != orgB {
		t.Fatalf("reconciled orgs = %+v", runPublisher.orgIDs)
	}
	if len(store.args) != 1 || store.args[0].RowLimit != DefaultRunQueueReconcileOrgLimit || runPublisher.limits[0] != DefaultRunQueueReconcileRunLimit {
		t.Fatalf("store args = %+v limits = %+v", store.args, runPublisher.limits)
	}
}

func TestRunQueueReconcilerPaginatesOrganizations(t *testing.T) {
	ctx := context.Background()
	orgA := ids.ToPG(ids.New())
	orgB := ids.ToPG(ids.New())
	orgC := ids.ToPG(ids.New())
	store := &fakeRunQueueReconcilerStore{pages: [][]pgtype.UUID{{orgA, orgB}, {orgC}}}
	runPublisher := &fakeRunQueuePublisher{}
	reconciler, err := NewRunQueueReconciler(
		store,
		runPublisher,
		WithRunQueueReconcileLimits(2, 10),
		WithRunQueueReconcileLogger(slog.New(slog.NewTextHandler(io.Discard, nil))),
	)
	if err != nil {
		t.Fatal(err)
	}

	if err := reconciler.ReconcileOnce(ctx); err != nil {
		t.Fatal(err)
	}
	if len(runPublisher.orgIDs) != 3 || runPublisher.orgIDs[0] != orgA || runPublisher.orgIDs[1] != orgB || runPublisher.orgIDs[2] != orgC {
		t.Fatalf("reconciled orgs = %+v", runPublisher.orgIDs)
	}
	if len(store.args) != 2 || store.args[1].AfterID != orgB {
		t.Fatalf("pagination args = %+v", store.args)
	}
}

func TestNewRunQueueReconcilerRejectsInvalidConfig(t *testing.T) {
	if _, err := NewRunQueueReconciler(nil, &fakeRunQueuePublisher{}); err == nil {
		t.Fatal("nil store error = nil")
	}
	if _, err := NewRunQueueReconciler(&fakeRunQueueReconcilerStore{}, nil); err == nil {
		t.Fatal("nil run queue publisher error = nil")
	}
	if _, err := NewRunQueueReconciler(&fakeRunQueueReconcilerStore{}, &fakeRunQueuePublisher{}, WithRunQueueReconcileInterval(0)); err == nil {
		t.Fatal("invalid interval error = nil")
	}
}

type fakeRunQueueReconcilerStore struct {
	orgIDs []pgtype.UUID
	pages  [][]pgtype.UUID
	args   []db.ListOrganizationIDsPageParams
}

func (f *fakeRunQueueReconcilerStore) ListOrganizationIDsPage(_ context.Context, arg db.ListOrganizationIDsPageParams) ([]pgtype.UUID, error) {
	f.args = append(f.args, arg)
	if len(f.pages) > 0 {
		page := f.pages[0]
		f.pages = f.pages[1:]
		return page, nil
	}
	if int32(len(f.orgIDs)) > arg.RowLimit {
		return f.orgIDs[:arg.RowLimit], nil
	}
	return f.orgIDs, nil
}

type fakeRunQueuePublisher struct {
	orgIDs []pgtype.UUID
	limits []int32
	stats  map[pgtype.UUID]publisher.ReconcileStats
	errs   map[pgtype.UUID]error
}

func (f *fakeRunQueuePublisher) ReconcileOrg(_ context.Context, orgID pgtype.UUID, limit int32) (publisher.ReconcileStats, error) {
	f.orgIDs = append(f.orgIDs, orgID)
	f.limits = append(f.limits, limit)
	return f.stats[orgID], f.errs[orgID]
}
