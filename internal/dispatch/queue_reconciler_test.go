package dispatch

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/helmrdotdev/helmr/internal/db"
	"github.com/helmrdotdev/helmr/internal/pgvalue"
	"github.com/jackc/pgx/v5/pgtype"
)

func TestQueueReconcilerReconcilesScopesRoundRobinByOrganization(t *testing.T) {
	ctx := context.Background()
	orgA := pgvalue.UUID(uuid.Must(uuid.NewV7()))
	orgB := pgvalue.UUID(uuid.Must(uuid.NewV7()))
	scopeA1 := testQueueScope(orgA, "queue-a")
	scopeA2 := testQueueScope(orgA, "queue-b")
	scopeB1 := testQueueScope(orgB, "queue-a")
	store := &fakeQueueReconcilerStore{scopes: []QueueScope{scopeA1, scopeA2, scopeB1}}
	enqueuer := &fakeQueueEnqueuer{
		stats: map[QueueScope]QueueReconcileStats{
			scopeA1: {Scanned: 2, Enqueued: 2},
			scopeB1: {Scanned: 1, Failed: 1},
		},
		errs: map[QueueScope]error{scopeB1: errors.New("redis unavailable")},
	}
	reconciler, err := NewQueueReconciler(store, enqueuer, WithQueueReconcileWorkerGroupID("worker-group-1"), WithQueueReconcileLogger(slog.New(slog.NewTextHandler(io.Discard, nil))))
	if err != nil {
		t.Fatal(err)
	}

	if err := reconciler.ReconcileOnce(ctx); err == nil {
		t.Fatal("reconcile error = nil")
	}
	wantScopes := []QueueScope{scopeA1, scopeB1, scopeA2}
	if !sameScopes(enqueuer.scopes, wantScopes) {
		t.Fatalf("reconciled scopes = %+v, want %+v", enqueuer.scopes, wantScopes)
	}
	if len(store.args) != 1 || store.args[0].WorkerGroupID != "worker-group-1" || store.args[0].RowLimit != DefaultQueueReconcileScopeLimit || enqueuer.limits[0] != DefaultQueueReconcileRunLimit {
		t.Fatalf("store args = %+v limits = %+v", store.args, enqueuer.limits)
	}
}

func TestQueueReconcilerPaginatesScopes(t *testing.T) {
	ctx := context.Background()
	scopeA := testQueueScope(pgvalue.UUID(uuid.Must(uuid.NewV7())), "queue-a")
	scopeB := testQueueScope(pgvalue.UUID(uuid.Must(uuid.NewV7())), "queue-b")
	scopeC := testQueueScope(pgvalue.UUID(uuid.Must(uuid.NewV7())), "queue-c")
	store := &fakeQueueReconcilerStore{pages: [][]QueueScope{{scopeA, scopeB}, {scopeC}}}
	enqueuer := &fakeQueueEnqueuer{}
	reconciler, err := NewQueueReconciler(
		store,
		enqueuer,
		WithQueueReconcileWorkerGroupID("worker-group-1"),
		WithQueueReconcileLimits(2, 10),
		WithQueueReconcileLogger(slog.New(slog.NewTextHandler(io.Discard, nil))),
	)
	if err != nil {
		t.Fatal(err)
	}

	if err := reconciler.ReconcileOnce(ctx); err != nil {
		t.Fatal(err)
	}
	wantScopes := []QueueScope{scopeA, scopeB, scopeC}
	if !sameScopes(enqueuer.scopes, wantScopes) {
		t.Fatalf("reconciled scopes = %+v, want %+v", enqueuer.scopes, wantScopes)
	}
	if len(store.args) != 2 || store.args[1].AfterSortKey != "sort-1" || store.args[1].AfterQueueName != scopeB.QueueName {
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
	if _, err := NewQueueReconciler(&fakeQueueReconcilerStore{}, &fakeQueueEnqueuer{}); err == nil {
		t.Fatal("missing worker group id error = nil")
	}
	if _, err := NewQueueReconciler(&fakeQueueReconcilerStore{}, &fakeQueueEnqueuer{}, WithQueueReconcileWorkerGroupID("worker-group-1"), WithQueueReconcileInterval(0)); err == nil {
		t.Fatal("invalid interval error = nil")
	}
	if _, err := NewQueueReconciler(&fakeQueueReconcilerStore{}, &fakeQueueEnqueuer{}, WithQueueReconcileWorkerGroupID("worker-group-1"), WithQueueReconcileLimits(0, 10)); err == nil {
		t.Fatal("invalid scope limit error = nil")
	}
	if _, err := NewQueueReconciler(&fakeQueueReconcilerStore{}, &fakeQueueEnqueuer{}, WithQueueReconcileWorkerGroupID("worker-group-1"), WithQueueReconcileScopeSelector(nil)); err == nil {
		t.Fatal("nil selector error = nil")
	}
	if _, err := NewQueueReconciler(&fakeQueueReconcilerStore{}, &fakeQueueEnqueuer{}, WithQueueReconcileWorkerGroupID("worker-group-1"), WithQueueReconcileConsecutiveFailureLimit(0)); err == nil {
		t.Fatal("invalid failure limit error = nil")
	}
}

func TestQueueReconcilerRunReturnsAfterConsecutiveFailures(t *testing.T) {
	listErr := errors.New("list scopes failed")
	store := &fakeQueueReconcilerStore{listErr: listErr}
	reconciler, err := NewQueueReconciler(
		store,
		&fakeQueueEnqueuer{},
		WithQueueReconcileWorkerGroupID("worker-group-1"),
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
		WithQueueReconcileWorkerGroupID("worker-group-1"),
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
	scopes           []QueueScope
	pages            [][]QueueScope
	args             []db.ListQueuedRunCandidateScopesParams
	listErr          error
	blockUntilCancel bool
	entered          chan struct{}
}

func (f *fakeQueueReconcilerStore) ListQueuedRunCandidateScopes(ctx context.Context, arg db.ListQueuedRunCandidateScopesParams) ([]db.ListQueuedRunCandidateScopesRow, error) {
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
	var scopes []QueueScope
	if len(f.pages) > 0 {
		scopes = f.pages[0]
		f.pages = f.pages[1:]
	} else if int32(len(f.scopes)) > arg.RowLimit {
		scopes = f.scopes[:arg.RowLimit]
	} else {
		scopes = f.scopes
	}
	rows := make([]db.ListQueuedRunCandidateScopesRow, 0, len(scopes))
	for index, scope := range scopes {
		rows = append(rows, db.ListQueuedRunCandidateScopesRow{
			OrgID:         scope.OrgID,
			WorkerGroupID: scope.WorkerGroupID,
			ProjectID:     scope.ProjectID,
			EnvironmentID: scope.EnvironmentID,
			QueueClass:    scope.QueueClass,
			QueueName:     scope.QueueName,
			SortKey:       "sort-" + string(rune('0'+index)),
		})
	}
	return rows, nil
}

type fakeQueueEnqueuer struct {
	scopes []QueueScope
	limits []int32
	stats  map[QueueScope]QueueReconcileStats
	errs   map[QueueScope]error
}

func (f *fakeQueueEnqueuer) ReconcileQueueScope(_ context.Context, scope QueueScope, limit int32) (QueueReconcileStats, error) {
	f.scopes = append(f.scopes, scope)
	f.limits = append(f.limits, limit)
	return f.stats[scope], f.errs[scope]
}

func sameScopes(a, b []QueueScope) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

func testQueueScope(orgID pgtype.UUID, queueName string) QueueScope {
	return QueueScope{
		OrgID:         orgID,
		WorkerGroupID: "us-east-1-worker-group-1",
		ProjectID:     pgvalue.UUID(uuid.Must(uuid.NewV7())),
		EnvironmentID: pgvalue.UUID(uuid.Must(uuid.NewV7())),
		QueueClass:    "default",
		QueueName:     queueName,
	}
}
