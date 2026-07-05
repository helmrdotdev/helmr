package schedule

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/helmrdotdev/helmr/internal/db"
	"github.com/helmrdotdev/helmr/internal/pgvalue"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgtype"
)

const workerTestCellID = "us-east-1-cell-1"

func TestEngineRepairRegistersEveryPage(t *testing.T) {
	ctx := context.Background()
	now := time.Date(2026, 6, 1, 0, 0, 0, 0, time.UTC)
	firstID := uuid.Must(uuid.NewV7())
	secondID := uuid.Must(uuid.NewV7())
	thirdID := uuid.Must(uuid.NewV7())
	store := &fakeRepairStore{
		pages: [][]db.ListScheduleRepairEntriesRow{
			{
				scheduleRepairRow(firstID, 1, now.Add(time.Minute), pgtype.Timestamptz{}),
				scheduleRepairRow(secondID, 1, now.Add(2*time.Minute), pgtype.Timestamptz{}),
			},
			{
				scheduleRepairRow(thirdID, 2, now.Add(3*time.Minute), pgtype.Timestamptz{}),
			},
		},
	}
	index := &fakeScheduleIndex{}
	engine, err := NewEngine(nil, fakeDBTX{}, index, fakeRunCreator{}, EngineConfig{
		CellID:        workerTestCellID,
		RepairLimit:   2,
		ReconcileLock: &fakeReconcileLock{store: store, locked: true},
		Now:           func() time.Time { return now },
	})
	if err != nil {
		t.Fatal(err)
	}

	if err := engine.Repair(ctx); err != nil {
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
	if !store.args[1].AfterAvailableAt.Time.Equal(now.Add(2*time.Minute)) || pgvalue.MustUUIDValue(store.args[1].AfterInstanceID) != secondID {
		t.Fatalf("second page cursor = %+v / %+v", store.args[1].AfterAvailableAt, store.args[1].AfterInstanceID)
	}
}

func TestEngineRepairSkipsWhenLockIsHeld(t *testing.T) {
	ctx := context.Background()
	index := &fakeScheduleIndex{}
	lock := &fakeReconcileLock{store: &fakeRepairStore{}, locked: false}
	engine, err := NewEngine(nil, fakeDBTX{}, index, fakeRunCreator{}, EngineConfig{CellID: workerTestCellID, ReconcileLock: lock})
	if err != nil {
		t.Fatal(err)
	}
	if err := engine.Repair(ctx); err != nil {
		t.Fatal(err)
	}
	if lock.guardRequested {
		t.Fatal("store was requested without lock")
	}
	if len(index.enqueued) != 0 {
		t.Fatalf("enqueued = %d, want 0", len(index.enqueued))
	}
}

func TestEngineDeferTriggerNacksWithoutConsumingAttempts(t *testing.T) {
	ctx := context.Background()
	now := time.Date(2026, 6, 1, 0, 0, 0, 0, time.UTC)
	instanceID := uuid.Must(uuid.NewV7())
	index := &fakeScheduleIndex{}
	engine, err := NewEngine(nil, fakeDBTX{allowExec: true, execTag: pgconn.NewCommandTag("UPDATE 1")}, index, fakeRunCreator{}, EngineConfig{CellID: workerTestCellID, Now: func() time.Time { return now }})
	if err != nil {
		t.Fatal(err)
	}
	lease := IndexLease{
		Entry: IndexEntry{
			CellID:      workerTestCellID,
			InstanceID:  instanceID,
			Generation:  7,
			ScheduledAt: now.Add(-time.Minute),
			AvailableAt: now,
		},
		Attempt: 2,
	}
	row := db.GetScheduleTriggerCandidateRow{
		CellID:              workerTestCellID,
		InstanceID:          pgvalue.UUID(instanceID),
		Generation:          7,
		NextFireAt:          pgtype.Timestamptz{Time: now.Add(-time.Minute), Valid: true},
		TriggerAttemptCount: 3,
	}

	if err := engine.deferTrigger(ctx, lease, row); err != nil {
		t.Fatal(err)
	}
	if len(index.nacks) != 1 {
		t.Fatalf("nacks = %d, want 1", len(index.nacks))
	}
	wantRetryAt := now.Add(RetryDelay(5))
	if !index.nacks[0].retryAt.Equal(wantRetryAt) {
		t.Fatalf("retryAt = %s, want %s", index.nacks[0].retryAt, wantRetryAt)
	}
	if len(index.enqueued) != 0 {
		t.Fatalf("enqueued = %d, want 0", len(index.enqueued))
	}
}

func TestEngineDeferTriggerAcksStaleScheduleRow(t *testing.T) {
	ctx := context.Background()
	now := time.Date(2026, 6, 1, 0, 0, 0, 0, time.UTC)
	instanceID := uuid.Must(uuid.NewV7())
	index := &fakeScheduleIndex{}
	engine, err := NewEngine(nil, fakeDBTX{allowExec: true, execTag: pgconn.NewCommandTag("UPDATE 0")}, index, fakeRunCreator{}, EngineConfig{CellID: workerTestCellID, Now: func() time.Time { return now }})
	if err != nil {
		t.Fatal(err)
	}
	lease := IndexLease{
		Entry: IndexEntry{
			CellID:      workerTestCellID,
			InstanceID:  instanceID,
			Generation:  7,
			ScheduledAt: now.Add(-time.Minute),
			AvailableAt: now,
		},
		Attempt: 1,
	}
	row := db.GetScheduleTriggerCandidateRow{
		CellID:              workerTestCellID,
		InstanceID:          pgvalue.UUID(instanceID),
		Generation:          7,
		NextFireAt:          pgtype.Timestamptz{Time: now.Add(-time.Minute), Valid: true},
		TriggerAttemptCount: 3,
	}

	if err := engine.deferTrigger(ctx, lease, row); err != nil {
		t.Fatal(err)
	}
	if len(index.acks) != 1 {
		t.Fatalf("acks = %d, want 1", len(index.acks))
	}
	if len(index.nacks) != 0 {
		t.Fatalf("nacks = %d, want 0", len(index.nacks))
	}
}

func scheduleRepairRow(instanceID uuid.UUID, generation int64, scheduledAt time.Time, retryAfter pgtype.Timestamptz) db.ListScheduleRepairEntriesRow {
	availableAt := pgtype.Timestamptz{Time: scheduledAt.UTC(), Valid: true}
	if retryAfter.Valid {
		availableAt = retryAfter
	}
	return db.ListScheduleRepairEntriesRow{
		ScheduleID:    pgvalue.UUID(uuid.Must(uuid.NewV7())),
		InstanceID:    pgvalue.UUID(instanceID),
		OrgID:         pgvalue.UUID(uuid.Must(uuid.NewV7())),
		CellID:        workerTestCellID,
		ProjectID:     pgvalue.UUID(uuid.Must(uuid.NewV7())),
		EnvironmentID: pgvalue.UUID(uuid.Must(uuid.NewV7())),
		Generation:    generation,
		NextFireAt:    pgtype.Timestamptz{Time: scheduledAt.UTC(), Valid: true},
		RetryAfter:    retryAfter,
		AvailableAt:   availableAt,
	}
}

type fakeRepairStore struct {
	pages [][]db.ListScheduleRepairEntriesRow
	args  []db.ListScheduleRepairEntriesParams
}

func (f *fakeRepairStore) ListScheduleRepairEntries(_ context.Context, arg db.ListScheduleRepairEntriesParams) ([]db.ListScheduleRepairEntriesRow, error) {
	f.args = append(f.args, arg)
	if len(f.pages) == 0 {
		return nil, nil
	}
	page := f.pages[0]
	f.pages = f.pages[1:]
	return page, nil
}

type fakeReconcileLock struct {
	store          RepairStore
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

func (f *fakeReconcileLockGuard) Store(RepairStore) RepairStore {
	f.owner.guardRequested = true
	return f.owner.store
}

func (f *fakeReconcileLockGuard) Unlock(context.Context) error {
	return nil
}

type fakeScheduleIndex struct {
	enqueued []IndexEntry
	acks     []IndexLease
	nacks    []fakeScheduleNack
}

type fakeScheduleNack struct {
	lease   IndexLease
	retryAt time.Time
}

func (f *fakeScheduleIndex) Enqueue(_ context.Context, entry IndexEntry) error {
	f.enqueued = append(f.enqueued, entry)
	return nil
}

func (f *fakeScheduleIndex) Delete(context.Context, string, uuid.UUID) error {
	return nil
}

func (f *fakeScheduleIndex) Dequeue(context.Context, DequeueRequest) ([]IndexLease, error) {
	return nil, nil
}

func (f *fakeScheduleIndex) Ack(_ context.Context, lease IndexLease) error {
	f.acks = append(f.acks, lease)
	return nil
}

func (f *fakeScheduleIndex) Nack(_ context.Context, lease IndexLease, retryAt time.Time) error {
	f.nacks = append(f.nacks, fakeScheduleNack{lease: lease, retryAt: retryAt})
	return nil
}

type fakeRunCreator struct{}

func (fakeRunCreator) CreateScheduleRun(context.Context, db.GetScheduleTriggerCandidateRow) (pgtype.UUID, error) {
	return pgtype.UUID{}, errors.New("unexpected schedule run creation")
}

type fakeDBTX struct {
	allowExec bool
	execTag   pgconn.CommandTag
	execErr   error
}

func (f fakeDBTX) Exec(context.Context, string, ...any) (pgconn.CommandTag, error) {
	if f.allowExec {
		return f.execTag, f.execErr
	}
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
