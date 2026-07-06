package dispatch

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/helmrdotdev/helmr/internal/db"
	"github.com/helmrdotdev/helmr/internal/pgvalue"
	"github.com/jackc/pgx/v5/pgtype"
)

const expirySweeperTestWorkerGroupID = "worker-group-1"

func TestSweepOnce(t *testing.T) {
	orgA := pgvalue.UUID(uuid.Must(uuid.NewV7()))
	orgB := pgvalue.UUID(uuid.Must(uuid.NewV7()))
	store := &fakeSweeperStore{orgIDs: []pgtype.UUID{orgA, orgB}}
	if err := sweepOnce(context.Background(), store, expirySweeperTestWorkerGroupID, DefaultExpirySweepOrgLimit); err != nil {
		t.Fatal(err)
	}
	if got := store.calls; got != "stop-runtime-instances,lose-expired-runtime-instances,requeue,fail,expire-runs,expire-sessions,expire-tokens,resolve-timers,expire-waits,publish-hot-resumes,publish-hot-checkpoints,fail-stale-waits,requeue-waits,requeue,fail,expire-runs,expire-sessions,expire-tokens,resolve-timers,expire-waits,publish-hot-resumes,publish-hot-checkpoints,fail-stale-waits,requeue-waits" {
		t.Fatalf("calls = %s", got)
	}
	if len(store.sweptOrgIDs) != 24 {
		t.Fatalf("swept org IDs = %+v", store.sweptOrgIDs)
	}
	if store.sweptOrgIDs[2] != orgA || store.sweptOrgIDs[13] != orgB {
		t.Fatalf("swept org IDs = %+v", store.sweptOrgIDs)
	}
	if len(store.createExpiredRuntimeStopParams) != 1 || store.createExpiredRuntimeStopParams[0].WorkerGroupID != expirySweeperTestWorkerGroupID {
		t.Fatalf("runtime stop params = %+v", store.createExpiredRuntimeStopParams)
	}
	if len(store.markExpiredRuntimeLostParams) != 1 || store.markExpiredRuntimeLostParams[0].WorkerGroupID != expirySweeperTestWorkerGroupID {
		t.Fatalf("runtime lost params = %+v", store.markExpiredRuntimeLostParams)
	}
}

func TestSweepOnceStopsAfterRequeueError(t *testing.T) {
	store := &fakeSweeperStore{
		fakeSweeperOrgStore: fakeSweeperOrgStore{requeueErr: errors.New("requeue failed")},
		orgIDs:              []pgtype.UUID{pgvalue.UUID(uuid.Must(uuid.NewV7()))},
	}
	if err := sweepOnce(context.Background(), store, expirySweeperTestWorkerGroupID, DefaultExpirySweepOrgLimit); err == nil {
		t.Fatal("expected error")
	}
	if got := store.calls; got != "stop-runtime-instances,lose-expired-runtime-instances,requeue" {
		t.Fatalf("calls = %s", got)
	}
}

func TestSweepOnceContinuesAfterOrgError(t *testing.T) {
	orgA := pgvalue.UUID(uuid.Must(uuid.NewV7()))
	orgB := pgvalue.UUID(uuid.Must(uuid.NewV7()))
	store := &fakeSweeperStore{
		fakeSweeperOrgStore: fakeSweeperOrgStore{requeueErrs: map[pgtype.UUID]error{orgA: errors.New("requeue failed")}},
		orgIDs:              []pgtype.UUID{orgA, orgB},
	}
	if err := sweepOnce(context.Background(), store, expirySweeperTestWorkerGroupID, DefaultExpirySweepOrgLimit); err == nil {
		t.Fatal("expected error")
	}
	if got := store.calls; got != "stop-runtime-instances,lose-expired-runtime-instances,requeue,requeue,fail,expire-runs,expire-sessions,expire-tokens,resolve-timers,expire-waits,publish-hot-resumes,publish-hot-checkpoints,fail-stale-waits,requeue-waits" {
		t.Fatalf("calls = %s", got)
	}
	if len(store.sweptOrgIDs) != 14 || store.sweptOrgIDs[2] != orgA || store.sweptOrgIDs[3] != orgB {
		t.Fatalf("swept org IDs = %+v", store.sweptOrgIDs)
	}
}

func TestSweeperPaginatesOrganizations(t *testing.T) {
	orgA := pgvalue.UUID(uuid.Must(uuid.NewV7()))
	orgB := pgvalue.UUID(uuid.Must(uuid.NewV7()))
	orgC := pgvalue.UUID(uuid.Must(uuid.NewV7()))
	store := &fakeSweeperStore{pages: [][]pgtype.UUID{{orgA, orgB}, {orgC}}}
	sweeper, err := NewExpirySweeper(store, WithExpirySweepWorkerGroupID(expirySweeperTestWorkerGroupID), WithExpirySweepOrgLimit(2))
	if err != nil {
		t.Fatal(err)
	}
	if err := sweeper.sweep(context.Background()); err != nil {
		t.Fatal(err)
	}
	if len(store.sweptOrgIDs) != 35 || store.sweptOrgIDs[2] != orgA || store.sweptOrgIDs[13] != orgB || store.sweptOrgIDs[24] != orgC {
		t.Fatalf("swept org IDs = %+v", store.sweptOrgIDs)
	}
	if len(store.args) != 2 || store.args[0].RowLimit != 2 || store.args[1].AfterID != orgB {
		t.Fatalf("pagination args = %+v", store.args)
	}
}

func TestSweepExpiredForOrgUsesProvidedOrg(t *testing.T) {
	orgID := pgvalue.UUID(uuid.Must(uuid.NewV7()))
	store := &fakeSweeperOrgStore{}
	if err := SweepExpiredForOrg(context.Background(), store, expirySweeperTestWorkerGroupID, orgID); err != nil {
		t.Fatal(err)
	}
	if got := store.calls; got != "requeue,fail,expire-runs,expire-sessions,expire-tokens,resolve-timers,expire-waits,publish-hot-resumes,publish-hot-checkpoints,fail-stale-waits,requeue-waits" {
		t.Fatalf("calls = %s", got)
	}
	if len(store.sweptOrgIDs) != 11 || store.sweptOrgIDs[0] != orgID {
		t.Fatalf("swept org IDs = %+v", store.sweptOrgIDs)
	}
	if len(store.expireDueTokensParams) != 1 || store.expireDueTokensParams[0] != orgID {
		t.Fatalf("expire token params = %+v", store.expireDueTokensParams)
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
	if _, err := NewExpirySweeper(&fakeSweeperStore{}, WithExpirySweepInterval(time.Second)); err == nil {
		t.Fatal("expected missing worker_group_id error")
	}
	if _, err := NewExpirySweeper(&fakeSweeperStore{}, WithExpirySweepWorkerGroupID(expirySweeperTestWorkerGroupID), WithExpirySweepInterval(time.Second)); err != nil {
		t.Fatal(err)
	}
}

func TestSweeperRunReturnsAfterConsecutiveFailures(t *testing.T) {
	listErr := errors.New("list organizations failed")
	store := &fakeSweeperStore{listErr: listErr}
	sweeper, err := NewExpirySweeper(
		store,
		WithExpirySweepWorkerGroupID(expirySweeperTestWorkerGroupID),
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
		WithExpirySweepWorkerGroupID(expirySweeperTestWorkerGroupID),
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
	sweeper, err := NewExpirySweeper(store, WithExpirySweepWorkerGroupID(expirySweeperTestWorkerGroupID), WithExpirySweepLock(&fakeSweepLock{}))
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
	sweeper, err := NewExpirySweeper(&fakeSweeperStore{orgIDs: []pgtype.UUID{pgvalue.UUID(uuid.Must(uuid.NewV7()))}}, WithExpirySweepWorkerGroupID(expirySweeperTestWorkerGroupID), WithExpirySweepLock(lock))
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
		orgIDs:              []pgtype.UUID{pgvalue.UUID(uuid.Must(uuid.NewV7()))},
	}, WithExpirySweepWorkerGroupID(expirySweeperTestWorkerGroupID), WithExpirySweepLock(lock))
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
	calls                          string
	sweptOrgIDs                    []pgtype.UUID
	requeueErr                     error
	failErr                        error
	tokenErr                       error
	timerErr                       error
	waitErr                        error
	requeueErrs                    map[pgtype.UUID]error
	expireDueTokensParams          []pgtype.UUID
	createExpiredRuntimeStopParams []db.CreateExpiredRuntimeStopCommandsParams
	markExpiredRuntimeLostParams   []db.MarkExpiredRuntimeInstancesLostParams
	requeueResolvedRunWaitsParams  db.RequeueResolvedRunWaitsParams
}

func (f *fakeSweeperOrgStore) RequeueExpiredLeasedRunLeases(_ context.Context, arg db.RequeueExpiredLeasedRunLeasesParams) error {
	f.sweptOrgIDs = append(f.sweptOrgIDs, arg.OrgID)
	f.calls = appendCall(f.calls, "requeue")
	if arg.WorkerGroupID != expirySweeperTestWorkerGroupID {
		return fmt.Errorf("requeue worker_group_id = %q", arg.WorkerGroupID)
	}
	if err := f.requeueErrs[arg.OrgID]; err != nil {
		return err
	}
	return f.requeueErr
}

func (f *fakeSweeperOrgStore) FailExpiredRunningRunLeases(_ context.Context, arg db.FailExpiredRunningRunLeasesParams) error {
	f.sweptOrgIDs = append(f.sweptOrgIDs, arg.OrgID)
	f.calls = appendCall(f.calls, "fail")
	if arg.WorkerGroupID != expirySweeperTestWorkerGroupID {
		return fmt.Errorf("fail worker_group_id = %q", arg.WorkerGroupID)
	}
	return f.failErr
}

func (f *fakeSweeperOrgStore) ExpireQueuedRuns(_ context.Context, arg db.ExpireQueuedRunsParams) error {
	f.sweptOrgIDs = append(f.sweptOrgIDs, arg.OrgID)
	f.calls = appendCall(f.calls, "expire-runs")
	if arg.WorkerGroupID != expirySweeperTestWorkerGroupID {
		return fmt.Errorf("expire-runs worker_group_id = %q", arg.WorkerGroupID)
	}
	return nil
}

func (f *fakeSweeperOrgStore) ExpireDueSessions(_ context.Context, arg db.ExpireDueSessionsParams) ([]db.Session, error) {
	f.sweptOrgIDs = append(f.sweptOrgIDs, arg.OrgID)
	f.calls = appendCall(f.calls, "expire-sessions")
	if arg.WorkerGroupID != expirySweeperTestWorkerGroupID {
		return nil, fmt.Errorf("expire-sessions worker_group_id = %q", arg.WorkerGroupID)
	}
	return nil, nil
}

func (f *fakeSweeperOrgStore) ExpireDueTokens(_ context.Context, orgID pgtype.UUID) ([]db.ExpireDueTokensRow, error) {
	f.expireDueTokensParams = append(f.expireDueTokensParams, orgID)
	f.sweptOrgIDs = append(f.sweptOrgIDs, orgID)
	f.calls = appendCall(f.calls, "expire-tokens")
	return nil, f.tokenErr
}

func (f *fakeSweeperOrgStore) ResolveDueTimerWaits(_ context.Context, arg db.ResolveDueTimerWaitsParams) ([]db.ResolveDueTimerWaitsRow, error) {
	if arg.WorkerGroupID != expirySweeperTestWorkerGroupID {
		return nil, fmt.Errorf("resolve-timers worker_group_id = %q", arg.WorkerGroupID)
	}
	f.sweptOrgIDs = append(f.sweptOrgIDs, arg.OrgID)
	f.calls = appendCall(f.calls, "resolve-timers")
	return nil, f.timerErr
}

func (f *fakeSweeperOrgStore) CreateResolvedLiveRuntimeResumeWaitCommandsForOrg(_ context.Context, arg db.CreateResolvedLiveRuntimeResumeWaitCommandsForOrgParams) ([]db.WorkerCommand, error) {
	if arg.WorkerGroupID != expirySweeperTestWorkerGroupID {
		return nil, fmt.Errorf("publish-hot-resumes worker_group_id = %q", arg.WorkerGroupID)
	}
	f.sweptOrgIDs = append(f.sweptOrgIDs, arg.OrgID)
	f.calls = appendCall(f.calls, "publish-hot-resumes")
	return nil, nil
}

func (f *fakeSweeperOrgStore) CreateDueLiveRuntimeCheckpointWaitCommandsForOrg(_ context.Context, arg db.CreateDueLiveRuntimeCheckpointWaitCommandsForOrgParams) ([]db.WorkerCommand, error) {
	if arg.WorkerGroupID != expirySweeperTestWorkerGroupID {
		return nil, fmt.Errorf("publish-hot-checkpoints worker_group_id = %q", arg.WorkerGroupID)
	}
	f.sweptOrgIDs = append(f.sweptOrgIDs, arg.OrgID)
	f.calls = appendCall(f.calls, "publish-hot-checkpoints")
	return nil, nil
}

func (f *fakeSweeperOrgStore) ExpireDueRunWaits(_ context.Context, arg db.ExpireDueRunWaitsParams) ([]db.RunWait, error) {
	if arg.WorkerGroupID != expirySweeperTestWorkerGroupID {
		return nil, fmt.Errorf("expire-waits worker_group_id = %q", arg.WorkerGroupID)
	}
	f.sweptOrgIDs = append(f.sweptOrgIDs, arg.OrgID)
	f.calls = appendCall(f.calls, "expire-waits")
	return nil, f.waitErr
}

func (f *fakeSweeperOrgStore) CreateExpiredRuntimeStopCommands(_ context.Context, arg db.CreateExpiredRuntimeStopCommandsParams) ([]db.WorkerCommand, error) {
	f.createExpiredRuntimeStopParams = append(f.createExpiredRuntimeStopParams, arg)
	f.sweptOrgIDs = append(f.sweptOrgIDs, pgtype.UUID{})
	f.calls = appendCall(f.calls, "stop-runtime-instances")
	return nil, nil
}

func (f *fakeSweeperOrgStore) MarkExpiredRuntimeInstancesLost(_ context.Context, arg db.MarkExpiredRuntimeInstancesLostParams) ([]db.RuntimeInstance, error) {
	f.markExpiredRuntimeLostParams = append(f.markExpiredRuntimeLostParams, arg)
	f.sweptOrgIDs = append(f.sweptOrgIDs, pgtype.UUID{})
	f.calls = appendCall(f.calls, "lose-expired-runtime-instances")
	return nil, nil
}

func (f *fakeSweeperOrgStore) FailStaleResolvedRunWaits(_ context.Context, arg db.FailStaleResolvedRunWaitsParams) ([]db.FailStaleResolvedRunWaitsRow, error) {
	if arg.WorkerGroupID != expirySweeperTestWorkerGroupID {
		return nil, fmt.Errorf("fail-stale-waits worker_group_id = %q", arg.WorkerGroupID)
	}
	f.sweptOrgIDs = append(f.sweptOrgIDs, arg.OrgID)
	f.calls = appendCall(f.calls, "fail-stale-waits")
	return nil, nil
}

func (f *fakeSweeperOrgStore) RequeueResolvedRunWaits(_ context.Context, arg db.RequeueResolvedRunWaitsParams) ([]db.RequeueResolvedRunWaitsRow, error) {
	if arg.WorkerGroupID != expirySweeperTestWorkerGroupID {
		return nil, fmt.Errorf("requeue-waits worker_group_id = %q", arg.WorkerGroupID)
	}
	f.sweptOrgIDs = append(f.sweptOrgIDs, arg.OrgID)
	f.calls = appendCall(f.calls, "requeue-waits")
	f.requeueResolvedRunWaitsParams = arg
	return nil, nil
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
