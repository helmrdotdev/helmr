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

func TestSweepOnce(t *testing.T) {
	orgA := ids.ToPG(ids.New())
	orgB := ids.ToPG(ids.New())
	store := &fakeSweeperStore{orgIDs: []pgtype.UUID{orgA, orgB}}
	if err := sweepOnce(context.Background(), store, DefaultExpirySweepOrgLimit); err != nil {
		t.Fatal(err)
	}
	if got := store.calls; got != "requeue,fail,expire-runs,expire-waits,requeue,fail,expire-runs,expire-waits" {
		t.Fatalf("calls = %s", got)
	}
	if len(store.sweptOrgIDs) != 8 {
		t.Fatalf("swept org IDs = %+v", store.sweptOrgIDs)
	}
	if store.sweptOrgIDs[0] != orgA || store.sweptOrgIDs[4] != orgB {
		t.Fatalf("swept org IDs = %+v", store.sweptOrgIDs)
	}
}

func TestSweepOnceStopsAfterRequeueError(t *testing.T) {
	store := &fakeSweeperStore{
		fakeSweeperOrgStore: fakeSweeperOrgStore{requeueErr: errors.New("requeue failed")},
		orgIDs:              []pgtype.UUID{ids.ToPG(ids.New())},
	}
	if err := sweepOnce(context.Background(), store, DefaultExpirySweepOrgLimit); err == nil {
		t.Fatal("expected error")
	}
	if got := store.calls; got != "requeue" {
		t.Fatalf("calls = %s", got)
	}
}

func TestSweepOnceContinuesAfterOrgError(t *testing.T) {
	orgA := ids.ToPG(ids.New())
	orgB := ids.ToPG(ids.New())
	store := &fakeSweeperStore{
		fakeSweeperOrgStore: fakeSweeperOrgStore{requeueErrs: map[pgtype.UUID]error{orgA: errors.New("requeue failed")}},
		orgIDs:              []pgtype.UUID{orgA, orgB},
	}
	if err := sweepOnce(context.Background(), store, DefaultExpirySweepOrgLimit); err == nil {
		t.Fatal("expected error")
	}
	if got := store.calls; got != "requeue,requeue,fail,expire-runs,expire-waits" {
		t.Fatalf("calls = %s", got)
	}
	if len(store.sweptOrgIDs) != 5 || store.sweptOrgIDs[0] != orgA || store.sweptOrgIDs[1] != orgB {
		t.Fatalf("swept org IDs = %+v", store.sweptOrgIDs)
	}
}

func TestSweeperPaginatesOrganizations(t *testing.T) {
	orgA := ids.ToPG(ids.New())
	orgB := ids.ToPG(ids.New())
	orgC := ids.ToPG(ids.New())
	store := &fakeSweeperStore{pages: [][]pgtype.UUID{{orgA, orgB}, {orgC}}}
	sweeper, err := NewExpirySweeper(store, WithExpirySweepOrgLimit(2))
	if err != nil {
		t.Fatal(err)
	}
	if err := sweeper.sweep(context.Background()); err != nil {
		t.Fatal(err)
	}
	if len(store.sweptOrgIDs) != 12 || store.sweptOrgIDs[0] != orgA || store.sweptOrgIDs[4] != orgB || store.sweptOrgIDs[8] != orgC {
		t.Fatalf("swept org IDs = %+v", store.sweptOrgIDs)
	}
	if len(store.args) != 2 || store.args[0].RowLimit != 2 || store.args[1].AfterID != orgB {
		t.Fatalf("pagination args = %+v", store.args)
	}
}

func TestSweepExpiredForOrgUsesProvidedOrg(t *testing.T) {
	orgID := ids.ToPG(ids.New())
	store := &fakeSweeperOrgStore{}
	if err := SweepExpiredForOrg(context.Background(), store, orgID); err != nil {
		t.Fatal(err)
	}
	if got := store.calls; got != "requeue,fail,expire-runs,expire-waits" {
		t.Fatalf("calls = %s", got)
	}
	if len(store.sweptOrgIDs) != 4 || store.sweptOrgIDs[0] != orgID {
		t.Fatalf("swept org IDs = %+v", store.sweptOrgIDs)
	}
}

func TestNewExpirySweeperValidatesInput(t *testing.T) {
	if _, err := NewExpirySweeper(nil); err == nil {
		t.Fatal("expected nil store error")
	}
	if _, err := NewExpirySweeper(&fakeSweeperStore{}, WithExpirySweepInterval(0)); err == nil {
		t.Fatal("expected invalid interval error")
	}
	if _, err := NewExpirySweeper(&fakeSweeperStore{}, WithExpirySweepOrgLimit(0)); err == nil {
		t.Fatal("expected invalid org limit error")
	}
	if _, err := NewExpirySweeper(&fakeSweeperStore{}, WithExpirySweepConsecutiveFailureLimit(0)); err == nil {
		t.Fatal("expected invalid failure limit error")
	}
	if _, err := NewExpirySweeper(&fakeSweeperStore{}, WithExpirySweepInterval(time.Second)); err != nil {
		t.Fatal(err)
	}
}

func TestSweeperRunReturnsAfterConsecutiveFailures(t *testing.T) {
	listErr := errors.New("list organizations failed")
	store := &fakeSweeperStore{listErr: listErr}
	sweeper, err := NewExpirySweeper(
		store,
		WithExpirySweepInterval(time.Millisecond),
		WithExpirySweepConsecutiveFailureLimit(2),
		WithExpirySweepLogger(slog.New(slog.NewTextHandler(io.Discard, nil))),
	)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()

	err = sweeper.Run(ctx)
	if !errors.Is(err, listErr) {
		t.Fatalf("run error = %v, want %v", err, listErr)
	}
	if len(store.args) != 2 {
		t.Fatalf("list calls = %d, want 2", len(store.args))
	}
}

func TestSweeperRunReturnsContextCancellation(t *testing.T) {
	entered := make(chan struct{})
	store := &fakeSweeperStore{blockUntilCancel: true, entered: entered}
	sweeper, err := NewExpirySweeper(
		store,
		WithExpirySweepInterval(time.Millisecond),
		WithExpirySweepConsecutiveFailureLimit(1),
		WithExpirySweepLogger(slog.New(slog.NewTextHandler(io.Discard, nil))),
	)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	errc := make(chan error, 1)
	go func() {
		errc <- sweeper.Run(ctx)
	}()
	<-entered
	cancel()

	select {
	case err := <-errc:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("run error = %v, want context canceled", err)
		}
	case <-time.After(time.Second):
		t.Fatal("timed out waiting for sweeper to stop")
	}
}

func TestSweeperSkipsWhenLockIsHeld(t *testing.T) {
	store := &fakeSweeperStore{}
	sweeper, err := NewExpirySweeper(store, WithExpirySweepLock(&fakeSweepLock{}))
	if err != nil {
		t.Fatal(err)
	}
	if err := sweeper.sweep(context.Background()); err != nil {
		t.Fatal(err)
	}
	if store.calls != "" {
		t.Fatalf("calls = %s", store.calls)
	}
}

func TestSweeperUnlocksAfterSweep(t *testing.T) {
	lock := &fakeSweepLock{locked: true}
	sweeper, err := NewExpirySweeper(&fakeSweeperStore{orgIDs: []pgtype.UUID{ids.ToPG(ids.New())}}, WithExpirySweepLock(lock))
	if err != nil {
		t.Fatal(err)
	}
	if err := sweeper.sweep(context.Background()); err != nil {
		t.Fatal(err)
	}
	if !lock.guard.unlocked {
		t.Fatal("lock was not released")
	}
}

func TestSweeperUnlocksAfterSweepError(t *testing.T) {
	lock := &fakeSweepLock{locked: true}
	sweeper, err := NewExpirySweeper(&fakeSweeperStore{
		fakeSweeperOrgStore: fakeSweeperOrgStore{requeueErr: errors.New("requeue failed")},
		orgIDs:              []pgtype.UUID{ids.ToPG(ids.New())},
	}, WithExpirySweepLock(lock))
	if err != nil {
		t.Fatal(err)
	}
	if err := sweeper.sweep(context.Background()); err == nil {
		t.Fatal("expected sweep error")
	}
	if !lock.guard.unlocked {
		t.Fatal("lock was not released")
	}
}

type fakeSweeperStore struct {
	fakeSweeperOrgStore
	orgIDs           []pgtype.UUID
	pages            [][]pgtype.UUID
	args             []db.ListOrganizationIDsPageParams
	listErr          error
	blockUntilCancel bool
	entered          chan struct{}
}

func (f *fakeSweeperStore) ListOrganizationIDsPage(ctx context.Context, arg db.ListOrganizationIDsPageParams) ([]pgtype.UUID, error) {
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

type fakeSweeperOrgStore struct {
	calls       string
	sweptOrgIDs []pgtype.UUID
	requeueErr  error
	failErr     error
	expireErr   error
	requeueErrs map[pgtype.UUID]error
}

func (f *fakeSweeperOrgStore) RequeueExpiredLeasedRunExecutions(_ context.Context, orgID pgtype.UUID) error {
	f.sweptOrgIDs = append(f.sweptOrgIDs, orgID)
	f.calls = appendCall(f.calls, "requeue")
	if err := f.requeueErrs[orgID]; err != nil {
		return err
	}
	return f.requeueErr
}

func (f *fakeSweeperOrgStore) FailExpiredRunningRunExecutions(_ context.Context, orgID pgtype.UUID) error {
	f.sweptOrgIDs = append(f.sweptOrgIDs, orgID)
	f.calls = appendCall(f.calls, "fail")
	return f.failErr
}

func (f *fakeSweeperOrgStore) ExpireQueuedRuns(_ context.Context, orgID pgtype.UUID) error {
	f.sweptOrgIDs = append(f.sweptOrgIDs, orgID)
	f.calls = appendCall(f.calls, "expire-runs")
	return nil
}

func (f *fakeSweeperOrgStore) ExpireDuePendingWaitpoints(_ context.Context, orgID pgtype.UUID) error {
	f.sweptOrgIDs = append(f.sweptOrgIDs, orgID)
	f.calls = appendCall(f.calls, "expire-waits")
	return f.expireErr
}

func appendCall(calls string, call string) string {
	if calls == "" {
		return call
	}
	return calls + "," + call
}

type fakeSweepLock struct {
	locked bool
	guard  fakeSweepGuard
}

func (f *fakeSweepLock) TryLock(context.Context) (ExpirySweepLockGuard, bool, error) {
	if !f.locked {
		return nil, false, nil
	}
	return &f.guard, true, nil
}

type fakeSweepGuard struct {
	unlocked bool
}

func (f *fakeSweepGuard) Store(fallback ExpirySweepStore) ExpirySweepStore {
	return fallback
}

func (f *fakeSweepGuard) Unlock(context.Context) error {
	f.unlocked = true
	return nil
}
