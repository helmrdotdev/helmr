package schedule

import (
	"context"
	"testing"
	"time"

	"github.com/alicebob/miniredis/v2"
	"github.com/google/uuid"
	"github.com/redis/go-redis/v9"
)

func TestRedisIndexLogicalDeliveryAck(t *testing.T) {
	ctx := context.Background()
	server := miniredis.RunT(t)
	client := redis.NewClient(&redis.Options{Addr: server.Addr()})
	defer client.Close()
	now := time.Date(2026, 7, 12, 0, 0, 0, 0, time.UTC)
	index, err := NewRedisIndex(client, WithRedisIndexClock(func() time.Time { return now }))
	if err != nil {
		t.Fatal(err)
	}
	entry := IndexEntry{InstanceID: uuid.Must(uuid.NewV7()), Generation: 3, ScheduledAt: now, AvailableAt: now}
	if err := index.Enqueue(ctx, entry); err != nil {
		t.Fatal(err)
	}
	leases, err := index.Dequeue(ctx, DequeueRequest{WorkerID: uuid.Must(uuid.NewV7()), Limit: 1, Now: now, Lease: time.Minute})
	if err != nil {
		t.Fatal(err)
	}
	if len(leases) != 1 || leases[0].Entry != entry {
		t.Fatalf("leases = %+v", leases)
	}
	if err := index.Ack(ctx, leases[0]); err != nil {
		t.Fatal(err)
	}
	leases, err = index.Dequeue(ctx, DequeueRequest{WorkerID: uuid.Must(uuid.NewV7()), Limit: 1, Now: now.Add(2 * time.Minute), Lease: time.Minute})
	if err != nil {
		t.Fatal(err)
	}
	if len(leases) != 0 {
		t.Fatalf("acked lease replayed: %+v", leases)
	}
}

func TestRedisIndexNackAndExpiredLeaseReplay(t *testing.T) {
	ctx := context.Background()
	server := miniredis.RunT(t)
	client := redis.NewClient(&redis.Options{Addr: server.Addr()})
	defer client.Close()
	now := time.Date(2026, 7, 12, 0, 0, 0, 0, time.UTC)
	index, err := NewRedisIndex(client, WithRedisIndexClock(func() time.Time { return now }))
	if err != nil {
		t.Fatal(err)
	}
	entry := IndexEntry{InstanceID: uuid.Must(uuid.NewV7()), Generation: 1, ScheduledAt: now, AvailableAt: now}
	if err := index.Enqueue(ctx, entry); err != nil {
		t.Fatal(err)
	}
	worker := uuid.Must(uuid.NewV7())
	leases, err := index.Dequeue(ctx, DequeueRequest{WorkerID: worker, Limit: 1, Now: now, Lease: time.Minute})
	if err != nil || len(leases) != 1 {
		t.Fatalf("leases=%+v err=%v", leases, err)
	}
	retryAt := now.Add(2 * time.Minute)
	if err := index.Nack(ctx, leases[0], retryAt); err != nil {
		t.Fatal(err)
	}
	leases, err = index.Dequeue(ctx, DequeueRequest{WorkerID: worker, Limit: 1, Now: retryAt, Lease: time.Minute})
	if err != nil || len(leases) != 1 || leases[0].Attempt != 2 {
		t.Fatalf("retry leases=%+v err=%v", leases, err)
	}
}
