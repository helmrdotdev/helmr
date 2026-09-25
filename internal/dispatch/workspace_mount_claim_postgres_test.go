package dispatch

import (
	"encoding/hex"
	"errors"
	"sync"
	"testing"
	"time"
	"uuid"

	"github.com/helmrdotdev/helmr/internal/db"
	"github.com/helmrdotdev/helmr/internal/db/dbtest"
	"github.com/helmrdotdev/helmr/internal/pgvalue"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
)

func TestWorkspaceMountClaimIsExclusive(t *testing.T) {
	t.Run("sequential delivery", func(t *testing.T) {
		fixture, mountID := prepareClaimableRunMount(t)
		queries := db.New(fixture.pool)
		first, err := queries.ClaimWorkspaceMount(
			fixture.ctx,
			claimWorkspaceMountParams(fixture, "first"),
		)
		if err != nil {
			t.Fatal(err)
		}
		if first.ID != mountID {
			t.Fatalf("claimed mount = %s, want %s", pgvalue.UUIDString(first.ID), pgvalue.UUIDString(mountID))
		}
		if _, err := queries.ClaimWorkspaceMount(
			fixture.ctx,
			claimWorkspaceMountParams(fixture, "second"),
		); !errors.Is(err, pgx.ErrNoRows) {
			t.Fatalf("second claim error = %v, want pgx.ErrNoRows", err)
		}
	})

	t.Run("concurrent delivery", func(t *testing.T) {
		fixture, mountID := prepareClaimableRunMount(t)
		start := make(chan struct{})
		results := make(chan error, 2)
		var wg sync.WaitGroup
		for _, token := range []string{"first", "second"} {
			token := token
			wg.Go(func() {
				<-start
				row, err := db.New(fixture.pool).ClaimWorkspaceMount(
					fixture.ctx,
					claimWorkspaceMountParams(fixture, token),
				)
				if err == nil && row.ID != mountID {
					err = errors.New("claimed an unexpected mount")
				}
				results <- err
			})
		}
		close(start)
		wg.Wait()
		close(results)
		claimed := 0
		rejected := 0
		for err := range results {
			switch {
			case err == nil:
				claimed++
			case errors.Is(err, pgx.ErrNoRows):
				rejected++
			default:
				t.Fatal(err)
			}
		}
		if claimed != 1 || rejected != 1 {
			t.Fatalf("claim results = %d claimed, %d rejected; want 1 and 1", claimed, rejected)
		}
	})
}

func TestWorkspaceMountClaimSkipsSpentMountForOtherWorkspace(t *testing.T) {
	fixture, firstMountID := prepareClaimableRunMount(t)
	secondMountID := cloneClaimableRunMount(t, fixture, firstMountID)
	queries := db.New(fixture.pool)
	first, err := queries.ClaimWorkspaceMount(
		fixture.ctx,
		claimWorkspaceMountParams(fixture, "first-workspace"),
	)
	if err != nil {
		t.Fatal(err)
	}
	if first.ID != firstMountID {
		t.Fatalf("first claim = %s, want %s", pgvalue.UUIDString(first.ID), pgvalue.UUIDString(firstMountID))
	}
	second, err := queries.ClaimWorkspaceMount(
		fixture.ctx,
		claimWorkspaceMountParams(fixture, "second-workspace"),
	)
	if err != nil {
		t.Fatal(err)
	}
	if second.ID != secondMountID {
		t.Fatalf("second claim = %s, want %s", pgvalue.UUIDString(second.ID), pgvalue.UUIDString(secondMountID))
	}
	var firstHash string
	if err := fixture.pool.QueryRow(fixture.ctx, `
SELECT guest_channel_token_hash FROM workspace_mounts WHERE id = $1`, firstMountID).Scan(&firstHash); err != nil {
		t.Fatal(err)
	}
	if firstHash != first.GuestChannelTokenHash {
		t.Fatalf("first claim hash changed from %q to %q", first.GuestChannelTokenHash, firstHash)
	}
}

func TestWorkspaceMountClaimCredentialPairIsConsistent(t *testing.T) {
	fixture, mountID := prepareClaimableRunMount(t)
	_, err := fixture.pool.Exec(fixture.ctx, `
UPDATE workspace_mounts
   SET guest_channel_token_expires_at = transaction_timestamp() + interval '5 minutes'
 WHERE id = $1`, mountID)
	if err == nil {
		t.Fatal("expiry without a token hash satisfied the workspace mount constraint")
	}
}

func TestExpiredWorkspaceMountClaimRecoversRunWithFreshAuthority(t *testing.T) {
	fixture, mountID := prepareClaimableRunMount(t)
	queries := db.New(fixture.pool)
	claimed, err := queries.ClaimWorkspaceMount(
		fixture.ctx,
		claimWorkspaceMountParams(fixture, "run"),
	)
	if err != nil {
		t.Fatal(err)
	}
	future := pgvalue.TimestamptzUTCZeroInvalid(time.Now().UTC().Add(10 * time.Minute))
	if _, err := queries.RenewWorkspaceMount(fixture.ctx, db.RenewWorkspaceMountParams{
		GuestChannelTokenExpiresAt: future,
		OrgID:                      claimed.OrgID,
		ID:                         claimed.ID,
		WorkerInstanceID:           claimed.WorkerInstanceID,
		WorkerEpoch:                claimed.WorkerEpoch,
		RuntimeInstanceID:          claimed.RuntimeInstanceID,
	}); err != nil {
		t.Fatal(err)
	}
	if rows, err := queries.LoseExpiredWorkspaceMountClaims(fixture.ctx, 8); err != nil {
		t.Fatal(err)
	} else if len(rows) != 0 {
		t.Fatalf("renewing claim was lost: %+v", rows)
	}

	dbtest.MustExec(t, fixture.ctx, fixture.pool, `
UPDATE workspace_mounts
   SET guest_channel_token_expires_at = transaction_timestamp() - interval '1 second'
 WHERE id = $1`, mountID)
	lost, err := queries.LoseExpiredWorkspaceMountClaims(fixture.ctx, 8)
	if err != nil {
		t.Fatal(err)
	}
	if len(lost) != 1 || lost[0].ID != mountID || lost[0].Status != "lost" {
		t.Fatalf("lost claims = %+v, want mount %s", lost, pgvalue.UUIDString(mountID))
	}
	var mountStatus, mountReason, desiredState, desiredReason string
	if err := fixture.pool.QueryRow(fixture.ctx, `
SELECT workspace_mounts.status, workspace_mounts.terminal_reason_code,
       runtime_instances.desired_state, runtime_instances.desired_reason
  FROM workspace_mounts
  JOIN runtime_instances ON runtime_instances.id = workspace_mounts.runtime_instance_id
 WHERE workspace_mounts.id = $1`, mountID).Scan(
		&mountStatus,
		&mountReason,
		&desiredState,
		&desiredReason,
	); err != nil {
		t.Fatal(err)
	}
	if mountStatus != "lost" || mountReason != "workspace_mount_claim_expired" ||
		desiredState != "closed" || desiredReason != "workspace_mount_claim_expired" {
		t.Fatalf("recovery state = %s/%s runtime=%s/%s", mountStatus, mountReason, desiredState, desiredReason)
	}

	markExpiredClaimRuntimeReclaimed(t, fixture, claimed.RuntimeInstanceID)
	fresh, err := fixture.authority.PlaceReadyRun(fixture.ctx, fixture.candidate())
	if err != nil {
		t.Fatal(err)
	}
	if !fresh.RuntimeInstanceID.Valid || fresh.RuntimeInstanceID == claimed.RuntimeInstanceID {
		t.Fatalf("fresh placement = %+v, expired runtime = %s", fresh, pgvalue.UUIDString(claimed.RuntimeInstanceID))
	}
}

func TestExpiredWorkspaceMountClaimRecoversProcessWithFreshAuthority(t *testing.T) {
	fixture, processID, mountID := prepareClaimableWorkspaceExecMount(t)
	queries := db.New(fixture.pool)
	claimed, err := queries.ClaimWorkspaceMount(
		fixture.ctx,
		claimWorkspaceMountParams(fixture, "process"),
	)
	if err != nil {
		t.Fatal(err)
	}
	dbtest.MustExec(t, fixture.ctx, fixture.pool, `
UPDATE workspace_mounts
   SET guest_channel_token_expires_at = transaction_timestamp() - interval '1 second'
 WHERE id = $1`, mountID)
	lost, err := queries.LoseExpiredWorkspaceMountClaims(fixture.ctx, 8)
	if err != nil {
		t.Fatal(err)
	}
	if len(lost) != 1 || lost[0].ID != mountID {
		t.Fatalf("lost claims = %+v, want mount %s", lost, pgvalue.UUIDString(mountID))
	}
	markExpiredClaimRuntimeReclaimed(t, fixture, claimed.RuntimeInstanceID)
	fresh, err := fixture.authority.PlaceWorkspaceExec(
		fixture.ctx,
		ReadyWorkspaceExecCandidate{
			OrgID:            pgvalue.UUID(fixture.orgID),
			ProcessID:        pgvalue.UUID(processID),
			ExpectedRevision: 1,
		},
	)
	if err != nil {
		t.Fatal(err)
	}
	if !fresh.RuntimeInstanceID.Valid || fresh.RuntimeInstanceID == claimed.RuntimeInstanceID {
		t.Fatalf("fresh placement = %+v, expired runtime = %s", fresh, pgvalue.UUIDString(claimed.RuntimeInstanceID))
	}
}

func prepareClaimableRunMount(t *testing.T) (runPlacementFixture, pgtype.UUID) {
	t.Helper()
	fixture := newRunPlacementFixture(t)
	reserved, err := fixture.authority.PlaceReadyRun(fixture.ctx, fixture.candidate())
	if err != nil {
		t.Fatal(err)
	}
	markRunPlacementRuntimeReady(t, fixture, reserved.RuntimeInstanceID)
	mounting, err := fixture.authority.PlaceReadyRun(fixture.ctx, fixture.candidate())
	if err != nil {
		t.Fatal(err)
	}
	if !mounting.WorkspaceMountID.Valid {
		t.Fatalf("mount placement = %+v", mounting)
	}
	return fixture, mounting.WorkspaceMountID
}

func prepareClaimableWorkspaceExecMount(t *testing.T) (runPlacementFixture, uuid.UUID, pgtype.UUID) {
	t.Helper()
	fixture := newRunPlacementFixture(t)
	claimID := uuid.NewV7()
	processID := uuid.NewV7()
	dbtest.MustExec(t, fixture.ctx, fixture.pool, `
UPDATE computers SET owner_run_id = NULL WHERE id = $1`, fixture.workspaceID)
	dbtest.MustExec(t, fixture.ctx, fixture.pool, `
INSERT INTO idempotency_claims (
    id, environment_id, operation, slot_hash, request_fingerprint, accepted_at, expires_at
) VALUES (
    $1, $2, 'workspace.exec', decode(repeat('61', 32), 'hex'),
    decode(repeat('62', 32), 'hex'), transaction_timestamp(), transaction_timestamp() + interval '30 days'
)`, claimID, fixture.environmentID)
	if _, err := db.New(fixture.pool).CreateWorkspaceExec(
		fixture.ctx,
		db.CreateWorkspaceExecParams{
			ID:                     pgvalue.UUID(processID),
			OrgID:                  pgvalue.UUID(fixture.orgID),
			ProjectID:              pgvalue.UUID(fixture.projectID),
			EnvironmentID:          pgvalue.UUID(fixture.environmentID),
			WorkspaceID:            pgvalue.UUID(fixture.workspaceID),
			BaseWorkspaceVersionID: workspaceHeadVersion(t, fixture),
			RestoreDesiredState:    "active",
			Request:                []byte(`{"command":["echo","ready"]}`),
			Stdin:                  []byte{},
			ClaimID:                pgvalue.UUID(claimID),
			CreatedBySubjectType:   "user",
			CreatedBySubjectID:     "test-user",
		},
	); err != nil {
		t.Fatal(err)
	}
	candidate := ReadyWorkspaceExecCandidate{
		OrgID:            pgvalue.UUID(fixture.orgID),
		ProcessID:        pgvalue.UUID(processID),
		ExpectedRevision: 1,
	}
	reserved, err := fixture.authority.PlaceWorkspaceExec(fixture.ctx, candidate)
	if err != nil {
		t.Fatal(err)
	}
	markRunPlacementRuntimeReady(t, fixture, reserved.RuntimeInstanceID)
	mounting, err := fixture.authority.PlaceWorkspaceExec(fixture.ctx, candidate)
	if err != nil {
		t.Fatal(err)
	}
	if !mounting.WorkspaceMountID.Valid {
		t.Fatalf("mount placement = %+v", mounting)
	}
	return fixture, processID, mounting.WorkspaceMountID
}

func cloneClaimableRunMount(t *testing.T, fixture runPlacementFixture, sourceMountID pgtype.UUID) pgtype.UUID {
	t.Helper()
	workspaceID := uuid.NewV7()
	versionID := uuid.NewV7()
	runID := uuid.NewV7()
	runtimeID := uuid.NewV7()
	mountID := uuid.NewV7()
	tx, err := fixture.pool.Begin(fixture.ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = tx.Rollback(fixture.ctx) }()
	dbtest.MustExec(t, fixture.ctx, tx, `SET CONSTRAINTS ALL DEFERRED`)
	dbtest.MustExec(t, fixture.ctx, tx, `
WITH cloned AS (
SELECT (jsonb_populate_record(
    NULL::computers,
    to_jsonb(source_workspace) || jsonb_build_object(
        'id', $2::text,
        'owner_run_id', $3::text,
        'head_version_id', $4::text,
        'created_at', transaction_timestamp(),
        'updated_at', transaction_timestamp()
    )
)) AS row
  FROM workspace_mounts source_mount
  JOIN computers source_workspace ON source_workspace.id = source_mount.workspace_id
 WHERE source_mount.id = $1
)
INSERT INTO computers (
    id, environment_id, region_id, sandbox_declared_id,
    deployment_definition_id, key, revision, owner_session_id,
    owner_run_id, ownership_generation, writer_generation, head_version_id,
    initial_config, write_key_id, status, desired_state,
    dirty_state, last_activity_at, created_at, updated_at,
    deleted_at, secret_ca_certificate, secret_ca_private_key_nonce, secret_ca_private_key_ciphertext,
    secret_ca_not_after
)
SELECT (row).id, (row).environment_id, (row).region_id, (row).sandbox_declared_id,
    (row).deployment_definition_id, (row).key, (row).revision, (row).owner_session_id,
    (row).owner_run_id, (row).ownership_generation, (row).writer_generation, (row).head_version_id,
    (row).initial_config, (row).write_key_id, (row).status, (row).desired_state,
    (row).dirty_state, (row).last_activity_at, (row).created_at, (row).updated_at,
    (row).deleted_at, (row).secret_ca_certificate, (row).secret_ca_private_key_nonce, (row).secret_ca_private_key_ciphertext,
    (row).secret_ca_not_after
FROM cloned`, sourceMountID, workspaceID, runID, versionID)
	dbtest.InsertCommittedComputerRoot(t, fixture.ctx, tx, versionID, fixture.environmentID, workspaceID)
	insertPlacementGeneration(t, fixture.ctx, tx, fixture.environmentID, workspaceID, versionID)
	dbtest.MustExec(t, fixture.ctx, tx, `
WITH cloned AS (
SELECT (jsonb_populate_record(
    NULL::runs,
    to_jsonb(source_run) || jsonb_build_object(
        'id', $2::text,
        'workspace_id', $3::text,
        'base_workspace_version_id', $4::text,
        'current_run_lease_id', NULL,
        'created_at', transaction_timestamp(),
        'updated_at', transaction_timestamp()
    )
)) AS row
  FROM runtime_instances source_runtime
  JOIN runs source_run ON source_run.id = source_runtime.reserved_run_id
 WHERE source_runtime.id = (
    SELECT runtime_instance_id FROM workspace_mounts WHERE id = $1
 )
)
INSERT INTO runs (
    id, org_id, project_id, environment_id,
    deployment_id, deployment_definition_id, entrypoint_kind, entrypoint_declared_id,
    session_id, cause_kind, schedule_id, schedule_generation,
    scheduled_at, previous_scheduled_at, schedule_timezone, parent_run_id,
    parent_owns_lifecycle, workspace_id, base_workspace_version_id, session_input_start_sequence,
    session_input_high_watermark, payload, output, failure,
    status, revision, current_attempt_number, current_run_lease_id,
    metadata, tags, queue_name, concurrency_key,
    queue_concurrency_limit, priority, queue_origin_at, queue_score_at,
    queued_expires_at, max_active_duration_ms, retry_policy, active_elapsed_ms,
    active_started_at, trace_id, root_span_id, claim_id,
    created_at, updated_at, first_lease_at, started_at,
    retry_at, runtime_preparation_count, next_runtime_preparation_at, terminal_at
)
SELECT (row).id, (row).org_id, (row).project_id, (row).environment_id,
    (row).deployment_id, (row).deployment_definition_id, (row).entrypoint_kind, (row).entrypoint_declared_id,
    (row).session_id, (row).cause_kind, (row).schedule_id, (row).schedule_generation,
    (row).scheduled_at, (row).previous_scheduled_at, (row).schedule_timezone, (row).parent_run_id,
    (row).parent_owns_lifecycle, (row).workspace_id, (row).base_workspace_version_id, (row).session_input_start_sequence,
    (row).session_input_high_watermark, (row).payload, (row).output, (row).failure,
    (row).status, (row).revision, (row).current_attempt_number, (row).current_run_lease_id,
    (row).metadata, (row).tags, (row).queue_name, (row).concurrency_key,
    (row).queue_concurrency_limit, (row).priority, (row).queue_origin_at, (row).queue_score_at,
    (row).queued_expires_at, (row).max_active_duration_ms, (row).retry_policy, (row).active_elapsed_ms,
    (row).active_started_at, (row).trace_id, (row).root_span_id, (row).claim_id,
    (row).created_at, (row).updated_at, (row).first_lease_at, (row).started_at,
    (row).retry_at, (row).runtime_preparation_count, (row).next_runtime_preparation_at, (row).terminal_at
FROM cloned`, sourceMountID, runID, workspaceID, versionID)
	dbtest.MustExec(t, fixture.ctx, tx, `
WITH cloned AS (
SELECT (jsonb_populate_record(
    NULL::run_attempts,
    to_jsonb(source_attempt) || jsonb_build_object(
        'run_id', $2::text,
        'workspace_id', $3::text,
        'base_workspace_version_id', $4::text,
        'created_at', transaction_timestamp()
    )
)) AS row
  FROM runtime_instances source_runtime
  JOIN run_attempts source_attempt
    ON source_attempt.run_id = source_runtime.reserved_run_id
   AND source_attempt.number = source_runtime.reserved_attempt_number
 WHERE source_runtime.id = (
    SELECT runtime_instance_id FROM workspace_mounts WHERE id = $1
 )
)
INSERT INTO run_attempts (
    run_id, number, entrypoint_kind, workspace_id,
    entrypoint_entered_at, session_input_start_sequence, base_workspace_version_id, terminal_session_input_sequence,
    terminal_outcome, terminal_reason_code, terminal_error, created_at,
    terminal_at
)
SELECT (row).run_id, (row).number, (row).entrypoint_kind, (row).workspace_id,
    (row).entrypoint_entered_at, (row).session_input_start_sequence, (row).base_workspace_version_id, (row).terminal_session_input_sequence,
    (row).terminal_outcome, (row).terminal_reason_code, (row).terminal_error, (row).created_at,
    (row).terminal_at
FROM cloned`, sourceMountID, runID, workspaceID, versionID)
	dbtest.MustExec(t, fixture.ctx, tx, `
WITH cloned AS (
SELECT (jsonb_populate_record(
    NULL::runtime_instances,
    to_jsonb(source_runtime) || jsonb_build_object(
        'id', $2::text,
        'workspace_id', $3::text,
        'reserved_run_id', $4::text,
        'reserved_workspace_version_id', $5::text,
        'computer_source_version_id', $5::text,
        'reservation_expires_at', transaction_timestamp() + interval '10 minutes',
        'updated_at', transaction_timestamp()
    )
)) AS row
  FROM workspace_mounts source_mount
  JOIN runtime_instances source_runtime ON source_runtime.id = source_mount.runtime_instance_id
 WHERE source_mount.id = $1
)
INSERT INTO runtime_instances (
    id, org_id, worker_group_id, project_id,
    environment_id, region_id, worker_instance_id, runtime_identity_id,
    deployment_definition_id, runtime_substrate_id, worker_epoch, vm_vcpu_count,
    cpu_config_digest, reserved_cpu_millis, reserved_memory_bytes, reserved_guest_ephemeral_disk_bytes,
    reserved_execution_slots, workspace_id, program_deployment_id, restore_checkpoint_id,
    reserved_run_id, reserved_attempt_number, reserved_process_id, reserved_workspace_version_id,
    computer_source_version_id, computer_write_key_id, preparation_expires_at, reservation_expires_at,
    desired_state, desired_version, desired_at, desired_reason,
    observed_state, observed_version, observed_desired_version, observed_at,
    allocated_at, ready_at, terminal_at, reclaimed_at,
    reclaim_evidence, terminal_reason_code, terminal_error, updated_at
)
SELECT (row).id, (row).org_id, (row).worker_group_id, (row).project_id,
    (row).environment_id, (row).region_id, (row).worker_instance_id, (row).runtime_identity_id,
    (row).deployment_definition_id, (row).runtime_substrate_id, (row).worker_epoch, (row).vm_vcpu_count,
    (row).cpu_config_digest, (row).reserved_cpu_millis, (row).reserved_memory_bytes, (row).reserved_guest_ephemeral_disk_bytes,
    (row).reserved_execution_slots, (row).workspace_id, (row).program_deployment_id, (row).restore_checkpoint_id,
    (row).reserved_run_id, (row).reserved_attempt_number, (row).reserved_process_id, (row).reserved_workspace_version_id,
    (row).computer_source_version_id, (row).computer_write_key_id, (row).preparation_expires_at, (row).reservation_expires_at,
    (row).desired_state, (row).desired_version, (row).desired_at, (row).desired_reason,
    (row).observed_state, (row).observed_version, (row).observed_desired_version, (row).observed_at,
    (row).allocated_at, (row).ready_at, (row).terminal_at, (row).reclaimed_at,
    (row).reclaim_evidence, (row).terminal_reason_code, (row).terminal_error, (row).updated_at
FROM cloned`, sourceMountID, runtimeID, workspaceID, runID, versionID)
	dbtest.MustExec(t, fixture.ctx, tx, `
INSERT INTO workspace_mounts
SELECT (jsonb_populate_record(
    NULL::workspace_mounts,
    to_jsonb(source_mount) || jsonb_build_object(
        'id', $2::text,
        'workspace_id', $3::text,
        'materialized_version_id', $4::text,
        'runtime_instance_id', $5::text,
        'guest_channel_token_hash', '',
        'guest_channel_token_expires_at', NULL,
        'created_at', source_mount.created_at + interval '1 minute',
        'updated_at', transaction_timestamp()
    )
)).*
  FROM workspace_mounts source_mount
 WHERE source_mount.id = $1`, sourceMountID, mountID, workspaceID, versionID, runtimeID)
	if err := tx.Commit(fixture.ctx); err != nil {
		t.Fatal(err)
	}
	return pgvalue.UUID(mountID)
}

func markExpiredClaimRuntimeReclaimed(t *testing.T, fixture runPlacementFixture, runtimeID pgtype.UUID) {
	t.Helper()
	dbtest.MustExec(t, fixture.ctx, fixture.pool, `
UPDATE runtime_instances
   SET observed_state = 'closed', observed_version = observed_version + 1,
       observed_desired_version = desired_version, observed_at = transaction_timestamp(),
       terminal_at = transaction_timestamp(), terminal_reason_code = desired_reason,
       reclaimed_at = transaction_timestamp(), reclaim_evidence = '{}'::jsonb,
       reserved_run_id = NULL, reserved_attempt_number = NULL,
       reserved_process_id = NULL, reserved_workspace_version_id = NULL,
       reservation_expires_at = NULL, updated_at = transaction_timestamp()
 WHERE id = $1`, runtimeID)
}

func claimWorkspaceMountParams(fixture runPlacementFixture, token string) db.ClaimWorkspaceMountParams {
	return db.ClaimWorkspaceMountParams{
		WorkerInstanceID:           pgvalue.UUID(fixture.workerID),
		WorkerEpoch:                1,
		GuestChannelTokenHash:      hex.EncodeToString(dbtest.Hash("workspace-mount-" + token)),
		GuestChannelTokenExpiresAt: pgvalue.TimestamptzUTCZeroInvalid(time.Now().UTC().Add(5 * time.Minute)),
	}
}

func TestWorkspaceMountCreationOrdersClaimsAndSurvivesReplay(t *testing.T) {
	fixture, firstID := prepareClaimableRunMount(t)
	secondID := cloneClaimableRunMount(t, fixture, firstID)
	queries := db.New(fixture.pool)
	var before db.WorkspaceMount
	if err := fixture.pool.QueryRow(fixture.ctx, `
		UPDATE workspace_mounts SET created_at=now()-interval '2 minutes', updated_at=now()-interval '1 minute'
		WHERE id=$1 RETURNING org_id, workspace_id, runtime_instance_id, materialized_version_id, fencing_generation, request, created_at, updated_at
	`, firstID).Scan(&before.OrgID, &before.WorkspaceID, &before.RuntimeInstanceID, &before.MaterializedVersionID, &before.FencingGeneration, &before.Request, &before.CreatedAt, &before.UpdatedAt); err != nil {
		t.Fatal(err)
	}
	var runID pgtype.UUID
	var attempt pgtype.Int4
	if err := fixture.pool.QueryRow(fixture.ctx, `SELECT reserved_run_id,reserved_attempt_number FROM runtime_instances WHERE id=$1`, before.RuntimeInstanceID).Scan(&runID, &attempt); err != nil {
		t.Fatal(err)
	}
	replayed, err := queries.EnsureRunWorkspaceMountRequested(fixture.ctx, db.EnsureRunWorkspaceMountRequestedParams{
		ID: pgvalue.UUID(uuid.NewV7()), OrgID: before.OrgID, WorkspaceID: before.WorkspaceID,
		RuntimeInstanceID: before.RuntimeInstanceID, WorkspaceVersionID: before.MaterializedVersionID,
		FencingGeneration: before.FencingGeneration, Request: before.Request, RunID: runID, AttemptNumber: attempt,
	})
	if err != nil {
		t.Fatal(err)
	}
	if replayed.Inserted || replayed.ID != firstID || replayed.CreatedAt != before.CreatedAt || replayed.UpdatedAt != before.UpdatedAt {
		t.Fatalf("mount replay changed identity or timestamps: %+v", replayed)
	}
	for _, id := range []pgtype.UUID{firstID, secondID} {
		claimed, err := queries.ClaimWorkspaceMount(fixture.ctx, claimWorkspaceMountParams(fixture, uuid.NewV7().String()))
		if err != nil || claimed.ID != id {
			t.Fatalf("claim = %v, %v; want %v", claimed.ID, err, id)
		}
	}
}
