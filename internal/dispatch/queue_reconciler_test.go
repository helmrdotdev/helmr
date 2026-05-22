package dispatch

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"testing"
	"time"

	"github.com/helmrdotdev/helmr/internal/db"
	"github.com/helmrdotdev/helmr/internal/ids"
	"github.com/jackc/pgx/v5/pgtype"
)

func TestQueueReconcilerReconcilesOrganizations(t *testing.T) {
	ctx := context.Background()
	orgA := ids.ToPG(ids.New())
	orgB := ids.ToPG(ids.New())
	store := &fakeQueueReconcilerStore{orgIDs: []pgtype.UUID{orgA, orgB}}
	enqueuer := &fakeQueueEnqueuer{
		stats: map[pgtype.UUID]QueueReconcileStats{
			orgA: {Scanned: 2, Enqueued: 2},
			orgB: {Scanned: 1, Failed: 1},
		},
		errs: map[pgtype.UUID]error{orgB: errors.New("redis unavailable")},
	}
	reconciler, err := NewQueueReconciler(store, enqueuer, WithQueueReconcileLogger(slog.New(slog.NewTextHandler(io.Discard, nil))))
	if err != nil {
		t.Fatal(err)
	}

	if err := reconciler.ReconcileOnce(ctx); err == nil {
		t.Fatal("reconcile error = nil")
	}
	if len(enqueuer.orgIDs) != 2 || enqueuer.orgIDs[0] != orgA || enqueuer.orgIDs[1] != orgB {
		t.Fatalf("reconciled orgs = %+v", enqueuer.orgIDs)
	}
	if len(store.args) != 1 || store.args[0].RowLimit != DefaultQueueReconcileOrgLimit || enqueuer.limits[0] != DefaultQueueReconcileRunLimit {
		t.Fatalf("store args = %+v limits = %+v", store.args, enqueuer.limits)
	}
}

func TestQueueReconcilerPaginatesOrganizations(t *testing.T) {
	ctx := context.Background()
	orgA := ids.ToPG(ids.New())
	orgB := ids.ToPG(ids.New())
	orgC := ids.ToPG(ids.New())
	store := &fakeQueueReconcilerStore{pages: [][]pgtype.UUID{{orgA, orgB}, {orgC}}}
	enqueuer := &fakeQueueEnqueuer{}
	reconciler, err := NewQueueReconciler(
		store,
		enqueuer,
		WithQueueReconcileLimits(2, 10),
		WithQueueReconcileLogger(slog.New(slog.NewTextHandler(io.Discard, nil))),
	)
	if err != nil {
		t.Fatal(err)
	}

	if err := reconciler.ReconcileOnce(ctx); err != nil {
		t.Fatal(err)
	}
	if len(enqueuer.orgIDs) != 3 || enqueuer.orgIDs[0] != orgA || enqueuer.orgIDs[1] != orgB || enqueuer.orgIDs[2] != orgC {
		t.Fatalf("reconciled orgs = %+v", enqueuer.orgIDs)
	}
	if len(store.args) != 2 || store.args[1].AfterID != orgB {
		t.Fatalf("pagination args = %+v", store.args)
	}
}

func TestNewQueueReconcilerRejectsInvalidConfig(t *testing.T) {
	if _, err := NewQueueReconciler(nil, &fakeQueueEnqueuer{}); err == nil {
		t.Fatal("nil store error = nil")
	}
	if _, err := NewQueueReconciler(&fakeQueueReconcilerStore{}, nil); err == nil {
		t.Fatal("nil queue enqueuer error = nil")
	}
	if _, err := NewQueueReconciler(&fakeQueueReconcilerStore{}, &fakeQueueEnqueuer{}, WithQueueReconcileInterval(0)); err == nil {
		t.Fatal("invalid interval error = nil")
	}
	if _, err := NewQueueReconciler(&fakeQueueReconcilerStore{}, &fakeQueueEnqueuer{}, WithQueueReconcileConsecutiveFailureLimit(0)); err == nil {
		t.Fatal("invalid failure limit error = nil")
	}
}

func TestQueueReconcilerRunReturnsAfterConsecutiveFailures(t *testing.T) {
	listErr := errors.New("list organizations failed")
	store := &fakeQueueReconcilerStore{listErr: listErr}
	reconciler, err := NewQueueReconciler(
		store,
		&fakeQueueEnqueuer{},
		WithQueueReconcileInterval(time.Millisecond),
		WithQueueReconcileConsecutiveFailureLimit(2),
		WithQueueReconcileLogger(slog.New(slog.NewTextHandler(io.Discard, nil))),
	)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()

	err = reconciler.Run(ctx)
	if !errors.Is(err, listErr) {
		t.Fatalf("run error = %v, want %v", err, listErr)
	}
	if len(store.args) != 2 {
		t.Fatalf("list calls = %d, want 2", len(store.args))
	}
}

func TestQueueReconcilerRunReturnsContextCancellation(t *testing.T) {
	entered := make(chan struct{})
	store := &fakeQueueReconcilerStore{blockUntilCancel: true, entered: entered}
	reconciler, err := NewQueueReconciler(
		store,
		&fakeQueueEnqueuer{},
		WithQueueReconcileInterval(time.Millisecond),
		WithQueueReconcileConsecutiveFailureLimit(1),
		WithQueueReconcileLogger(slog.New(slog.NewTextHandler(io.Discard, nil))),
	)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	errc := make(chan error, 1)
	go func() {
		errc <- reconciler.Run(ctx)
	}()
	<-entered
	cancel()

	select {
	case err := <-errc:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("run error = %v, want context canceled", err)
		}
	case <-time.After(time.Second):
		t.Fatal("timed out waiting for reconciler to stop")
	}
}

type fakeQueueReconcilerStore struct {
	orgIDs           []pgtype.UUID
	pages            [][]pgtype.UUID
	args             []db.ListOrganizationIDsPageParams
	listErr          error
	blockUntilCancel bool
	entered          chan struct{}
}

func (f *fakeQueueReconcilerStore) ListOrganizationIDsPage(ctx context.Context, arg db.ListOrganizationIDsPageParams) ([]pgtype.UUID, error) {
	f.args = append(f.args, arg)
	if f.entered != nil {
		close(f.entered)
		f.entered = nil
	}
	if f.blockUntilCancel {
		<-ctx.Done()
		return nil, ctx.Err()
	}
	if f.listErr != nil {
		return nil, f.listErr
	}
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

type fakeQueueEnqueuer struct {
	orgIDs []pgtype.UUID
	limits []int32
	stats  map[pgtype.UUID]QueueReconcileStats
	errs   map[pgtype.UUID]error
}

func (f *fakeQueueEnqueuer) ReconcileOrgQueue(_ context.Context, orgID pgtype.UUID, limit int32) (QueueReconcileStats, error) {
	f.orgIDs = append(f.orgIDs, orgID)
	f.limits = append(f.limits, limit)
	return f.stats[orgID], f.errs[orgID]
}
