package dispatch

import (
	"context"
	"errors"
	"strings"
	"testing"
	"uuid"

	"github.com/helmrdotdev/helmr/internal/capacity"
	"github.com/helmrdotdev/helmr/internal/db"
	"github.com/helmrdotdev/helmr/internal/db/dbtest"
	"github.com/helmrdotdev/helmr/internal/pgvalue"
	"github.com/helmrdotdev/helmr/internal/workspace"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
)

func TestSameWorkspaceChildUsesFreshRuntimeAndParentRestoresBIntoC(t *testing.T) {
	fixture := newRunPlacementFixture(t)
	parentCandidate := fixture.candidate()
	parentRuntime, parentMount, parentLease := placeRunForTest(t, fixture, parentCandidate)

	var parentWorkspaceLeaseID, aVersionID, taskDefinitionID pgtype.UUID
	if err := fixture.pool.QueryRow(fixture.ctx, `
SELECT workspace_leases.id, workspace_leases.base_version_id,
       runs.deployment_definition_id
  FROM workspace_leases
  JOIN runs ON runs.id = $2
 WHERE workspace_leases.owner_run_lease_id = $1`,
		parentLease.ID, fixture.runID,
	).Scan(&parentWorkspaceLeaseID, &aVersionID, &taskDefinitionID); err != nil {
		t.Fatal(err)
	}

	waitID := uuid.NewV7()
	bCheckpointID := uuid.NewV7()
	childID := uuid.NewV7()
	claimID := uuid.NewV7()
	bVersionID := uuid.NewV7()
	bArtifactID := uuid.NewV7()
	bDigest := "sha256:" + strings.Repeat("9", 64)
	tx, err := fixture.pool.Begin(fixture.ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = tx.Rollback(context.Background()) }()
	dbtest.MustExec(t, fixture.ctx, tx, `SET CONSTRAINTS ALL DEFERRED`)
	dbtest.MustExec(t, fixture.ctx, tx, `
INSERT INTO idempotency_claims (
    id, environment_id, operation, slot_hash, request_fingerprint, accepted_at
) VALUES (
    $1, $2, 'task.child.invoke', decode(repeat('12', 32), 'hex'),
    decode(repeat('14', 32), 'hex'), transaction_timestamp()
)`, claimID, fixture.environmentID)
	dbtest.MustExec(t, fixture.ctx, tx, `
INSERT INTO cas_objects (org_id, digest, size_bytes, media_type)
VALUES ($1, $2, 1, $3)`, fixture.orgID, bDigest, workspace.ArtifactMediaType)
	dbtest.MustExec(t, fixture.ctx, tx, `
INSERT INTO artifacts (
    id, org_id, project_id, environment_id, digest, kind, size_bytes, media_type
) VALUES ($1, $2, $3, $4, $5, 'workspace_version', 1, $6)`,
		bArtifactID, fixture.orgID, fixture.projectID, fixture.environmentID,
		bDigest, workspace.ArtifactMediaType)
	dbtest.MustExec(t, fixture.ctx, tx, `
INSERT INTO workspace_versions (
    id, environment_id, workspace_id, parent_version_id, artifact_id,
    artifact_kind, kind, content_digest, size_bytes, entry_count, state,
    source_workspace_lease_id, ownership_generation, writer_generation
) VALUES (
    $1, $2, $3, $4, $5, 'workspace_version', 'user', $6, 1, 1,
    'private', $7, 1, 1
)`, bVersionID, fixture.environmentID, fixture.workspaceID, aVersionID,
		bArtifactID, bDigest, parentWorkspaceLeaseID)
	dbtest.MustExec(t, fixture.ctx, tx, `
INSERT INTO runs (
    id, org_id, project_id, environment_id, deployment_id,
    deployment_definition_id, entrypoint_kind, entrypoint_declared_id,
    cause_kind, parent_run_id, parent_owns_lifecycle, workspace_id,
    base_workspace_version_id, payload, queue_name, queue_origin_at,
    queue_score_at, max_active_duration_ms, retry_policy, trace_id,
    root_span_id, claim_id
) VALUES (
    $1, $2, $3, $4, $5, $6, 'task', 'test-task', 'child', $7, true,
    $8, $9, '{}'::jsonb, 'default', transaction_timestamp(),
    transaction_timestamp(), 300000, '{"enabled":false}'::jsonb,
    '33333333333333333333333333333333', '4444444444444444', $10
)`, childID, fixture.orgID, fixture.projectID, fixture.environmentID,
		fixture.deploymentID, taskDefinitionID, fixture.runID, fixture.workspaceID,
		bVersionID, claimID)
	dbtest.MustExec(t, fixture.ctx, tx, `
INSERT INTO run_attempts (
    run_id, number, entrypoint_kind, workspace_id, base_workspace_version_id
) VALUES ($1, 1, 'task', $2, $3)`, childID, fixture.workspaceID, bVersionID)
	dbtest.MustExec(t, fixture.ctx, tx, `
INSERT INTO run_waits (
    id, environment_id, run_id, workspace_id, kind, child_run_id,
    child_parent_owned, child_target_declared_id, child_claim_id,
    child_request, expected_run_state_version, attempt_number,
    prior_run_lease_id, resume_attach_id, suspension_state
) VALUES (
    $1, $2, $3, $4, 'child', $5, true, 'test-task', $6,
    '{"Method":"call"}'::jsonb, 3, 1, $7, $8, 'parked'
)`, waitID, fixture.environmentID, fixture.runID, fixture.workspaceID,
		childID, claimID, parentLease.ID, uuid.NewV7())
	dbtest.MustExec(t, fixture.ctx, tx, `
INSERT INTO run_checkpoints (
    id, run_id, attempt_number, run_wait_id, source_run_lease_id,
    source_workspace_lease_id, workspace_id, base_workspace_version_id,
    private_workspace_version_id, state, restore_manifest,
    ready_request_fingerprint, ready_at
) VALUES (
    $1, $2, 1, $3, $4, $5, $6, $7, $8, 'ready',
    '{"kind":"suspend"}'::jsonb, 'test-ready', transaction_timestamp()
)`, bCheckpointID, fixture.runID, waitID, parentLease.ID,
		parentWorkspaceLeaseID, fixture.workspaceID, aVersionID, bVersionID)
	dbtest.MustExec(t, fixture.ctx, tx, `
UPDATE run_waits
   SET suspend_checkpoint_id = $2,
       base_workspace_version_id = $3,
       base_workspace_content_digest = $4,
       ownership_generation = 1,
       parent_writer_generation = 1
 WHERE id = $1`, waitID, bCheckpointID, bVersionID, bDigest)
	dbtest.MustExec(t, fixture.ctx, tx, `
UPDATE runs
   SET status = 'waiting', state_version = 3,
       current_run_lease_id = NULL, active_started_at = NULL
 WHERE id = $1`, fixture.runID)
	dbtest.MustExec(t, fixture.ctx, tx, `
UPDATE run_leases
   SET state = 'checkpointed', claimed_at = assigned_at,
       started_at = assigned_at, checkpointed_at = transaction_timestamp(),
       terminal_at = transaction_timestamp(), terminal_reason_code = 'checkpointed'
 WHERE id = $1`, parentLease.ID)
	dbtest.MustExec(t, fixture.ctx, tx, `
UPDATE workspace_leases
   SET state = 'released', released_at = transaction_timestamp(),
       terminal_at = transaction_timestamp()
 WHERE id = $1`, parentWorkspaceLeaseID)
	reclaimRunRuntimeForTest(t, fixture, tx, parentRuntime, parentMount, pgvalue.UUID(bVersionID))
	if err := tx.Commit(fixture.ctx); err != nil {
		t.Fatal(err)
	}
	hasQueuedDemand, err := capacity.HasQueuedDemand(fixture.ctx, db.New(fixture.pool), fixture.groupID)
	if err != nil {
		t.Fatal(err)
	}
	if !hasQueuedDemand {
		t.Fatal("queued same-Workspace child was absent from the final drain demand check")
	}

	childCandidate := ReadyRunCandidate{
		OrgID: pgvalue.UUID(fixture.orgID), RunID: pgvalue.UUID(childID),
		ExpectedRunStateVersion: 1,
	}
	childReservation, err := fixture.authority.PlaceReadyRun(fixture.ctx, childCandidate)
	if err != nil {
		t.Fatal(err)
	}
	markRunPlacementRuntimeReady(t, fixture, childReservation.RuntimeInstanceID)
	assertSameWorkspaceChildMountRejectsStaleReceipt(
		t,
		fixture,
		childReservation.RuntimeInstanceID,
		childID,
		bVersionID,
		waitID,
	)
	childMountReservation, err := fixture.authority.PlaceReadyRun(fixture.ctx, childCandidate)
	if err != nil {
		t.Fatal(err)
	}
	markRunPlacementMountReady(t, fixture, childMountReservation.WorkspaceMountID)
	childGranted, err := fixture.authority.PlaceReadyRun(fixture.ctx, childCandidate)
	if err != nil {
		t.Fatal(err)
	}
	if !childGranted.LeaseCreated {
		t.Fatalf("child was not granted: %+v", childGranted)
	}
	childRuntime, childMount, childLease := childGranted.RuntimeInstanceID,
		childGranted.WorkspaceMountID, childGranted.Lease
	if childRuntime == parentRuntime || childMount == parentMount {
		t.Fatalf("child reused parent physical state: runtime=%s mount=%s",
			pgvalue.UUIDString(childRuntime), pgvalue.UUIDString(childMount))
	}
	var childWriter, workspaceWriter int64
	var childLeaseBase pgtype.UUID
	if err := fixture.pool.QueryRow(fixture.ctx, `
SELECT run_waits.child_writer_generation, workspaces.writer_generation,
       workspace_leases.base_version_id
  FROM run_waits
  JOIN workspaces ON workspaces.id = run_waits.workspace_id
  JOIN workspace_leases ON workspace_leases.owner_run_lease_id = $2
 WHERE run_waits.id = $1`, waitID, childLease.ID).Scan(
		&childWriter, &workspaceWriter, &childLeaseBase,
	); err != nil {
		t.Fatal(err)
	}
	if childWriter != 2 || workspaceWriter != 2 || childLeaseBase != pgvalue.UUID(bVersionID) {
		t.Fatalf("child receipts writer=%d workspace=%d base=%s",
			childWriter, workspaceWriter, pgvalue.UUIDString(childLeaseBase))
	}

	var childWorkspaceLeaseID pgtype.UUID
	if err := fixture.pool.QueryRow(fixture.ctx, `
SELECT id FROM workspace_leases WHERE owner_run_lease_id = $1`, childLease.ID).Scan(
		&childWorkspaceLeaseID,
	); err != nil {
		t.Fatal(err)
	}
	childMountGeneration := int64(2)

	// B now performs its own same-Workspace call to C. This makes B's later
	// restore a real nested A→B→C authority path while Workspace ownership stays A.
	innerWaitID := uuid.NewV7()
	innerCheckpointID := uuid.NewV7()
	grandchildID := uuid.NewV7()
	grandchildClaimID := uuid.NewV7()
	nestedBaseVersionID := uuid.NewV7()
	nestedBaseArtifactID := uuid.NewV7()
	nestedBaseDigest := "sha256:" + strings.Repeat("7", 64)
	tx, err = fixture.pool.Begin(fixture.ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = tx.Rollback(context.Background()) }()
	dbtest.MustExec(t, fixture.ctx, tx, `SET CONSTRAINTS ALL DEFERRED`)
	dbtest.MustExec(t, fixture.ctx, tx, `
INSERT INTO cas_objects (org_id, digest, size_bytes, media_type)
VALUES ($1, $2, 3, $3)`, fixture.orgID, nestedBaseDigest, workspace.ArtifactMediaType)
	dbtest.MustExec(t, fixture.ctx, tx, `
INSERT INTO artifacts (
    id, org_id, project_id, environment_id, digest, kind, size_bytes, media_type
) VALUES ($1, $2, $3, $4, $5, 'workspace_version', 3, $6)`,
		nestedBaseArtifactID, fixture.orgID, fixture.projectID, fixture.environmentID,
		nestedBaseDigest, workspace.ArtifactMediaType)
	dbtest.MustExec(t, fixture.ctx, tx, `
INSERT INTO workspace_versions (
    id, environment_id, workspace_id, parent_version_id, artifact_id,
    artifact_kind, kind, content_digest, size_bytes, entry_count, state,
    source_workspace_lease_id, ownership_generation, writer_generation
) VALUES (
    $1, $2, $3, $4, $5, 'workspace_version', 'user', $6, 3, 3,
    'private', $7, 1, 2
)`, nestedBaseVersionID, fixture.environmentID, fixture.workspaceID, bVersionID,
		nestedBaseArtifactID, nestedBaseDigest, childWorkspaceLeaseID)
	dbtest.MustExec(t, fixture.ctx, tx, `
INSERT INTO idempotency_claims (
    id, environment_id, operation, slot_hash, request_fingerprint, accepted_at
) VALUES (
    $1, $2, 'task.child.invoke', decode(repeat('22', 32), 'hex'),
    decode(repeat('24', 32), 'hex'), transaction_timestamp()
)`, grandchildClaimID, fixture.environmentID)
	dbtest.MustExec(t, fixture.ctx, tx, `
INSERT INTO runs (
    id, org_id, project_id, environment_id, deployment_id,
    deployment_definition_id, entrypoint_kind, entrypoint_declared_id,
    cause_kind, parent_run_id, parent_owns_lifecycle, workspace_id,
    base_workspace_version_id, payload, queue_name, queue_origin_at,
    queue_score_at, max_active_duration_ms, retry_policy, trace_id,
    root_span_id, claim_id
) VALUES (
    $1, $2, $3, $4, $5, $6, 'task', 'test-task', 'child', $7, true,
    $8, $9, '{}'::jsonb, 'default', transaction_timestamp(),
    transaction_timestamp(), 300000, '{"enabled":false}'::jsonb,
    '55555555555555555555555555555555', '6666666666666666', $10
)`, grandchildID, fixture.orgID, fixture.projectID, fixture.environmentID,
		fixture.deploymentID, taskDefinitionID, childID, fixture.workspaceID,
		nestedBaseVersionID, grandchildClaimID)
	dbtest.MustExec(t, fixture.ctx, tx, `
INSERT INTO run_attempts (
    run_id, number, entrypoint_kind, workspace_id, base_workspace_version_id
) VALUES ($1, 1, 'task', $2, $3)`, grandchildID, fixture.workspaceID, nestedBaseVersionID)
	dbtest.MustExec(t, fixture.ctx, tx, `
INSERT INTO run_waits (
    id, environment_id, run_id, workspace_id, kind, child_run_id,
    child_parent_owned, child_target_declared_id, child_claim_id,
    child_request, expected_run_state_version, attempt_number,
    prior_run_lease_id, resume_attach_id, suspension_state
) VALUES (
    $1, $2, $3, $4, 'child', $5, true, 'test-task', $6,
    '{"Method":"call"}'::jsonb, 3, 1, $7, $8, 'parked'
)`, innerWaitID, fixture.environmentID, childID, fixture.workspaceID,
		grandchildID, grandchildClaimID, childLease.ID, uuid.NewV7())
	dbtest.MustExec(t, fixture.ctx, tx, `
INSERT INTO run_checkpoints (
    id, run_id, attempt_number, run_wait_id, source_run_lease_id,
    source_workspace_lease_id, workspace_id, base_workspace_version_id,
    private_workspace_version_id, state, restore_manifest,
    ready_request_fingerprint, ready_at
) VALUES (
    $1, $2, 1, $3, $4, $5, $6, $7, $8, 'ready',
    '{"kind":"suspend"}'::jsonb, 'nested-ready', transaction_timestamp()
)`, innerCheckpointID, childID, innerWaitID, childLease.ID,
		childWorkspaceLeaseID, fixture.workspaceID, bVersionID, nestedBaseVersionID)
	dbtest.MustExec(t, fixture.ctx, tx, `
UPDATE run_waits
   SET suspend_checkpoint_id = $2,
       base_workspace_version_id = $3,
       base_workspace_content_digest = $4,
       ownership_generation = 1,
       parent_writer_generation = 2
 WHERE id = $1`, innerWaitID, innerCheckpointID, nestedBaseVersionID, nestedBaseDigest)
	dbtest.MustExec(t, fixture.ctx, tx, `
UPDATE runs
   SET status = 'waiting', state_version = 3,
       current_run_lease_id = NULL, active_started_at = NULL
 WHERE id = $1`, childID)
	dbtest.MustExec(t, fixture.ctx, tx, `
UPDATE run_leases
   SET state = 'checkpointed', claimed_at = assigned_at,
       started_at = assigned_at, checkpointed_at = transaction_timestamp(),
       terminal_at = transaction_timestamp(), terminal_reason_code = 'checkpointed'
 WHERE id = $1`, childLease.ID)
	dbtest.MustExec(t, fixture.ctx, tx, `
UPDATE workspace_leases
   SET state = 'released', released_at = transaction_timestamp(),
       terminal_at = transaction_timestamp()
 WHERE id = $1`, childWorkspaceLeaseID)
	reclaimRunRuntimeForTest(t, fixture, tx, childRuntime, childMount, pgvalue.UUID(nestedBaseVersionID))
	if err := tx.Commit(fixture.ctx); err != nil {
		t.Fatal(err)
	}

	nestedResultVersionID := uuid.NewV7()
	nestedResultArtifactID := uuid.NewV7()
	grandchildRunLeaseID := uuid.NewV7()
	grandchildWorkspaceLeaseID := uuid.NewV7()
	nestedResultDigest := "sha256:" + strings.Repeat("6", 64)
	tx, err = fixture.pool.Begin(fixture.ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = tx.Rollback(context.Background()) }()
	dbtest.MustExec(t, fixture.ctx, tx, `SET CONSTRAINTS ALL DEFERRED`)
	dbtest.MustExec(t, fixture.ctx, tx, `UPDATE workspaces SET writer_generation = 3 WHERE id = $1`, fixture.workspaceID)
	dbtest.MustExec(t, fixture.ctx, tx, `UPDATE run_waits SET child_writer_generation = 3 WHERE id = $1`, innerWaitID)
	dbtest.MustExec(t, fixture.ctx, tx, `
INSERT INTO run_leases (
    id, org_id, project_id, environment_id, run_id, workspace_id, region_id,
    lease_sequence, attempt_number, worker_group_id, worker_instance_id,
    worker_epoch, runtime_instance_id, runtime_identity_id,
    requested_cpu_millis, requested_memory_bytes,
    requested_guest_ephemeral_disk_bytes, requested_execution_slots,
    trace_id, span_id, state, assigned_at, start_deadline_at,
    claimed_at, started_at, expires_at, terminal_at, terminal_reason_code
)
SELECT $1, org_id, project_id, environment_id, $2, workspace_id, region_id,
       1, 1, worker_group_id, worker_instance_id, worker_epoch,
       runtime_instance_id, runtime_identity_id, requested_cpu_millis,
       requested_memory_bytes, requested_guest_ephemeral_disk_bytes,
       requested_execution_slots, '55555555555555555555555555555555',
       '6666666666666666', 'completed', assigned_at, start_deadline_at,
       assigned_at, assigned_at, expires_at, transaction_timestamp(), 'completed'
  FROM run_leases
 WHERE id = $3`, grandchildRunLeaseID, grandchildID, childLease.ID)
	dbtest.MustExec(t, fixture.ctx, tx, `
INSERT INTO workspace_leases (
    id, org_id, worker_group_id, project_id, environment_id, region_id,
    worker_instance_id, worker_epoch, runtime_instance_id, workspace_id,
    workspace_mount_id, state, owner_run_lease_id, base_version_id,
    ownership_generation, writer_generation, mount_fencing_generation,
    fencing_token_hash, acquired_at, renewed_at, expires_at,
    released_at, terminal_at
)
SELECT $1, org_id, worker_group_id, project_id, environment_id, region_id,
       worker_instance_id, worker_epoch, runtime_instance_id, workspace_id,
       workspace_mount_id, 'released', $2, $3, ownership_generation, 3,
       mount_fencing_generation, 'test-grandchild-fence', acquired_at, renewed_at,
       expires_at, transaction_timestamp(), transaction_timestamp()
  FROM workspace_leases
 WHERE id = $4`, grandchildWorkspaceLeaseID, grandchildRunLeaseID,
		nestedBaseVersionID, childWorkspaceLeaseID)
	dbtest.MustExec(t, fixture.ctx, tx, `
INSERT INTO cas_objects (org_id, digest, size_bytes, media_type)
VALUES ($1, $2, 4, $3)`, fixture.orgID, nestedResultDigest, workspace.ArtifactMediaType)
	dbtest.MustExec(t, fixture.ctx, tx, `
INSERT INTO artifacts (
    id, org_id, project_id, environment_id, digest, kind, size_bytes, media_type
) VALUES ($1, $2, $3, $4, $5, 'workspace_version', 4, $6)`,
		nestedResultArtifactID, fixture.orgID, fixture.projectID, fixture.environmentID,
		nestedResultDigest, workspace.ArtifactMediaType)
	dbtest.MustExec(t, fixture.ctx, tx, `
INSERT INTO workspace_versions (
    id, environment_id, workspace_id, parent_version_id, artifact_id,
    artifact_kind, kind, content_digest, size_bytes, entry_count, state,
    source_workspace_lease_id, ownership_generation, writer_generation
) VALUES (
    $1, $2, $3, $4, $5, 'workspace_version', 'user', $6, 4, 4,
    'private', $7, 1, 3
)`, nestedResultVersionID, fixture.environmentID, fixture.workspaceID, nestedBaseVersionID,
		nestedResultArtifactID, nestedResultDigest, grandchildWorkspaceLeaseID)
	dbtest.MustExec(t, fixture.ctx, tx, `
UPDATE run_attempts
   SET entrypoint_entered_at = transaction_timestamp(), terminal_outcome = 'succeeded',
       terminal_reason_code = 'completed', terminal_at = transaction_timestamp()
 WHERE run_id = $1 AND number = 1`, grandchildID)
	dbtest.MustExec(t, fixture.ctx, tx, `
UPDATE runs
   SET status = 'succeeded', output = '{}'::jsonb, terminal_at = transaction_timestamp(),
       state_version = state_version + 1
 WHERE id = $1`, grandchildID)
	dbtest.MustExec(t, fixture.ctx, tx, `
UPDATE runs SET status = 'queued', state_version = 4, updated_at = transaction_timestamp()
 WHERE id = $1 AND status = 'waiting'`, childID)
	dbtest.MustExec(t, fixture.ctx, tx, `
UPDATE run_waits
   SET condition_state = 'completed', condition_result = '{}'::jsonb,
       condition_terminal_at = transaction_timestamp(),
       suspension_state = 'resume_pending', resume_request_version = 1,
       expected_run_state_version = 4, resume_workspace_version_id = $2
 WHERE id = $1`, innerWaitID, nestedResultVersionID)
	if err := tx.Commit(fixture.ctx); err != nil {
		t.Fatal(err)
	}

	nestedCandidate := ReadyRunCandidate{
		OrgID: pgvalue.UUID(fixture.orgID), RunID: pgvalue.UUID(childID),
		ExpectedRunStateVersion: 4,
	}
	nestedReservation, err := fixture.authority.PlaceReadyRun(fixture.ctx, nestedCandidate)
	if err != nil {
		t.Fatal(err)
	}
	markRunPlacementRuntimeReady(t, fixture, nestedReservation.RuntimeInstanceID)
	nestedMountReservation, err := fixture.authority.PlaceReadyRun(fixture.ctx, nestedCandidate)
	if err != nil {
		t.Fatal(err)
	}
	markRunPlacementMountReady(t, fixture, nestedMountReservation.WorkspaceMountID)
	nestedGranted, err := fixture.authority.PlaceReadyRun(fixture.ctx, nestedCandidate)
	if err != nil {
		t.Fatal(err)
	}
	if !nestedGranted.LeaseCreated {
		t.Fatalf("nested B restore was not granted: %+v", nestedGranted)
	}
	childRuntime, childMount, childLease = nestedGranted.RuntimeInstanceID,
		nestedGranted.WorkspaceMountID, nestedGranted.Lease
	var ownerRunID pgtype.UUID
	var ownershipGeneration, resumeWriter int64
	if err := fixture.pool.QueryRow(fixture.ctx, `
SELECT workspace_leases.id, workspace_mounts.fencing_generation,
       workspaces.owner_run_id, workspaces.ownership_generation,
       workspaces.writer_generation, outer_edge.child_writer_generation,
       inner_edge.resume_writer_generation
  FROM workspace_leases
  JOIN workspace_mounts ON workspace_mounts.id = workspace_leases.workspace_mount_id
  JOIN workspaces ON workspaces.id = workspace_leases.workspace_id
  JOIN run_waits AS outer_edge ON outer_edge.id = $2
	  JOIN run_waits AS inner_edge ON inner_edge.id = $3
	 WHERE workspace_leases.owner_run_lease_id = $1`, childLease.ID, waitID, innerWaitID).Scan(
		&childWorkspaceLeaseID, &childMountGeneration, &ownerRunID, &ownershipGeneration,
		&workspaceWriter, &childWriter, &resumeWriter,
	); err != nil {
		t.Fatal(err)
	}
	if ownerRunID != pgvalue.UUID(fixture.runID) || ownershipGeneration != 1 ||
		workspaceWriter != 4 || childWriter != 4 || resumeWriter != 4 {
		t.Fatalf("nested grant owner=%s ownership=%d workspaceWriter=%d outerWriter=%d resumeWriter=%d",
			pgvalue.UUIDString(ownerRunID), ownershipGeneration, workspaceWriter, childWriter, resumeWriter)
	}
	dbtest.MustExec(t, fixture.ctx, fixture.pool, `
WITH expired AS (
    UPDATE run_leases
       SET assigned_at = transaction_timestamp() - interval '2 minutes',
           start_deadline_at = transaction_timestamp() - interval '1 minute',
           expires_at = transaction_timestamp() - interval '30 seconds'
     WHERE id = $1
    RETURNING id, expires_at
)
UPDATE workspace_leases
   SET acquired_at = transaction_timestamp() - interval '2 minutes',
       renewed_at = transaction_timestamp() - interval '2 minutes',
       expires_at = expired.expires_at
  FROM expired
 WHERE workspace_leases.id = $2
   AND workspace_leases.owner_run_lease_id = expired.id`, childLease.ID, childWorkspaceLeaseID)
	terminalRecoveryTx, err := fixture.pool.Begin(fixture.ctx)
	if err != nil {
		t.Fatal(err)
	}
	dbtest.MustExec(t, fixture.ctx, terminalRecoveryTx, `
UPDATE run_checkpoints
   SET state = 'invalid', invalidated_at = transaction_timestamp(),
       invalidation_reason_code = 'test_unavailable'
 WHERE id = $1`, innerCheckpointID)
	terminalRecovered, err := db.New(terminalRecoveryTx).RecoverExpiredRunResumes(
		fixture.ctx,
		recoverExpiredRunResumesParams(10),
	)
	if err != nil {
		_ = terminalRecoveryTx.Rollback(fixture.ctx)
		t.Fatal(err)
	}
	if len(terminalRecovered) != 0 {
		_ = terminalRecoveryTx.Rollback(fixture.ctx)
		t.Fatalf("unrecoverable nested resume was requeued: %+v", terminalRecovered)
	}
	var terminalOwner pgtype.UUID
	var terminalOwnership int64
	var terminalChildStatus, terminalParentStatus, terminalOuterCondition, terminalOuterSuspension string
	if err := terminalRecoveryTx.QueryRow(fixture.ctx, `
SELECT workspaces.owner_run_id, workspaces.ownership_generation,
       child.status, parent.status, outer_edge.condition_state,
       outer_edge.suspension_state
  FROM workspaces
  JOIN runs AS child ON child.id = $2
  JOIN runs AS parent ON parent.id = $3
  JOIN run_waits AS outer_edge ON outer_edge.id = $4
 WHERE workspaces.id = $1`, fixture.workspaceID, childID, fixture.runID, waitID).Scan(
		&terminalOwner, &terminalOwnership, &terminalChildStatus, &terminalParentStatus,
		&terminalOuterCondition, &terminalOuterSuspension,
	); err != nil {
		_ = terminalRecoveryTx.Rollback(fixture.ctx)
		t.Fatal(err)
	}
	if terminalOwner != pgvalue.UUID(fixture.runID) || terminalOwnership != 1 ||
		terminalChildStatus != db.RunStatusSystemFailed || terminalParentStatus != db.RunStatusQueued ||
		terminalOuterCondition != string(db.WaitStateFailed) || terminalOuterSuspension != "resume_pending" {
		_ = terminalRecoveryTx.Rollback(fixture.ctx)
		t.Fatalf("unrecoverable nested recovery owner=%s ownership=%d child=%s parent=%s outer=%s/%s",
			pgvalue.UUIDString(terminalOwner), terminalOwnership, terminalChildStatus,
			terminalParentStatus, terminalOuterCondition, terminalOuterSuspension)
	}
	if err := terminalRecoveryTx.Rollback(fixture.ctx); err != nil {
		t.Fatal(err)
	}
	recovered, err := db.New(fixture.pool).RecoverExpiredRunResumes(
		fixture.ctx,
		recoverExpiredRunResumesParams(10),
	)
	if err != nil {
		t.Fatal(err)
	}
	if len(recovered) != 1 || recovered[0].ID != pgvalue.UUID(innerWaitID) ||
		recovered[0].RunID != pgvalue.UUID(childID) {
		var runState, leaseState, workspaceLeaseState, waitState string
		if queryErr := fixture.pool.QueryRow(fixture.ctx, `
SELECT runs.status, run_leases.state, workspace_leases.state, run_waits.suspension_state
  FROM runs
  JOIN run_leases ON run_leases.id = $2
  JOIN workspace_leases ON workspace_leases.id = $3
  JOIN run_waits ON run_waits.id = $4
 WHERE runs.id = $1`, childID, childLease.ID, childWorkspaceLeaseID, innerWaitID).Scan(
			&runState, &leaseState, &workspaceLeaseState, &waitState,
		); queryErr != nil {
			t.Fatal(queryErr)
		}
		t.Logf("nested recovery states run=%s lease=%s workspaceLease=%s wait=%s",
			runState, leaseState, workspaceLeaseState, waitState)
		t.Fatalf("nested recovery = %+v", recovered)
	}
	if _, err := db.New(fixture.pool).StopWorkspaceMount(
		fixture.ctx,
		db.StopWorkspaceMountParams{
			ReasonCode: pgvalue.Text("run_resume_lease_expired"),
			OrgID:      pgvalue.UUID(fixture.orgID), ID: childMount,
			WorkerInstanceID: childLease.WorkerInstanceID, WorkerEpoch: childLease.WorkerEpoch,
			RuntimeInstanceID: childRuntime, FencingGeneration: childMountGeneration,
			CleanupProof: []byte(`{"method":"session_closed","completed_at":"2026-08-20T00:00:00Z"}`),
		},
	); err != nil {
		t.Fatal(err)
	}
	if err := fixture.pool.QueryRow(fixture.ctx, `SELECT state_version FROM runs WHERE id = $1`, childID).Scan(
		&nestedCandidate.ExpectedRunStateVersion,
	); err != nil {
		t.Fatal(err)
	}
	debugTx, err := fixture.pool.Begin(fixture.ctx)
	if err != nil {
		t.Fatal(err)
	}
	if _, debugErr := lockRunPlacementAuthority(fixture.ctx, debugTx, nestedCandidate); debugErr != nil {
		_ = debugTx.Rollback(fixture.ctx)
		t.Fatalf("lock recovered nested placement authority: %v", debugErr)
	}
	if err := debugTx.Rollback(fixture.ctx); err != nil {
		t.Fatal(err)
	}
	nestedReservation, err = fixture.authority.PlaceReadyRun(fixture.ctx, nestedCandidate)
	if err != nil {
		t.Fatal(err)
	}
	markRunPlacementRuntimeReady(t, fixture, nestedReservation.RuntimeInstanceID)
	nestedMountReservation, err = fixture.authority.PlaceReadyRun(fixture.ctx, nestedCandidate)
	if err != nil {
		t.Fatal(err)
	}
	markRunPlacementMountReady(t, fixture, nestedMountReservation.WorkspaceMountID)
	nestedGranted, err = fixture.authority.PlaceReadyRun(fixture.ctx, nestedCandidate)
	if err != nil {
		t.Fatal(err)
	}
	if !nestedGranted.LeaseCreated {
		t.Fatalf("recovered nested B restore was not granted: %+v", nestedGranted)
	}
	childRuntime, childMount, childLease = nestedGranted.RuntimeInstanceID,
		nestedGranted.WorkspaceMountID, nestedGranted.Lease
	if err := fixture.pool.QueryRow(fixture.ctx, `
SELECT workspace_leases.id, workspace_mounts.fencing_generation,
       workspaces.owner_run_id, workspaces.ownership_generation,
       workspaces.writer_generation, outer_edge.child_writer_generation,
       inner_edge.resume_writer_generation
  FROM workspace_leases
  JOIN workspace_mounts ON workspace_mounts.id = workspace_leases.workspace_mount_id
  JOIN workspaces ON workspaces.id = workspace_leases.workspace_id
  JOIN run_waits AS outer_edge ON outer_edge.id = $2
  JOIN run_waits AS inner_edge ON inner_edge.id = $3
 WHERE workspace_leases.owner_run_lease_id = $1`, childLease.ID, waitID, innerWaitID).Scan(
		&childWorkspaceLeaseID, &childMountGeneration, &ownerRunID, &ownershipGeneration,
		&workspaceWriter, &childWriter, &resumeWriter,
	); err != nil {
		t.Fatal(err)
	}
	if ownerRunID != pgvalue.UUID(fixture.runID) || ownershipGeneration != 1 ||
		workspaceWriter != 5 || childWriter != 5 || resumeWriter != 5 {
		t.Fatalf("recovered nested grant owner=%s ownership=%d workspaceWriter=%d outerWriter=%d resumeWriter=%d",
			pgvalue.UUIDString(ownerRunID), ownershipGeneration, workspaceWriter, childWriter, resumeWriter)
	}
	cVersionID := uuid.NewV7()
	cArtifactID := uuid.NewV7()
	cDigest := "sha256:" + strings.Repeat("8", 64)
	tx, err = fixture.pool.Begin(fixture.ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = tx.Rollback(context.Background()) }()
	dbtest.MustExec(t, fixture.ctx, tx, `SET CONSTRAINTS ALL DEFERRED`)
	dbtest.MustExec(t, fixture.ctx, tx, `
INSERT INTO cas_objects (org_id, digest, size_bytes, media_type)
VALUES ($1, $2, 2, $3)`, fixture.orgID, cDigest, workspace.ArtifactMediaType)
	dbtest.MustExec(t, fixture.ctx, tx, `
INSERT INTO artifacts (
    id, org_id, project_id, environment_id, digest, kind, size_bytes, media_type
) VALUES ($1, $2, $3, $4, $5, 'workspace_version', 2, $6)`,
		cArtifactID, fixture.orgID, fixture.projectID, fixture.environmentID,
		cDigest, workspace.ArtifactMediaType)
	dbtest.MustExec(t, fixture.ctx, tx, `
INSERT INTO workspace_versions (
    id, environment_id, workspace_id, parent_version_id, artifact_id,
    artifact_kind, kind, content_digest, size_bytes, entry_count, state,
    source_workspace_lease_id, ownership_generation, writer_generation
) VALUES (
    $1, $2, $3, $4, $5, 'workspace_version', 'user', $6, 2, 2,
    'private', $7, 1, 5
)`, cVersionID, fixture.environmentID, fixture.workspaceID, nestedResultVersionID,
		cArtifactID, cDigest, childWorkspaceLeaseID)
	dbtest.MustExec(t, fixture.ctx, tx, `
UPDATE run_attempts
   SET entrypoint_entered_at = transaction_timestamp(), terminal_outcome = 'succeeded',
       terminal_reason_code = 'completed', terminal_at = transaction_timestamp()
 WHERE run_id = $1 AND number = 1`, childID)
	dbtest.MustExec(t, fixture.ctx, tx, `
UPDATE runs
   SET status = 'succeeded', output = '{}'::jsonb, current_run_lease_id = NULL,
       terminal_at = transaction_timestamp(), state_version = state_version + 1
 WHERE id = $1 AND current_run_lease_id = $2`, childID, childLease.ID)
	dbtest.MustExec(t, fixture.ctx, tx, `
UPDATE run_leases
   SET state = 'completed', claimed_at = assigned_at, started_at = assigned_at,
       terminal_at = transaction_timestamp(), terminal_reason_code = 'completed'
 WHERE id = $1`, childLease.ID)
	dbtest.MustExec(t, fixture.ctx, tx, `
UPDATE workspace_leases
   SET state = 'released', released_at = transaction_timestamp(),
       terminal_at = transaction_timestamp()
 WHERE id = $1`, childWorkspaceLeaseID)
	dbtest.MustExec(t, fixture.ctx, tx, `
UPDATE workspace_mounts
   SET materialized_version_id = $2, dirty_generation = dirty_generation + 1,
       updated_at = transaction_timestamp()
 WHERE id = $1 AND state = 'mounted'`, childMount, cVersionID)
	txq := db.New(tx)
	completedAt, err := txq.GetTaskCompletionTime(fixture.ctx)
	if err != nil {
		t.Fatal(err)
	}
	discardParams := db.RequestSameWorkspaceChildAttemptRuntimeDiscardParams{
		CompletedAt: completedAt, WorkspaceLeaseID: childWorkspaceLeaseID,
		WorkspaceMountID: childMount, RuntimeInstanceID: childRuntime,
		OwnershipGeneration: 1, WriterGeneration: childWriter,
		MountFencingGeneration: childMountGeneration, OrgID: pgvalue.UUID(fixture.orgID),
		ProjectID: pgvalue.UUID(fixture.projectID), EnvironmentID: pgvalue.UUID(fixture.environmentID),
		WorkspaceID: pgvalue.UUID(fixture.workspaceID), WorkerGroupID: childLease.WorkerGroupID,
		WorkerInstanceID: childLease.WorkerInstanceID, WorkerEpoch: childLease.WorkerEpoch,
		RunID: pgvalue.UUID(childID), AttemptNumber: 1,
		RunLeaseID: childLease.ID,
	}
	if _, err := txq.RequestSameWorkspaceChildAttemptRuntimeDiscard(fixture.ctx, discardParams); err != nil {
		t.Fatal(err)
	}
	if _, err := txq.RequestSameWorkspaceChildAttemptRuntimeDiscard(fixture.ctx, discardParams); err != nil {
		t.Fatal(err)
	}
	dbtest.MustExec(t, fixture.ctx, tx, `
UPDATE runs SET status = 'queued', state_version = 4, updated_at = transaction_timestamp()
 WHERE id = $1 AND status = 'waiting'`, fixture.runID)
	dbtest.MustExec(t, fixture.ctx, tx, `
UPDATE run_waits
   SET condition_state = 'completed', condition_result = '{}'::jsonb,
       condition_terminal_at = transaction_timestamp(),
       suspension_state = 'resume_pending', resume_request_version = 1,
	       expected_run_state_version = 4, resume_workspace_version_id = $2
 WHERE id = $1`, waitID, cVersionID)
	if err := tx.Commit(fixture.ctx); err != nil {
		t.Fatal(err)
	}

	if _, err := fixture.pool.Exec(fixture.ctx, `
UPDATE run_waits SET suspend_checkpoint_id = $2 WHERE id = $1`, waitID, cVersionID); err == nil {
		t.Fatal("workspace version C was accepted as the restore checkpoint source")
	}
	var persistedCheckpointID pgtype.UUID
	if err := fixture.pool.QueryRow(fixture.ctx, `
SELECT suspend_checkpoint_id FROM run_waits WHERE id = $1`, waitID).Scan(
		&persistedCheckpointID,
	); err != nil {
		t.Fatal(err)
	}
	if persistedCheckpointID != pgvalue.UUID(bCheckpointID) {
		t.Fatalf("restore source changed to %s", pgvalue.UUIDString(persistedCheckpointID))
	}

	parentResume := ReadyRunCandidate{
		OrgID: pgvalue.UUID(fixture.orgID), RunID: pgvalue.UUID(fixture.runID),
		ExpectedRunStateVersion: 4,
	}
	if _, err := fixture.authority.PlaceReadyRun(fixture.ctx, parentResume); !errors.Is(err, ErrCapacityUnavailable) {
		t.Fatalf("parent placement before child cleanup proof = %v, want capacity unavailable", err)
	}
	var childDesiredState, childMountState string
	if err := fixture.pool.QueryRow(fixture.ctx, `
SELECT runtime_instances.desired_state, workspace_mounts.state
  FROM runtime_instances
  JOIN workspace_mounts ON workspace_mounts.runtime_instance_id = runtime_instances.id
 WHERE runtime_instances.id = $1 AND workspace_mounts.id = $2`, childRuntime, childMount).Scan(
		&childDesiredState, &childMountState,
	); err != nil {
		t.Fatal(err)
	}
	if childDesiredState != "closed" || childMountState != "unmounting" {
		t.Fatalf("child cleanup request runtime=%s mount=%s", childDesiredState, childMountState)
	}
	if _, err := db.New(fixture.pool).StopWorkspaceMount(
		fixture.ctx,
		db.StopWorkspaceMountParams{
			ReasonCode: pgvalue.Text("same_workspace_child_attempt_finished"),
			OrgID:      pgvalue.UUID(fixture.orgID), ID: childMount,
			WorkerInstanceID: childLease.WorkerInstanceID, WorkerEpoch: childLease.WorkerEpoch,
			RuntimeInstanceID: childRuntime, FencingGeneration: childMountGeneration - 1,
			CleanupProof: []byte(`{"method":"session_closed","completed_at":"2026-08-20T00:00:00Z"}`),
		},
	); !errors.Is(err, pgx.ErrNoRows) {
		t.Fatalf("stale child cleanup proof error = %v, want no rows", err)
	}
	if _, err := db.New(fixture.pool).StopWorkspaceMount(
		fixture.ctx,
		db.StopWorkspaceMountParams{
			ReasonCode: pgvalue.Text("same_workspace_child_attempt_finished"),
			OrgID:      pgvalue.UUID(fixture.orgID), ID: childMount,
			WorkerInstanceID: childLease.WorkerInstanceID, WorkerEpoch: childLease.WorkerEpoch,
			RuntimeInstanceID: childRuntime, FencingGeneration: childMountGeneration,
			CleanupProof: []byte(`{"method":"session_closed","completed_at":"2026-08-20T00:00:00Z"}`),
		},
	); err != nil {
		t.Fatal(err)
	}
	reserved, err := fixture.authority.PlaceReadyRun(fixture.ctx, parentResume)
	if err != nil {
		t.Fatal(err)
	}
	if reserved.LeaseCreated || reserved.RuntimeInstanceID == childRuntime || reserved.WorkspaceMountID.Valid {
		t.Fatalf("parent restore reservation = %+v", reserved)
	}
	var restoreCheckpointID, targetVersionID pgtype.UUID
	if err := fixture.pool.QueryRow(fixture.ctx, `
SELECT restore_checkpoint_id, reserved_workspace_version_id
  FROM runtime_instances
 WHERE id = $1`, reserved.RuntimeInstanceID).Scan(
		&restoreCheckpointID, &targetVersionID,
	); err != nil {
		t.Fatal(err)
	}
	if restoreCheckpointID != pgvalue.UUID(bCheckpointID) ||
		targetVersionID != pgvalue.UUID(cVersionID) {
		t.Fatalf("parent reservation restore=%s target=%s, want B=%s C=%s",
			pgvalue.UUIDString(restoreCheckpointID), pgvalue.UUIDString(targetVersionID),
			bCheckpointID, cVersionID)
	}
}

func assertSameWorkspaceChildMountRejectsStaleReceipt(
	t *testing.T,
	fixture runPlacementFixture,
	runtimeID pgtype.UUID,
	childID uuid.UUID,
	baseVersionID uuid.UUID,
	waitID uuid.UUID,
) {
	t.Helper()
	q := db.New(fixture.pool)
	for _, test := range []struct {
		name   string
		column string
	}{
		{name: "ownership generation", column: "ownership_generation"},
		{name: "writer generation", column: "parent_writer_generation"},
	} {
		t.Run(test.name, func(t *testing.T) {
			dbtest.MustExec(t, fixture.ctx, fixture.pool, `UPDATE run_waits SET `+test.column+` = 2 WHERE id = $1`, waitID)
			t.Cleanup(func() {
				dbtest.MustExec(t, fixture.ctx, fixture.pool, `UPDATE run_waits SET `+test.column+` = 1 WHERE id = $1`, waitID)
			})
			_, err := q.EnsureRunWorkspaceMountRequested(
				fixture.ctx,
				db.EnsureRunWorkspaceMountRequestedParams{
					ID:                 pgvalue.UUID(uuid.NewV7()),
					Request:            []byte(`{"kind":"run"}`),
					OrgID:              pgvalue.UUID(fixture.orgID),
					WorkspaceID:        pgvalue.UUID(fixture.workspaceID),
					RuntimeInstanceID:  runtimeID,
					RunID:              pgvalue.UUID(childID),
					AttemptNumber:      pgtype.Int4{Int32: 1, Valid: true},
					WorkspaceVersionID: pgvalue.UUID(baseVersionID),
					FencingGeneration:  1,
				},
			)
			if !errors.Is(err, pgx.ErrNoRows) {
				t.Fatalf("stale %s mount error = %v, want no rows", test.name, err)
			}
			dbtest.MustExec(t, fixture.ctx, fixture.pool, `UPDATE run_waits SET `+test.column+` = 1 WHERE id = $1`, waitID)
		})
	}
}

func placeRunForTest(
	t *testing.T,
	fixture runPlacementFixture,
	candidate ReadyRunCandidate,
) (pgtype.UUID, pgtype.UUID, db.RunLease) {
	t.Helper()
	reserved, err := fixture.authority.PlaceReadyRun(fixture.ctx, candidate)
	if err != nil {
		t.Fatal(err)
	}
	markRunPlacementRuntimeReady(t, fixture, reserved.RuntimeInstanceID)
	mount, err := fixture.authority.PlaceReadyRun(fixture.ctx, candidate)
	if err != nil {
		t.Fatal(err)
	}
	markRunPlacementMountReady(t, fixture, mount.WorkspaceMountID)
	granted, err := fixture.authority.PlaceReadyRun(fixture.ctx, candidate)
	if err != nil {
		t.Fatal(err)
	}
	if !granted.LeaseCreated {
		t.Fatalf("run was not granted: %+v", granted)
	}
	return granted.RuntimeInstanceID, granted.WorkspaceMountID, granted.Lease
}

func reclaimRunRuntimeForTest(
	t *testing.T,
	fixture runPlacementFixture,
	tx pgx.Tx,
	runtimeID pgtype.UUID,
	mountID pgtype.UUID,
	materializedVersionID pgtype.UUID,
) {
	t.Helper()
	dbtest.MustExec(t, fixture.ctx, tx, `
UPDATE workspace_mounts
   SET materialized_version_id = $2, state = 'unmounted',
       unmounted_at = transaction_timestamp(), terminal_at = transaction_timestamp(),
       terminal_reason_code = 'checkpointed'
 WHERE id = $1`, mountID, materializedVersionID)
	dbtest.MustExec(t, fixture.ctx, tx, `
UPDATE runtime_instances
   SET desired_state = 'closed', desired_version = desired_version + 1,
       observed_state = 'closed', observed_desired_version = desired_version + 1,
       observed_version = observed_version + 1, closing_at = transaction_timestamp(),
       closed_at = transaction_timestamp(), terminal_at = transaction_timestamp(),
       terminal_reason_code = 'checkpointed', reclaimed_at = transaction_timestamp(),
       reclaim_evidence = jsonb_build_object(
           'method', 'session_closed',
           'completed_at', to_char(transaction_timestamp() AT TIME ZONE 'UTC',
                                   'YYYY-MM-DD"T"HH24:MI:SS.MS"Z"')
       ),
       reserved_run_id = NULL, reserved_attempt_number = NULL,
       reserved_workspace_version_id = NULL, reservation_expires_at = NULL
 WHERE id = $1`, runtimeID)
}
