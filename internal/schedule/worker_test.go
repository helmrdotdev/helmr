package schedule

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/helmrdotdev/helmr/internal/db"
	"github.com/helmrdotdev/helmr/internal/ids"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgtype"
)

func TestWorkerReconcileIndexesEveryPage(t *testing.T) {
	ctx := context.Background()
	now := time.Date(2026, 6, 1, 0, 0, 0, 0, time.UTC)
	firstID := ids.New()
	secondID := ids.New()
	thirdID := ids.New()
	store := &fakeReconcileStore{
		pages: [][]db.ListScheduleIndexEntriesRow{
			{
				scheduleIndexRow(firstID, 1, now.Add(time.Minute), pgtype.Timestamptz{}),
				scheduleIndexRow(secondID, 1, now.Add(2*time.Minute), pgtype.Timestamptz{}),
			},
			{
				scheduleIndexRow(thirdID, 2, now.Add(3*time.Minute), pgtype.Timestamptz{}),
			},
		},
	}
	index := &fakeScheduleIndex{}
	worker, err := NewWorker(nil, fakeDBTX{}, index, fakeRunCreator{}, WithSweepLimit(2), WithReconcileLock(&fakeReconcileLock{store: store, locked: true}))
	if err != nil {
		t.Fatal(err)
	}
	worker.now = func() time.Time { return now }

	if err := worker.reconcileIndex(ctx); err != nil {
		t.Fatal(err)
	}
	if len(index.enqueued) != 3 {
		t.Fatalf("enqueued = %d, want 3", len(index.enqueued))
	}
	if len(store.args) != 2 {
		t.Fatalf("list calls = %d, want 2", len(store.args))
	}
	if store.args[0].AfterAvailableAt.Valid {
		t.Fatalf("first page after_available_at = %+v, want invalid", store.args[0].AfterAvailableAt)
	}
	if !store.args[1].AfterAvailableAt.Time.Equal(now.Add(2*time.Minute)) || ids.MustFromPG(store.args[1].AfterInstanceID) != secondID {
		t.Fatalf("second page cursor = %+v / %+v", store.args[1].AfterAvailableAt, store.args[1].AfterInstanceID)
	}
}

func TestWorkerReconcileSkipsWhenLockIsHeld(t *testing.T) {
	ctx := context.Background()
	index := &fakeScheduleIndex{}
	lock := &fakeReconcileLock{store: &fakeReconcileStore{}, locked: false}
	worker, err := NewWorker(nil, fakeDBTX{}, index, fakeRunCreator{}, WithReconcileLock(lock))
	if err != nil {
		t.Fatal(err)
	}
	if err := worker.reconcileIndex(ctx); err != nil {
		t.Fatal(err)
	}
	if lock.guardRequested {
		t.Fatal("store was requested without lock")
	}
	if len(index.enqueued) != 0 {
		t.Fatalf("enqueued = %d, want 0", len(index.enqueued))
	}
}

func scheduleIndexRow(instanceID uuid.UUID, generation int64, scheduledAt time.Time, retryAfter pgtype.Timestamptz) db.ListScheduleIndexEntriesRow {
	availableAt := pgtype.Timestamptz{Time: scheduledAt.UTC(), Valid: true}
	if retryAfter.Valid {
		availableAt = retryAfter
	}
	return db.ListScheduleIndexEntriesRow{
		ScheduleID:      ids.ToPG(ids.New()),
		InstanceID:      ids.ToPG(instanceID),
		OrgID:           ids.ToPG(ids.New()),
		ProjectID:       ids.ToPG(ids.New()),
		EnvironmentID:   ids.ToPG(ids.New()),
		Generation:      generation,
		NextScheduledAt: pgtype.Timestamptz{Time: scheduledAt.UTC(), Valid: true},
		RetryAfter:      retryAfter,
		AvailableAt:     availableAt,
	}
}

type fakeReconcileStore struct {
	pages [][]db.ListScheduleIndexEntriesRow
	args  []db.ListScheduleIndexEntriesParams
}

func (f *fakeReconcileStore) ListScheduleIndexEntries(_ context.Context, arg db.ListScheduleIndexEntriesParams) ([]db.ListScheduleIndexEntriesRow, error) {
	f.args = append(f.args, arg)
	if len(f.pages) == 0 {
		return nil, nil
	}
	page := f.pages[0]
	f.pages = f.pages[1:]
	return page, nil
}

type fakeReconcileLock struct {
	store          ReconcileStore
	locked         bool
	guardRequested bool
}

func (f *fakeReconcileLock) TryLock(context.Context) (ReconcileLockGuard, bool, error) {
	if !f.locked {
		return nil, false, nil
	}
	return &fakeReconcileLockGuard{owner: f}, true, nil
}

type fakeReconcileLockGuard struct {
	owner *fakeReconcileLock
}

func (f *fakeReconcileLockGuard) Store(ReconcileStore) ReconcileStore {
	f.owner.guardRequested = true
	return f.owner.store
}

func (f *fakeReconcileLockGuard) Unlock(context.Context) error {
	return nil
}

type fakeScheduleIndex struct {
	enqueued []IndexEntry
}

func (f *fakeScheduleIndex) Enqueue(_ context.Context, entry IndexEntry) error {
	f.enqueued = append(f.enqueued, entry)
	return nil
}

func (f *fakeScheduleIndex) Dequeue(context.Context, DequeueRequest) ([]IndexLease, error) {
	return nil, nil
}

func (f *fakeScheduleIndex) Ack(context.Context, IndexLease) error {
	return nil
}

func (f *fakeScheduleIndex) Nack(context.Context, IndexLease, time.Time) error {
	return nil
}

type fakeRunCreator struct{}

func (fakeRunCreator) CreateScheduleRun(context.Context, db.GetScheduleTriggerCandidateRow) (pgtype.UUID, error) {
	return pgtype.UUID{}, errors.New("unexpected schedule run creation")
}

type fakeDBTX struct{}

func (fakeDBTX) Exec(context.Context, string, ...any) (pgconn.CommandTag, error) {
	return pgconn.CommandTag{}, errors.New("unexpected exec")
}

func (fakeDBTX) Query(context.Context, string, ...any) (pgx.Rows, error) {
	return nil, errors.New("unexpected query")
}

func (fakeDBTX) QueryRow(context.Context, string, ...any) pgx.Row {
	return fakeRow{}
}

type fakeRow struct{}

func (fakeRow) Scan(...any) error {
	return errors.New("unexpected query row")
}
