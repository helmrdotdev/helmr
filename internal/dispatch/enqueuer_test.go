package dispatch

import (
	"context"
	"errors"
	"testing"
	"time"
	"unicode/utf8"

	"github.com/google/uuid"
	"github.com/helmrdotdev/helmr/internal/db"
	"github.com/helmrdotdev/helmr/internal/pgvalue"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
)

func TestEnqueueRunPublishesPreparedMessageAndMarksEnqueued(t *testing.T) {
	ctx := context.Background()
	runID := pgvalue.UUID(uuid.Must(uuid.NewV7()))
	orgID := pgvalue.UUID(uuid.Must(uuid.NewV7()))
	store := &fakeEnqueuerStore{
		prepare: testPreparedRunQueueItem(orgID, runID),
	}
	store.prepare.QueueConcurrencyLimit = pgtype.Int4{Int32: 3, Valid: true}
	queue := &fakeEnqueuerQueue{result: EnqueueResult{QueueName: "queue-a", MessageID: "message-1", Depth: 1}}
	enqueuer, err := NewEnqueuer(store, queue)
	if err != nil {
		t.Fatal(err)
	}

	result, err := enqueuer.EnqueueRun(ctx, orgID, runID)
	if err != nil {
		t.Fatal(err)
	}

	if result.MessageID != "message-1" || len(queue.messages) != 1 {
		t.Fatalf("result = %+v messages = %+v", result, queue.messages)
	}
	message := queue.messages[0]
	if message.OrgID != pgvalue.MustUUIDValue(orgID).String() || message.RunID != pgvalue.MustUUIDValue(runID).String() || message.QueueName == "" {
		t.Fatalf("message ids = %+v", message)
	}
	if err := message.Validate(); err != nil {
		t.Fatalf("message validation error = %v", err)
	}
	if message.QueueConcurrencyScope != "queue-a" || message.QueueConcurrencyLimit != 3 {
		t.Fatalf("message queue concurrency = %+v", message)
	}
	if message.Requirements.Resources.MilliCPU != 3000 || message.Requirements.Resources.MemoryMiB != 4096 || message.Requirements.Resources.Slots != 1 {
		t.Fatalf("message requirements = %+v", message.Requirements)
	}
	if store.markEnqueued.DispatchMessageID.String != "message-1" ||
		store.markEnqueued.CellID != store.prepare.CellID ||
		store.markEnqueued.RouteGeneration != store.prepare.RouteGeneration ||
		store.markEnqueued.QueueClass != store.prepare.QueueClass ||
		store.markEnqueued.ExpectedDispatchGeneration != store.prepare.DispatchGeneration ||
		store.markError.RunID.Valid {
		t.Fatalf("mark enqueued = %+v mark error = %+v", store.markEnqueued, store.markError)
	}
}

func TestEnqueueRunMarksQueueErrors(t *testing.T) {
	ctx := context.Background()
	runID := pgvalue.UUID(uuid.Must(uuid.NewV7()))
	orgID := pgvalue.UUID(uuid.Must(uuid.NewV7()))
	store := &fakeEnqueuerStore{
		prepare: testPreparedRunQueueItem(orgID, runID),
	}
	queue := &fakeEnqueuerQueue{err: errors.New("redis unavailable")}
	enqueuer, err := NewEnqueuer(store, queue)
	if err != nil {
		t.Fatal(err)
	}

	if _, err := enqueuer.EnqueueRun(ctx, orgID, runID); err == nil {
		t.Fatal("enqueue error = nil")
	}
	if store.markError.LastError != "redis unavailable" ||
		store.markError.CellID != store.prepare.CellID ||
		store.markError.RouteGeneration != store.prepare.RouteGeneration ||
		store.markError.QueueClass != store.prepare.QueueClass ||
		store.markError.ExpectedDispatchGeneration != store.prepare.DispatchGeneration ||
		store.markEnqueued.RunID.Valid {
		t.Fatalf("mark error = %+v mark enqueued = %+v", store.markError, store.markEnqueued)
	}
}

func TestTruncateErrorPreservesUTF8(t *testing.T) {
	got := truncateError(errors.New("prefix 日本語 suffix"), len("prefix 日")+1)
	if !utf8.ValidString(got) {
		t.Fatalf("truncated error is invalid utf8: %q", got)
	}
	if got != "prefix 日" {
		t.Fatalf("truncated error = %q", got)
	}
}

func TestReconcileQueueScopeContinuesAfterFailures(t *testing.T) {
	ctx := context.Background()
	orgID := pgvalue.UUID(uuid.Must(uuid.NewV7()))
	projectID := pgvalue.UUID(uuid.Must(uuid.NewV7()))
	environmentID := pgvalue.UUID(uuid.Must(uuid.NewV7()))
	scope := QueueScope{OrgID: orgID, ProjectID: projectID, EnvironmentID: environmentID, QueueClass: "standard", QueueName: "queue-a"}
	firstRunID := pgvalue.UUID(uuid.Must(uuid.NewV7()))
	secondRunID := pgvalue.UUID(uuid.Must(uuid.NewV7()))
	store := &fakeEnqueuerStore{
		prepareByRun: map[pgtype.UUID]db.PrepareQueuedRunQueueItemRow{
			firstRunID:  testPreparedRunQueueItemWithScope(orgID, projectID, environmentID, firstRunID),
			secondRunID: testPreparedRunQueueItemWithScope(orgID, projectID, environmentID, secondRunID),
		},
		candidates: []db.ListQueuedRunQueueItemCandidatesForScopeRow{
			{OrgID: orgID, RunID: firstRunID},
			{OrgID: orgID, RunID: secondRunID},
		},
	}
	queue := &fakeEnqueuerQueue{
		result: EnqueueResult{QueueName: "queue-a", MessageID: "message-1", Depth: 1},
		errByRun: map[string]error{
			pgvalue.MustUUIDValue(secondRunID).String(): errors.New("redis unavailable"),
		},
	}
	enqueuer, err := NewEnqueuer(store, queue)
	if err != nil {
		t.Fatal(err)
	}

	stats, err := enqueuer.ReconcileQueueScope(ctx, scope, 10)
	if err == nil {
		t.Fatal("reconcile error = nil")
	}
	if stats.Scanned != 2 || stats.Enqueued != 1 || stats.Failed != 1 || stats.Skipped != 0 {
		t.Fatalf("stats = %+v", stats)
	}
	if len(queue.messages) != 2 || store.markError.RunID != secondRunID {
		t.Fatalf("messages = %+v mark error = %+v", queue.messages, store.markError)
	}
	if store.scopeArgs.QueueName != scope.QueueName || store.scopeArgs.QueueClass != scope.QueueClass || store.scopeArgs.OrgID != scope.OrgID || store.scopeArgs.ProjectID != scope.ProjectID || store.scopeArgs.EnvironmentID != scope.EnvironmentID {
		t.Fatalf("scope args = %+v", store.scopeArgs)
	}
}

func TestReconcileQueueScopeSkipsQueuedRunWhenRedisReadyMessageExists(t *testing.T) {
	ctx := context.Background()
	orgID := pgvalue.UUID(uuid.Must(uuid.NewV7()))
	projectID := pgvalue.UUID(uuid.Must(uuid.NewV7()))
	environmentID := pgvalue.UUID(uuid.Must(uuid.NewV7()))
	scope := QueueScope{OrgID: orgID, ProjectID: projectID, EnvironmentID: environmentID, QueueName: "queue-a"}
	runID := pgvalue.UUID(uuid.Must(uuid.NewV7()))
	store := &fakeEnqueuerStore{
		prepare: testPreparedRunQueueItemWithScope(orgID, projectID, environmentID, runID),
		candidates: []db.ListQueuedRunQueueItemCandidatesForScopeRow{
			{OrgID: orgID, RunID: runID, DispatchMessageID: "message-existing"},
		},
	}
	queue := &fakeEnqueuerQueue{existingMessages: map[string]bool{"message-existing": true}}
	enqueuer, err := NewEnqueuer(store, queue)
	if err != nil {
		t.Fatal(err)
	}

	stats, err := enqueuer.ReconcileQueueScope(ctx, scope, 10)
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

func TestReconcileQueueScopeReenqueuesQueuedRunWhenRedisMessageMissing(t *testing.T) {
	ctx := context.Background()
	orgID := pgvalue.UUID(uuid.Must(uuid.NewV7()))
	projectID := pgvalue.UUID(uuid.Must(uuid.NewV7()))
	environmentID := pgvalue.UUID(uuid.Must(uuid.NewV7()))
	scope := QueueScope{OrgID: orgID, ProjectID: projectID, EnvironmentID: environmentID, QueueName: "queue-a"}
	runID := pgvalue.UUID(uuid.Must(uuid.NewV7()))
	store := &fakeEnqueuerStore{
		prepare: testPreparedRunQueueItemWithScope(orgID, projectID, environmentID, runID),
		candidates: []db.ListQueuedRunQueueItemCandidatesForScopeRow{
			{OrgID: orgID, RunID: runID, DispatchMessageID: "message-missing"},
		},
	}
	queue := &fakeEnqueuerQueue{}
	enqueuer, err := NewEnqueuer(store, queue)
	if err != nil {
		t.Fatal(err)
	}

	stats, err := enqueuer.ReconcileQueueScope(ctx, scope, 10)
	if err != nil {
		t.Fatal(err)
	}
	if stats.Scanned != 1 || stats.Enqueued != 1 || stats.Skipped != 0 || stats.Failed != 0 {
		t.Fatalf("stats = %+v", stats)
	}
	if len(queue.messages) != 1 || !store.markEnqueued.DispatchMessageID.Valid {
		t.Fatalf("messages = %+v mark enqueued = %+v", queue.messages, store.markEnqueued)
	}
}

func TestReconcileQueueScopeReenqueuesQueuedRunWhenRedisMessageInvalidated(t *testing.T) {
	ctx := context.Background()
	orgID := pgvalue.UUID(uuid.Must(uuid.NewV7()))
	projectID := pgvalue.UUID(uuid.Must(uuid.NewV7()))
	environmentID := pgvalue.UUID(uuid.Must(uuid.NewV7()))
	scope := QueueScope{OrgID: orgID, ProjectID: projectID, EnvironmentID: environmentID, QueueName: "queue-a"}
	runID := pgvalue.UUID(uuid.Must(uuid.NewV7()))
	store := &fakeEnqueuerStore{
		prepare: testPreparedRunQueueItemWithScope(orgID, projectID, environmentID, runID),
		candidates: []db.ListQueuedRunQueueItemCandidatesForScopeRow{
			{OrgID: orgID, RunID: runID, DispatchMessageID: "message-invalidated"},
		},
	}
	queue := &fakeEnqueuerQueue{invalidatedMessages: map[string]bool{"message-invalidated": true}}
	enqueuer, err := NewEnqueuer(store, queue)
	if err != nil {
		t.Fatal(err)
	}

	stats, err := enqueuer.ReconcileQueueScope(ctx, scope, 10)
	if err != nil {
		t.Fatal(err)
	}
	if stats.Scanned != 1 || stats.Enqueued != 1 || stats.Skipped != 0 || stats.Failed != 0 {
		t.Fatalf("stats = %+v", stats)
	}
	if len(queue.messages) != 1 || !store.markEnqueued.DispatchMessageID.Valid || !queue.invalidatedMessages["message-invalidated"] {
		t.Fatalf("messages = %+v mark enqueued = %+v invalidated = %+v", queue.messages, store.markEnqueued, queue.invalidatedMessages)
	}
}

func TestEnqueueRunReturnsNoCandidate(t *testing.T) {
	ctx := context.Background()
	enqueuer, err := NewEnqueuer(&fakeEnqueuerStore{prepareErr: pgx.ErrNoRows}, &fakeEnqueuerQueue{})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := enqueuer.EnqueueRun(ctx, pgvalue.UUID(uuid.Must(uuid.NewV7())), pgvalue.UUID(uuid.Must(uuid.NewV7()))); !errors.Is(err, ErrNoEnqueueCandidate) {
		t.Fatalf("enqueue error = %v, want no queue candidate", err)
	}
}

type fakeEnqueuerStore struct {
	prepare      db.PrepareQueuedRunQueueItemRow
	prepareByRun map[pgtype.UUID]db.PrepareQueuedRunQueueItemRow
	prepareErr   error
	candidates   []db.ListQueuedRunQueueItemCandidatesForScopeRow
	scopeArgs    db.ListQueuedRunQueueItemCandidatesForScopeParams
	markEnqueued db.MarkRunQueueItemEnqueuedParams
	markError    db.MarkRunQueueItemEnqueueErrorParams
}

func (f *fakeEnqueuerStore) PrepareQueuedRunQueueItem(_ context.Context, arg db.PrepareQueuedRunQueueItemParams) (db.PrepareQueuedRunQueueItemRow, error) {
	if f.prepareErr != nil {
		return db.PrepareQueuedRunQueueItemRow{}, f.prepareErr
	}
	if row, ok := f.prepareByRun[arg.RunID]; ok {
		return row, nil
	}
	return f.prepare, nil
}

func (f *fakeEnqueuerStore) ListQueuedRunQueueItemCandidatesForScope(_ context.Context, arg db.ListQueuedRunQueueItemCandidatesForScopeParams) ([]db.ListQueuedRunQueueItemCandidatesForScopeRow, error) {
	f.scopeArgs = arg
	if int32(len(f.candidates)) > arg.RowLimit {
		return f.candidates[:arg.RowLimit], nil
	}
	return f.candidates, nil
}

func (f *fakeEnqueuerStore) MarkRunQueueItemEnqueued(_ context.Context, arg db.MarkRunQueueItemEnqueuedParams) (db.RunQueueItem, error) {
	f.markEnqueued = arg
	return db.RunQueueItem{}, nil
}

func (f *fakeEnqueuerStore) MarkRunQueueItemEnqueueError(_ context.Context, arg db.MarkRunQueueItemEnqueueErrorParams) (db.RunQueueItem, error) {
	f.markError = arg
	return db.RunQueueItem{}, nil
}

type fakeEnqueuerQueue struct {
	result              EnqueueResult
	err                 error
	errByRun            map[string]error
	existingMessages    map[string]bool
	invalidatedMessages map[string]bool
	messages            []Message
}

func (f *fakeEnqueuerQueue) Enqueue(_ context.Context, message Message) (EnqueueResult, error) {
	f.messages = append(f.messages, message)
	if err := f.errByRun[message.RunID]; err != nil {
		return EnqueueResult{}, err
	}
	if f.err != nil {
		return EnqueueResult{}, f.err
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

func (f *fakeEnqueuerQueue) Dequeue(context.Context, DequeueRequest) ([]Lease, error) {
	return nil, nil
}

func (f *fakeEnqueuerQueue) ReadyMessageExists(_ context.Context, messageID string) (bool, error) {
	if f.invalidatedMessages[messageID] {
		return false, nil
	}
	return f.existingMessages[messageID], nil
}

func (f *fakeEnqueuerQueue) Ack(context.Context, Lease) error {
	return nil
}

func (f *fakeEnqueuerQueue) Nack(context.Context, Lease, NackReason) error {
	return nil
}

func (f *fakeEnqueuerQueue) Renew(_ context.Context, lease Lease, expiresAt time.Time) (Lease, error) {
	lease.ExpiresAt = expiresAt
	return lease, nil
}

func testPreparedRunQueueItem(orgID pgtype.UUID, runID pgtype.UUID) db.PrepareQueuedRunQueueItemRow {
	return testPreparedRunQueueItemWithScope(orgID, pgvalue.UUID(uuid.Must(uuid.NewV7())), pgvalue.UUID(uuid.Must(uuid.NewV7())), runID)
}

func testPreparedRunQueueItemWithScope(orgID pgtype.UUID, projectID pgtype.UUID, environmentID pgtype.UUID, runID pgtype.UUID) db.PrepareQueuedRunQueueItemRow {
	return db.PrepareQueuedRunQueueItemRow{
		RunID:                   runID,
		OrgID:                   orgID,
		CellID:                  "cell-1",
		RouteGeneration:         1,
		QueueClass:              "default",
		ProjectID:               projectID,
		EnvironmentID:           environmentID,
		QueueName:               "queue-a",
		DispatchGeneration:      7,
		EnqueuedAt:              pgtype.Timestamptz{Time: time.Date(2026, 5, 19, 0, 0, 0, 0, time.UTC), Valid: true},
		RequestedMilliCpu:       3000,
		RequestedMemoryMib:      4096,
		RequestedDiskMib:        0,
		RequestedExecutionSlots: 1,
		RuntimeID:               "sha256:runtime",
		RuntimeArch:             "arm64",
		RuntimeABI:              "helmr.firecracker.snapshot.v0",
		KernelDigest:            "sha256:kernel",
		InitramfsDigest:         "sha256:initramfs",
		RootfsDigest:            "sha256:rootfs",
		CniProfile:              "helmr/v0",
		NetworkPolicy:           []byte(`{}`),
		Placement:               []byte(`{}`),
	}
}

var _ EnqueuerStore = (*fakeEnqueuerStore)(nil)
var _ Queue = (*fakeEnqueuerQueue)(nil)

func TestRequirementsFromRowRejectsInvalidJSON(t *testing.T) {
	row := testPreparedRunQueueItem(pgvalue.UUID(uuid.Must(uuid.NewV7())), pgvalue.UUID(uuid.Must(uuid.NewV7())))
	row.NetworkPolicy = []byte(`{`)
	if _, err := queueMessage(row); err == nil {
		t.Fatal("queueMessage error = nil")
	}
}
