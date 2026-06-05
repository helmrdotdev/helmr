package redis

import (
	"context"
	"encoding/json"
	"errors"
	"testing"
	"time"

	"github.com/alicebob/miniredis/v2"
	"github.com/helmrdotdev/helmr/internal/compute"
	dispatch "github.com/helmrdotdev/helmr/internal/dispatch"
	goredis "github.com/redis/go-redis/v9"
)

func TestQueueEnqueueDequeueAck(t *testing.T) {
	ctx := context.Background()
	queue, cleanup := newTestQueue(t)
	defer cleanup()

	if _, err := queue.Enqueue(ctx, testMessage("run-1", 10, compute.ResourceVector{MilliCPU: 1000, MemoryMiB: 1024, DiskMiB: 2048, Slots: 1})); err != nil {
		t.Fatal(err)
	}
	leases, err := queue.Dequeue(ctx, dispatch.DequeueRequest{
		OrgID:            "org-1",
		WorkerInstanceID: "host-1",
		QueueName:        "queue-a",
		Available:        compute.ResourceVector{MilliCPU: 2000, MemoryMiB: 4096, DiskMiB: 4096, Slots: 2},
		Runtime:          testRuntime(),
		MaxMessages:      1,
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(leases) != 1 || leases[0].Message.RunID != "run-1" || leases[0].MessageID == "" || leases[0].AttemptNumber != 1 {
		t.Fatalf("leases = %+v", leases)
	}
	if err := queue.Ack(ctx, leases[0]); err != nil {
		t.Fatal(err)
	}
	leases, err = queue.Dequeue(ctx, dispatch.DequeueRequest{
		OrgID:            "org-1",
		WorkerInstanceID: "host-1",
		QueueName:        "queue-a",
		Available:        compute.ResourceVector{MilliCPU: 2000, MemoryMiB: 4096, DiskMiB: 4096, Slots: 2},
		Runtime:          testRuntime(),
		MaxMessages:      1,
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(leases) != 0 {
		t.Fatalf("leases after ack = %+v", leases)
	}
}

func TestQueueReadyMessageExistsTracksReadyCurrentGeneration(t *testing.T) {
	ctx := context.Background()
	queue, cleanup := newTestQueue(t)
	defer cleanup()

	first, err := queue.Enqueue(ctx, testMessage("run-1", 10, compute.ResourceVector{MilliCPU: 1000, MemoryMiB: 1024, DiskMiB: 2048, Slots: 1}))
	if err != nil {
		t.Fatal(err)
	}
	exists, err := queue.ReadyMessageExists(ctx, first.MessageID)
	if err != nil {
		t.Fatal(err)
	}
	if !exists {
		t.Fatal("first message exists = false")
	}
	mustDequeueOne(t, ctx, queue, "host-1")
	exists, err = queue.ReadyMessageExists(ctx, first.MessageID)
	if err != nil {
		t.Fatal(err)
	}
	if !exists {
		t.Fatal("leased message exists = false")
	}
	second, err := queue.Enqueue(ctx, testMessage("run-1", 10, compute.ResourceVector{MilliCPU: 1000, MemoryMiB: 1024, DiskMiB: 2048, Slots: 1}))
	if err != nil {
		t.Fatal(err)
	}
	exists, err = queue.ReadyMessageExists(ctx, first.MessageID)
	if err != nil {
		t.Fatal(err)
	}
	if exists {
		t.Fatal("stale message exists = true")
	}
	exists, err = queue.ReadyMessageExists(ctx, second.MessageID)
	if err != nil {
		t.Fatal(err)
	}
	if !exists {
		t.Fatal("second message exists = false")
	}
}

func TestQueueReadyMessageExistsInvalidatesMessageWithoutRuntimeMetadata(t *testing.T) {
	ctx := context.Background()
	queue, cleanup := newTestQueue(t)
	defer cleanup()

	result, err := queue.Enqueue(ctx, testMessage("run-1", 10, compute.ResourceVector{MilliCPU: 1000, MemoryMiB: 1024, DiskMiB: 2048, Slots: 1}))
	if err != nil {
		t.Fatal(err)
	}
	messageKey := queue.prefix + ":message:" + result.MessageID
	if err := queue.client.HDel(ctx, messageKey, "runtime_id", "initramfs_digest").Err(); err != nil {
		t.Fatal(err)
	}

	exists, err := queue.ReadyMessageExists(ctx, result.MessageID)
	if err != nil {
		t.Fatal(err)
	}
	if exists {
		t.Fatal("message without runtime metadata exists = true")
	}
	if count, err := queue.client.Exists(ctx, messageKey).Result(); err != nil {
		t.Fatal(err)
	} else if count != 0 {
		t.Fatal("message without runtime metadata was not deleted")
	}
	keys := queue.keys("org-1", "queue-a")
	if score, err := queue.client.ZScore(ctx, keys.ready, result.MessageID).Result(); err == nil {
		t.Fatalf("message without runtime metadata remained ready with score %f", score)
	} else if !errors.Is(err, goredis.Nil) {
		t.Fatal(err)
	}
}

func TestQueueDequeueInvalidatesMessageWithoutRuntimeMetadata(t *testing.T) {
	ctx := context.Background()
	queue, cleanup := newTestQueue(t)
	defer cleanup()

	invalid, err := queue.Enqueue(ctx, testMessage("invalid", 10, compute.ResourceVector{MilliCPU: 1000, MemoryMiB: 1024, DiskMiB: 2048, Slots: 1}))
	if err != nil {
		t.Fatal(err)
	}
	valid, err := queue.Enqueue(ctx, testMessage("valid", 0, compute.ResourceVector{MilliCPU: 1000, MemoryMiB: 1024, DiskMiB: 2048, Slots: 1}))
	if err != nil {
		t.Fatal(err)
	}
	invalidMessageKey := queue.prefix + ":message:" + invalid.MessageID
	if err := queue.client.HDel(ctx, invalidMessageKey, "runtime_id", "initramfs_digest").Err(); err != nil {
		t.Fatal(err)
	}

	leases, err := queue.Dequeue(ctx, dispatch.DequeueRequest{
		OrgID:            "org-1",
		WorkerInstanceID: "host-1",
		QueueName:        "queue-a",
		Available:        compute.ResourceVector{MilliCPU: 2000, MemoryMiB: 4096, DiskMiB: 4096, Slots: 2},
		Runtime:          testRuntime(),
		MaxMessages:      1,
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(leases) != 1 || leases[0].MessageID != valid.MessageID || leases[0].Message.RunID != "valid" {
		t.Fatalf("leases = %+v, want valid message %s", leases, valid.MessageID)
	}
	if count, err := queue.client.Exists(ctx, invalidMessageKey).Result(); err != nil {
		t.Fatal(err)
	} else if count != 0 {
		t.Fatal("message without runtime metadata was not deleted by dequeue")
	}
}

func TestQueueReadyMessageExistsReclaimsExpiredActiveLease(t *testing.T) {
	ctx := context.Background()
	now := time.Date(2026, 5, 19, 0, 0, 0, 0, time.UTC)
	queue, cleanup := newTestQueue(t, WithClock(func() time.Time { return now }), WithLeaseTimeout(time.Second))
	defer cleanup()

	result, err := queue.Enqueue(ctx, testMessage("run-1", 10, compute.ResourceVector{MilliCPU: 1000, MemoryMiB: 1024, DiskMiB: 2048, Slots: 1}))
	if err != nil {
		t.Fatal(err)
	}
	lease := mustDequeueOne(t, ctx, queue, "host-1")
	now = now.Add(2 * time.Second)
	exists, err := queue.ReadyMessageExists(ctx, result.MessageID)
	if err != nil {
		t.Fatal(err)
	}
	if !exists {
		t.Fatal("expired active message exists = false")
	}
	if got, err := queue.client.Exists(ctx, activeMessageKey(queue, lease.MessageID)).Result(); err != nil {
		t.Fatal(err)
	} else if got != 0 {
		t.Fatalf("active message index exists after reclaim = %d, want 0", got)
	}
	released := mustDequeueOne(t, ctx, queue, "host-2")
	if released.MessageID != result.MessageID || released.AttemptNumber != 2 {
		t.Fatalf("released lease = %+v, want message %s attempt 2", released, result.MessageID)
	}
}

func TestQueueReadyMessageExistsHandlesQueueNamedRun(t *testing.T) {
	ctx := context.Background()
	queue, cleanup := newTestQueue(t)
	defer cleanup()

	message := testMessage("run-1", 10, compute.ResourceVector{MilliCPU: 1000, MemoryMiB: 1024, DiskMiB: 2048, Slots: 1})
	message.QueueName = "run"
	result, err := queue.Enqueue(ctx, message)
	if err != nil {
		t.Fatal(err)
	}
	exists, err := queue.ReadyMessageExists(ctx, result.MessageID)
	if err != nil {
		t.Fatal(err)
	}
	if !exists {
		t.Fatal("message in queue named run exists = false")
	}
	leases, err := queue.Dequeue(ctx, dispatch.DequeueRequest{
		OrgID:            "org-1",
		WorkerInstanceID: "host-1",
		QueueName:        "run",
		Available:        compute.ResourceVector{MilliCPU: 2000, MemoryMiB: 4096, DiskMiB: 4096, Slots: 2},
		Runtime:          testRuntime(),
		MaxMessages:      1,
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(leases) != 1 || leases[0].MessageID != result.MessageID {
		t.Fatalf("leases = %+v, want message %s", leases, result.MessageID)
	}
}

func TestQueueLeaseConflictNackBacksOff(t *testing.T) {
	ctx := context.Background()
	now := time.Date(2026, 5, 19, 0, 0, 0, 0, time.UTC)
	queue, cleanup := newTestQueue(t, WithClock(func() time.Time { return now }), WithLeaseTimeout(time.Minute))
	defer cleanup()

	first := testMessage("run-1", 10, compute.ResourceVector{MilliCPU: 1000, MemoryMiB: 1024, DiskMiB: 2048, Slots: 1})
	first.QueueConcurrencyScope = "queue-a"
	first.QueueConcurrencyLimit = 1
	second := testMessage("run-2", 9, compute.ResourceVector{MilliCPU: 1000, MemoryMiB: 1024, DiskMiB: 2048, Slots: 1})
	second.QueueConcurrencyScope = "queue-a"
	second.QueueConcurrencyLimit = 1
	if _, err := queue.Enqueue(ctx, first); err != nil {
		t.Fatal(err)
	}
	if _, err := queue.Enqueue(ctx, second); err != nil {
		t.Fatal(err)
	}
	lease := mustDequeueOne(t, ctx, queue, "host-1")
	if err := queue.Nack(ctx, lease, dispatch.NackReasonLeaseConflict); err != nil {
		t.Fatal(err)
	}
	leases, err := queue.Dequeue(ctx, dispatch.DequeueRequest{
		OrgID:            "org-1",
		WorkerInstanceID: "host-2",
		QueueName:        "queue-a",
		Available:        compute.ResourceVector{MilliCPU: 2000, MemoryMiB: 4096, DiskMiB: 4096, Slots: 2},
		Runtime:          testRuntime(),
		MaxMessages:      1,
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(leases) != 1 || leases[0].Message.RunID != "run-2" {
		t.Fatalf("leases after lease-conflict nack = %+v, want second run while first backs off", leases)
	}
	if err := queue.Ack(ctx, leases[0]); err != nil {
		t.Fatal(err)
	}
	now = now.Add(time.Minute)
	released := mustDequeueOne(t, ctx, queue, "host-2")
	if released.MessageID != lease.MessageID || released.AttemptNumber != 2 {
		t.Fatalf("released lease = %+v, want message %s attempt 2", released, lease.MessageID)
	}
}

func TestQueueHonorsQueueConcurrencyLimit(t *testing.T) {
	ctx := context.Background()
	queue, cleanup := newTestQueue(t)
	defer cleanup()

	first := testMessage("run-1", 10, compute.ResourceVector{MilliCPU: 1000, MemoryMiB: 1024, DiskMiB: 2048, Slots: 1})
	first.QueueConcurrencyScope = "queue-a"
	first.QueueConcurrencyLimit = 1
	second := testMessage("run-2", 9, compute.ResourceVector{MilliCPU: 1000, MemoryMiB: 1024, DiskMiB: 2048, Slots: 1})
	second.QueueConcurrencyScope = "queue-a"
	second.QueueConcurrencyLimit = 1
	if _, err := queue.Enqueue(ctx, first); err != nil {
		t.Fatal(err)
	}
	if _, err := queue.Enqueue(ctx, second); err != nil {
		t.Fatal(err)
	}

	leases, err := queue.Dequeue(ctx, dispatch.DequeueRequest{
		OrgID:            "org-1",
		WorkerInstanceID: "host-1",
		QueueName:        "queue-a",
		Available:        compute.ResourceVector{MilliCPU: 4000, MemoryMiB: 4096, DiskMiB: 4096, Slots: 2},
		Runtime:          testRuntime(),
		MaxMessages:      2,
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(leases) != 1 {
		t.Fatalf("leases = %+v, want one lease under queue concurrency limit", leases)
	}
	if leases[0].Message.QueueConcurrencyScope != "queue-a" || leases[0].Message.QueueConcurrencyLimit != 1 {
		t.Fatalf("leased message queue concurrency = %+v", leases[0].Message)
	}
	if err := queue.Ack(ctx, leases[0]); err != nil {
		t.Fatal(err)
	}
	leases, err = queue.Dequeue(ctx, dispatch.DequeueRequest{
		OrgID:            "org-1",
		WorkerInstanceID: "host-1",
		QueueName:        "queue-a",
		Available:        compute.ResourceVector{MilliCPU: 4000, MemoryMiB: 4096, DiskMiB: 4096, Slots: 2},
		Runtime:          testRuntime(),
		MaxMessages:      2,
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(leases) != 1 || leases[0].Message.RunID != "run-2" {
		t.Fatalf("leases after ack = %+v, want second run", leases)
	}
}

func TestQueueConcurrencyLimitSpansRuntimeQueues(t *testing.T) {
	ctx := context.Background()
	queue, cleanup := newTestQueue(t)
	defer cleanup()

	first := testMessage("run-1", 10, compute.ResourceVector{MilliCPU: 1000, MemoryMiB: 1024, DiskMiB: 2048, Slots: 1})
	first.QueueName = "queue-a:rt:arm64"
	first.QueueConcurrencyScope = "queue-a"
	first.QueueConcurrencyLimit = 1
	second := testMessage("run-2", 9, compute.ResourceVector{MilliCPU: 1000, MemoryMiB: 1024, DiskMiB: 2048, Slots: 1})
	second.QueueName = "queue-a:rt:amd64"
	second.QueueConcurrencyScope = "queue-a"
	second.QueueConcurrencyLimit = 1
	if _, err := queue.Enqueue(ctx, first); err != nil {
		t.Fatal(err)
	}
	if _, err := queue.Enqueue(ctx, second); err != nil {
		t.Fatal(err)
	}

	firstLease, err := queue.Dequeue(ctx, dispatch.DequeueRequest{
		OrgID:            "org-1",
		WorkerInstanceID: "host-1",
		QueueName:        "queue-a:rt:arm64",
		Available:        compute.ResourceVector{MilliCPU: 4000, MemoryMiB: 4096, DiskMiB: 4096, Slots: 2},
		Runtime:          testRuntime(),
		MaxMessages:      1,
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(firstLease) != 1 || firstLease[0].Message.RunID != "run-1" {
		t.Fatalf("first lease = %+v", firstLease)
	}
	blocked, err := queue.Dequeue(ctx, dispatch.DequeueRequest{
		OrgID:            "org-1",
		WorkerInstanceID: "host-1",
		QueueName:        "queue-a:rt:amd64",
		Available:        compute.ResourceVector{MilliCPU: 4000, MemoryMiB: 4096, DiskMiB: 4096, Slots: 2},
		Runtime:          testRuntime(),
		MaxMessages:      1,
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(blocked) != 0 {
		t.Fatalf("blocked lease = %+v, want no lease while shared scope is full", blocked)
	}
	if err := queue.Ack(ctx, firstLease[0]); err != nil {
		t.Fatal(err)
	}
	secondLease, err := queue.Dequeue(ctx, dispatch.DequeueRequest{
		OrgID:            "org-1",
		WorkerInstanceID: "host-1",
		QueueName:        "queue-a:rt:amd64",
		Available:        compute.ResourceVector{MilliCPU: 4000, MemoryMiB: 4096, DiskMiB: 4096, Slots: 2},
		Runtime:          testRuntime(),
		MaxMessages:      1,
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(secondLease) != 1 || secondLease[0].Message.RunID != "run-2" {
		t.Fatalf("second lease = %+v", secondLease)
	}
}

func TestQueueDefaultLeaseMatchesWorkerExecutionLease(t *testing.T) {
	ctx := context.Background()
	now := time.Date(2026, 5, 19, 0, 0, 0, 0, time.UTC)
	queue, cleanup := newTestQueue(t, WithClock(func() time.Time { return now }))
	defer cleanup()

	if _, err := queue.Enqueue(ctx, testMessage("run-1", 10, compute.ResourceVector{MilliCPU: 1000, MemoryMiB: 1024, DiskMiB: 2048, Slots: 1})); err != nil {
		t.Fatal(err)
	}
	lease := mustDequeueOne(t, ctx, queue, "host-1")
	if !lease.ExpiresAt.Equal(now.Add(5 * time.Minute)) {
		t.Fatalf("lease expires at %s, want %s", lease.ExpiresAt, now.Add(5*time.Minute))
	}
}

func TestQueuePriorityAndCapacity(t *testing.T) {
	ctx := context.Background()
	queue, cleanup := newTestQueue(t)
	defer cleanup()

	if _, err := queue.Enqueue(ctx, testMessage("low", 1, compute.ResourceVector{MilliCPU: 4000, MemoryMiB: 4096, DiskMiB: 4096, Slots: 2})); err != nil {
		t.Fatal(err)
	}
	if _, err := queue.Enqueue(ctx, testMessage("high", 100, compute.ResourceVector{MilliCPU: 1000, MemoryMiB: 1024, DiskMiB: 1024, Slots: 1})); err != nil {
		t.Fatal(err)
	}
	leases, err := queue.Dequeue(ctx, dispatch.DequeueRequest{
		OrgID:            "org-1",
		WorkerInstanceID: "host-1",
		QueueName:        "queue-a",
		Available:        compute.ResourceVector{MilliCPU: 1000, MemoryMiB: 1024, DiskMiB: 1024, Slots: 1},
		Runtime:          testRuntime(),
		MaxMessages:      2,
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(leases) != 1 || leases[0].Message.RunID != "high" {
		t.Fatalf("leases = %+v", leases)
	}
}

func TestQueueSkipsOversizedHeadForCurrentHost(t *testing.T) {
	ctx := context.Background()
	queue, cleanup := newTestQueue(t)
	defer cleanup()

	if _, err := queue.Enqueue(ctx, testMessage("oversized", 100, compute.ResourceVector{MilliCPU: 4000, MemoryMiB: 4096, DiskMiB: 4096, Slots: 2})); err != nil {
		t.Fatal(err)
	}
	if _, err := queue.Enqueue(ctx, testMessage("fits", 1, compute.ResourceVector{MilliCPU: 1000, MemoryMiB: 1024, DiskMiB: 1024, Slots: 1})); err != nil {
		t.Fatal(err)
	}
	leases, err := queue.Dequeue(ctx, dispatch.DequeueRequest{
		OrgID:            "org-1",
		WorkerInstanceID: "host-1",
		QueueName:        "queue-a",
		Available:        compute.ResourceVector{MilliCPU: 1000, MemoryMiB: 1024, DiskMiB: 1024, Slots: 1},
		Runtime:          testRuntime(),
		MaxMessages:      1,
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(leases) != 1 || leases[0].Message.RunID != "fits" {
		t.Fatalf("leases = %+v", leases)
	}
}

func TestQueueStoresRuntimeMetadata(t *testing.T) {
	ctx := context.Background()
	queue, cleanup := newTestQueue(t)
	defer cleanup()

	message := testMessage("run-1", 0, compute.ResourceVector{MilliCPU: 1000, MemoryMiB: 1024, Slots: 1})
	message.Requirements.Runtime = compute.RuntimeSelector{
		ID:              "sha256:runtime-arm64",
		Arch:            "arm64",
		ABI:             "helmr.firecracker.snapshot.v0",
		KernelDigest:    "sha256:kernel",
		InitramfsDigest: "sha256:initramfs",
		RootfsDigest:    "sha256:rootfs",
		CNIProfile:      "helmr/v0",
	}
	message.Requirements.Placement = compute.Placement{
		Region:       "us-east-1",
		Tags:         map[string]string{"pool": "snapshot"},
		DedicatedKey: "tenant-a",
		SnapshotKey:  "snapshot-a",
	}
	result, err := queue.Enqueue(ctx, message)
	if err != nil {
		t.Fatal(err)
	}

	metadata, err := queue.client.HGetAll(ctx, "test:message:"+result.MessageID).Result()
	if err != nil {
		t.Fatal(err)
	}
	for key, want := range map[string]string{
		"runtime_arch":            "arm64",
		"runtime_id":              "sha256:runtime-arm64",
		"runtime_abi":             "helmr.firecracker.snapshot.v0",
		"kernel_digest":           "sha256:kernel",
		"initramfs_digest":        "sha256:initramfs",
		"rootfs_digest":           "sha256:rootfs",
		"cni_profile":             "helmr/v0",
		"placement_region":        "us-east-1",
		"placement_dedicated_key": "tenant-a",
		"placement_snapshot_key":  "snapshot-a",
	} {
		if metadata[key] != want {
			t.Fatalf("%s = %q, want %q; metadata = %+v", key, metadata[key], want, metadata)
		}
	}
	var labels map[string]string
	if err := json.Unmarshal([]byte(metadata["placement_labels"]), &labels); err != nil {
		t.Fatal(err)
	}
	if labels["pool"] != "snapshot" {
		t.Fatalf("placement_labels = %+v", labels)
	}
}

func TestQueueFiltersByRuntimeIdentity(t *testing.T) {
	ctx := context.Background()
	queue, cleanup := newTestQueue(t)
	defer cleanup()

	requiresArm := testMessage("requires-arm", 100, compute.ResourceVector{MilliCPU: 1000, MemoryMiB: 1024, Slots: 1})
	requiresArm.Requirements.Runtime = testRuntimeFor("sha256:runtime-arm", "arm64", "sha256:kernel-arm")
	if _, err := queue.Enqueue(ctx, requiresArm); err != nil {
		t.Fatal(err)
	}
	requiresAMD := testMessage("requires-amd", 1, compute.ResourceVector{MilliCPU: 1000, MemoryMiB: 1024, Slots: 1})
	requiresAMD.Requirements.Runtime = testRuntimeFor("sha256:runtime-amd", "amd64", "sha256:kernel-amd")
	if _, err := queue.Enqueue(ctx, requiresAMD); err != nil {
		t.Fatal(err)
	}

	leases, err := queue.Dequeue(ctx, dispatch.DequeueRequest{
		OrgID:            "org-1",
		WorkerInstanceID: "host-amd",
		QueueName:        "queue-a",
		Available:        compute.ResourceVector{MilliCPU: 2000, MemoryMiB: 4096, DiskMiB: 4096, Slots: 2},
		Runtime:          testRuntimeFor("sha256:runtime-amd", "amd64", "sha256:kernel-amd"),
		MaxMessages:      1,
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(leases) != 1 || leases[0].Message.RunID != "requires-amd" {
		t.Fatalf("leases = %+v", leases)
	}
	if err := queue.Ack(ctx, leases[0]); err != nil {
		t.Fatal(err)
	}

	leases, err = queue.Dequeue(ctx, dispatch.DequeueRequest{
		OrgID:            "org-1",
		WorkerInstanceID: "host-arm",
		QueueName:        "queue-a",
		Available:        compute.ResourceVector{MilliCPU: 2000, MemoryMiB: 4096, DiskMiB: 4096, Slots: 2},
		Runtime:          testRuntimeFor("sha256:runtime-arm", "arm64", "sha256:kernel-arm"),
		MaxMessages:      1,
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(leases) != 1 || leases[0].Message.RunID != "requires-arm" || leases[0].AttemptNumber != 1 {
		t.Fatalf("leases = %+v", leases)
	}
}

func TestQueueFiltersByPlacementCompatibility(t *testing.T) {
	ctx := context.Background()
	queue, cleanup := newTestQueue(t)
	defer cleanup()

	special := testMessage("special-placement", 100, compute.ResourceVector{MilliCPU: 1000, MemoryMiB: 1024, Slots: 1})
	special.Requirements.Placement = compute.Placement{
		Region:       "us-east-1",
		Tags:         map[string]string{"pool": "snapshot", "gpu": "true"},
		DedicatedKey: "tenant-a",
		SnapshotKey:  "snapshot-a",
	}
	if _, err := queue.Enqueue(ctx, special); err != nil {
		t.Fatal(err)
	}
	standard := testMessage("standard-placement", 1, compute.ResourceVector{MilliCPU: 1000, MemoryMiB: 1024, Slots: 1})
	standard.Requirements.Placement = compute.Placement{
		Region: "us-west-2",
		Tags:   map[string]string{"pool": "standard"},
	}
	if _, err := queue.Enqueue(ctx, standard); err != nil {
		t.Fatal(err)
	}

	leases, err := queue.Dequeue(ctx, dispatch.DequeueRequest{
		OrgID:            "org-1",
		WorkerInstanceID: "host-standard",
		QueueName:        "queue-a",
		Available:        compute.ResourceVector{MilliCPU: 2000, MemoryMiB: 4096, DiskMiB: 4096, Slots: 2},
		Runtime:          testRuntime(),
		Region:           "us-west-2",
		Labels:           map[string]string{"pool": "standard"},
		MaxMessages:      1,
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(leases) != 1 || leases[0].Message.RunID != "standard-placement" {
		t.Fatalf("leases = %+v", leases)
	}
	if err := queue.Ack(ctx, leases[0]); err != nil {
		t.Fatal(err)
	}

	leases, err = queue.Dequeue(ctx, dispatch.DequeueRequest{
		OrgID:            "org-1",
		WorkerInstanceID: "host-special",
		QueueName:        "queue-a",
		Available:        compute.ResourceVector{MilliCPU: 2000, MemoryMiB: 4096, DiskMiB: 4096, Slots: 2},
		Runtime:          testRuntime(),
		Region:           "us-east-1",
		Labels:           map[string]string{"pool": "snapshot", "gpu": "true", "dedicated_key": "tenant-a", "snapshot_key": "snapshot-a"},
		MaxMessages:      1,
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(leases) != 1 || leases[0].Message.RunID != "special-placement" || leases[0].AttemptNumber != 1 {
		t.Fatalf("leases = %+v", leases)
	}
}

func TestQueueNamespacesByOrgAndQueue(t *testing.T) {
	ctx := context.Background()
	queue, cleanup := newTestQueue(t)
	defer cleanup()

	if _, err := queue.Enqueue(ctx, testMessage("run-1", 0, compute.ResourceVector{MilliCPU: 1000, MemoryMiB: 1024, Slots: 1})); err != nil {
		t.Fatal(err)
	}
	leases, err := queue.Dequeue(ctx, dispatch.DequeueRequest{
		OrgID:            "org-2",
		WorkerInstanceID: "host-1",
		QueueName:        "queue-a",
		Available:        compute.ResourceVector{MilliCPU: 2000, MemoryMiB: 4096, DiskMiB: 4096, Slots: 2},
		Runtime:          testRuntime(),
		MaxMessages:      1,
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(leases) != 0 {
		t.Fatalf("cross-org leases = %+v", leases)
	}
	leases, err = queue.Dequeue(ctx, dispatch.DequeueRequest{
		OrgID:            "org-1",
		WorkerInstanceID: "host-1",
		QueueName:        "queue-b",
		Available:        compute.ResourceVector{MilliCPU: 2000, MemoryMiB: 4096, DiskMiB: 4096, Slots: 2},
		Runtime:          testRuntime(),
		MaxMessages:      1,
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(leases) != 0 {
		t.Fatalf("cross-queue leases = %+v", leases)
	}
	if got := mustDequeueOne(t, ctx, queue, "host-1"); got.Message.RunID != "run-1" {
		t.Fatalf("same-queue lease = %+v", got)
	}
}

func TestQueueReenqueueIsLeaseFenced(t *testing.T) {
	ctx := context.Background()
	queue, cleanup := newTestQueue(t)
	defer cleanup()

	if _, err := queue.Enqueue(ctx, testMessage("run-1", 0, compute.ResourceVector{MilliCPU: 1000, MemoryMiB: 1024, Slots: 1})); err != nil {
		t.Fatal(err)
	}
	oldLease := mustDequeueOne(t, ctx, queue, "host-1")
	if _, err := queue.Enqueue(ctx, testMessage("run-1", 0, compute.ResourceVector{MilliCPU: 1000, MemoryMiB: 1024, Slots: 1})); err != nil {
		t.Fatal(err)
	}
	if err := queue.Ack(ctx, oldLease); !errors.Is(err, dispatch.ErrLeaseConflict) {
		t.Fatalf("stale ack error = %v, want lease conflict", err)
	}
	newLease := mustDequeueOne(t, ctx, queue, "host-1")
	if newLease.ID == oldLease.ID || newLease.Message.RunID != "run-1" {
		t.Fatalf("new lease = %+v, old = %+v", newLease, oldLease)
	}
}

func TestQueueGenerationTTLIsRefreshedAndTerminallyCleaned(t *testing.T) {
	ctx := context.Background()
	now := time.Date(2026, 5, 19, 0, 0, 0, 0, time.UTC)
	queue, cleanup := newTestQueue(t, WithClock(func() time.Time { return now }), WithGenerationSafetyTTL(time.Hour))
	defer cleanup()

	result, err := queue.Enqueue(ctx, testMessage("run-1", 0, compute.ResourceVector{MilliCPU: 1000, MemoryMiB: 1024, Slots: 1}))
	if err != nil {
		t.Fatal(err)
	}
	generationKey := messageGenerationKey(t, ctx, queue, result.MessageID)
	if ttl := generationTTL(t, ctx, queue, generationKey); ttl <= 30*time.Minute {
		t.Fatalf("generation ttl after enqueue = %s, want refreshed safety ttl", ttl)
	}
	if err := queue.client.PExpire(ctx, generationKey, 5*time.Second).Err(); err != nil {
		t.Fatal(err)
	}
	lease := mustDequeueOne(t, ctx, queue, "host-1")
	if ttl := generationTTL(t, ctx, queue, generationKey); ttl <= 30*time.Minute {
		t.Fatalf("generation ttl after dequeue = %s, want refreshed safety ttl", ttl)
	}
	if err := queue.client.PExpire(ctx, generationKey, 5*time.Second).Err(); err != nil {
		t.Fatal(err)
	}
	renewed, err := queue.Renew(ctx, lease, now.Add(10*time.Minute))
	if err != nil {
		t.Fatal(err)
	}
	if ttl := generationTTL(t, ctx, queue, generationKey); ttl <= 30*time.Minute {
		t.Fatalf("generation ttl after renew = %s, want refreshed safety ttl", ttl)
	}
	if got, err := queue.client.Get(ctx, activeMessageKey(queue, result.MessageID)).Result(); err != nil {
		t.Fatal(err)
	} else if got != renewed.ID {
		t.Fatalf("active message index = %q, want %q", got, renewed.ID)
	}
	if err := queue.Ack(ctx, renewed); err != nil {
		t.Fatal(err)
	}
	if got, err := queue.client.Exists(ctx, generationKey, activeMessageKey(queue, result.MessageID)).Result(); err != nil {
		t.Fatal(err)
	} else if got != 0 {
		t.Fatalf("terminal redis keys exist = %d, want 0", got)
	}
}

func TestQueueReenqueuePreventsExpiredOldLeaseReclaim(t *testing.T) {
	ctx := context.Background()
	now := time.Date(2026, 5, 19, 0, 0, 0, 0, time.UTC)
	queue, cleanup := newTestQueue(t, WithClock(func() time.Time { return now }), WithLeaseTimeout(time.Second))
	defer cleanup()

	if _, err := queue.Enqueue(ctx, testMessage("run-1", 0, compute.ResourceVector{MilliCPU: 1000, MemoryMiB: 1024, Slots: 1})); err != nil {
		t.Fatal(err)
	}
	oldLease := mustDequeueOne(t, ctx, queue, "host-1")
	if _, err := queue.Enqueue(ctx, testMessage("run-1", 0, compute.ResourceVector{MilliCPU: 1000, MemoryMiB: 1024, Slots: 1})); err != nil {
		t.Fatal(err)
	}
	now = now.Add(2 * time.Second)
	lease := mustDequeueOne(t, ctx, queue, "host-2")
	if lease.ID == oldLease.ID || lease.AttemptNumber != 1 || lease.WorkerInstanceID != "host-2" {
		t.Fatalf("new generation lease = %+v, old = %+v", lease, oldLease)
	}
}

func TestQueueReenqueueFencesOldLeaseAcrossQueues(t *testing.T) {
	ctx := context.Background()
	now := time.Date(2026, 5, 19, 0, 0, 0, 0, time.UTC)
	queue, cleanup := newTestQueue(t, WithClock(func() time.Time { return now }), WithLeaseTimeout(time.Second))
	defer cleanup()

	if _, err := queue.Enqueue(ctx, testMessage("run-1", 0, compute.ResourceVector{MilliCPU: 1000, MemoryMiB: 1024, Slots: 1})); err != nil {
		t.Fatal(err)
	}
	oldLease := mustDequeueOne(t, ctx, queue, "host-1")
	requeued := testMessage("run-1", 0, compute.ResourceVector{MilliCPU: 1000, MemoryMiB: 1024, Slots: 1})
	requeued.QueueName = "queue-b"
	if _, err := queue.Enqueue(ctx, requeued); err != nil {
		t.Fatal(err)
	}
	if _, err := queue.Renew(ctx, oldLease, now.Add(time.Second)); !errors.Is(err, dispatch.ErrLeaseConflict) {
		t.Fatalf("stale renew error = %v, want lease conflict", err)
	}
	now = now.Add(2 * time.Second)
	leases, err := queue.Dequeue(ctx, dispatch.DequeueRequest{
		OrgID:            "org-1",
		WorkerInstanceID: "host-2",
		QueueName:        "queue-a",
		Available:        compute.ResourceVector{MilliCPU: 2000, MemoryMiB: 4096, DiskMiB: 4096, Slots: 2},
		Runtime:          testRuntime(),
		MaxMessages:      1,
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(leases) != 0 {
		t.Fatalf("stale queue leases = %+v", leases)
	}
	leases, err = queue.Dequeue(ctx, dispatch.DequeueRequest{
		OrgID:            "org-1",
		WorkerInstanceID: "host-2",
		QueueName:        "queue-b",
		Available:        compute.ResourceVector{MilliCPU: 2000, MemoryMiB: 4096, DiskMiB: 4096, Slots: 2},
		Runtime:          testRuntime(),
		MaxMessages:      1,
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(leases) != 1 || leases[0].ID == oldLease.ID || leases[0].AttemptNumber != 1 {
		t.Fatalf("new queue leases = %+v, old = %+v", leases, oldLease)
	}
}

func TestQueueNackRequeues(t *testing.T) {
	ctx := context.Background()
	queue, cleanup := newTestQueue(t)
	defer cleanup()

	if _, err := queue.Enqueue(ctx, testMessage("run-1", 0, compute.ResourceVector{MilliCPU: 1000, MemoryMiB: 1024, Slots: 1})); err != nil {
		t.Fatal(err)
	}
	lease := mustDequeueOne(t, ctx, queue, "host-1")
	if err := queue.Nack(ctx, lease, dispatch.NackReasonRetry); err != nil {
		t.Fatal(err)
	}
	lease = mustDequeueOne(t, ctx, queue, "host-1")
	if lease.Message.RunID != "run-1" || lease.AttemptNumber != 2 {
		t.Fatalf("redelivered lease = %+v", lease)
	}
}

func TestQueueExpiredLeaseIsReleased(t *testing.T) {
	ctx := context.Background()
	now := time.Date(2026, 5, 19, 0, 0, 0, 0, time.UTC)
	queue, cleanup := newTestQueue(t, WithClock(func() time.Time { return now }), WithLeaseTimeout(time.Second))
	defer cleanup()

	if _, err := queue.Enqueue(ctx, testMessage("run-1", 0, compute.ResourceVector{MilliCPU: 1000, MemoryMiB: 1024, Slots: 1})); err != nil {
		t.Fatal(err)
	}
	lease := mustDequeueOne(t, ctx, queue, "host-1")
	now = now.Add(2 * time.Second)
	if err := queue.Ack(ctx, lease); !errors.Is(err, dispatch.ErrLeaseExpired) {
		t.Fatalf("expired ack error = %v, want lease expired", err)
	}
	released := mustDequeueOne(t, ctx, queue, "host-2")
	if released.Message.RunID != "run-1" || released.WorkerInstanceID != "host-2" || released.AttemptNumber != 2 {
		t.Fatalf("released lease = %+v", released)
	}
}

func TestQueueRenewFencesExpiredAndConflictingLeases(t *testing.T) {
	ctx := context.Background()
	now := time.Date(2026, 5, 19, 0, 0, 0, 0, time.UTC)
	queue, cleanup := newTestQueue(t, WithClock(func() time.Time { return now }), WithLeaseTimeout(time.Second))
	defer cleanup()

	if _, err := queue.Enqueue(ctx, testMessage("run-1", 0, compute.ResourceVector{MilliCPU: 1000, MemoryMiB: 1024, Slots: 1})); err != nil {
		t.Fatal(err)
	}
	lease := mustDequeueOne(t, ctx, queue, "host-1")
	conflicting := lease
	conflicting.WorkerInstanceID = "host-2"
	if _, err := queue.Renew(ctx, conflicting, now.Add(time.Second)); !errors.Is(err, dispatch.ErrLeaseConflict) {
		t.Fatalf("conflicting renew error = %v, want lease conflict", err)
	}
	now = now.Add(2 * time.Second)
	if _, err := queue.Renew(ctx, lease, now.Add(time.Second)); !errors.Is(err, dispatch.ErrLeaseExpired) {
		t.Fatalf("expired renew error = %v, want lease expired", err)
	}
}

func newTestQueue(t *testing.T, opts ...Option) (*Queue, func()) {
	t.Helper()
	server := miniredis.RunT(t)
	client := goredis.NewClient(&goredis.Options{Addr: server.Addr()})
	allOpts := append([]Option{WithPrefix("test")}, opts...)
	queue, err := New(client, allOpts...)
	if err != nil {
		t.Fatal(err)
	}
	return queue, func() {
		_ = client.Close()
		server.Close()
	}
}

func testMessage(runID string, priority int32, resources compute.ResourceVector) dispatch.Message {
	if resources.DiskMiB == 0 {
		resources.DiskMiB = 1024
	}
	return dispatch.Message{
		RunID:         runID,
		OrgID:         "org-1",
		ProjectID:     "project-1",
		EnvironmentID: "env-1",
		QueueName:     "queue-a",
		Requirements:  dispatchRequirements(resources),
		Priority:      priority,
		EnqueuedAt:    time.Date(2026, 5, 19, 0, 0, 0, 0, time.UTC),
	}
}

func dispatchRequirements(resources compute.ResourceVector) compute.RunRuntimeRequirements {
	return compute.RunRuntimeRequirements{Resources: resources, Runtime: testRuntime()}
}

func testRuntime() compute.RuntimeSelector {
	return compute.RuntimeSelector{
		ID:              "sha256:runtime-arm64",
		Arch:            "arm64",
		ABI:             "helmr.firecracker.snapshot.v0",
		KernelDigest:    "sha256:kernel-arm64",
		InitramfsDigest: "sha256:initramfs",
		RootfsDigest:    "sha256:rootfs",
		CNIProfile:      "helmr/v0",
	}
}

func testRuntimeFor(id string, arch string, kernelDigest string) compute.RuntimeSelector {
	runtime := testRuntime()
	runtime.ID = id
	runtime.Arch = arch
	runtime.KernelDigest = kernelDigest
	return runtime
}

func mustDequeueOne(t *testing.T, ctx context.Context, queue *Queue, workerInstanceID string) dispatch.Lease {
	t.Helper()
	leases, err := queue.Dequeue(ctx, dispatch.DequeueRequest{
		OrgID:            "org-1",
		WorkerInstanceID: workerInstanceID,
		QueueName:        "queue-a",
		Available:        compute.ResourceVector{MilliCPU: 2000, MemoryMiB: 4096, DiskMiB: 4096, Slots: 2},
		Runtime:          testRuntime(),
		MaxMessages:      1,
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(leases) != 1 {
		t.Fatalf("leases = %+v, want one", leases)
	}
	return leases[0]
}

func messageGenerationKey(t *testing.T, ctx context.Context, queue *Queue, messageID string) string {
	t.Helper()
	generationKey, err := queue.client.HGet(ctx, queue.prefix+":message:"+messageID, "run_generation_key").Result()
	if err != nil {
		t.Fatal(err)
	}
	return generationKey
}

func generationTTL(t *testing.T, ctx context.Context, queue *Queue, generationKey string) time.Duration {
	t.Helper()
	ttl, err := queue.client.PTTL(ctx, generationKey).Result()
	if err != nil {
		t.Fatal(err)
	}
	return ttl
}

func activeMessageKey(queue *Queue, messageID string) string {
	return queue.prefix + ":message_active:" + messageID
}
