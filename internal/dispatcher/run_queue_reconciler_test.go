package dispatcher

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"testing"
	"time"

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
	if _, err := NewRunQueueReconciler(&fakeRunQueueReconcilerStore{}, &fakeRunQueuePublisher{}, WithRunQueueReconcileConsecutiveFailureLimit(0)); err == nil {
		t.Fatal("invalid failure limit error = nil")
	}
}

func TestRunQueueReconcilerRunReturnsAfterConsecutiveFailures(t *testing.T) {
	listErr := errors.New("list organizations failed")
	store := &fakeRunQueueReconcilerStore{listErr: listErr}
	reconciler, err := NewRunQueueReconciler(
		store,
		&fakeRunQueuePublisher{},
		WithRunQueueReconcileInterval(time.Millisecond),
		WithRunQueueReconcileConsecutiveFailureLimit(2),
		WithRunQueueReconcileLogger(slog.New(slog.NewTextHandler(io.Discard, nil))),
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

func TestRunQueueReconcilerRunReturnsContextCancellation(t *testing.T) {
	entered := make(chan struct{})
	store := &fakeRunQueueReconcilerStore{blockUntilCancel: true, entered: entered}
	reconciler, err := NewRunQueueReconciler(
		store,
		&fakeRunQueuePublisher{},
		WithRunQueueReconcileInterval(time.Millisecond),
		WithRunQueueReconcileConsecutiveFailureLimit(1),
		WithRunQueueReconcileLogger(slog.New(slog.NewTextHandler(io.Discard, nil))),
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

type fakeRunQueueReconcilerStore struct {
	orgIDs           []pgtype.UUID
	pages            [][]pgtype.UUID
	args             []db.ListOrganizationIDsPageParams
	listErr          error
	blockUntilCancel bool
	entered          chan struct{}
}

func (f *fakeRunQueueReconcilerStore) ListOrganizationIDsPage(ctx context.Context, arg db.ListOrganizationIDsPageParams) ([]pgtype.UUID, error) {
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
