package dispatcher

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"testing"

	"github.com/helmrdotdev/helmr/internal/db"
	"github.com/helmrdotdev/helmr/internal/dispatch/queuewriter"
	"github.com/helmrdotdev/helmr/internal/ids"
	"github.com/jackc/pgx/v5/pgtype"
)

func TestDispatchReconcilerReconcilesOrganizations(t *testing.T) {
	ctx := context.Background()
	orgA := ids.ToPG(ids.New())
	orgB := ids.ToPG(ids.New())
	store := &fakeDispatchReconcilerStore{orgIDs: []pgtype.UUID{orgA, orgB}}
	runEnqueuer := &fakeDispatchReconcilerEnqueuer{
		stats: map[pgtype.UUID]queuewriter.ReconcileStats{
			orgA: {Scanned: 2, Enqueued: 2},
			orgB: {Scanned: 1, Failed: 1},
		},
		errs: map[pgtype.UUID]error{orgB: errors.New("redis unavailable")},
	}
	reconciler, err := NewDispatchReconciler(store, runEnqueuer, WithDispatchReconcileLogger(slog.New(slog.NewTextHandler(io.Discard, nil))))
	if err != nil {
		t.Fatal(err)
	}

	if err := reconciler.ReconcileOnce(ctx); err == nil {
		t.Fatal("reconcile error = nil")
	}
	if len(runEnqueuer.orgIDs) != 2 || runEnqueuer.orgIDs[0] != orgA || runEnqueuer.orgIDs[1] != orgB {
		t.Fatalf("reconciled orgs = %+v", runEnqueuer.orgIDs)
	}
	if len(store.args) != 1 || store.args[0].RowLimit != DefaultDispatchReconcileOrgLimit || runEnqueuer.limits[0] != DefaultDispatchReconcileRunLimit {
		t.Fatalf("store args = %+v limits = %+v", store.args, runEnqueuer.limits)
	}
}

func TestDispatchReconcilerPaginatesOrganizations(t *testing.T) {
	ctx := context.Background()
	orgA := ids.ToPG(ids.New())
	orgB := ids.ToPG(ids.New())
	orgC := ids.ToPG(ids.New())
	store := &fakeDispatchReconcilerStore{pages: [][]pgtype.UUID{{orgA, orgB}, {orgC}}}
	runEnqueuer := &fakeDispatchReconcilerEnqueuer{}
	reconciler, err := NewDispatchReconciler(
		store,
		runEnqueuer,
		WithDispatchReconcileLimits(2, 10),
		WithDispatchReconcileLogger(slog.New(slog.NewTextHandler(io.Discard, nil))),
	)
	if err != nil {
		t.Fatal(err)
	}

	if err := reconciler.ReconcileOnce(ctx); err != nil {
		t.Fatal(err)
	}
	if len(runEnqueuer.orgIDs) != 3 || runEnqueuer.orgIDs[0] != orgA || runEnqueuer.orgIDs[1] != orgB || runEnqueuer.orgIDs[2] != orgC {
		t.Fatalf("reconciled orgs = %+v", runEnqueuer.orgIDs)
	}
	if len(store.args) != 2 || store.args[1].AfterID != orgB {
		t.Fatalf("pagination args = %+v", store.args)
	}
}

func TestNewDispatchReconcilerRejectsInvalidConfig(t *testing.T) {
	if _, err := NewDispatchReconciler(nil, &fakeDispatchReconcilerEnqueuer{}); err == nil {
		t.Fatal("nil store error = nil")
	}
	if _, err := NewDispatchReconciler(&fakeDispatchReconcilerStore{}, nil); err == nil {
		t.Fatal("nil queuewriter error = nil")
	}
	if _, err := NewDispatchReconciler(&fakeDispatchReconcilerStore{}, &fakeDispatchReconcilerEnqueuer{}, WithDispatchReconcileInterval(0)); err == nil {
		t.Fatal("invalid interval error = nil")
	}
}

type fakeDispatchReconcilerStore struct {
	orgIDs []pgtype.UUID
	pages  [][]pgtype.UUID
	args   []db.ListOrganizationIDsPageParams
}

func (f *fakeDispatchReconcilerStore) ListOrganizationIDsPage(_ context.Context, arg db.ListOrganizationIDsPageParams) ([]pgtype.UUID, error) {
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

type fakeDispatchReconcilerEnqueuer struct {
	orgIDs []pgtype.UUID
	limits []int32
	stats  map[pgtype.UUID]queuewriter.ReconcileStats
	errs   map[pgtype.UUID]error
}

func (f *fakeDispatchReconcilerEnqueuer) ReconcileOrg(_ context.Context, orgID pgtype.UUID, limit int32) (queuewriter.ReconcileStats, error) {
	f.orgIDs = append(f.orgIDs, orgID)
	f.limits = append(f.limits, limit)
	return f.stats[orgID], f.errs[orgID]
}
