package dispatch

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/helmrdotdev/helmr/internal/compute"
	"github.com/helmrdotdev/helmr/internal/db"
	"github.com/helmrdotdev/helmr/internal/pgvalue"
	"github.com/jackc/pgx/v5"
)

func TestClaimMarksDequeuedDispatchLeased(t *testing.T) {
	ctx := context.Background()
	orgID := uuid.Must(uuid.NewV7())
	runID := uuid.Must(uuid.NewV7())
	hostID := uuid.Must(uuid.NewV7())
	expiresAt := time.Now().Add(time.Minute).UTC()
	queue := &fakeClaimerQueue{
		leases: []Lease{{
			ID:        "lease-1",
			MessageID: "message-1",
			Message: Message{
				OrgID:              orgID.String(),
				WorkerGroupID:      "us-east-1-worker-group-1",
				QueueClass:         "default",
				RunID:              runID.String(),
				QueueName:          "queue-a",
				DispatchGeneration: 1,
				Requirements:       compute.RunRuntimeRequirements{Resources: compute.ResourceVector{MilliCPU: 1000, MemoryMiB: 1024, Slots: 1}},
			},
			WorkerInstanceID: hostID.String(),
			AttemptNumber:    1,
			ExpiresAt:        expiresAt,
		}},
	}
	store := &fakeClaimerStore{dispatch: db.Run{
		OrgID:              pgvalue.UUID(orgID),
		ID:                 pgvalue.UUID(runID),
		Status:             db.RunStatusQueued,
		QueueName:          "queue-a",
		Priority:           0,
		DispatchGeneration: 1,
	}}

	claimer, err := NewClaimer(store, queue)
	if err != nil {
		t.Fatal(err)
	}
	result, err := claimer.Claim(ctx, ClaimRequest{DequeueRequest: DequeueRequest{
		OrgID:            orgID.String(),
		WorkerGroupID:    "us-east-1-worker-group-1",
		QueueClass:       "default",
		WorkerInstanceID: hostID.String(),
		QueueName:        "queue-a",
		Available:        compute.ResourceVector{MilliCPU: 1000, MemoryMiB: 1024, Slots: 1},
		MaxMessages:      1,
	}})
	if err != nil {
		t.Fatal(err)
	}
	if result.Lease.MessageID != "message-1" || result.Entry.Status != db.RunStatusQueued {
		t.Fatalf("claim result = %+v", result)
	}
	if result.Entry.DispatchGeneration != 1 || result.Entry.ID != pgvalue.UUID(runID) {
		t.Fatalf("claimed entry = %+v", result.Entry)
	}
	if len(queue.requeued) != 0 {
		t.Fatalf("requeued leases = %+v", queue.requeued)
	}
}

func TestClaimDeletesInvalidDispatchLease(t *testing.T) {
	ctx := context.Background()
	hostID := uuid.Must(uuid.NewV7())
	queue := &fakeClaimerQueue{
		leases: []Lease{{
			ID:        "lease-1",
			MessageID: "message-invalid",
			Message: Message{
				OrgID:              "not-a-uuid",
				RunID:              uuid.Must(uuid.NewV7()).String(),
				QueueName:          "queue-a",
				DispatchGeneration: 1,
				Requirements:       compute.RunRuntimeRequirements{Resources: compute.ResourceVector{MilliCPU: 1000, MemoryMiB: 1024, Slots: 1}},
			},
			WorkerInstanceID: hostID.String(),
			AttemptNumber:    1,
			ExpiresAt:        time.Now().Add(time.Minute).UTC(),
		}},
	}
	claimer, err := NewClaimer(&fakeClaimerStore{}, queue)
	if err != nil {
		t.Fatal(err)
	}

	_, err = claimer.Claim(ctx, ClaimRequest{DequeueRequest: DequeueRequest{
		OrgID:            uuid.Must(uuid.NewV7()).String(),
		WorkerInstanceID: hostID.String(),
		QueueName:        "queue-a",
		Available:        compute.ResourceVector{MilliCPU: 1000, MemoryMiB: 1024, Slots: 1},
		MaxMessages:      1,
	}})
	if !errors.Is(err, ErrNoClaim) {
		t.Fatalf("claim error = %v, want ErrNoClaim", err)
	}
	if len(queue.requeued) != 1 || queue.requeued[0].reason != NackReasonInvalid {
		t.Fatalf("requeued = %+v", queue.requeued)
	}
}

func TestClaimDeadLettersAfterMaxAttempts(t *testing.T) {
	ctx := context.Background()
	orgID := uuid.Must(uuid.NewV7())
	runID := uuid.Must(uuid.NewV7())
	hostID := uuid.Must(uuid.NewV7())
	lease := Lease{
		ID:        "lease-1",
		MessageID: "message-dead",
		Message: Message{
			OrgID:              orgID.String(),
			WorkerGroupID:      "us-east-1-worker-group-1",
			QueueClass:         "default",
			RunID:              runID.String(),
			QueueName:          "queue-a",
			DispatchGeneration: 1,
			Requirements:       compute.RunRuntimeRequirements{Resources: compute.ResourceVector{MilliCPU: 1000, MemoryMiB: 1024, Slots: 1}},
		},
		WorkerInstanceID: hostID.String(),
		AttemptNumber:    3,
		ExpiresAt:        time.Now().Add(time.Minute).UTC(),
	}
	queue := &fakeClaimerQueue{leases: []Lease{lease}}
	store := &fakeClaimerStore{attemptsExhausted: true}
	claimer, err := NewClaimer(store, queue, WithMaxDispatchAttempts(2))
	if err != nil {
		t.Fatal(err)
	}

	_, err = claimer.Claim(ctx, ClaimRequest{DequeueRequest: DequeueRequest{
		OrgID:            orgID.String(),
		WorkerGroupID:    "us-east-1-worker-group-1",
		QueueClass:       "default",
		WorkerInstanceID: hostID.String(),
		QueueName:        "queue-a",
		Available:        compute.ResourceVector{MilliCPU: 1000, MemoryMiB: 1024, Slots: 1},
		MaxMessages:      1,
	}})
	if !errors.Is(err, ErrNoClaim) {
		t.Fatalf("claim error = %v, want ErrNoClaim", err)
	}
	if store.deadLettered.RunID != pgvalue.UUID(runID) {
		t.Fatalf("dead letter params = %+v", store.deadLettered)
	}
	if len(queue.acked) != 1 || queue.acked[0].ID != lease.ID {
		t.Fatalf("acked leases = %+v", queue.acked)
	}
	if len(queue.requeued) != 0 {
		t.Fatalf("requeued leases = %+v", queue.requeued)
	}
}

func TestClaimDoesNotAckWhenDeadLetterFails(t *testing.T) {
	ctx := context.Background()
	orgID := uuid.Must(uuid.NewV7())
	runID := uuid.Must(uuid.NewV7())
	hostID := uuid.Must(uuid.NewV7())
	lease := Lease{
		ID:        "lease-1",
		MessageID: "message-dead",
		Message: Message{
			OrgID:              orgID.String(),
			WorkerGroupID:      "us-east-1-worker-group-1",
			QueueClass:         "default",
			RunID:              runID.String(),
			QueueName:          "queue-a",
			DispatchGeneration: 1,
			Requirements:       compute.RunRuntimeRequirements{Resources: compute.ResourceVector{MilliCPU: 1000, MemoryMiB: 1024, Slots: 1}},
		},
		WorkerInstanceID: hostID.String(),
		AttemptNumber:    3,
		ExpiresAt:        time.Now().Add(time.Minute).UTC(),
	}
	queue := &fakeClaimerQueue{leases: []Lease{lease}}
	deadErr := errors.New("route authority denied")
	store := &fakeClaimerStore{attemptsExhausted: true, deadErr: deadErr}
	claimer, err := NewClaimer(store, queue, WithMaxDispatchAttempts(2))
	if err != nil {
		t.Fatal(err)
	}

	_, err = claimer.Claim(ctx, ClaimRequest{DequeueRequest: DequeueRequest{
		OrgID:            orgID.String(),
		WorkerGroupID:    "us-east-1-worker-group-1",
		QueueClass:       "default",
		WorkerInstanceID: hostID.String(),
		QueueName:        "queue-a",
		Available:        compute.ResourceVector{MilliCPU: 1000, MemoryMiB: 1024, Slots: 1},
		MaxMessages:      1,
	}})
	if !errors.Is(err, deadErr) {
		t.Fatalf("claim error = %v, want dead-letter error", err)
	}
	if len(queue.acked) != 0 {
		t.Fatalf("acked leases = %+v", queue.acked)
	}
	if len(queue.requeued) != 0 {
		t.Fatalf("requeued leases = %+v", queue.requeued)
	}
}

func TestClaimDrainsNonMatchingDeadLetterLease(t *testing.T) {
	ctx := context.Background()
	orgID := uuid.Must(uuid.NewV7())
	runID := uuid.Must(uuid.NewV7())
	hostID := uuid.Must(uuid.NewV7())
	lease := Lease{
		ID:        "lease-1",
		MessageID: "message-dead",
		Message: Message{
			OrgID:              orgID.String(),
			WorkerGroupID:      "us-east-1-worker-group-1",
			QueueClass:         "default",
			RunID:              runID.String(),
			QueueName:          "queue-a",
			DispatchGeneration: 1,
			Requirements:       compute.RunRuntimeRequirements{Resources: compute.ResourceVector{MilliCPU: 1000, MemoryMiB: 1024, Slots: 1}},
		},
		WorkerInstanceID: hostID.String(),
		AttemptNumber:    3,
		ExpiresAt:        time.Now().Add(time.Minute).UTC(),
	}
	queue := &fakeClaimerQueue{leases: []Lease{lease}}
	store := &fakeClaimerStore{attemptsExhausted: true, deadErr: pgx.ErrNoRows}
	claimer, err := NewClaimer(store, queue, WithMaxDispatchAttempts(2))
	if err != nil {
		t.Fatal(err)
	}

	_, err = claimer.Claim(ctx, ClaimRequest{DequeueRequest: DequeueRequest{
		OrgID:            orgID.String(),
		WorkerGroupID:    "us-east-1-worker-group-1",
		QueueClass:       "default",
		WorkerInstanceID: hostID.String(),
		QueueName:        "queue-a",
		Available:        compute.ResourceVector{MilliCPU: 1000, MemoryMiB: 1024, Slots: 1},
		MaxMessages:      1,
	}})
	if !errors.Is(err, ErrNoClaim) {
		t.Fatalf("claim error = %v, want ErrNoClaim", err)
	}
	if len(queue.acked) != 0 {
		t.Fatalf("acked leases = %+v", queue.acked)
	}
	if len(queue.requeued) != 1 || queue.requeued[0].reason != NackReasonInvalid {
		t.Fatalf("requeued leases = %+v", queue.requeued)
	}
}

func TestClaimDrainsInvalidDeadLetterLeaseWithoutAck(t *testing.T) {
	ctx := context.Background()
	orgID := uuid.Must(uuid.NewV7())
	runID := uuid.Must(uuid.NewV7())
	hostID := uuid.Must(uuid.NewV7())
	lease := Lease{
		ID:        "lease-1",
		MessageID: "message-dead",
		Message: Message{
			OrgID:              orgID.String(),
			RunID:              runID.String(),
			QueueName:          "queue-a",
			DispatchGeneration: 1,
			Requirements:       compute.RunRuntimeRequirements{Resources: compute.ResourceVector{MilliCPU: 1000, MemoryMiB: 1024, Slots: 1}},
		},
		WorkerInstanceID: hostID.String(),
		AttemptNumber:    3,
		ExpiresAt:        time.Now().Add(time.Minute).UTC(),
	}
	queue := &fakeClaimerQueue{leases: []Lease{lease}}
	store := &fakeClaimerStore{attemptsExhausted: true}
	claimer, err := NewClaimer(store, queue, WithMaxDispatchAttempts(2))
	if err != nil {
		t.Fatal(err)
	}

	_, err = claimer.Claim(ctx, ClaimRequest{DequeueRequest: DequeueRequest{
		OrgID:            orgID.String(),
		WorkerGroupID:    "us-east-1-worker-group-1",
		QueueClass:       "default",
		WorkerInstanceID: hostID.String(),
		QueueName:        "queue-a",
		Available:        compute.ResourceVector{MilliCPU: 1000, MemoryMiB: 1024, Slots: 1},
		MaxMessages:      1,
	}})
	if !errors.Is(err, ErrNoClaim) {
		t.Fatalf("claim error = %v, want ErrNoClaim", err)
	}
	if store.deadLettered.RunID.Valid {
		t.Fatalf("dead letter params = %+v", store.deadLettered)
	}
	if len(queue.acked) != 0 {
		t.Fatalf("acked leases = %+v", queue.acked)
	}
	if len(queue.requeued) != 1 || queue.requeued[0].reason != NackReasonInvalid {
		t.Fatalf("requeued leases = %+v", queue.requeued)
	}
}

func TestClaimDoesNotDeadLetterInflatedRedisAttempts(t *testing.T) {
	ctx := context.Background()
	orgID := uuid.Must(uuid.NewV7())
	runID := uuid.Must(uuid.NewV7())
	hostID := uuid.Must(uuid.NewV7())
	expiresAt := time.Now().Add(time.Minute).UTC()
	queue := &fakeClaimerQueue{
		leases: []Lease{{
			ID:        "lease-1",
			MessageID: "message-1",
			Message: Message{
				OrgID:              orgID.String(),
				WorkerGroupID:      "us-east-1-worker-group-1",
				QueueClass:         "default",
				RunID:              runID.String(),
				QueueName:          "queue-a",
				DispatchGeneration: 1,
				Requirements:       compute.RunRuntimeRequirements{Resources: compute.ResourceVector{MilliCPU: 1000, MemoryMiB: 1024, Slots: 1}},
			},
			WorkerInstanceID: hostID.String(),
			AttemptNumber:    DefaultMaxDispatchAttempts + 10,
			ExpiresAt:        expiresAt,
		}},
	}
	store := &fakeClaimerStore{dispatch: db.Run{
		OrgID:              pgvalue.UUID(orgID),
		ID:                 pgvalue.UUID(runID),
		Status:             db.RunStatusQueued,
		QueueName:          "queue-a",
		Priority:           0,
		DispatchGeneration: 1,
	}}
	claimer, err := NewClaimer(store, queue)
	if err != nil {
		t.Fatal(err)
	}

	result, err := claimer.Claim(ctx, ClaimRequest{DequeueRequest: DequeueRequest{
		OrgID:            orgID.String(),
		WorkerGroupID:    "us-east-1-worker-group-1",
		QueueClass:       "default",
		WorkerInstanceID: hostID.String(),
		QueueName:        "queue-a",
		Available:        compute.ResourceVector{MilliCPU: 1000, MemoryMiB: 1024, Slots: 1},
		MaxMessages:      1,
	}})
	if err != nil {
		t.Fatal(err)
	}
	if result.Entry.Status != db.RunStatusQueued || store.deadLettered.RunID.Valid {
		t.Fatalf("result = %+v dead letter = %+v", result, store.deadLettered)
	}
}

type fakeClaimerStore struct {
	dispatch          db.Run
	deadLettered      db.DeadLetterRunDispatchParams
	deadErr           error
	exhaustedErr      error
	attemptsExhausted bool
}

func (f *fakeClaimerStore) DeadLetterRunDispatch(_ context.Context, arg db.DeadLetterRunDispatchParams) (db.DeadLetterRunDispatchRow, error) {
	f.deadLettered = arg
	if f.deadErr != nil {
		return db.DeadLetterRunDispatchRow{}, f.deadErr
	}
	return db.DeadLetterRunDispatchRow{
		OrgID: arg.OrgID,
		RunID: arg.RunID,
	}, nil
}

func (f *fakeClaimerStore) RunLeaseDispatchAttemptsExhausted(context.Context, db.RunLeaseDispatchAttemptsExhaustedParams) (bool, error) {
	if f.exhaustedErr != nil {
		return false, f.exhaustedErr
	}
	return f.attemptsExhausted, nil
}

type fakeClaimerQueue struct {
	leases   []Lease
	acked    []Lease
	requeued []requeuedLease
	err      error
}

type requeuedLease struct {
	lease  Lease
	reason NackReason
}

func (f *fakeClaimerQueue) Enqueue(context.Context, Message) (EnqueueResult, error) {
	panic("not implemented")
}

func (f *fakeClaimerQueue) Dequeue(context.Context, DequeueRequest) ([]Lease, error) {
	if f.err != nil {
		return nil, f.err
	}
	for i := range f.leases {
		if strings.TrimSpace(f.leases[i].Message.WorkerGroupID) == "" {
			f.leases[i].Message.WorkerGroupID = "worker-group-1"
		}
	}
	return f.leases, nil
}

func (f *fakeClaimerQueue) Ack(_ context.Context, lease Lease) error {
	f.acked = append(f.acked, lease)
	return nil
}

func (f *fakeClaimerQueue) Nack(_ context.Context, lease Lease, reason NackReason) error {
	f.requeued = append(f.requeued, requeuedLease{lease: lease, reason: reason})
	return nil
}

func (f *fakeClaimerQueue) Renew(context.Context, Lease, time.Time) (Lease, error) {
	panic("not implemented")
}
