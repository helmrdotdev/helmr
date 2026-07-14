package db_test

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/google/uuid"
	"github.com/helmrdotdev/helmr/internal/db"
	"github.com/helmrdotdev/helmr/internal/db/dbtest"
	"github.com/helmrdotdev/helmr/internal/pgvalue"
	"github.com/jackc/pgx/v5/pgtype"
)

func TestGetNextRuntimeReconcileTargetWithoutRuntimeSubstrate(t *testing.T) {
	ctx := context.Background()
	pool := newIntegrationDB(t, ctx)
	ids := seedIntegration(t, ctx, pool)
	queries := db.New(pool)

	workerID := uuid.Must(uuid.NewV7())
	serviceID := uuid.Must(uuid.NewV7())
	groupID := "runtime-" + shortUUID(workerID)
	mustExec(t, ctx, pool, `
		INSERT INTO worker_groups (id, region_id, name, enrollment_policy_fingerprint, allowed_attestation_fingerprints)
		VALUES ($1, $2, $1, 'sha256:test-enrollment-policy', ARRAY['sha256:test-attestation'])
	`, groupID, dbtest.DefaultRegionID)
	mustExec(t, ctx, pool, `
		INSERT INTO worker_instances (
			id, resource_id, worker_group_id, attestation_fingerprint, state,
			current_epoch, current_service_id, epoch_started_at
		) VALUES ($1, $2, $3, 'sha256:test-attestation', 'registering', 1, $4, now())
	`, workerID, workerID.String(), groupID, serviceID)

	runtimeID := uuid.Must(uuid.NewV7())
	mustExec(t, ctx, pool, `
		INSERT INTO runtime_instances (
			id, org_id, worker_group_id, project_id, environment_id, region_id,
			worker_instance_id, runtime_identity_id, deployment_sandbox_id, worker_epoch,
			runtime_key_hash, runtime_key, sandbox_fingerprint, rootfs_digest,
			image_digest, image_format, sandbox_image_artifact_id,
			sandbox_image_artifact_digest, sandbox_image_artifact_format,
			runtime_abi, guestd_abi, adapter_abi, network_policy,
			reserved_cpu_millis, reserved_memory_bytes, reserved_workload_disk_bytes,
			reserved_scratch_bytes, reserved_execution_slots, desired_reason, allocated_at
		)
		SELECT $1, sandboxes.org_id, $2, sandboxes.project_id, sandboxes.environment_id, $3,
		       $4, 'test-runtime', sandboxes.id, 1,
		       'runtime-without-substrate', '{}', sandboxes.fingerprint, sandboxes.rootfs_digest,
		       sandboxes.image_digest, sandboxes.image_format, sandboxes.image_artifact_id,
		       artifacts.digest, sandboxes.image_artifact_format,
		       sandboxes.runtime_abi, sandboxes.guestd_abi, sandboxes.adapter_abi, '{}',
		       1000, 1073741824, 1073741824, 0, 1, 'reconcile-test', now()
		  FROM deployment_sandboxes AS sandboxes
		  JOIN artifacts ON artifacts.org_id = sandboxes.org_id
		                AND artifacts.project_id = sandboxes.project_id
		                AND artifacts.environment_id = sandboxes.environment_id
		                AND artifacts.id = sandboxes.image_artifact_id
		 WHERE sandboxes.id = $5
	`, runtimeID, groupID, dbtest.DefaultRegionID, workerID, ids.deploymentSandboxID)
	mustExec(t, ctx, pool, `
		INSERT INTO worker_network_slots (
			id, worker_group_id, worker_instance_id, worker_epoch, slot_name,
			generation, state, runtime_instance_id, assigned_at
		) VALUES ($1, $2, $3, 1, 'vm-0001', 1, 'assigned', $4, now())
	`, uuid.Must(uuid.NewV7()), groupID, workerID, runtimeID)
	mustExec(t, ctx, pool, `
		UPDATE worker_instances
		   SET state = 'active', supports_run = true, runtime_identity_id = 'test-runtime',
		       certified_cpu_millis = 4000, certified_memory_bytes = 8589934592,
		       certified_workload_disk_bytes = 10737418240, certified_scratch_bytes = 10737418240,
		       per_vm_cpu_millis = 2000, per_vm_memory_bytes = 2147483648,
		       per_vm_workload_disk_bytes = 4294967296, per_vm_scratch_bytes = 4294967296,
		       max_vm_slots = 2, max_run_consumers = 2, max_runtime_starts = 2,
		       certification_profile = 'test', certification_fingerprint = 'test-fingerprint',
		       certified_at = now(), activated_at = now()
		 WHERE id = $1
	`, workerID)

	target, err := queries.GetNextRuntimeReconcileTarget(ctx, db.GetNextRuntimeReconcileTargetParams{
		WorkerGroupID:    groupID,
		WorkerInstanceID: pgvalue.UUID(workerID),
		WorkerEpoch:      1,
	})
	if err != nil {
		t.Fatal(err)
	}
	if target.RuntimeSubstrateID.Valid {
		t.Fatalf("runtime substrate ID = %s, want NULL", target.RuntimeSubstrateID.String())
	}
	if target.RuntimeSubstrateBlobDigest != "" || target.RuntimeSubstrateBlobSizeBytes != 0 || target.RuntimeSubstrateBlobMediaType != "" {
		t.Fatalf("runtime substrate artifact = (%q, %d, %q), want empty values",
			target.RuntimeSubstrateBlobDigest,
			target.RuntimeSubstrateBlobSizeBytes,
			target.RuntimeSubstrateBlobMediaType,
		)
	}
	if target.WorkspaceMountPath != "/workspace" {
		t.Fatalf("workspace mount path = %q, want /workspace", target.WorkspaceMountPath)
	}

	if _, err := queries.DrainWorkerInstance(ctx, db.DrainWorkerInstanceParams{
		ID: pgvalue.UUID(workerID), WorkerGroupID: groupID,
		ExpectedEpoch: pgtype.Int8{Int64: 1, Valid: true},
	}); err != nil {
		t.Fatal(err)
	}
	drainingTarget, err := queries.GetNextRuntimeReconcileTarget(ctx, db.GetNextRuntimeReconcileTargetParams{
		WorkerGroupID: groupID, WorkerInstanceID: pgvalue.UUID(workerID), WorkerEpoch: 1,
	})
	if err != nil {
		t.Fatal(err)
	}
	if drainingTarget.ID != pgvalue.UUID(runtimeID) || drainingTarget.DesiredState != db.RuntimeDesiredStateClosed {
		t.Fatalf("draining reconcile target = %v/%s, want idle runtime %s/closed", drainingTarget.ID, drainingTarget.DesiredState, runtimeID)
	}
	cleanupProof := []byte(`{"method":"host_reconciled","completed_at":"2026-07-13T13:33:14Z"}`)
	if _, err := queries.MarkRuntimeInstanceClosed(ctx, db.MarkRuntimeInstanceClosedParams{
		ReasonCode:              pgtype.Text{String: "desired_state_reconciled", Valid: true},
		ID:                      drainingTarget.ID,
		WorkerInstanceID:        pgvalue.UUID(workerID),
		WorkerEpoch:             drainingTarget.WorkerEpoch,
		DesiredVersion:          drainingTarget.DesiredVersion,
		NetworkSlotID:           drainingTarget.NetworkSlotID,
		NetworkSlotGeneration:   drainingTarget.NetworkSlotGeneration,
		ExpectedObservedVersion: drainingTarget.ObservedVersion,
		CleanupProof:            cleanupProof,
	}); err != nil {
		t.Fatal(err)
	}
	var reclaimEvidence []byte
	if err := pool.QueryRow(ctx, `SELECT reclaim_evidence FROM worker_network_slots WHERE id = $1`, drainingTarget.NetworkSlotID).Scan(&reclaimEvidence); err != nil {
		t.Fatal(err)
	}
	var proof map[string]any
	if err := json.Unmarshal(reclaimEvidence, &proof); err != nil {
		t.Fatal(err)
	}
	if proof["method"] != "host_reconciled" || proof["completed_at"] != "2026-07-13T13:33:14Z" {
		t.Fatalf("reclaim evidence = %s, want exact cleanup proof", reclaimEvidence)
	}
}
