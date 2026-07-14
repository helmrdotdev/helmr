package redis

import (
	"context"
	"fmt"
	"strings"
	"testing"
	"time"

	miniredis "github.com/alicebob/miniredis/v2"
	"github.com/helmrdotdev/helmr/internal/compute"
	"github.com/helmrdotdev/helmr/internal/dispatch"
	redisv9 "github.com/redis/go-redis/v9"
)

func TestQueueStoresOnlyReconstructableReadyIndex(t *testing.T) {
	server := miniredis.RunT(t)
	client := redisv9.NewClient(&redisv9.Options{Addr: server.Addr()})
	queue, err := New(client, WithPrefix("test"))
	if err != nil {
		t.Fatal(err)
	}
	now := time.Unix(100, 0).UTC()
	result, err := queue.Enqueue(context.Background(), dispatch.Message{
		WorkKind: dispatch.WorkKindRun, RunID: "run-1", OrgID: "org-1", RegionID: "us-east-1",
		ProjectID: "project-1", EnvironmentID: "env-1", QueueClass: "default",
		QueueName: "jobs", RunStateVersion: 3, QueueTimestamp: now, EnqueuedAt: now,
		Requirements: compute.RunRuntimeRequirements{
			Resources: compute.ResourceVector{MilliCPU: 100, MemoryMiB: 128, Slots: 1},
			Runtime: compute.RuntimeSelector{ID: "runtime", Arch: "x86_64", ABI: "abi",
				KernelDigest: "kernel", InitramfsDigest: "initramfs", RootfsDigest: "rootfs", CNIProfile: "cni"},
			Network: compute.DefaultNetworkPolicy(),
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	if result.MessageID != "run-1" || result.Depth != 1 {
		t.Fatalf("enqueue result = %+v", result)
	}
	keys := server.Keys()
	for _, key := range keys {
		if containsAny(key, "lease", "active", "worker_group", "dispatch_generation") {
			t.Fatalf("forbidden authority key created: %s", key)
		}
	}
}

func TestSelectReadyBoundsTenantContributionAndInterleavesOrganizations(t *testing.T) {
	server := miniredis.RunT(t)
	client := redisv9.NewClient(&redisv9.Options{Addr: server.Addr()})
	now := time.Unix(1_000, 0).UTC()
	queue, err := New(client, WithPrefix("fair"), WithClock(func() time.Time { return now }))
	if err != nil {
		t.Fatal(err)
	}
	for i := range 6 {
		message := testMessage("a-"+time.Unix(int64(i), 0).Format("05"), "org-a", "env-a", now.Add(time.Duration(i)*time.Millisecond))
		if _, err := queue.Enqueue(context.Background(), message); err != nil {
			t.Fatal(err)
		}
	}
	for i := range 3 {
		message := testMessage("b-"+time.Unix(int64(i), 0).Format("05"), "org-b", "env-b", now.Add(time.Duration(i)*time.Millisecond))
		if _, err := queue.Enqueue(context.Background(), message); err != nil {
			t.Fatal(err)
		}
	}

	selected, err := queue.SelectReady(context.Background(), dispatch.ReadySelection{
		WorkKind: dispatch.WorkKindRun, RegionID: "us-east-1", Limit: 8, TenantContributionLimit: 2, OldestWorkAfter: time.Hour,
	})
	if err != nil {
		t.Fatal(err)
	}
	counts := map[string]int{}
	for _, message := range selected {
		counts[message.OrgID]++
	}
	if counts["org-a"] != 2 || counts["org-b"] != 2 || len(selected) != 4 {
		t.Fatalf("selected tenant contributions = %#v (%d total), want two each", counts, len(selected))
	}
	if selected[0].OrgID == selected[1].OrgID {
		t.Fatalf("first fair selections did not interleave organizations: %s, %s", selected[0].OrgID, selected[1].OrgID)
	}
}

func TestSelectReadyOldestWorkEscapesFairOrder(t *testing.T) {
	server := miniredis.RunT(t)
	client := redisv9.NewClient(&redisv9.Options{Addr: server.Addr()})
	now := time.Unix(2_000, 0).UTC()
	queue, err := New(client, WithPrefix("oldest"), WithClock(func() time.Time { return now }))
	if err != nil {
		t.Fatal(err)
	}
	old := testMessage("old-run", "org-z", "env-z", now.Add(-time.Minute))
	fresh := testMessage("fresh-run", "org-a", "env-a", now)
	if _, err := queue.Enqueue(context.Background(), fresh); err != nil {
		t.Fatal(err)
	}
	if _, err := queue.Enqueue(context.Background(), old); err != nil {
		t.Fatal(err)
	}
	selected, err := queue.SelectReady(context.Background(), dispatch.ReadySelection{
		WorkKind: dispatch.WorkKindRun, RegionID: "us-east-1", Limit: 1, OldestWorkAfter: 30 * time.Second,
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(selected) != 1 || selected[0].RunID != "old-run" {
		t.Fatalf("oldest escape selection = %+v, want old-run", selected)
	}
}

func TestRemoveReadyCleansReconstructableSourceAndLeaf(t *testing.T) {
	server := miniredis.RunT(t)
	client := redisv9.NewClient(&redisv9.Options{Addr: server.Addr()})
	queue, err := New(client, WithPrefix("cleanup"))
	if err != nil {
		t.Fatal(err)
	}
	message := testMessage("stale-run", "org-a", "env-a", time.Now().UTC())
	if _, err := queue.Enqueue(context.Background(), message); err != nil {
		t.Fatal(err)
	}
	if err := queue.RemoveReady(context.Background(), dispatch.WorkKindRun, message.RunID, message.ReadyFence()); err != nil {
		t.Fatal(err)
	}
	if server.Exists(queue.sourceKey(dispatch.WorkKindRun, message.RunID)) {
		t.Fatal("ready source survived cleanup")
	}
	if members, err := client.ZRange(context.Background(), queue.readyKey(message), 0, -1).Result(); err != nil || len(members) != 0 {
		t.Fatalf("ready leaf after cleanup = %v, %v", members, err)
	}
}

func TestSelectReadyRotatesContinuouslyBackloggedSiblingLeaves(t *testing.T) {
	server := miniredis.RunT(t)
	client := redisv9.NewClient(&redisv9.Options{Addr: server.Addr()})
	now := time.Unix(4_000, 0).UTC()
	queue, err := New(client, WithPrefix("leaf-fair"), WithClock(func() time.Time { return now }))
	if err != nil {
		t.Fatal(err)
	}
	first := testMessage("run-a", "org-a", "env-a", now)
	first.QueueName = "queue-a"
	second := testMessage("run-b", "org-a", "env-a", now.Add(time.Millisecond))
	second.QueueName = "queue-b"
	for _, message := range []dispatch.Message{first, second} {
		if _, err := queue.Enqueue(context.Background(), message); err != nil {
			t.Fatal(err)
		}
	}

	counts := map[string]int{}
	for range 8 {
		selected, err := queue.SelectReady(context.Background(), dispatch.ReadySelection{
			WorkKind: dispatch.WorkKindRun, RegionID: "us-east-1", Limit: 1, TenantContributionLimit: 1,
			OldestWorkAfter: time.Hour,
		})
		if err != nil {
			t.Fatal(err)
		}
		if len(selected) != 1 {
			t.Fatalf("selected = %+v, want one ready item", selected)
		}
		counts[selected[0].QueueName]++
	}
	if counts["queue-a"] == 0 || counts["queue-b"] == 0 {
		t.Fatalf("sibling leaf starved under sustained backlog: %#v", counts)
	}
	if delta := counts["queue-a"] - counts["queue-b"]; delta < -1 || delta > 1 {
		t.Fatalf("sibling leaf service is not bounded: %#v", counts)
	}
}

func TestRegionKeyEncodingIsInjective(t *testing.T) {
	server := miniredis.RunT(t)
	client := redisv9.NewClient(&redisv9.Options{Addr: server.Addr()})
	queue, err := New(client, WithPrefix("region-encoding"))
	if err != nil {
		t.Fatal(err)
	}
	colon := testMessage("run-colon", "org-a", "env-a", time.Now().UTC())
	colon.RegionID = "a:b"
	underscore := testMessage("run-underscore", "org-a", "env-a", time.Now().UTC())
	underscore.RegionID = "a_b"
	if queue.oldestKey(dispatch.WorkKindRun, colon.RegionID) == queue.oldestKey(dispatch.WorkKindRun, underscore.RegionID) ||
		queue.organizationsKey(dispatch.WorkKindRun, colon.RegionID) == queue.organizationsKey(dispatch.WorkKindRun, underscore.RegionID) {
		t.Fatal("distinct DB-valid region IDs produced the same hierarchy key")
	}
	for _, message := range []dispatch.Message{colon, underscore} {
		if _, err := queue.Enqueue(context.Background(), message); err != nil {
			t.Fatal(err)
		}
	}
	for _, test := range []struct {
		region string
		want   string
	}{{"a:b", "run-colon"}, {"a_b", "run-underscore"}} {
		selected, err := queue.SelectReady(context.Background(), dispatch.ReadySelection{
			WorkKind: dispatch.WorkKindRun, RegionID: test.region, Limit: 1, OldestWorkAfter: time.Hour,
		})
		if err != nil {
			t.Fatal(err)
		}
		if len(selected) != 1 || selected[0].RunID != test.want {
			t.Fatalf("region %q selected %+v, want %s", test.region, selected, test.want)
		}
	}
}

func TestReadyRegionsRotatesBeyondFirstScanBatch(t *testing.T) {
	server := miniredis.RunT(t)
	client := redisv9.NewClient(&redisv9.Options{Addr: server.Addr()})
	queue, err := New(client, WithPrefix("region-rotation"))
	if err != nil {
		t.Fatal(err)
	}
	const regionCount = 11
	for i := range regionCount {
		message := testMessage(fmt.Sprintf("run-%02d", i), "org-a", "env-a", time.Now().UTC())
		message.RegionID = fmt.Sprintf("region-%02d", i)
		if _, err := queue.Enqueue(context.Background(), message); err != nil {
			t.Fatal(err)
		}
	}
	seen := map[string]bool{}
	for range 8 {
		regions, err := queue.ReadyRegions(context.Background(), dispatch.WorkKindRun, 3)
		if err != nil {
			t.Fatal(err)
		}
		if len(regions) > 3 {
			t.Fatalf("ready region batch exceeded limit: %v", regions)
		}
		for _, region := range regions {
			seen[region] = true
		}
		if len(seen) == regionCount {
			break
		}
	}
	if len(seen) != regionCount {
		t.Fatalf("bounded SSCAN rotation starved regions: saw %d/%d: %#v", len(seen), regionCount, seen)
	}
}

func TestRemoveReadyDoesNotDeleteNewerReconstructedEntry(t *testing.T) {
	server := miniredis.RunT(t)
	client := redisv9.NewClient(&redisv9.Options{Addr: server.Addr()})
	queue, err := New(client, WithPrefix("versioned"))
	if err != nil {
		t.Fatal(err)
	}
	message := testMessage("run-1", "org-a", "env-a", time.Now().UTC())
	message.RunStateVersion = 2
	if _, err := queue.Enqueue(context.Background(), message); err != nil {
		t.Fatal(err)
	}
	if err := queue.RemoveReady(context.Background(), dispatch.WorkKindRun, message.RunID, "run:1"); err != nil {
		t.Fatal(err)
	}
	loaded, ok, err := queue.loadMessage(context.Background(), dispatch.WorkKindRun, message.RunID)
	if err != nil || !ok || loaded.RunStateVersion != 2 {
		t.Fatalf("newer ready source was removed: loaded=%+v ok=%v err=%v", loaded, ok, err)
	}
}

func TestBuildReadyIndexUsesSameFairnessAndPreservesFrozenResources(t *testing.T) {
	server := miniredis.RunT(t)
	client := redisv9.NewClient(&redisv9.Options{Addr: server.Addr()})
	now := time.Unix(3_000, 0).UTC()
	queue, err := New(client, WithPrefix("build-fair"), WithClock(func() time.Time { return now }))
	if err != nil {
		t.Fatal(err)
	}
	for i := range 4 {
		if _, err := queue.Enqueue(context.Background(), testBuildMessage("dep-a-"+time.Unix(int64(i), 0).Format("05"), "org-a", "env-a", now.Add(time.Duration(i)*time.Millisecond))); err != nil {
			t.Fatal(err)
		}
	}
	for i := range 2 {
		if _, err := queue.Enqueue(context.Background(), testBuildMessage("dep-b-"+time.Unix(int64(i), 0).Format("05"), "org-b", "env-b", now.Add(time.Duration(i)*time.Millisecond))); err != nil {
			t.Fatal(err)
		}
	}
	regions, err := queue.ReadyRegions(context.Background(), dispatch.WorkKindBuild, 4)
	if err != nil || len(regions) != 1 || regions[0] != "us-east-1" {
		t.Fatalf("build regions = %v, %v", regions, err)
	}
	selected, err := queue.SelectReady(context.Background(), dispatch.ReadySelection{WorkKind: dispatch.WorkKindBuild,
		RegionID: "us-east-1", Limit: 6, TenantContributionLimit: 2, OldestWorkAfter: time.Hour})
	if err != nil {
		t.Fatal(err)
	}
	counts := map[string]int{}
	for _, message := range selected {
		counts[message.OrgID]++
		if message.WorkKind != dispatch.WorkKindBuild || message.BuildResources.BuildCacheBytes != 4096 ||
			message.BuildResources.ArtifactCacheBytes != 8192 || message.BuildResources.Executors != 2 {
			t.Fatalf("frozen build message changed: %+v", message)
		}
	}
	if len(selected) != 4 || counts["org-a"] != 2 || counts["org-b"] != 2 {
		t.Fatalf("build fair selection = %#v (%d)", counts, len(selected))
	}
}

func TestBuildReadyCASRemovalPreservesNewAttempt(t *testing.T) {
	server := miniredis.RunT(t)
	client := redisv9.NewClient(&redisv9.Options{Addr: server.Addr()})
	queue, err := New(client, WithPrefix("build-cas"))
	if err != nil {
		t.Fatal(err)
	}
	message := testBuildMessage("dep-1", "org-a", "env-a", time.Now().UTC())
	message.BuildAttemptNumber = 2
	message.LeaseSequence = 1
	if _, err := queue.Enqueue(context.Background(), message); err != nil {
		t.Fatal(err)
	}
	if err := queue.RemoveReady(context.Background(), dispatch.WorkKindBuild, message.DeploymentID, "build:1:4"); err != nil {
		t.Fatal(err)
	}
	loaded, ok, err := queue.loadMessage(context.Background(), dispatch.WorkKindBuild, message.DeploymentID)
	if err != nil || !ok || loaded.ReadyFence() != "build:2:1" {
		t.Fatalf("new build attempt removed: loaded=%+v ok=%v err=%v", loaded, ok, err)
	}
}

func testMessage(runID, orgID, environmentID string, queuedAt time.Time) dispatch.Message {
	return dispatch.Message{
		WorkKind: dispatch.WorkKindRun, RunID: runID, OrgID: orgID, RegionID: "us-east-1", ProjectID: "project-1",
		EnvironmentID: environmentID, QueueClass: "default", QueueName: "jobs",
		RunStateVersion: 1, QueueTimestamp: queuedAt, EnqueuedAt: queuedAt,
		Requirements: compute.RunRuntimeRequirements{
			Resources: compute.ResourceVector{MilliCPU: 100, MemoryMiB: 128, Slots: 1},
			Runtime: compute.RuntimeSelector{ID: "runtime", Arch: "x86_64", ABI: "abi",
				KernelDigest: "kernel", InitramfsDigest: "initramfs", RootfsDigest: "rootfs", CNIProfile: "cni"},
			Network: compute.DefaultNetworkPolicy(),
		},
	}
}

func testBuildMessage(deploymentID, orgID, environmentID string, queuedAt time.Time) dispatch.Message {
	return dispatch.Message{WorkKind: dispatch.WorkKindBuild, DeploymentID: deploymentID, OrgID: orgID,
		RegionID: "us-east-1", ProjectID: "project-1", EnvironmentID: environmentID,
		QueueClass: "build", QueueName: "deployment-build", BuildAttemptNumber: 1, LeaseSequence: 1,
		QueueTimestamp: queuedAt, EnqueuedAt: queuedAt,
		BuildResources: dispatch.BuildResourceVector{CPUMillis: 1000, MemoryBytes: 1024,
			WorkloadDiskBytes: 2048, ScratchBytes: 1024, BuildCacheBytes: 4096,
			ArtifactCacheBytes: 8192, Executors: 2}}
}

func containsAny(value string, needles ...string) bool {
	for _, needle := range needles {
		if strings.Contains(value, needle) {
			return true
		}
	}
	return false
}
