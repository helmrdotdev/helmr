package control

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgtype"
)

func TestSweepOnce(t *testing.T) {
	store := &fakeSweeperStore{}
	if err := SweepOnce(context.Background(), store); err != nil {
		t.Fatal(err)
	}
	if got := store.calls; got != "requeue,fail,expire-waits" {
		t.Fatalf("calls = %s", got)
	}
	if !store.orgID.Valid {
		t.Fatal("org id was not passed")
	}
}

func TestSweepOnceStopsAfterRequeueError(t *testing.T) {
	store := &fakeSweeperStore{requeueErr: errors.New("requeue failed")}
	if err := SweepOnce(context.Background(), store); err == nil {
		t.Fatal("expected error")
	}
	if got := store.calls; got != "requeue" {
		t.Fatalf("calls = %s", got)
	}
}

func TestNewSweeperValidatesInput(t *testing.T) {
	if _, err := NewSweeper(nil); err == nil {
		t.Fatal("expected nil store error")
	}
	if _, err := NewSweeper(&fakeSweeperStore{}, WithSweepInterval(0)); err == nil {
		t.Fatal("expected invalid interval error")
	}
	if _, err := NewSweeper(&fakeSweeperStore{}, WithSweepInterval(time.Second)); err != nil {
		t.Fatal(err)
	}
}

func TestSweeperSkipsWhenLockIsHeld(t *testing.T) {
	store := &fakeSweeperStore{}
	sweeper, err := NewSweeper(store, WithSweepLock(&fakeSweepLock{}))
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
	sweeper, err := NewSweeper(&fakeSweeperStore{}, WithSweepLock(lock))
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
	sweeper, err := NewSweeper(&fakeSweeperStore{requeueErr: errors.New("requeue failed")}, WithSweepLock(lock))
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
	calls      string
	orgID      pgtype.UUID
	requeueErr error
	failErr    error
	expireErr  error
}

func (f *fakeSweeperStore) RequeueExpiredClaimedRunExecutions(_ context.Context, orgID pgtype.UUID) error {
	f.orgID = orgID
	f.calls = appendCall(f.calls, "requeue")
	return f.requeueErr
}

func (f *fakeSweeperStore) FailExpiredRunningRunExecutions(_ context.Context, orgID pgtype.UUID) error {
	f.orgID = orgID
	f.calls = appendCall(f.calls, "fail")
	return f.failErr
}

func (f *fakeSweeperStore) ExpireDuePendingWaitpoints(_ context.Context, orgID pgtype.UUID) error {
	f.orgID = orgID
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

func (f *fakeSweepLock) TryLock(context.Context) (SweepLockGuard, bool, error) {
	if !f.locked {
		return nil, false, nil
	}
	return &f.guard, true, nil
}

type fakeSweepGuard struct {
	unlocked bool
}

func (f *fakeSweepGuard) Store(fallback SweeperStore) SweeperStore {
	return fallback
}

func (f *fakeSweepGuard) Unlock(context.Context) error {
	f.unlocked = true
	return nil
}
