package schedule

import (
	"context"
	"testing"
	"time"

	"github.com/alicebob/miniredis/v2"
	"github.com/helmrdotdev/helmr/internal/ids"
	goredis "github.com/redis/go-redis/v9"
)

func TestRedisIndexDequeuesDueEntriesAndAcks(t *testing.T) {
	ctx := context.Background()
	server := miniredis.RunT(t)
	client := goredis.NewClient(&goredis.Options{Addr: server.Addr()})
	defer client.Close()

	now := time.Date(2026, 6, 1, 0, 0, 0, 0, time.UTC)
	index, err := NewRedisIndex(client, WithRedisIndexClock(func() time.Time { return now }))
	if err != nil {
		t.Fatal(err)
	}
	instanceID := ids.New()
	entry := IndexEntry{
		InstanceID:  instanceID,
		Generation:  4,
		ScheduledAt: now,
		AvailableAt: now,
	}
	if err := index.Enqueue(ctx, entry); err != nil {
		t.Fatal(err)
	}

	leases, err := index.Dequeue(ctx, DequeueRequest{
		WorkerID: ids.New(),
		Limit:    1,
		Now:      now,
		Lease:    time.Minute,
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(leases) != 1 {
		t.Fatalf("leases = %d, want 1", len(leases))
	}
	if leases[0].Entry.InstanceID != instanceID || leases[0].Entry.Generation != 4 || !leases[0].Entry.ScheduledAt.Equal(now) {
		t.Fatalf("lease entry = %+v", leases[0].Entry)
	}
	if err := index.Ack(ctx, leases[0]); err != nil {
		t.Fatal(err)
	}

	leases, err = index.Dequeue(ctx, DequeueRequest{
		WorkerID: ids.New(),
		Limit:    1,
		Now:      now.Add(time.Hour),
		Lease:    time.Minute,
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(leases) != 0 {
		t.Fatalf("leases after ack = %d, want 0", len(leases))
	}
}

func TestRedisIndexNackDelaysRetry(t *testing.T) {
	ctx := context.Background()
	server := miniredis.RunT(t)
	client := goredis.NewClient(&goredis.Options{Addr: server.Addr()})
	defer client.Close()

	now := time.Date(2026, 6, 1, 0, 0, 0, 0, time.UTC)
	index, err := NewRedisIndex(client, WithRedisIndexClock(func() time.Time { return now }))
	if err != nil {
		t.Fatal(err)
	}
	workerID := ids.New()
	entry := IndexEntry{
		InstanceID:  ids.New(),
		Generation:  1,
		ScheduledAt: now,
		AvailableAt: now,
	}
	if err := index.Enqueue(ctx, entry); err != nil {
		t.Fatal(err)
	}
	leases, err := index.Dequeue(ctx, DequeueRequest{WorkerID: workerID, Limit: 1, Now: now, Lease: time.Minute})
	if err != nil {
		t.Fatal(err)
	}
	if len(leases) != 1 {
		t.Fatalf("leases = %d, want 1", len(leases))
	}
	retryAt := now.Add(5 * time.Minute)
	if err := index.Nack(ctx, leases[0], retryAt); err != nil {
		t.Fatal(err)
	}

	leases, err = index.Dequeue(ctx, DequeueRequest{WorkerID: workerID, Limit: 1, Now: now.Add(time.Minute), Lease: time.Minute})
	if err != nil {
		t.Fatal(err)
	}
	if len(leases) != 0 {
		t.Fatalf("early retry leases = %d, want 0", len(leases))
	}
	leases, err = index.Dequeue(ctx, DequeueRequest{WorkerID: workerID, Limit: 1, Now: retryAt, Lease: time.Minute})
	if err != nil {
		t.Fatal(err)
	}
	if len(leases) != 1 || leases[0].Attempt != 2 {
		t.Fatalf("retry leases = %+v", leases)
	}
}

func TestRedisIndexNackAfterExpiredLeaseStillDelaysRetry(t *testing.T) {
	ctx := context.Background()
	server := miniredis.RunT(t)
	client := goredis.NewClient(&goredis.Options{Addr: server.Addr()})
	defer client.Close()

	now := time.Date(2026, 6, 1, 0, 0, 0, 0, time.UTC)
	clock := now
	index, err := NewRedisIndex(client, WithRedisIndexClock(func() time.Time { return clock }))
	if err != nil {
		t.Fatal(err)
	}
	workerID := ids.New()
	entry := IndexEntry{InstanceID: ids.New(), Generation: 1, ScheduledAt: now, AvailableAt: now}
	if err := index.Enqueue(ctx, entry); err != nil {
		t.Fatal(err)
	}
	leases, err := index.Dequeue(ctx, DequeueRequest{WorkerID: workerID, Limit: 1, Now: now, Lease: time.Minute})
	if err != nil {
		t.Fatal(err)
	}
	if len(leases) != 1 {
		t.Fatalf("leases = %d, want 1", len(leases))
	}
	retryAt := now.Add(5 * time.Minute)
	clock = now.Add(2 * time.Minute)
	if err := index.Nack(ctx, leases[0], retryAt); err != nil {
		t.Fatal(err)
	}
	leases, err = index.Dequeue(ctx, DequeueRequest{WorkerID: workerID, Limit: 1, Now: clock, Lease: time.Minute})
	if err != nil {
		t.Fatal(err)
	}
	if len(leases) != 0 {
		t.Fatalf("early retry leases = %d, want 0", len(leases))
	}
	leases, err = index.Dequeue(ctx, DequeueRequest{WorkerID: workerID, Limit: 1, Now: retryAt, Lease: time.Minute})
	if err != nil {
		t.Fatal(err)
	}
	if len(leases) != 1 || leases[0].Attempt != 2 {
		t.Fatalf("retry leases = %+v", leases)
	}
}

func TestRedisIndexReclaimAppliesBackoff(t *testing.T) {
	ctx := context.Background()
	server := miniredis.RunT(t)
	client := goredis.NewClient(&goredis.Options{Addr: server.Addr()})
	defer client.Close()

	index, err := NewRedisIndex(client)
	if err != nil {
		t.Fatal(err)
	}
	now := time.Date(2026, 6, 1, 0, 0, 0, 0, time.UTC)
	workerID := ids.New()
	entry := IndexEntry{InstanceID: ids.New(), Generation: 1, ScheduledAt: now, AvailableAt: now}
	if err := index.Enqueue(ctx, entry); err != nil {
		t.Fatal(err)
	}
	leases, err := index.Dequeue(ctx, DequeueRequest{WorkerID: workerID, Limit: 1, Now: now, Lease: time.Minute})
	if err != nil {
		t.Fatal(err)
	}
	if len(leases) != 1 {
		t.Fatalf("leases = %d, want 1", len(leases))
	}

	reclaimAt := now.Add(2 * time.Minute)
	leases, err = index.Dequeue(ctx, DequeueRequest{WorkerID: workerID, Limit: 1, Now: reclaimAt, Lease: time.Minute})
	if err != nil {
		t.Fatal(err)
	}
	if len(leases) != 0 {
		t.Fatalf("reclaim leases = %d, want 0", len(leases))
	}
	leases, err = index.Dequeue(ctx, DequeueRequest{WorkerID: workerID, Limit: 1, Now: reclaimAt.Add(time.Minute), Lease: time.Minute})
	if err != nil {
		t.Fatal(err)
	}
	if len(leases) != 1 || leases[0].Attempt != 2 {
		t.Fatalf("backoff leases = %+v", leases)
	}
}

func TestRedisIndexEnqueueDoesNotResetInflightAttempt(t *testing.T) {
	ctx := context.Background()
	server := miniredis.RunT(t)
	client := goredis.NewClient(&goredis.Options{Addr: server.Addr()})
	defer client.Close()

	now := time.Date(2026, 6, 1, 0, 0, 0, 0, time.UTC)
	index, err := NewRedisIndex(client)
	if err != nil {
		t.Fatal(err)
	}
	workerID := ids.New()
	entry := IndexEntry{InstanceID: ids.New(), Generation: 1, ScheduledAt: now, AvailableAt: now}
	if err := index.Enqueue(ctx, entry); err != nil {
		t.Fatal(err)
	}
	leases, err := index.Dequeue(ctx, DequeueRequest{WorkerID: workerID, Limit: 1, Now: now, Lease: time.Minute})
	if err != nil {
		t.Fatal(err)
	}
	if len(leases) != 1 {
		t.Fatalf("leases = %d, want 1", len(leases))
	}
	if err := index.Enqueue(ctx, entry); err != nil {
		t.Fatal(err)
	}
	reclaimAt := now.Add(2 * time.Minute)
	leases, err = index.Dequeue(ctx, DequeueRequest{WorkerID: workerID, Limit: 1, Now: reclaimAt, Lease: time.Minute})
	if err != nil {
		t.Fatal(err)
	}
	if len(leases) != 0 {
		t.Fatalf("reclaim leases = %d, want 0", len(leases))
	}
	leases, err = index.Dequeue(ctx, DequeueRequest{WorkerID: workerID, Limit: 1, Now: reclaimAt.Add(time.Minute), Lease: time.Minute})
	if err != nil {
		t.Fatal(err)
	}
	if len(leases) != 1 || leases[0].Attempt != 2 {
		t.Fatalf("backoff leases = %+v", leases)
	}
}

func TestRedisIndexAckAfterExpiredLeaseCleansMessage(t *testing.T) {
	ctx := context.Background()
	server := miniredis.RunT(t)
	client := goredis.NewClient(&goredis.Options{Addr: server.Addr()})
	defer client.Close()

	now := time.Date(2026, 6, 1, 0, 0, 0, 0, time.UTC)
	clock := now
	index, err := NewRedisIndex(client, WithRedisIndexClock(func() time.Time { return clock }))
	if err != nil {
		t.Fatal(err)
	}
	workerID := ids.New()
	entry := IndexEntry{InstanceID: ids.New(), Generation: 1, ScheduledAt: now, AvailableAt: now}
	if err := index.Enqueue(ctx, entry); err != nil {
		t.Fatal(err)
	}
	leases, err := index.Dequeue(ctx, DequeueRequest{WorkerID: workerID, Limit: 1, Now: now, Lease: time.Minute})
	if err != nil {
		t.Fatal(err)
	}
	if len(leases) != 1 {
		t.Fatalf("leases = %d, want 1", len(leases))
	}
	clock = now.Add(2 * time.Minute)
	if err := index.Ack(ctx, leases[0]); err != nil {
		t.Fatal(err)
	}
	leases, err = index.Dequeue(ctx, DequeueRequest{WorkerID: workerID, Limit: 1, Now: clock, Lease: time.Minute})
	if err != nil {
		t.Fatal(err)
	}
	if len(leases) != 0 {
		t.Fatalf("leases after expired ack = %d, want 0", len(leases))
	}
}
