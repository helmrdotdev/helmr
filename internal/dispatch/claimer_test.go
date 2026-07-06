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
)

func TestClaimReturnsDequeuedRedisLease(t *testing.T) {
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

	claimer, err := NewClaimer(queue)
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
	if len(queue.acked) != 0 || len(queue.nacked) != 0 {
		t.Fatalf("acked=%+v nacked=%+v, want none", queue.acked, queue.nacked)
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
	claimer, err := NewClaimer(queue)
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
	if len(queue.nacked) != 1 || queue.nacked[0].reason != NackReasonInvalid {
		t.Fatalf("nacked = %+v", queue.nacked)
	}
}

func TestClaimDoesNotDeadLetterInflatedRedisAttempts(t *testing.T) {
	ctx := context.Background()
	orgID := uuid.Must(uuid.NewV7())
	runID := uuid.Must(uuid.NewV7())
	hostID := uuid.Must(uuid.NewV7())
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
			ExpiresAt:        time.Now().Add(time.Minute).UTC(),
		}},
	}
	claimer, err := NewClaimer(queue)
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
	if result.Entry.Status != db.RunStatusQueued || result.Lease.AttemptNumber <= DefaultMaxDispatchAttempts {
		t.Fatalf("result = %+v", result)
	}
	if len(queue.acked) != 0 || len(queue.nacked) != 0 {
		t.Fatalf("acked=%+v nacked=%+v, want none", queue.acked, queue.nacked)
	}
}

func TestNewClaimerRejectsMissingQueue(t *testing.T) {
	if _, err := NewClaimer(nil); err == nil {
		t.Fatal("NewClaimer(nil) error = nil, want error")
	}
}

type fakeClaimerQueue struct {
	leases []Lease
	acked  []Lease
	nacked []nackedLease
	err    error
}

type nackedLease struct {
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
	f.nacked = append(f.nacked, nackedLease{lease: lease, reason: reason})
	return nil
}

func (f *fakeClaimerQueue) Renew(context.Context, Lease, time.Time) (Lease, error) {
	panic("not implemented")
}
