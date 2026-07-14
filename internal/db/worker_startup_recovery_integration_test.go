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

func TestRecordWorkerStartupRecoveryTerminalizesEveryOldEpochSlotIdempotently(t *testing.T) {
	ctx := context.Background()
	pool := newIntegrationDB(t, ctx)
	ids := seedIntegration(t, ctx, pool)
	queries := db.New(pool)

	workerID := uuid.Must(uuid.NewV7())
	serviceID := uuid.Must(uuid.NewV7())
	groupID := "recovery-" + shortUUID(workerID)
	mustExec(t, ctx, pool, `
		INSERT INTO worker_groups (id, region_id, name, enrollment_policy_fingerprint, allowed_attestation_fingerprints)
		VALUES ($1, $2, $1, 'sha256:test-enrollment-policy', ARRAY['sha256:test-attestation'])
	`, groupID, dbtest.DefaultRegionID)
	mustExec(t, ctx, pool, `
		INSERT INTO worker_instances (
			id, resource_id, worker_group_id, attestation_fingerprint, state,
			current_epoch, current_service_id, epoch_started_at
		) VALUES ($1, $2, $3, 'sha256:test-attestation', 'registering', 2, $4, now())
	`, workerID, workerID.String(), groupID, serviceID)

	quarantinedRuntimeID := uuid.Must(uuid.NewV7())
	mustExec(t, ctx, pool, `
		INSERT INTO runtime_instances (
			id, org_id, worker_group_id, project_id, environment_id, region_id,
			worker_instance_id, runtime_identity_id, deployment_sandbox_id, worker_epoch,
			runtime_key_hash, runtime_key, sandbox_fingerprint, rootfs_digest,
			image_digest, image_format, runtime_abi, guestd_abi, adapter_abi,
			network_policy, reserved_cpu_millis, reserved_memory_bytes,
			reserved_workload_disk_bytes, reserved_scratch_bytes,
			reserved_execution_slots, desired_reason
		) VALUES (
			$1, $2, $3, $4, $5, $6, $7, 'test-runtime', $8, 1,
			'recovery-runtime', '{}', 'sandbox-fingerprint', 'sha256:rootfs',
			'sha256:image', 'oci-tar', 'test', 'guestd-test', 'adapter-test',
			'{}', 1000, 1073741824, 1073741824, 0, 1, 'startup-recovery-test'
		)
	`, quarantinedRuntimeID, ids.orgID, groupID, ids.projectID, ids.environmentID,
		dbtest.DefaultRegionID, workerID, ids.deploymentSandboxID)

	quarantinedSlotID := uuid.Must(uuid.NewV7())
	unlinkedSlotID := uuid.Must(uuid.NewV7())
	alreadyTerminalSlotID := uuid.Must(uuid.NewV7())
	currentSlotID := uuid.Must(uuid.NewV7())
	mustExec(t, ctx, pool, `
		INSERT INTO worker_network_slots (
			id, worker_group_id, worker_instance_id, worker_epoch, slot_name,
			generation, state, runtime_instance_id, assigned_at
		) VALUES ($1, $2, $3, 1, 'vm-0001', 4, 'assigned', $4, now())
	`, quarantinedSlotID, groupID, workerID, quarantinedRuntimeID)
	mustExec(t, ctx, pool, `
		INSERT INTO worker_network_slots (
			id, worker_group_id, worker_instance_id, worker_epoch, slot_name,
			generation, state
		) VALUES ($1, $2, $3, 1, 'vm-0002', 1, 'available')
	`, unlinkedSlotID, groupID, workerID)
	mustExec(t, ctx, pool, `
		INSERT INTO worker_network_slots (
			id, worker_group_id, worker_instance_id, worker_epoch, slot_name,
			generation, state, lost_at, reclaimed_at, reclaim_evidence, state_reason_code
		) VALUES ($1, $2, $3, 1, 'vm-0003', 6, 'lost', now(), now(),
			'{"reason":"already_terminal"}', 'already_terminal')
	`, alreadyTerminalSlotID, groupID, workerID)
	mustExec(t, ctx, pool, `
		INSERT INTO worker_network_slots (
			id, worker_group_id, worker_instance_id, worker_epoch, slot_name,
			generation, state
		) VALUES ($1, $2, $3, 2, 'vm-0001', 1, 'available')
	`, currentSlotID, groupID, workerID)

	evidence, err := json.Marshal(map[string]any{
		"quarantined": []string{quarantinedRuntimeID.String()},
		"observed_at": "2026-07-12T00:00:00Z",
	})
	if err != nil {
		t.Fatal(err)
	}
	params := db.RecordWorkerStartupRecoveryParams{
		RecoveryEvidence: evidence, WorkerInstanceID: pgvalue.UUID(workerID),
		WorkerGroupID: groupID, WorkerEpoch: pgtype.Int8{Int64: 2, Valid: true},
	}
	if _, err := queries.RecordWorkerStartupRecovery(ctx, params); err != nil {
		t.Fatal(err)
	}

	type slotState struct {
		state       string
		generation  int64
		runtimeID   pgtype.UUID
		reclaimedAt pgtype.Timestamptz
		evidence    []byte
	}
	readSlot := func(id uuid.UUID) slotState {
		t.Helper()
		var got slotState
		if err := pool.QueryRow(ctx, `
			SELECT state::text, generation, runtime_instance_id, reclaimed_at, reclaim_evidence
			  FROM worker_network_slots WHERE id = $1
		`, id).Scan(&got.state, &got.generation, &got.runtimeID, &got.reclaimedAt, &got.evidence); err != nil {
			t.Fatal(err)
		}
		return got
	}

	quarantined := readSlot(quarantinedSlotID)
	if quarantined.state != "quarantined" || quarantined.generation != 4 || !quarantined.runtimeID.Valid || quarantined.reclaimedAt.Valid {
		t.Fatalf("quarantined linked slot = %+v", quarantined)
	}
	unlinked := readSlot(unlinkedSlotID)
	if unlinked.state != "lost" || unlinked.generation != 2 || unlinked.runtimeID.Valid || !unlinked.reclaimedAt.Valid || len(unlinked.evidence) == 0 {
		t.Fatalf("terminalized unlinked slot = %+v", unlinked)
	}
	alreadyTerminal := readSlot(alreadyTerminalSlotID)
	if alreadyTerminal.state != "lost" || alreadyTerminal.generation != 6 {
		t.Fatalf("already-terminal slot changed = %+v", alreadyTerminal)
	}
	current := readSlot(currentSlotID)
	if current.state != "available" || current.generation != 1 || current.reclaimedAt.Valid {
		t.Fatalf("current-epoch slot changed = %+v", current)
	}

	if _, err := queries.RecordWorkerStartupRecovery(ctx, params); err != nil {
		t.Fatal(err)
	}
	if repeated := readSlot(unlinkedSlotID); repeated.generation != unlinked.generation || repeated.state != "lost" {
		t.Fatalf("repeated recovery was not idempotent: before=%+v after=%+v", unlinked, repeated)
	}
}
