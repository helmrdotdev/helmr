package db_test

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/helmrdotdev/helmr/internal/db"
	"github.com/helmrdotdev/helmr/internal/db/dbtest"
	"github.com/helmrdotdev/helmr/internal/pgvalue"
	"github.com/helmrdotdev/helmr/internal/publicid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
)

func TestCreateScopedRunFreezesCertifiedRegionalRuntime(t *testing.T) {
	ctx := context.Background()
	pool := newIntegrationDB(t, ctx)
	ids := seedIntegration(t, ctx, pool)
	queries := db.New(pool)

	groupID := "run-" + shortUUID(ids.runID)
	workerID := uuid.Must(uuid.NewV7())
	serviceID := uuid.Must(uuid.NewV7())
	mustExec(t, ctx, pool, `
		INSERT INTO worker_groups (id, region_id, name, enrollment_policy_fingerprint, allowed_attestation_fingerprints)
		VALUES ($1, $2, $1, 'sha256:test-enrollment-policy', ARRAY['sha256:test-attestation'])
	`, groupID, dbtest.DefaultRegionID)
	mustExec(t, ctx, pool, `
		INSERT INTO worker_instances (
			id, resource_id, worker_group_id, attestation_fingerprint, state,
			current_epoch, current_service_id, protocol_version,
			supports_run, runtime_identity_id,
			certified_cpu_millis, certified_memory_bytes,
			certified_workload_disk_bytes, certified_scratch_bytes,
			per_vm_cpu_millis, per_vm_memory_bytes,
			per_vm_workload_disk_bytes, per_vm_scratch_bytes,
			max_vm_slots, max_run_consumers, max_runtime_starts,
			certification_profile, certification_fingerprint,
			epoch_started_at, certified_at, activated_at
		) VALUES (
			$1, $2, $3, 'sha256:test-attestation', 'active', 1, $4, 'helmr.worker.v0',
			true, 'test-runtime', 4000, 8589934592, 10737418240, 10737418240,
			2000, 2147483648, 4294967296, 4294967296,
			2, 2, 2, 'test', 'test-fingerprint', now(), now(), now()
		)
	`, workerID, workerID.String(), groupID, serviceID)

	var sessionID pgtype.UUID
	if err := pool.QueryRow(ctx, `
		SELECT id FROM sessions
		 WHERE org_id = $1 AND workspace_id = $2
	`, ids.orgID, ids.workspaceID).Scan(&sessionID); err != nil {
		t.Fatal(err)
	}

	now := time.Now().UTC()
	runID := uuid.Must(uuid.NewV7())
	created, err := queries.CreateScopedRun(ctx, db.CreateScopedRunParams{
		OrgID:               pgvalue.UUID(ids.orgID),
		ProjectID:           pgvalue.UUID(ids.projectID),
		EnvironmentID:       pgvalue.UUID(ids.environmentID),
		SessionID:           sessionID,
		WorkspaceID:         pgvalue.UUID(ids.workspaceID),
		DeploymentID:        pgvalue.UUID(ids.deploymentID),
		DeploymentTaskID:    pgvalue.UUID(ids.taskID),
		TaskID:              "approval-task",
		ID:                  pgvalue.UUID(runID),
		PublicID:            testPublicID(t, publicid.Run),
		DeploymentVersion:   "v1",
		ApiVersion:          "2026-06-06",
		SdkVersion:          "test",
		CliVersion:          "test",
		Payload:             []byte(`{}`),
		Metadata:            []byte(`{}`),
		Tags:                []string{},
		LockedRetryPolicy:   []byte(`{"enabled":false}`),
		QueueName:           "default",
		Priority:            0,
		QueueTimestamp:      pgvalue.Timestamptz(now),
		Ttl:                 "",
		MaxActiveDurationMs: 300000,
		TraceID:             pgtype.Text{String: "11111111111111111111111111111111", Valid: true},
		RootSpanID:          "2222222222222222",
		ScheduleGeneration:  0,
		EventPayload:        []byte(`{"kind":"run.created"}`),
	})
	if err != nil {
		t.Fatal(err)
	}
	if created.RuntimeIdentityID != "test-runtime" || created.RuntimeABI != "test" || created.RuntimeArch != "arm64" {
		t.Fatalf("runtime target = %s/%s/%s", created.RuntimeIdentityID, created.RuntimeABI, created.RuntimeArch)
	}
}

func TestCreateScopedRunRejectsWorkerRuntimeWithDifferentRootfs(t *testing.T) {
	ctx := context.Background()
	pool := newIntegrationDB(t, ctx)
	ids := seedIntegration(t, ctx, pool)
	queries := db.New(pool)

	groupID := "run-incompatible-" + shortUUID(ids.runID)
	workerID := uuid.Must(uuid.NewV7())
	serviceID := uuid.Must(uuid.NewV7())
	mustExec(t, ctx, pool, `
		INSERT INTO runtime_identities (
			id, runtime_arch, runtime_abi, kernel_digest, initramfs_digest, rootfs_digest, cni_profile
		) VALUES (
			'incompatible-runtime', 'arm64', 'test', 'sha256:kernel',
			'sha256:initramfs', 'sha256:different-rootfs', 'default'
		)
	`)
	mustExec(t, ctx, pool, `
		INSERT INTO worker_groups (id, region_id, name, enrollment_policy_fingerprint, allowed_attestation_fingerprints)
		VALUES ($1, $2, $1, 'sha256:test-enrollment-policy', ARRAY['sha256:test-attestation'])
	`, groupID, dbtest.DefaultRegionID)
	mustExec(t, ctx, pool, `
		INSERT INTO worker_instances (
			id, resource_id, worker_group_id, attestation_fingerprint, state,
			current_epoch, current_service_id, protocol_version,
			supports_run, runtime_identity_id,
			certified_cpu_millis, certified_memory_bytes,
			certified_workload_disk_bytes, certified_scratch_bytes,
			per_vm_cpu_millis, per_vm_memory_bytes,
			per_vm_workload_disk_bytes, per_vm_scratch_bytes,
			max_vm_slots, max_run_consumers, max_runtime_starts,
			certification_profile, certification_fingerprint,
			epoch_started_at, certified_at, activated_at
		) VALUES (
			$1, $2, $3, 'sha256:test-attestation', 'active', 1, $4, 'helmr.worker.v0',
			true, 'incompatible-runtime', 4000, 8589934592, 10737418240, 10737418240,
			2000, 2147483648, 4294967296, 4294967296,
			2, 2, 2, 'test', 'test-fingerprint', now(), now(), now()
		)
	`, workerID, workerID.String(), groupID, serviceID)

	var sessionID pgtype.UUID
	if err := pool.QueryRow(ctx, `
		SELECT id FROM sessions
		 WHERE org_id = $1 AND workspace_id = $2
	`, ids.orgID, ids.workspaceID).Scan(&sessionID); err != nil {
		t.Fatal(err)
	}

	now := time.Now().UTC()
	_, err := queries.CreateScopedRun(ctx, db.CreateScopedRunParams{
		OrgID:               pgvalue.UUID(ids.orgID),
		ProjectID:           pgvalue.UUID(ids.projectID),
		EnvironmentID:       pgvalue.UUID(ids.environmentID),
		SessionID:           sessionID,
		WorkspaceID:         pgvalue.UUID(ids.workspaceID),
		DeploymentID:        pgvalue.UUID(ids.deploymentID),
		DeploymentTaskID:    pgvalue.UUID(ids.taskID),
		TaskID:              "approval-task",
		ID:                  pgvalue.UUID(uuid.Must(uuid.NewV7())),
		PublicID:            testPublicID(t, publicid.Run),
		DeploymentVersion:   "v1",
		ApiVersion:          "2026-06-06",
		SdkVersion:          "test",
		CliVersion:          "test",
		Payload:             []byte(`{}`),
		Metadata:            []byte(`{}`),
		Tags:                []string{},
		LockedRetryPolicy:   []byte(`{"enabled":false}`),
		QueueName:           "default",
		Priority:            0,
		QueueTimestamp:      pgvalue.Timestamptz(now),
		Ttl:                 "",
		MaxActiveDurationMs: 300000,
		TraceID:             pgtype.Text{String: "11111111111111111111111111111111", Valid: true},
		RootSpanID:          "2222222222222222",
		ScheduleGeneration:  0,
		EventPayload:        []byte(`{"kind":"run.created"}`),
	})
	if !errors.Is(err, pgx.ErrNoRows) {
		t.Fatalf("CreateScopedRun error = %v, want pgx.ErrNoRows", err)
	}
}

func TestCancelQueuedRunAppliesOperation(t *testing.T) {
	ctx := context.Background()
	pool := newIntegrationDB(t, ctx)
	ids := seedIntegration(t, ctx, pool)
	queries := db.New(pool)

	operationID := uuid.Must(uuid.NewV7())
	operation, err := queries.CreateRunOperation(ctx, db.CreateRunOperationParams{
		ID:             pgvalue.UUID(operationID),
		PublicID:       testPublicID(t, publicid.RunOperation),
		Kind:           db.RunOperationKindCancel,
		ActorKind:      "user",
		ActorID:        "integration-test",
		Reason:         "test cancellation",
		Request:        []byte(`{"reason":"test cancellation"}`),
		IdempotencyKey: "cancel-queued-run",
		OrgID:          pgvalue.UUID(ids.orgID),
		RunID:          pgvalue.UUID(ids.runID),
	})
	if err != nil {
		t.Fatal(err)
	}

	cancelled, err := queries.CancelRun(ctx, db.CancelRunParams{
		OperationID: operation.ID,
		OrgID:       pgvalue.UUID(ids.orgID),
		RunID:       pgvalue.UUID(ids.runID),
	})
	if err != nil {
		t.Fatal(err)
	}
	if cancelled.Status != db.RunStatusCancelled || cancelled.ExecutionStatus != db.RunExecutionStatusFinished {
		t.Fatalf("cancelled run state = %s/%s", cancelled.Status, cancelled.ExecutionStatus)
	}

	applied, err := queries.GetRunOperation(ctx, db.GetRunOperationParams{
		OrgID: pgvalue.UUID(ids.orgID),
		RunID: pgvalue.UUID(ids.runID),
		ID:    operation.ID,
	})
	if err != nil {
		t.Fatal(err)
	}
	if applied.Status != db.RunOperationStatusApplied {
		t.Fatalf("cancel operation status = %s, want applied", applied.Status)
	}
}

func TestCreateRunOperationReturnsExistingIdempotentOperation(t *testing.T) {
	ctx := context.Background()
	pool := newIntegrationDB(t, ctx)
	ids := seedIntegration(t, ctx, pool)
	queries := db.New(pool)

	params := db.CreateRunOperationParams{
		ID:             pgvalue.UUID(uuid.Must(uuid.NewV7())),
		PublicID:       testPublicID(t, publicid.RunOperation),
		Kind:           db.RunOperationKindCancel,
		ActorKind:      "user",
		ActorID:        "integration-test",
		Reason:         "first request",
		Request:        []byte(`{"reason":"first request"}`),
		IdempotencyKey: "same-operation",
		OrgID:          pgvalue.UUID(ids.orgID),
		RunID:          pgvalue.UUID(ids.runID),
	}
	first, err := queries.CreateRunOperation(ctx, params)
	if err != nil {
		t.Fatal(err)
	}
	params.ID = pgvalue.UUID(uuid.Must(uuid.NewV7()))
	params.PublicID = testPublicID(t, publicid.RunOperation)
	params.Reason = "different request"
	params.Request = []byte(`{"reason":"different request"}`)
	second, err := queries.CreateRunOperation(ctx, params)
	if err != nil {
		t.Fatal(err)
	}
	if second.ID != first.ID || string(second.Request) != string(first.Request) {
		t.Fatalf("idempotent operation = %s %s, want existing %s %s", second.ID, second.Request, first.ID, first.Request)
	}
}
