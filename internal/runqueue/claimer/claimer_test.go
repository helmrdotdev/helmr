package claimer

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/helmrdotdev/helmr/internal/compute"
	"github.com/helmrdotdev/helmr/internal/db"
	"github.com/helmrdotdev/helmr/internal/ids"
	"github.com/helmrdotdev/helmr/internal/runqueue"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
)

func TestClaimMarksDequeuedDispatchLeased(t *testing.T) {
	ctx := context.Background()
	orgID := ids.New()
	runID := ids.New()
	hostID := ids.New()
	expiresAt := time.Now().Add(time.Minute).UTC()
	queue := &fakeQueue{
		leases: []runqueue.Lease{{
			ID:        "lease-1",
			MessageID: "message-1",
			Message: runqueue.Message{
				OrgID:        orgID.String(),
				RunID:        runID.String(),
				QueueName:    "queue-a",
				Requirements: compute.RunRuntimeRequirements{Resources: compute.ResourceVector{MilliCPU: 1000, MemoryMiB: 1024, Slots: 1}},
			},
			WorkerInstanceID: hostID.String(),
			AttemptNumber:    1,
			ExpiresAt:        expiresAt,
		}},
	}
	store := &fakeStore{dispatch: db.RunQueueItem{
		OrgID:                      ids.ToPG(orgID),
		RunID:                      ids.ToPG(runID),
		Status:                     db.RunQueueStatusReserved,
		DispatchMessageID:          pgtype.Text{String: "message-1", Valid: true},
		ReservedByWorkerInstanceID: ids.ToPG(hostID),
		ReservationExpiresAt:       pgtype.Timestamptz{Time: expiresAt, Valid: true},
		QueueName:                  "queue-a",
		Priority:                   0,
		DispatchGeneration:         1,
		LastError:                  "",
		EnqueuedAt:                 pgtype.Timestamptz{Time: time.Now().UTC(), Valid: true},
		UpdatedAt:                  pgtype.Timestamptz{Time: time.Now().UTC(), Valid: true},
		FinishedAt:                 pgtype.Timestamptz{},
	}}

	claimer, err := New(store, queue)
	if err != nil {
		t.Fatal(err)
	}
	result, err := claimer.Lease(ctx, LeaseRequest{DequeueRequest: runqueue.DequeueRequest{
		OrgID:            orgID.String(),
		WorkerInstanceID: hostID.String(),
		QueueName:        "queue-a",
		Available:        compute.ResourceVector{MilliCPU: 1000, MemoryMiB: 1024, Slots: 1},
		MaxMessages:      1,
	}})
	if err != nil {
		t.Fatal(err)
	}
	if result.Lease.MessageID != "message-1" || result.Entry.Status != db.RunQueueStatusReserved {
		t.Fatalf("claim result = %+v", result)
	}
	if store.marked.DispatchMessageID.String != "message-1" || store.marked.WorkerInstanceID != ids.ToPG(hostID) {
		t.Fatalf("marked params = %+v", store.marked)
	}
	if len(queue.requeued) != 0 {
		t.Fatalf("requeued leases = %+v", queue.requeued)
	}
}

func TestClaimDeletesStaleDispatchLease(t *testing.T) {
	ctx := context.Background()
	orgID := ids.New()
	runID := ids.New()
	hostID := ids.New()
	queue := &fakeQueue{
		leases: []runqueue.Lease{{
			ID:        "lease-1",
			MessageID: "message-stale",
			Message: runqueue.Message{
				OrgID:        orgID.String(),
				RunID:        runID.String(),
				QueueName:    "queue-a",
				Requirements: compute.RunRuntimeRequirements{Resources: compute.ResourceVector{MilliCPU: 1000, MemoryMiB: 1024, Slots: 1}},
			},
			WorkerInstanceID: hostID.String(),
			AttemptNumber:    1,
			ExpiresAt:        time.Now().Add(time.Minute).UTC(),
		}},
	}
	store := &fakeStore{err: pgx.ErrNoRows}
	claimer, err := New(store, queue)
	if err != nil {
		t.Fatal(err)
	}

	_, err = claimer.Lease(ctx, LeaseRequest{DequeueRequest: runqueue.DequeueRequest{
		OrgID:            orgID.String(),
		WorkerInstanceID: hostID.String(),
		QueueName:        "queue-a",
		Available:        compute.ResourceVector{MilliCPU: 1000, MemoryMiB: 1024, Slots: 1},
		MaxMessages:      1,
	}})
	if !errors.Is(err, ErrNoLease) {
		t.Fatalf("claim error = %v, want ErrNoLease", err)
	}
	if len(queue.requeued) != 1 || queue.requeued[0].reason != runqueue.NackReasonInvalid {
		t.Fatalf("requeued = %+v", queue.requeued)
	}
}

func TestClaimDeletesInvalidDispatchLease(t *testing.T) {
	ctx := context.Background()
	hostID := ids.New()
	queue := &fakeQueue{
		leases: []runqueue.Lease{{
			ID:        "lease-1",
			MessageID: "message-invalid",
			Message: runqueue.Message{
				OrgID:        "not-a-uuid",
				RunID:        ids.New().String(),
				QueueName:    "queue-a",
				Requirements: compute.RunRuntimeRequirements{Resources: compute.ResourceVector{MilliCPU: 1000, MemoryMiB: 1024, Slots: 1}},
			},
			WorkerInstanceID: hostID.String(),
			AttemptNumber:    1,
			ExpiresAt:        time.Now().Add(time.Minute).UTC(),
		}},
	}
	claimer, err := New(&fakeStore{}, queue)
	if err != nil {
		t.Fatal(err)
	}

	_, err = claimer.Lease(ctx, LeaseRequest{DequeueRequest: runqueue.DequeueRequest{
		OrgID:            ids.New().String(),
		WorkerInstanceID: hostID.String(),
		QueueName:        "queue-a",
		Available:        compute.ResourceVector{MilliCPU: 1000, MemoryMiB: 1024, Slots: 1},
		MaxMessages:      1,
	}})
	if !errors.Is(err, ErrNoLease) {
		t.Fatalf("claim error = %v, want ErrNoLease", err)
	}
	if len(queue.requeued) != 1 || queue.requeued[0].reason != runqueue.NackReasonInvalid {
		t.Fatalf("requeued = %+v", queue.requeued)
	}
}

func TestClaimDeadLettersAfterMaxAttempts(t *testing.T) {
	ctx := context.Background()
	orgID := ids.New()
	runID := ids.New()
	hostID := ids.New()
	lease := runqueue.Lease{
		ID:        "lease-1",
		MessageID: "message-dead",
		Message: runqueue.Message{
			OrgID:        orgID.String(),
			RunID:        runID.String(),
			QueueName:    "queue-a",
			Requirements: compute.RunRuntimeRequirements{Resources: compute.ResourceVector{MilliCPU: 1000, MemoryMiB: 1024, Slots: 1}},
		},
		WorkerInstanceID: hostID.String(),
		AttemptNumber:    3,
		ExpiresAt:        time.Now().Add(time.Minute).UTC(),
	}
	queue := &fakeQueue{leases: []runqueue.Lease{lease}}
	store := &fakeStore{attemptsExhausted: true}
	claimer, err := New(store, queue, WithMaxDispatchAttempts(2))
	if err != nil {
		t.Fatal(err)
	}

	_, err = claimer.Lease(ctx, LeaseRequest{DequeueRequest: runqueue.DequeueRequest{
		OrgID:            orgID.String(),
		WorkerInstanceID: hostID.String(),
		QueueName:        "queue-a",
		Available:        compute.ResourceVector{MilliCPU: 1000, MemoryMiB: 1024, Slots: 1},
		MaxMessages:      1,
	}})
	if !errors.Is(err, ErrNoLease) {
		t.Fatalf("claim error = %v, want ErrNoLease", err)
	}
	if store.marked.DispatchMessageID.Valid {
		t.Fatalf("marked leased params = %+v", store.marked)
	}
	if store.deadLettered.DispatchMessageID.String != "message-dead" || store.deadLettered.RunID != ids.ToPG(runID) {
		t.Fatalf("dead letter params = %+v", store.deadLettered)
	}
	if store.deadLettered.EventKind != "run.dead_lettered" || len(store.deadLettered.EventPayload) == 0 {
		t.Fatalf("dead letter event = %q %s", store.deadLettered.EventKind, string(store.deadLettered.EventPayload))
	}
	if len(queue.acked) != 1 || queue.acked[0].ID != lease.ID {
		t.Fatalf("acked leases = %+v", queue.acked)
	}
	if len(queue.requeued) != 0 {
		t.Fatalf("requeued leases = %+v", queue.requeued)
	}
}

func TestClaimDoesNotDeadLetterInflatedRedisAttempts(t *testing.T) {
	ctx := context.Background()
	orgID := ids.New()
	runID := ids.New()
	hostID := ids.New()
	expiresAt := time.Now().Add(time.Minute).UTC()
	queue := &fakeQueue{
		leases: []runqueue.Lease{{
			ID:        "lease-1",
			MessageID: "message-1",
			Message: runqueue.Message{
				OrgID:        orgID.String(),
				RunID:        runID.String(),
				QueueName:    "queue-a",
				Requirements: compute.RunRuntimeRequirements{Resources: compute.ResourceVector{MilliCPU: 1000, MemoryMiB: 1024, Slots: 1}},
			},
			WorkerInstanceID: hostID.String(),
			AttemptNumber:    DefaultMaxDispatchAttempts + 10,
			ExpiresAt:        expiresAt,
		}},
	}
	store := &fakeStore{dispatch: db.RunQueueItem{
		OrgID:                      ids.ToPG(orgID),
		RunID:                      ids.ToPG(runID),
		Status:                     db.RunQueueStatusReserved,
		DispatchMessageID:          pgtype.Text{String: "message-1", Valid: true},
		ReservedByWorkerInstanceID: ids.ToPG(hostID),
		ReservationExpiresAt:       pgtype.Timestamptz{Time: expiresAt, Valid: true},
		QueueName:                  "queue-a",
		Priority:                   0,
		DispatchGeneration:         1,
		LastError:                  "",
		EnqueuedAt:                 pgtype.Timestamptz{Time: time.Now().UTC(), Valid: true},
		UpdatedAt:                  pgtype.Timestamptz{Time: time.Now().UTC(), Valid: true},
		FinishedAt:                 pgtype.Timestamptz{},
	}}
	claimer, err := New(store, queue)
	if err != nil {
		t.Fatal(err)
	}

	result, err := claimer.Lease(ctx, LeaseRequest{DequeueRequest: runqueue.DequeueRequest{
		OrgID:            orgID.String(),
		WorkerInstanceID: hostID.String(),
		QueueName:        "queue-a",
		Available:        compute.ResourceVector{MilliCPU: 1000, MemoryMiB: 1024, Slots: 1},
		MaxMessages:      1,
	}})
	if err != nil {
		t.Fatal(err)
	}
	if result.Entry.Status != db.RunQueueStatusReserved || store.deadLettered.DispatchMessageID.Valid {
		t.Fatalf("result = %+v dead letter = %+v", result, store.deadLettered)
	}
}

type fakeStore struct {
	dispatch          db.RunQueueItem
	marked            db.ReserveRunQueueItemParams
	deadLettered      db.DeadLetterRunQueueItemParams
	err               error
	deadErr           error
	exhaustedErr      error
	attemptsExhausted bool
}

func (f *fakeStore) DeadLetterRunQueueItem(_ context.Context, arg db.DeadLetterRunQueueItemParams) (db.DeadLetterRunQueueItemRow, error) {
	f.deadLettered = arg
	if f.deadErr != nil {
		return db.DeadLetterRunQueueItemRow{}, f.deadErr
	}
	return db.DeadLetterRunQueueItemRow{
		OrgID:             arg.OrgID,
		RunID:             arg.RunID,
		Status:            db.RunQueueStatusDeadLettered,
		DispatchMessageID: arg.DispatchMessageID,
		LastError:         arg.LastError,
	}, nil
}

func (f *fakeStore) ReserveRunQueueItem(_ context.Context, arg db.ReserveRunQueueItemParams) (db.RunQueueItem, error) {
	f.marked = arg
	if f.err != nil {
		return db.RunQueueItem{}, f.err
	}
	return f.dispatch, nil
}

func (f *fakeStore) RunExecutionDispatchAttemptsExhausted(_ context.Context, _ db.RunExecutionDispatchAttemptsExhaustedParams) (bool, error) {
	if f.exhaustedErr != nil {
		return false, f.exhaustedErr
	}
	return f.attemptsExhausted, nil
}

type fakeQueue struct {
	leases   []runqueue.Lease
	acked    []runqueue.Lease
	requeued []requeuedLease
	err      error
}

type requeuedLease struct {
	lease  runqueue.Lease
	reason runqueue.NackReason
}

func (f *fakeQueue) Enqueue(context.Context, runqueue.Message) (runqueue.EnqueueResult, error) {
	panic("not implemented")
}

func (f *fakeQueue) Dequeue(context.Context, runqueue.DequeueRequest) ([]runqueue.Lease, error) {
	if f.err != nil {
		return nil, f.err
	}
	return f.leases, nil
}

func (f *fakeQueue) ReadyMessageExists(context.Context, string) (bool, error) {
	return false, nil
}

func (f *fakeQueue) Ack(_ context.Context, lease runqueue.Lease) error {
	f.acked = append(f.acked, lease)
	return nil
}

func (f *fakeQueue) Nack(_ context.Context, lease runqueue.Lease, reason runqueue.NackReason) error {
	f.requeued = append(f.requeued, requeuedLease{lease: lease, reason: reason})
	return nil
}

func (f *fakeQueue) Renew(context.Context, runqueue.Lease, time.Time) (runqueue.Lease, error) {
	panic("not implemented")
}
