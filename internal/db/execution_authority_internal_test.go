package db

import (
	"os"
	"strings"
	"testing"
)

func TestRunLeaseAuthorityUsesTypedWorkerFences(t *testing.T) {
	for _, token := range []string{
		"worker_instance_id",
		"worker_group_id",
		"worker_epoch",
		"lease_sequence",
		"runtime_instance_id",
		"network_slot_id",
		"network_slot_generation",
	} {
		if !strings.Contains(claimAssignedRunLease, token) {
			t.Fatalf("ClaimAssignedRunLease must fence %s", token)
		}
		if !strings.Contains(renewRunLease, token) {
			t.Fatalf("RenewRunLease must fence %s", token)
		}
	}
	for _, obsolete := range []string{"dispatch_message_id", "dispatch_lease_id", "dispatch_generation"} {
		if strings.Contains(claimAssignedRunLease, obsolete) || strings.Contains(renewRunLease, obsolete) {
			t.Fatalf("typed run lease authority must not depend on %s", obsolete)
		}
	}
}

func TestRunLeaseRedrivePreservesAttemptAndRequiresANewLeaseSequence(t *testing.T) {
	source, err := os.ReadFile("query/run_leases.sql")
	if err != nil {
		t.Fatal(err)
	}
	runLeaseQueries := string(source)
	if !strings.Contains(runLeaseQueries, "-- name: RequeueExpiredRunningRunLeases :exec") ||
		!strings.Contains(runLeaseQueries, "state = 'lost'") ||
		!strings.Contains(runLeaseQueries, "run.lease_lost_requeued") {
		t.Fatal("expired running lease must become lost and requeue the same run")
	}
	if strings.Contains(runLeaseQueries, "current_attempt_number = current_attempt_number + 1") {
		t.Fatal("lease-loss redrive must preserve the task attempt number")
	}
	if !strings.Contains(leaseRunLease, "lease_sequence") {
		t.Fatal("replacement placement must persist its higher lease sequence")
	}
}

func TestWaitAndCheckpointAuthorityIsTyped(t *testing.T) {
	for _, token := range []string{
		"worker_instance_id",
		"worker_group_id",
		"worker_epoch",
		"runtime_instance_id",
		"network_slot_id",
		"network_slot_generation",
	} {
		if !strings.Contains(getWorkerRunWaitCreateScope, token) {
			t.Fatalf("run wait creation must fence %s", token)
		}
	}
	if !strings.Contains(claimRunCheckpointWait, "checkpoint_attempt_id") {
		t.Fatal("checkpoint work must use a durable typed attempt id")
	}
	if strings.Contains(claimRunCheckpointWait, "worker_commands") {
		t.Fatal("checkpoint work must not use the removed generic worker command queue")
	}
	if !strings.Contains(getClaimedRunRestorePayload, "run_checkpoint_id") ||
		!strings.Contains(getRunRestorePayload, "reserved_workspace_version_id") {
		t.Fatal("restore payload must bind the checkpoint and workspace version")
	}
}

func TestBuildPlacementFreezesResourceVector(t *testing.T) {
	for _, token := range []string{
		"requested_cpu_millis",
		"requested_memory_bytes",
		"requested_workload_disk_bytes",
		"requested_scratch_bytes",
		"requested_build_cache_bytes",
		"requested_artifact_cache_bytes",
		"requested_executors",
	} {
		if !strings.Contains(leaseQueuedDeploymentBuild, token) {
			t.Fatalf("build placement must freeze %s", token)
		}
	}
	if !strings.Contains(claimDeploymentBuildLease, "lease_sequence") ||
		!strings.Contains(renewDeploymentBuildLease, "lease_sequence") {
		t.Fatal("build claim and renewal must share the lease-sequence fence")
	}
}

func TestCapacityAccountsForRunAndBuildIsolation(t *testing.T) {
	for _, query := range []string{getWorkerInstanceQueueCapacity, getWorkerInstanceRunDispatchCapacity} {
		for _, token := range []string{
			"FROM run_leases",
			"FROM deployment_build_leases",
			"requested_cpu_millis",
			"requested_memory_bytes",
			"requested_workload_disk_bytes",
			"requested_scratch_bytes",
		} {
			if !strings.Contains(query, token) {
				t.Fatalf("worker capacity must account for %s", token)
			}
		}
	}
}

func TestMeteringIsTransactionallyLinkedToTelemetry(t *testing.T) {
	for name, query := range map[string]string{
		"run logs":          appendRunLogChunk,
		"run release":       releaseRunLease,
		"checkpoint commit": setRunWaitWorkspaceVersion,
		"build completion":  completeDeploymentBuild,
		"build failure":     failDeploymentBuild,
	} {
		if !strings.Contains(query, "INSERT INTO meter_events") {
			t.Fatalf("%s must append a durable meter event", name)
		}
		if !strings.Contains(query, "meter_event_id") {
			t.Fatalf("%s must link its telemetry outbox row to the meter event", name)
		}
	}
}

func TestWorkspaceTransitionsUseRuntimeAndFencingGeneration(t *testing.T) {
	for _, query := range []string{claimWorkspaceMount, markWorkspaceMountMounted, renewWorkspaceMount, stopWorkspaceMount} {
		if !strings.Contains(query, "runtime_instance_id") {
			t.Fatal("workspace mount transition is missing runtime identity")
		}
		if !strings.Contains(query, "fencing_generation") {
			t.Fatal("workspace mount transition is missing fencing generation")
		}
	}
}

func TestNetworkSlotsBindOnlyObservedCNIFacts(t *testing.T) {
	if strings.Contains(certifyWorkerInstance, "host_interface_name") ||
		strings.Contains(certifyWorkerInstance, "guest_address") ||
		strings.Contains(certifyWorkerInstance, "tap_name") {
		t.Fatal("activation slot permits must contain identity and capacity only")
	}
	for _, token := range []string{"generate_series", "slot_name", "startup_inventory_epoch"} {
		if !strings.Contains(certifyWorkerInstance, token) {
			t.Fatalf("worker activation slot certification is missing %s", token)
		}
	}
	for _, token := range []string{"state = 'bound'", "host_interface_name", "guest_address", "gateway_address", "subnet", "tap_name", "netns_name", "guest_mac"} {
		if !strings.Contains(markRuntimeInstanceReady, token) {
			t.Fatalf("runtime ready transition must bind observed CNI fact %s", token)
		}
	}
	if !strings.Contains(recordWorkerStartupRecovery, "state = 'lost'") ||
		strings.Contains(recordWorkerStartupRecovery, "state = 'available'") {
		t.Fatal("startup proof must terminally reclaim old-epoch slots, never make them available")
	}
}
