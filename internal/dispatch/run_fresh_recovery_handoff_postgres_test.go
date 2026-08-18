package dispatch

import (
	"context"
	"strings"
	"testing"

	"github.com/google/uuid"
	"github.com/helmrdotdev/helmr/internal/db"
	"github.com/helmrdotdev/helmr/internal/db/dbtest"
	"github.com/helmrdotdev/helmr/internal/pgvalue"
	"github.com/helmrdotdev/helmr/internal/workspace"
	"github.com/jackc/pgx/v5/pgtype"
)

type freshHandoffRecoveryFixture struct {
	runPlacementFixture
	childID   uuid.UUID
	waitID    uuid.UUID
	leaseID   pgtype.UUID
	runtimeID pgtype.UUID
	mountID   pgtype.UUID
}

func TestFreshAssignedHandoffRecoveryRetainsHealthyRuntime(t *testing.T) {
	fixture := prepareFreshHandoffRecovery(t)

	recovered, err := fixture.authority.RecoverRunExecutionLeases(fixture.ctx, 10)
	if err != nil {
		t.Fatal(err)
	}
	if recovered != 1 {
		t.Fatalf("recovered = %d, want 1", recovered)
	}

	var childStatus, leaseState, workspaceLeaseState, runtimeDesired, runtimeObserved string
	var currentLease pgtype.UUID
	var attemptNumber int32
	var childWriter pgtype.Int8
	if err := fixture.pool.QueryRow(fixture.ctx, `
SELECT child.status, child.current_attempt_number, child.current_run_lease_id,
       run_leases.state, workspace_leases.state,
       runtime_instances.desired_state, runtime_instances.observed_state,
       handoff.child_writer_generation
  FROM runs AS child
  JOIN run_leases ON run_leases.id = $2
  JOIN workspace_leases ON workspace_leases.owner_run_lease_id = run_leases.id
  JOIN runtime_instances ON runtime_instances.id = $3
  JOIN run_waits AS handoff ON handoff.id = $4
 WHERE child.id = $1`, fixture.childID, fixture.leaseID, fixture.runtimeID, fixture.waitID).Scan(
		&childStatus, &attemptNumber, &currentLease, &leaseState, &workspaceLeaseState,
		&runtimeDesired, &runtimeObserved, &childWriter,
	); err != nil {
		t.Fatal(err)
	}
	if childStatus != "queued" || attemptNumber != 1 || currentLease.Valid ||
		leaseState != "expired" || workspaceLeaseState != "fenced" ||
		runtimeDesired != "ready" || runtimeObserved != "ready" ||
		!childWriter.Valid || childWriter.Int64 != 2 {
		t.Fatalf("retained handoff child=%s attempt=%d current=%v lease=%s workspace=%s runtime=%s/%s writer=%v",
			childStatus, attemptNumber, currentLease, leaseState, workspaceLeaseState,
			runtimeDesired, runtimeObserved, childWriter)
	}
	planning, err := db.New(fixture.pool).ListQueuedRunPlanningCandidatesForScopes(
		fixture.ctx,
		mustRunCandidateParams(t, planningScope{
			OrgID: pgvalue.UUID(fixture.orgID), ProjectID: pgvalue.UUID(fixture.projectID),
			EnvironmentID: pgvalue.UUID(fixture.environmentID), RegionID: "us-east-1",
			QueueName: "default",
		}, 10),
	)
	if err != nil {
		t.Fatal(err)
	}
	var workerPoolID pgtype.UUID
	if err := fixture.pool.QueryRow(fixture.ctx, `
SELECT worker_pool_id FROM worker_instances WHERE id = $1`, fixture.workerID).Scan(&workerPoolID); err != nil {
		t.Fatal(err)
	}
	if len(planning) != 1 || planning[0].RunID != pgvalue.UUID(fixture.childID) ||
		!planning[0].RequiresRetainedRuntime || planning[0].RetainedWorkerPoolID != workerPoolID {
		t.Fatalf("retained handoff planning candidates = %+v", planning)
	}

	regranted, err := fixture.authority.PlaceReadyRun(fixture.ctx, ReadyRunCandidate{
		OrgID: pgvalue.UUID(fixture.orgID), RunID: pgvalue.UUID(fixture.childID),
		ExpectedRunStateVersion: 3,
	})
	if err != nil {
		t.Fatal(err)
	}
	if !regranted.LeaseCreated || regranted.RuntimeInstanceID != fixture.runtimeID ||
		regranted.WorkspaceMountID != fixture.mountID || regranted.Lease.AttemptNumber != 1 {
		t.Fatalf("retained handoff regrant = %+v", regranted)
	}
}

func TestSameWorkspaceTaskRetryReplacesTerminalChildWriterReceipt(t *testing.T) {
	fixture := prepareFreshHandoffRecovery(t)
	dbtest.MustExec(t, fixture.ctx, fixture.pool, `
UPDATE run_leases
   SET state = 'failed', claimed_at = assigned_at, started_at = assigned_at,
       terminal_at = transaction_timestamp(), terminal_reason_code = 'task_failed',
       terminal_request_fingerprint = 'test-task-failed', updated_at = transaction_timestamp()
 WHERE id = $1`, fixture.leaseID)
	dbtest.MustExec(t, fixture.ctx, fixture.pool, `
UPDATE workspace_leases
   SET state = 'released', released_at = transaction_timestamp(),
       terminal_at = transaction_timestamp()
 WHERE owner_run_lease_id = $1`, fixture.leaseID)
	dbtest.MustExec(t, fixture.ctx, fixture.pool, `
UPDATE run_attempts
   SET terminal_outcome = 'failed', terminal_reason_code = 'task_failed',
       terminal_at = transaction_timestamp()
 WHERE run_id = $1 AND number = 1`, fixture.childID)
	dbtest.MustExec(t, fixture.ctx, fixture.pool, `
INSERT INTO run_attempts (
    run_id, number, entrypoint_kind, workspace_id, base_workspace_version_id
)
SELECT run_id, 2, entrypoint_kind, workspace_id, base_workspace_version_id
  FROM run_attempts
 WHERE run_id = $1 AND number = 1`, fixture.childID)
	dbtest.MustExec(t, fixture.ctx, fixture.pool, `
UPDATE runs
   SET status = 'queued', current_attempt_number = 2,
       current_run_lease_id = NULL, state_version = state_version + 1,
       queue_score_at = transaction_timestamp(), next_runtime_preparation_at = NULL
 WHERE id = $1`, fixture.childID)

	regranted, err := fixture.authority.PlaceReadyRun(fixture.ctx, ReadyRunCandidate{
		OrgID: pgvalue.UUID(fixture.orgID), RunID: pgvalue.UUID(fixture.childID),
		ExpectedRunStateVersion: 3,
	})
	if err != nil {
		t.Fatal(err)
	}
	if !regranted.LeaseCreated || regranted.RuntimeInstanceID != fixture.runtimeID ||
		regranted.WorkspaceMountID != fixture.mountID || regranted.Lease.AttemptNumber != 2 {
		t.Fatalf("same-Workspace retry regrant = %+v", regranted)
	}
	var childWriter pgtype.Int8
	if err := fixture.pool.QueryRow(fixture.ctx, `
SELECT child_writer_generation FROM run_waits WHERE id = $1`, fixture.waitID).Scan(&childWriter); err != nil {
		t.Fatal(err)
	}
	if !childWriter.Valid || childWriter.Int64 != 3 {
		t.Fatalf("replacement child writer = %v, want 3", childWriter)
	}
}

func TestFreshAmbiguousHandoffRecoveryUnwindsToParentCheckpoint(t *testing.T) {
	for _, tc := range []struct {
		name  string
		state string
		loss  string
	}{
		{name: "starting", state: "starting"},
		{name: "running despite retry policy", state: "running"},
		{name: "assigned physical loss", state: "assigned", loss: "worker_epoch"},
		{name: "assigned cleanup already started", state: "assigned", loss: "workspace_lease_releasing"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			fixture := prepareFreshHandoffRecovery(t)
			switch tc.state {
			case "starting":
				dbtest.MustExec(t, fixture.ctx, fixture.pool, `
UPDATE run_leases SET state = 'starting', claimed_at = assigned_at WHERE id = $1`, fixture.leaseID)
			case "running":
				dbtest.MustExec(t, fixture.ctx, fixture.pool, `
UPDATE run_leases
   SET state = 'running', claimed_at = assigned_at, started_at = assigned_at,
       expires_at = transaction_timestamp() - interval '1 second'
 WHERE id = $1`, fixture.leaseID)
				dbtest.MustExec(t, fixture.ctx, fixture.pool, `
UPDATE runs
   SET status = 'running', active_started_at = transaction_timestamp() - interval '10 seconds',
       started_at = transaction_timestamp() - interval '10 seconds',
       retry_policy = '{"backoff":{"factor":1,"jitter":"none","maxMs":1,"minMs":1},"enabled":true,"maxAttempts":2}'::jsonb,
       state_version = state_version + 1
 WHERE id = $1`, fixture.childID)
			}
			if tc.loss == "worker_epoch" {
				dbtest.MustExec(t, fixture.ctx, fixture.pool, `
UPDATE worker_instances
   SET current_epoch = current_epoch + 1, epoch_started_at = transaction_timestamp()
 WHERE id = $1`, fixture.workerID)
			}
			if tc.loss == "workspace_lease_releasing" {
				dbtest.MustExec(t, fixture.ctx, fixture.pool, `
UPDATE workspace_leases
   SET state = 'releasing'
 WHERE owner_run_lease_id = $1`, fixture.leaseID)
			}

			recovered, err := fixture.authority.RecoverRunExecutionLeases(fixture.ctx, 10)
			if err != nil {
				t.Fatal(err)
			}
			if recovered != 1 {
				t.Fatalf("recovered = %d, want 1", recovered)
			}

			var childStatus, reason, parentStatus, conditionState, suspensionState, runtimeDesired string
			var mountState, mountFinalizationKind, mountFinalizationReason string
			var currentLease pgtype.UUID
			var attempts int
			if err := fixture.pool.QueryRow(fixture.ctx, `
SELECT child.status, child.current_run_lease_id,
       child_attempt.terminal_reason_code,
       (SELECT count(*) FROM run_attempts WHERE run_id = child.id),
       parent.status, handoff.condition_state, handoff.suspension_state,
       runtime_instances.desired_state, workspace_mounts.state,
       workspace_mounts.finalization_kind,
       workspace_mounts.finalization_reason_code
  FROM runs AS child
  JOIN run_attempts AS child_attempt
    ON child_attempt.run_id = child.id AND child_attempt.number = 1
  JOIN runs AS parent ON parent.id = $2
  JOIN run_waits AS handoff ON handoff.id = $3
  JOIN runtime_instances ON runtime_instances.id = $4
  JOIN workspace_mounts ON workspace_mounts.id = $5
 WHERE child.id = $1`, fixture.childID, fixture.runID, fixture.waitID, fixture.runtimeID, fixture.mountID).Scan(
				&childStatus, &currentLease, &reason, &attempts, &parentStatus,
				&conditionState, &suspensionState, &runtimeDesired, &mountState,
				&mountFinalizationKind, &mountFinalizationReason,
			); err != nil {
				t.Fatal(err)
			}
			if childStatus != "system_failed" || currentLease.Valid ||
				reason != "same_workspace_handoff_runtime_lost" || attempts != 1 ||
				parentStatus != "queued" || conditionState != "failed" ||
				suspensionState != "resume_pending" || runtimeDesired != "closed" ||
				mountState != "unmounting" || mountFinalizationKind != "discard" ||
				mountFinalizationReason != "same_workspace_handoff_runtime_lost" {
				t.Fatalf("unwind child=%s current=%v reason=%s attempts=%d parent=%s wait=%s/%s runtime=%s",
					childStatus, currentLease, reason, attempts, parentStatus,
					conditionState, suspensionState, runtimeDesired)
			}
		})
	}
}

func prepareFreshHandoffRecovery(t *testing.T) freshHandoffRecoveryFixture {
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
	markRunPlacementMountReady(t, fixture, mounting.WorkspaceMountID)
	parent, err := fixture.authority.PlaceReadyRun(fixture.ctx, fixture.candidate())
	if err != nil {
		t.Fatal(err)
	}

	var parentWorkspaceLeaseID, originalVersionID, taskDefinitionID pgtype.UUID
	if err := fixture.pool.QueryRow(fixture.ctx, `
SELECT workspace_leases.id, workspace_leases.base_version_id,
       runs.deployment_definition_id
  FROM workspace_leases JOIN runs ON runs.id = $2
 WHERE workspace_leases.owner_run_lease_id = $1`, parent.Lease.ID, fixture.runID).Scan(
		&parentWorkspaceLeaseID, &originalVersionID, &taskDefinitionID,
	); err != nil {
		t.Fatal(err)
	}

	claimID, waitID := uuid.Must(uuid.NewV7()), uuid.Must(uuid.NewV7())
	checkpointID, resumeAttachID := uuid.Must(uuid.NewV7()), uuid.Must(uuid.NewV7())
	childID, privateVersionID := uuid.Must(uuid.NewV7()), uuid.Must(uuid.NewV7())
	privateArtifactID := uuid.Must(uuid.NewV7())
	privateDigest := "sha256:" + strings.Repeat("9", 64)
	tx, err := fixture.pool.Begin(fixture.ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = tx.Rollback(context.Background()) }()
	dbtest.MustExec(t, fixture.ctx, tx, `SET CONSTRAINTS ALL DEFERRED`)
	dbtest.MustExec(t, fixture.ctx, tx, `
INSERT INTO idempotency_claims (
    id, environment_id, operation, slot_hash, request_fingerprint, accepted_at
) VALUES ($1, $2, 'task.child.invoke', decode(repeat('12', 32), 'hex'),
          decode(repeat('14', 32), 'hex'), now())`, claimID, fixture.environmentID)
	dbtest.MustExec(t, fixture.ctx, tx, `
INSERT INTO cas_objects (org_id, digest, size_bytes, media_type)
VALUES ($1, $2, 1, $3)`, fixture.orgID, privateDigest, workspace.ArtifactMediaType)
	dbtest.MustExec(t, fixture.ctx, tx, `
INSERT INTO artifacts (
    id, org_id, project_id, environment_id, digest, kind, size_bytes, media_type
) VALUES ($1, $2, $3, $4, $5, 'workspace_version', 1, $6)`,
		privateArtifactID, fixture.orgID, fixture.projectID, fixture.environmentID,
		privateDigest, workspace.ArtifactMediaType)
	dbtest.MustExec(t, fixture.ctx, tx, `
INSERT INTO workspace_versions (
    id, environment_id, workspace_id, parent_version_id, artifact_id,
    artifact_kind, kind, content_digest, size_bytes, entry_count, state,
    source_workspace_lease_id, ownership_generation, writer_generation
) VALUES ($1, $2, $3, $4, $5, 'workspace_version', 'user', $6, 1, 1,
          'private', $7, 1, 1)`, privateVersionID, fixture.environmentID,
		fixture.workspaceID, originalVersionID, privateArtifactID, privateDigest,
		parentWorkspaceLeaseID)
	dbtest.MustExec(t, fixture.ctx, tx, `
INSERT INTO runs (
    id, org_id, project_id, environment_id, deployment_id,
    deployment_definition_id, entrypoint_kind, entrypoint_declared_id,
    cause_kind, parent_run_id, parent_owns_lifecycle, workspace_id,
    base_workspace_version_id, payload, queue_name, queue_origin_at,
    queue_score_at, max_active_duration_ms, retry_policy, trace_id,
    root_span_id, claim_id
) VALUES ($1, $2, $3, $4, $5, $6, 'task', 'test-task', 'child',
          $7, true, $8, $9, '{}'::jsonb, 'default', now(), now(), 300000,
          '{"enabled":false}'::jsonb,
          '33333333333333333333333333333333', '4444444444444444', $10)`,
		childID, fixture.orgID, fixture.projectID, fixture.environmentID,
		fixture.deploymentID, taskDefinitionID, fixture.runID, fixture.workspaceID,
		privateVersionID, claimID)
	dbtest.MustExec(t, fixture.ctx, tx, `
INSERT INTO run_attempts (
    run_id, number, entrypoint_kind, workspace_id, base_workspace_version_id
) VALUES ($1, 1, 'task', $2, $3)`, childID, fixture.workspaceID, privateVersionID)
	dbtest.MustExec(t, fixture.ctx, tx, `
INSERT INTO run_waits (
    id, environment_id, run_id, workspace_id, kind, child_run_id,
    child_parent_owned, child_target_declared_id, child_claim_id,
    child_request, expected_run_state_version, attempt_number,
    prior_run_lease_id, resume_attach_id, suspension_state
) VALUES ($1, $2, $3, $4, 'child', $5, true, 'test-task', $6,
          '{"Method":"call"}'::jsonb, 3, 1, $7, $8, 'parked')`,
		waitID, fixture.environmentID, fixture.runID, fixture.workspaceID,
		childID, claimID, parent.Lease.ID, resumeAttachID)
	dbtest.MustExec(t, fixture.ctx, tx, `
INSERT INTO run_checkpoints (
    id, kind, run_id, attempt_number, run_wait_id, source_run_lease_id,
    source_workspace_lease_id, workspace_id, base_workspace_version_id,
    private_workspace_version_id, state, restore_manifest,
    ready_request_fingerprint, ready_at
) VALUES ($1, 'suspend', $2, 1, $3, $4, $5, $6, $7, $8,
          'ready', '{"kind":"suspend"}'::jsonb, 'test-ready', now())`,
		checkpointID, fixture.runID, waitID, parent.Lease.ID,
		parentWorkspaceLeaseID, fixture.workspaceID, originalVersionID, privateVersionID)
	dbtest.MustExec(t, fixture.ctx, tx, `
UPDATE run_waits
   SET suspend_checkpoint_id = $2, base_workspace_version_id = $3,
       base_workspace_content_digest = $4, handoff_runtime_instance_id = $5,
       handoff_workspace_mount_id = $6, handoff_mount_generation = 2,
       ownership_generation = 1, parent_writer_generation = 1
 WHERE id = $1`, waitID, checkpointID, privateVersionID, privateDigest,
		reserved.RuntimeInstanceID, mounting.WorkspaceMountID)
	dbtest.MustExec(t, fixture.ctx, tx, `
UPDATE runs
   SET status = 'waiting', state_version = 3,
       current_run_lease_id = NULL, active_started_at = NULL
 WHERE id = $1`, fixture.runID)
	dbtest.MustExec(t, fixture.ctx, tx, `
UPDATE run_leases
   SET state = 'checkpointed', claimed_at = assigned_at, started_at = assigned_at,
       checkpointed_at = now(), terminal_at = now(), terminal_reason_code = 'checkpointed'
 WHERE id = $1`, parent.Lease.ID)
	dbtest.MustExec(t, fixture.ctx, tx, `
UPDATE workspace_leases
   SET state = 'released', released_at = now(), terminal_at = now()
 WHERE id = $1`, parentWorkspaceLeaseID)
	dbtest.MustExec(t, fixture.ctx, tx, `
UPDATE workspace_mounts
   SET materialized_version_id = $2, dirty_generation = 1
 WHERE id = $1`, mounting.WorkspaceMountID, privateVersionID)
	if err := tx.Commit(fixture.ctx); err != nil {
		t.Fatal(err)
	}

	granted, err := fixture.authority.PlaceReadyRun(fixture.ctx, ReadyRunCandidate{
		OrgID: pgvalue.UUID(fixture.orgID), RunID: pgvalue.UUID(childID),
		ExpectedRunStateVersion: 1,
	})
	if err != nil {
		t.Fatal(err)
	}
	if !granted.LeaseCreated {
		t.Fatalf("same-Workspace child placement = %+v", granted)
	}
	dbtest.MustExec(t, fixture.ctx, fixture.pool, `
UPDATE run_leases
   SET assigned_at = transaction_timestamp() - interval '10 minutes',
       start_deadline_at = transaction_timestamp() - interval '9 minutes',
       expires_at = transaction_timestamp() + interval '5 minutes'
 WHERE id = $1`, granted.Lease.ID)
	return freshHandoffRecoveryFixture{
		runPlacementFixture: fixture, childID: childID, waitID: waitID,
		leaseID: granted.Lease.ID, runtimeID: reserved.RuntimeInstanceID,
		mountID: mounting.WorkspaceMountID,
	}
}
