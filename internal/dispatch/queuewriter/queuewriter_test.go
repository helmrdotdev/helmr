package queuewriter

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/helmrdotdev/helmr/internal/db"
	"github.com/helmrdotdev/helmr/internal/dispatch"
	"github.com/helmrdotdev/helmr/internal/ids"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
)

func TestEnqueueRunPublishesPreparedMessageAndMarksEnqueued(t *testing.T) {
	ctx := context.Background()
	runID := ids.ToPG(ids.New())
	orgID := ids.ToPG(ids.New())
	store := &fakeStore{
		prepare: testPreparedRunQueueEntry(orgID, runID),
	}
	queue := &fakeQueue{result: dispatch.EnqueueResult{QueueName: "queue-a", MessageID: "message-1", Depth: 1}}
	queueWriter, err := New(store, queue)
	if err != nil {
		t.Fatal(err)
	}

	result, err := queueWriter.EnqueueRun(ctx, orgID, runID)
	if err != nil {
		t.Fatal(err)
	}

	if result.MessageID != "message-1" || len(queue.messages) != 1 {
		t.Fatalf("result = %+v messages = %+v", result, queue.messages)
	}
	message := queue.messages[0]
	if message.OrgID != ids.MustFromPG(orgID).String() || message.RunID != ids.MustFromPG(runID).String() || message.WorkerGroupID == "" {
		t.Fatalf("message ids = %+v", message)
	}
	if message.Requirements.Resources.MilliCPU != 3000 || message.Requirements.Resources.MemoryMiB != 4096 || message.Requirements.Resources.Slots != 1 {
		t.Fatalf("message requirements = %+v", message.Requirements)
	}
	if store.markEnqueued.QueueMessageID != "message-1" || store.markEnqueued.ExpectedQueueVersion != store.prepare.QueueVersion || store.markError.RunID.Valid {
		t.Fatalf("mark enqueued = %+v mark error = %+v", store.markEnqueued, store.markError)
	}
}

func TestEnqueueRunMarksQueueErrors(t *testing.T) {
	ctx := context.Background()
	runID := ids.ToPG(ids.New())
	orgID := ids.ToPG(ids.New())
	store := &fakeStore{
		prepare: testPreparedRunQueueEntry(orgID, runID),
	}
	queue := &fakeQueue{err: errors.New("redis unavailable")}
	queueWriter, err := New(store, queue)
	if err != nil {
		t.Fatal(err)
	}

	if _, err := queueWriter.EnqueueRun(ctx, orgID, runID); err == nil {
		t.Fatal("enqueue error = nil")
	}
	if store.markError.LastError != "redis unavailable" || store.markError.ExpectedQueueVersion != store.prepare.QueueVersion || store.markEnqueued.RunID.Valid {
		t.Fatalf("mark error = %+v mark enqueued = %+v", store.markError, store.markEnqueued)
	}
}

func TestReconcileOrgContinuesAfterFailures(t *testing.T) {
	ctx := context.Background()
	orgID := ids.ToPG(ids.New())
	firstRunID := ids.ToPG(ids.New())
	secondRunID := ids.ToPG(ids.New())
	store := &fakeStore{
		prepareByRun: map[pgtype.UUID]db.PrepareQueuedRunQueueEntryRow{
			firstRunID:  testPreparedRunQueueEntry(orgID, firstRunID),
			secondRunID: testPreparedRunQueueEntry(orgID, secondRunID),
		},
		candidates: []db.ListQueuedRunQueueEntryCandidatesRow{
			{OrgID: orgID, RunID: firstRunID},
			{OrgID: orgID, RunID: secondRunID},
		},
	}
	queue := &fakeQueue{
		result: dispatch.EnqueueResult{QueueName: "queue-a", MessageID: "message-1", Depth: 1},
		errByRun: map[string]error{
			ids.MustFromPG(secondRunID).String(): errors.New("redis unavailable"),
		},
	}
	queueWriter, err := New(store, queue)
	if err != nil {
		t.Fatal(err)
	}

	stats, err := queueWriter.ReconcileOrg(ctx, orgID, 10)
	if err == nil {
		t.Fatal("reconcile error = nil")
	}
	if stats.Scanned != 2 || stats.Enqueued != 1 || stats.Failed != 1 || stats.Skipped != 0 {
		t.Fatalf("stats = %+v", stats)
	}
	if len(queue.messages) != 2 || store.markError.RunID != secondRunID {
		t.Fatalf("messages = %+v mark error = %+v", queue.messages, store.markError)
	}
}

func TestReconcileOrgSkipsQueuedRunWhenRedisReadyMessageExists(t *testing.T) {
	ctx := context.Background()
	orgID := ids.ToPG(ids.New())
	runID := ids.ToPG(ids.New())
	store := &fakeStore{
		prepare: testPreparedRunQueueEntry(orgID, runID),
		candidates: []db.ListQueuedRunQueueEntryCandidatesRow{
			{OrgID: orgID, RunID: runID, QueueMessageID: "message-existing"},
		},
	}
	queue := &fakeQueue{existingMessages: map[string]bool{"message-existing": true}}
	queueWriter, err := New(store, queue)
	if err != nil {
		t.Fatal(err)
	}

	stats, err := queueWriter.ReconcileOrg(ctx, orgID, 10)
	if err != nil {
		t.Fatal(err)
	}
	if stats.Scanned != 1 || stats.Enqueued != 0 || stats.Skipped != 1 || stats.Failed != 0 {
		t.Fatalf("stats = %+v", stats)
	}
	if len(queue.messages) != 0 {
		t.Fatalf("messages = %+v", queue.messages)
	}
}

func TestReconcileOrgReenqueuesQueuedRunWhenRedisMessageMissing(t *testing.T) {
	ctx := context.Background()
	orgID := ids.ToPG(ids.New())
	runID := ids.ToPG(ids.New())
	store := &fakeStore{
		prepare: testPreparedRunQueueEntry(orgID, runID),
		candidates: []db.ListQueuedRunQueueEntryCandidatesRow{
			{OrgID: orgID, RunID: runID, QueueMessageID: "message-missing"},
		},
	}
	queue := &fakeQueue{}
	queueWriter, err := New(store, queue)
	if err != nil {
		t.Fatal(err)
	}

	stats, err := queueWriter.ReconcileOrg(ctx, orgID, 10)
	if err != nil {
		t.Fatal(err)
	}
	if stats.Scanned != 1 || stats.Enqueued != 1 || stats.Skipped != 0 || stats.Failed != 0 {
		t.Fatalf("stats = %+v", stats)
	}
	if len(queue.messages) != 1 || store.markEnqueued.QueueMessageID == "" {
		t.Fatalf("messages = %+v mark enqueued = %+v", queue.messages, store.markEnqueued)
	}
}

func TestEnqueueRunReturnsNoCandidate(t *testing.T) {
	ctx := context.Background()
	queueWriter, err := New(&fakeStore{prepareErr: pgx.ErrNoRows}, &fakeQueue{})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := queueWriter.EnqueueRun(ctx, ids.ToPG(ids.New()), ids.ToPG(ids.New())); !errors.Is(err, ErrNoQueueCandidate) {
		t.Fatalf("enqueue error = %v, want no queue candidate", err)
	}
}

type fakeStore struct {
	prepare      db.PrepareQueuedRunQueueEntryRow
	prepareByRun map[pgtype.UUID]db.PrepareQueuedRunQueueEntryRow
	prepareErr   error
	candidates   []db.ListQueuedRunQueueEntryCandidatesRow
	markEnqueued db.MarkRunQueueEntryEnqueuedParams
	markError    db.MarkRunQueueEntryEnqueueErrorParams
}

func (f *fakeStore) PrepareQueuedRunQueueEntry(_ context.Context, arg db.PrepareQueuedRunQueueEntryParams) (db.PrepareQueuedRunQueueEntryRow, error) {
	if f.prepareErr != nil {
		return db.PrepareQueuedRunQueueEntryRow{}, f.prepareErr
	}
	if row, ok := f.prepareByRun[arg.RunID]; ok {
		return row, nil
	}
	return f.prepare, nil
}

func (f *fakeStore) ListQueuedRunQueueEntryCandidates(_ context.Context, arg db.ListQueuedRunQueueEntryCandidatesParams) ([]db.ListQueuedRunQueueEntryCandidatesRow, error) {
	if int32(len(f.candidates)) > arg.RowLimit {
		return f.candidates[:arg.RowLimit], nil
	}
	return f.candidates, nil
}

func (f *fakeStore) MarkRunQueueEntryEnqueued(_ context.Context, arg db.MarkRunQueueEntryEnqueuedParams) (db.RunQueueEntry, error) {
	f.markEnqueued = arg
	return db.RunQueueEntry{}, nil
}

func (f *fakeStore) MarkRunQueueEntryEnqueueError(_ context.Context, arg db.MarkRunQueueEntryEnqueueErrorParams) (db.RunQueueEntry, error) {
	f.markError = arg
	return db.RunQueueEntry{}, nil
}

type fakeQueue struct {
	result           dispatch.EnqueueResult
	err              error
	errByRun         map[string]error
	existingMessages map[string]bool
	messages         []dispatch.QueueMessage
}

func (f *fakeQueue) Enqueue(_ context.Context, message dispatch.QueueMessage) (dispatch.EnqueueResult, error) {
	f.messages = append(f.messages, message)
	if err := f.errByRun[message.RunID]; err != nil {
		return dispatch.EnqueueResult{}, err
	}
	if f.err != nil {
		return dispatch.EnqueueResult{}, f.err
	}
	result := f.result
	if result.MessageID == "" {
		result.MessageID = "message-" + message.RunID
	}
	if result.QueueName == "" {
		result.QueueName = message.QueueName
	}
	return result, nil
}

func (f *fakeQueue) Dequeue(context.Context, dispatch.DequeueRequest) ([]dispatch.Lease, error) {
	return nil, nil
}

func (f *fakeQueue) ReadyMessageExists(_ context.Context, messageID string) (bool, error) {
	return f.existingMessages[messageID], nil
}

func (f *fakeQueue) Ack(context.Context, dispatch.Lease) error {
	return nil
}

func (f *fakeQueue) Nack(context.Context, dispatch.Lease, dispatch.NackReason) error {
	return nil
}

func (f *fakeQueue) Renew(_ context.Context, lease dispatch.Lease, expiresAt time.Time) (dispatch.Lease, error) {
	lease.ExpiresAt = expiresAt
	return lease, nil
}

func testPreparedRunQueueEntry(orgID pgtype.UUID, runID pgtype.UUID) db.PrepareQueuedRunQueueEntryRow {
	return db.PrepareQueuedRunQueueEntryRow{
		RunID:                   runID,
		OrgID:                   orgID,
		ProjectID:               ids.ToPG(ids.New()),
		EnvironmentID:           ids.ToPG(ids.New()),
		WorkerGroupID:           ids.ToPG(ids.New()),
		QueueName:               "queue-a",
		QueueVersion:            7,
		EnqueuedAt:              pgtype.Timestamptz{Time: time.Date(2026, 5, 19, 0, 0, 0, 0, time.UTC), Valid: true},
		RequestedMilliCpu:       3000,
		RequestedMemoryMib:      4096,
		RequestedDiskMib:        0,
		RequestedExecutionSlots: 1,
		NetworkPolicy:           []byte(`{}`),
		Placement:               []byte(`{}`),
	}
}

var _ Store = (*fakeStore)(nil)
var _ dispatch.RunQueue = (*fakeQueue)(nil)

func TestRequirementsFromRowRejectsInvalidJSON(t *testing.T) {
	row := testPreparedRunQueueEntry(ids.ToPG(ids.New()), ids.ToPG(ids.New()))
	row.NetworkPolicy = []byte(`{`)
	if _, err := queueMessage(row); err == nil {
		t.Fatal("queueMessage error = nil")
	}
}
