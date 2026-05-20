package redis

import (
	"context"
	"encoding/json"
	"errors"
	"testing"
	"time"

	"github.com/alicebob/miniredis/v2"
	"github.com/helmrdotdev/helmr/internal/compute"
	"github.com/helmrdotdev/helmr/internal/runqueue"
	goredis "github.com/redis/go-redis/v9"
)

func TestQueueEnqueueDequeueAck(t *testing.T) {
	ctx := context.Background()
	queue, cleanup := newTestQueue(t)
	defer cleanup()

	if _, err := queue.Enqueue(ctx, testMessage("run-1", 10, compute.ResourceVector{MilliCPU: 1000, MemoryMiB: 1024, DiskMiB: 2048, Slots: 1})); err != nil {
		t.Fatal(err)
	}
	leases, err := queue.Dequeue(ctx, runqueue.DequeueRequest{
		OrgID:         "org-1",
		WorkerGroupID: "group-1",
		WorkerHostID:  "host-1",
		QueueName:     "queue-a",
		Available:     compute.ResourceVector{MilliCPU: 2000, MemoryMiB: 4096, DiskMiB: 4096, Slots: 2},
		MaxMessages:   1,
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
	leases, err = queue.Dequeue(ctx, runqueue.DequeueRequest{
		OrgID:         "org-1",
		WorkerGroupID: "group-1",
		WorkerHostID:  "host-1",
		QueueName:     "queue-a",
		Available:     compute.ResourceVector{MilliCPU: 2000, MemoryMiB: 4096, DiskMiB: 4096, Slots: 2},
		MaxMessages:   1,
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
	if exists {
		t.Fatal("leased message exists = true")
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
	leases, err := queue.Dequeue(ctx, runqueue.DequeueRequest{
		OrgID:         "org-1",
		WorkerGroupID: "group-1",
		WorkerHostID:  "host-1",
		QueueName:     "queue-a",
		Available:     compute.ResourceVector{MilliCPU: 1000, MemoryMiB: 1024, DiskMiB: 1024, Slots: 1},
		MaxMessages:   2,
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
	leases, err := queue.Dequeue(ctx, runqueue.DequeueRequest{
		OrgID:         "org-1",
		WorkerGroupID: "group-1",
		WorkerHostID:  "host-1",
		QueueName:     "queue-a",
		Available:     compute.ResourceVector{MilliCPU: 1000, MemoryMiB: 1024, DiskMiB: 1024, Slots: 1},
		MaxMessages:   1,
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(leases) != 1 || leases[0].Message.RunID != "fits" {
		t.Fatalf("leases = %+v", leases)
	}
}

func TestQueueStoresCompatibilityMetadata(t *testing.T) {
	ctx := context.Background()
	queue, cleanup := newTestQueue(t)
	defer cleanup()

	message := testMessage("run-1", 0, compute.ResourceVector{MilliCPU: 1000, MemoryMiB: 1024, Slots: 1})
	message.Requirements.Runtime = compute.RuntimeSelector{
		Arch:         "arm64",
		ABI:          "helmr.firecracker.snapshot.v0",
		KernelDigest: "sha256:kernel",
		RootfsDigest: "sha256:rootfs",
		CNIProfile:   "helmr/v1",
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
		"runtime_abi":             "helmr.firecracker.snapshot.v0",
		"kernel_digest":           "sha256:kernel",
		"rootfs_digest":           "sha256:rootfs",
		"cni_profile":             "helmr/v1",
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

func TestQueueFiltersByRuntimeCompatibility(t *testing.T) {
	ctx := context.Background()
	queue, cleanup := newTestQueue(t)
	defer cleanup()

	requiresArm := testMessage("requires-arm", 100, compute.ResourceVector{MilliCPU: 1000, MemoryMiB: 1024, Slots: 1})
	requiresArm.Requirements.Runtime = compute.RuntimeSelector{
		Arch:         "arm64",
		ABI:          "helmr.firecracker.snapshot.v0",
		KernelDigest: "sha256:kernel-arm",
		RootfsDigest: "sha256:rootfs",
		CNIProfile:   "helmr/v1",
	}
	if _, err := queue.Enqueue(ctx, requiresArm); err != nil {
		t.Fatal(err)
	}
	requiresAMD := testMessage("requires-amd", 1, compute.ResourceVector{MilliCPU: 1000, MemoryMiB: 1024, Slots: 1})
	requiresAMD.Requirements.Runtime = compute.RuntimeSelector{
		Arch:         "amd64",
		ABI:          "helmr.firecracker.snapshot.v0",
		KernelDigest: "sha256:kernel-amd",
		RootfsDigest: "sha256:rootfs",
		CNIProfile:   "helmr/v1",
	}
	if _, err := queue.Enqueue(ctx, requiresAMD); err != nil {
		t.Fatal(err)
	}

	leases, err := queue.Dequeue(ctx, runqueue.DequeueRequest{
		OrgID:         "org-1",
		WorkerGroupID: "group-1",
		WorkerHostID:  "host-amd",
		QueueName:     "queue-a",
		Available:     compute.ResourceVector{MilliCPU: 2000, MemoryMiB: 4096, DiskMiB: 4096, Slots: 2},
		Runtime:       compute.RuntimeSelector{Arch: "amd64", ABI: "helmr.firecracker.snapshot.v0", KernelDigest: "sha256:kernel-amd", RootfsDigest: "sha256:rootfs", CNIProfile: "helmr/v1"},
		MaxMessages:   1,
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

	leases, err = queue.Dequeue(ctx, runqueue.DequeueRequest{
		OrgID:         "org-1",
		WorkerGroupID: "group-1",
		WorkerHostID:  "host-arm",
		QueueName:     "queue-a",
		Available:     compute.ResourceVector{MilliCPU: 2000, MemoryMiB: 4096, DiskMiB: 4096, Slots: 2},
		Runtime:       compute.RuntimeSelector{Arch: "arm64", ABI: "helmr.firecracker.snapshot.v0", KernelDigest: "sha256:kernel-arm", RootfsDigest: "sha256:rootfs", CNIProfile: "helmr/v1"},
		MaxMessages:   1,
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

	leases, err := queue.Dequeue(ctx, runqueue.DequeueRequest{
		OrgID:         "org-1",
		WorkerGroupID: "group-1",
		WorkerHostID:  "host-standard",
		QueueName:     "queue-a",
		Available:     compute.ResourceVector{MilliCPU: 2000, MemoryMiB: 4096, DiskMiB: 4096, Slots: 2},
		Region:        "us-west-2",
		Labels:        map[string]string{"pool": "standard"},
		MaxMessages:   1,
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

	leases, err = queue.Dequeue(ctx, runqueue.DequeueRequest{
		OrgID:         "org-1",
		WorkerGroupID: "group-1",
		WorkerHostID:  "host-special",
		QueueName:     "queue-a",
		Available:     compute.ResourceVector{MilliCPU: 2000, MemoryMiB: 4096, DiskMiB: 4096, Slots: 2},
		Region:        "us-east-1",
		Labels:        map[string]string{"pool": "snapshot", "gpu": "true", "dedicated_key": "tenant-a", "snapshot_key": "snapshot-a"},
		MaxMessages:   1,
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(leases) != 1 || leases[0].Message.RunID != "special-placement" || leases[0].AttemptNumber != 1 {
		t.Fatalf("leases = %+v", leases)
	}
}

func TestQueueNamespacesByOrgAndWorkerGroup(t *testing.T) {
	ctx := context.Background()
	queue, cleanup := newTestQueue(t)
	defer cleanup()

	if _, err := queue.Enqueue(ctx, testMessage("run-1", 0, compute.ResourceVector{MilliCPU: 1000, MemoryMiB: 1024, Slots: 1})); err != nil {
		t.Fatal(err)
	}
	leases, err := queue.Dequeue(ctx, runqueue.DequeueRequest{
		OrgID:         "org-2",
		WorkerGroupID: "group-1",
		WorkerHostID:  "host-1",
		QueueName:     "queue-a",
		Available:     compute.ResourceVector{MilliCPU: 2000, MemoryMiB: 4096, DiskMiB: 4096, Slots: 2},
		MaxMessages:   1,
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(leases) != 0 {
		t.Fatalf("cross-org leases = %+v", leases)
	}
	leases, err = queue.Dequeue(ctx, runqueue.DequeueRequest{
		OrgID:         "org-1",
		WorkerGroupID: "group-2",
		WorkerHostID:  "host-1",
		QueueName:     "queue-a",
		Available:     compute.ResourceVector{MilliCPU: 2000, MemoryMiB: 4096, DiskMiB: 4096, Slots: 2},
		MaxMessages:   1,
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(leases) != 0 {
		t.Fatalf("cross-group leases = %+v", leases)
	}
	if got := mustDequeueOne(t, ctx, queue, "host-1"); got.Message.RunID != "run-1" {
		t.Fatalf("same-group lease = %+v", got)
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
	if err := queue.Ack(ctx, oldLease); !errors.Is(err, runqueue.ErrLeaseConflict) {
		t.Fatalf("stale ack error = %v, want lease conflict", err)
	}
	newLease := mustDequeueOne(t, ctx, queue, "host-1")
	if newLease.ID == oldLease.ID || newLease.Message.RunID != "run-1" {
		t.Fatalf("new lease = %+v, old = %+v", newLease, oldLease)
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
	if lease.ID == oldLease.ID || lease.AttemptNumber != 1 || lease.WorkerHostID != "host-2" {
		t.Fatalf("new generation lease = %+v, old = %+v", lease, oldLease)
	}
}

func TestQueueReenqueueFencesOldLeaseAcrossWorkerGroups(t *testing.T) {
	ctx := context.Background()
	now := time.Date(2026, 5, 19, 0, 0, 0, 0, time.UTC)
	queue, cleanup := newTestQueue(t, WithClock(func() time.Time { return now }), WithLeaseTimeout(time.Second))
	defer cleanup()

	if _, err := queue.Enqueue(ctx, testMessage("run-1", 0, compute.ResourceVector{MilliCPU: 1000, MemoryMiB: 1024, Slots: 1})); err != nil {
		t.Fatal(err)
	}
	oldLease := mustDequeueOne(t, ctx, queue, "host-1")
	requeued := testMessage("run-1", 0, compute.ResourceVector{MilliCPU: 1000, MemoryMiB: 1024, Slots: 1})
	requeued.WorkerGroupID = "group-2"
	if _, err := queue.Enqueue(ctx, requeued); err != nil {
		t.Fatal(err)
	}
	if _, err := queue.Renew(ctx, oldLease, now.Add(time.Second)); !errors.Is(err, runqueue.ErrLeaseConflict) {
		t.Fatalf("stale renew error = %v, want lease conflict", err)
	}
	now = now.Add(2 * time.Second)
	leases, err := queue.Dequeue(ctx, runqueue.DequeueRequest{
		OrgID:         "org-1",
		WorkerGroupID: "group-1",
		WorkerHostID:  "host-2",
		QueueName:     "queue-a",
		Available:     compute.ResourceVector{MilliCPU: 2000, MemoryMiB: 4096, DiskMiB: 4096, Slots: 2},
		MaxMessages:   1,
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(leases) != 0 {
		t.Fatalf("stale group leases = %+v", leases)
	}
	leases, err = queue.Dequeue(ctx, runqueue.DequeueRequest{
		OrgID:         "org-1",
		WorkerGroupID: "group-2",
		WorkerHostID:  "host-2",
		QueueName:     "queue-a",
		Available:     compute.ResourceVector{MilliCPU: 2000, MemoryMiB: 4096, DiskMiB: 4096, Slots: 2},
		MaxMessages:   1,
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(leases) != 1 || leases[0].ID == oldLease.ID || leases[0].AttemptNumber != 1 {
		t.Fatalf("new group leases = %+v, old = %+v", leases, oldLease)
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
	if err := queue.Nack(ctx, lease, runqueue.NackReasonRetry); err != nil {
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
	if err := queue.Ack(ctx, lease); !errors.Is(err, runqueue.ErrLeaseExpired) {
		t.Fatalf("expired ack error = %v, want lease expired", err)
	}
	released := mustDequeueOne(t, ctx, queue, "host-2")
	if released.Message.RunID != "run-1" || released.WorkerHostID != "host-2" || released.AttemptNumber != 2 {
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
	conflicting.WorkerHostID = "host-2"
	if _, err := queue.Renew(ctx, conflicting, now.Add(time.Second)); !errors.Is(err, runqueue.ErrLeaseConflict) {
		t.Fatalf("conflicting renew error = %v, want lease conflict", err)
	}
	now = now.Add(2 * time.Second)
	if _, err := queue.Renew(ctx, lease, now.Add(time.Second)); !errors.Is(err, runqueue.ErrLeaseExpired) {
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

func testMessage(runID string, priority int32, resources compute.ResourceVector) runqueue.Message {
	if resources.DiskMiB == 0 {
		resources.DiskMiB = 1024
	}
	return runqueue.Message{
		RunID:         runID,
		OrgID:         "org-1",
		ProjectID:     "project-1",
		EnvironmentID: "env-1",
		WorkerGroupID: "group-1",
		QueueName:     "queue-a",
		Requirements:  dispatchRequirements(resources),
		Priority:      priority,
		EnqueuedAt:    time.Date(2026, 5, 19, 0, 0, 0, 0, time.UTC),
	}
}

func dispatchRequirements(resources compute.ResourceVector) compute.RunRequirements {
	return compute.RunRequirements{Resources: resources}
}

func mustDequeueOne(t *testing.T, ctx context.Context, queue *Queue, runnerHostID string) runqueue.Lease {
	t.Helper()
	leases, err := queue.Dequeue(ctx, runqueue.DequeueRequest{
		OrgID:         "org-1",
		WorkerGroupID: "group-1",
		WorkerHostID:  runnerHostID,
		QueueName:     "queue-a",
		Available:     compute.ResourceVector{MilliCPU: 2000, MemoryMiB: 4096, DiskMiB: 4096, Slots: 2},
		MaxMessages:   1,
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(leases) != 1 {
		t.Fatalf("leases = %+v, want one", leases)
	}
	return leases[0]
}
