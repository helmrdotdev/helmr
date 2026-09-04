package controlplane

import (
	"bytes"
	"context"
	"os"
	"testing"
	"time"
	"uuid"

	"github.com/helmrdotdev/helmr/internal/cas"
	"github.com/helmrdotdev/helmr/internal/db"
	"github.com/helmrdotdev/helmr/internal/db/dbtest"
	"github.com/helmrdotdev/helmr/internal/run/runtest"
	"github.com/helmrdotdev/helmr/internal/workerapi"
	"github.com/helmrdotdev/helmr/internal/workspace"
)

func TestRestoredActorCompletionAdvancesFromPrivateLeaseBase(t *testing.T) {
	fixture := newRestoredActorCompletionPostgresFixture(t, false)
	completion, err := parseActorCompletionRequest(fixture.request)
	if err != nil {
		t.Fatal(err)
	}
	if err := fixture.server.completeActor(t.Context(), fixture.worker, fixture.request, completion); err != nil {
		t.Fatal(err)
	}

	var runStatus, leaseState, attemptOutcome, workspaceLeaseState string
	var headVersionID, mountVersionID, publishedParentID uuid.UUID
	var committedInput, terminalInput int64
	if err := fixture.pool.QueryRow(t.Context(), `
SELECT runs.status,
       run_leases.state,
       run_attempts.terminal_outcome,
       workspace_leases.state,
       workspaces.head_version_id,
       workspace_mounts.materialized_version_id,
       published.parent_version_id,
       sessions.committed_input_sequence,
       run_attempts.terminal_session_input_sequence
  FROM runs
  JOIN run_leases ON run_leases.id = $2
  JOIN run_attempts ON run_attempts.run_id = runs.id AND run_attempts.number = 1
  JOIN sessions ON sessions.id = runs.session_id
  JOIN workspaces ON workspaces.id = runs.workspace_id
  JOIN workspace_leases ON workspace_leases.owner_run_lease_id = run_leases.id
  JOIN workspace_mounts ON workspace_mounts.id = workspace_leases.workspace_mount_id
  JOIN workspace_versions AS published ON published.id = workspaces.head_version_id
 WHERE runs.id = $1`, fixture.runID, fixture.leaseID).Scan(
		&runStatus, &leaseState, &attemptOutcome, &workspaceLeaseState,
		&headVersionID, &mountVersionID, &publishedParentID,
		&committedInput, &terminalInput,
	); err != nil {
		t.Fatal(err)
	}
	if runStatus != "succeeded" || leaseState != "completed" || attemptOutcome != "succeeded" || workspaceLeaseState != "released" {
		t.Fatalf("terminal state = run:%s lease:%s attempt:%s workspace lease:%s",
			runStatus, leaseState, attemptOutcome, workspaceLeaseState)
	}
	if headVersionID == fixture.headVersionID || headVersionID == fixture.privateVersionID ||
		mountVersionID != headVersionID || publishedParentID != fixture.privateVersionID {
		t.Fatalf("Workspace frontier = head:%s mount:%s parent:%s; want new D mounted with parent C:%s",
			headVersionID, mountVersionID, publishedParentID, fixture.privateVersionID)
	}
	if committedInput != 2 || terminalInput != 2 {
		t.Fatalf("Actor cursor = committed:%d terminal:%d", committedInput, terminalInput)
	}
}

func TestRestoredActorFailureRollsMountBackToDurableHead(t *testing.T) {
	fixture := newRestoredActorCompletionPostgresFixture(t, true)
	completion, err := parseActorCompletionRequest(fixture.request)
	if err != nil {
		t.Fatal(err)
	}
	if err := fixture.server.completeActor(t.Context(), fixture.worker, fixture.request, completion); err != nil {
		t.Fatal(err)
	}

	var runStatus, leaseState, attemptOutcome, workspaceLeaseState, actorState string
	var headVersionID, mountVersionID uuid.UUID
	var committedInput int64
	if err := fixture.pool.QueryRow(t.Context(), `
SELECT runs.status,
       run_leases.state,
       run_attempts.terminal_outcome,
       workspace_leases.state,
       sessions.state,
       workspaces.head_version_id,
       workspace_mounts.materialized_version_id,
       sessions.committed_input_sequence
  FROM runs
  JOIN run_leases ON run_leases.id = $2
  JOIN run_attempts ON run_attempts.run_id = runs.id AND run_attempts.number = 1
  JOIN sessions ON sessions.id = runs.session_id
  JOIN workspaces ON workspaces.id = runs.workspace_id
  JOIN workspace_leases ON workspace_leases.owner_run_lease_id = run_leases.id
  JOIN workspace_mounts ON workspace_mounts.id = workspace_leases.workspace_mount_id
 WHERE runs.id = $1`, fixture.runID, fixture.leaseID).Scan(
		&runStatus, &leaseState, &attemptOutcome, &workspaceLeaseState, &actorState,
		&headVersionID, &mountVersionID, &committedInput,
	); err != nil {
		t.Fatal(err)
	}
	if runStatus != "failed" || leaseState != "failed" || attemptOutcome != "failed" ||
		workspaceLeaseState != "released" || actorState != "failed" {
		t.Fatalf("terminal state = run:%s lease:%s attempt:%s workspace lease:%s Actor:%s",
			runStatus, leaseState, attemptOutcome, workspaceLeaseState, actorState)
	}
	if headVersionID != fixture.headVersionID || mountVersionID != fixture.headVersionID {
		t.Fatalf("rollback frontier = head:%s mount:%s, want B:%s", headVersionID, mountVersionID, fixture.headVersionID)
	}
	if committedInput != 1 {
		t.Fatalf("failed Actor advanced committed input to %d", committedInput)
	}
}

type restoredActorCompletionPostgresFixture struct {
	server           *Server
	pool             db.DBTX
	worker           workerActor
	request          workerapi.CompleteActorRequest
	runID            uuid.UUID
	leaseID          uuid.UUID
	headVersionID    uuid.UUID
	privateVersionID uuid.UUID
}

func newRestoredActorCompletionPostgresFixture(t *testing.T, rollback bool) restoredActorCompletionPostgresFixture {
	t.Helper()
	base := runtest.New(t)
	work := base.AddRunLease(t, "starting", time.Now().Add(-time.Minute))
	ctx := t.Context()
	base.ConvertToActor(t, ctx, work, `{"enabled":false}`)

	var workspaceID, headVersionID, runtimeID, mountID, workspaceLeaseID uuid.UUID
	var ownershipGeneration, writerGeneration, mountGeneration int64
	if err := base.Pool.QueryRow(ctx, `
SELECT runs.workspace_id,
       workspaces.head_version_id,
       run_leases.runtime_instance_id,
       workspace_leases.workspace_mount_id,
       workspace_leases.id,
       workspaces.ownership_generation,
       workspaces.writer_generation,
       workspace_mounts.fencing_generation
  FROM runs
  JOIN workspaces ON workspaces.id = runs.workspace_id
  JOIN run_leases ON run_leases.id = $2 AND run_leases.run_id = runs.id
  JOIN workspace_leases ON workspace_leases.owner_run_lease_id = run_leases.id
  JOIN workspace_mounts ON workspace_mounts.id = workspace_leases.workspace_mount_id
 WHERE runs.id = $1`, work.RunID, work.LeaseID).Scan(
		&workspaceID, &headVersionID, &runtimeID, &mountID, &workspaceLeaseID,
		&ownershipGeneration, &writerGeneration, &mountGeneration,
	); err != nil {
		t.Fatal(err)
	}

	privateVersionID := uuid.NewV7()
	privateArtifactID := uuid.NewV7()
	checkpointVersionID := uuid.NewV7()
	checkpointArtifactID := uuid.NewV7()
	sourceRuntimeID := uuid.NewV7()
	sourceMountID := uuid.NewV7()
	sourceLeaseID := uuid.NewV7()
	sourceWorkspaceLeaseID := uuid.NewV7()
	childRunID := uuid.NewV7()
	childClaimID := uuid.NewV7()
	childLeaseID := uuid.NewV7()
	childWorkspaceLeaseID := uuid.NewV7()
	waitID := uuid.NewV7()
	checkpointID := uuid.NewV7()
	operationID := uuid.NewV7()
	expiresAt := time.Now().Add(10 * time.Minute).UTC().Truncate(time.Microsecond)
	privateDigest := dbtest.Digest("restored-actor-private-base")
	checkpointDigest := dbtest.Digest("restored-actor-checkpoint-base")

	tx, err := base.Pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = tx.Rollback(context.Background()) }()
	dbtest.MustExec(t, ctx, tx, `SET CONSTRAINTS ALL DEFERRED`)
	dbtest.MustExec(t, ctx, tx, `
UPDATE workspaces SET writer_generation = 3 WHERE id = $1`, workspaceID)
	dbtest.MustExec(t, ctx, tx, `
UPDATE workspace_leases SET writer_generation = 3 WHERE id = $1`, workspaceLeaseID)
	dbtest.MustExec(t, ctx, tx, `
UPDATE run_leases SET lease_sequence = 2 WHERE id = $1`, work.LeaseID)
	dbtest.MustExec(t, ctx, tx, `
INSERT INTO runtime_instances (
    id, org_id, worker_group_id, project_id, environment_id, region_id,
    worker_instance_id, runtime_identity_id, deployment_definition_id,
    runtime_substrate_id, worker_epoch, vm_vcpu_count, cpu_config_digest,
    reserved_cpu_millis, reserved_memory_bytes,
    reserved_guest_ephemeral_disk_bytes, reserved_execution_slots,
    workspace_id, program_deployment_id, desired_state, desired_version,
    desired_at, desired_reason, observed_state, observed_version,
    observed_desired_version, observed_at, allocated_at, ready_at,
    reclaimed_at, reclaim_evidence, terminal_at,
    terminal_reason_code
)
SELECT $2, org_id, worker_group_id, project_id, environment_id, region_id,
       worker_instance_id, runtime_identity_id, deployment_definition_id,
       runtime_substrate_id, worker_epoch, vm_vcpu_count, cpu_config_digest,
       reserved_cpu_millis, reserved_memory_bytes,
       reserved_guest_ephemeral_disk_bytes, reserved_execution_slots,
       workspace_id, program_deployment_id, 'closed', 2,
       transaction_timestamp(), 'checkpointed', 'closed', 2, 2,
       transaction_timestamp(), allocated_at, ready_at,
       transaction_timestamp(),
       jsonb_build_object('method', 'session_closed', 'completed_at', to_char(transaction_timestamp() AT TIME ZONE 'UTC', 'YYYY-MM-DD"T"HH24:MI:SS.MS"Z"')),
       transaction_timestamp(), 'checkpointed'
  FROM runtime_instances WHERE id = $1`, runtimeID, sourceRuntimeID)
	dbtest.MustExec(t, ctx, tx, `
INSERT INTO workspace_mounts (
    id, org_id, worker_group_id, project_id, environment_id, region_id,
    worker_instance_id, worker_epoch, workspace_id, materialized_version_id,
    runtime_instance_id, state, fencing_generation, mounted_at,
    unmounted_at, terminal_at, terminal_reason_code
)
SELECT $2, org_id, worker_group_id, project_id, environment_id, region_id,
       worker_instance_id, worker_epoch, workspace_id, materialized_version_id,
       $3, 'unmounted', 1, mounted_at,
       transaction_timestamp(), transaction_timestamp(), 'checkpointed'
  FROM workspace_mounts WHERE id = $1`, mountID, sourceMountID, sourceRuntimeID)
	dbtest.MustExec(t, ctx, tx, `
INSERT INTO run_leases (
    id, org_id, project_id, environment_id, run_id, workspace_id, region_id,
    lease_sequence, attempt_number, worker_group_id, worker_instance_id,
    worker_epoch, runtime_instance_id, runtime_identity_id,
    requested_cpu_millis, requested_memory_bytes,
    requested_guest_ephemeral_disk_bytes, requested_execution_slots,
    trace_id, span_id, parent_span_id, traceparent,
    state, assigned_at, start_deadline_at, claimed_at, started_at, expires_at,
    checkpointed_at, terminal_at, terminal_reason_code
)
SELECT $2, org_id, project_id, environment_id, run_id, workspace_id, region_id,
       1, attempt_number, worker_group_id, worker_instance_id,
       worker_epoch, $3, runtime_identity_id,
       requested_cpu_millis, requested_memory_bytes,
       requested_guest_ephemeral_disk_bytes, requested_execution_slots,
       trace_id, span_id, parent_span_id, traceparent,
       'checkpointed', assigned_at - interval '1 minute',
       start_deadline_at - interval '1 minute', assigned_at - interval '1 minute',
       assigned_at - interval '1 minute', expires_at,
       transaction_timestamp(), transaction_timestamp(), 'checkpointed'
  FROM run_leases WHERE id = $1`, work.LeaseID, sourceLeaseID, sourceRuntimeID)
	dbtest.MustExec(t, ctx, tx, `
INSERT INTO workspace_leases (
    id, org_id, worker_group_id, project_id, environment_id, region_id,
    worker_instance_id, worker_epoch, runtime_instance_id, workspace_id,
    workspace_mount_id, state, owner_run_lease_id, base_version_id,
    ownership_generation, writer_generation, mount_fencing_generation,
    fencing_token_hash, acquired_at, renewed_at, expires_at,
    released_at, terminal_at
)
SELECT $2, org_id, worker_group_id, project_id, environment_id, region_id,
       worker_instance_id, worker_epoch, $3, workspace_id,
       $4, 'released', $5, $6,
       ownership_generation, 1, 1,
       'restored-actor-source', acquired_at, renewed_at, expires_at,
       transaction_timestamp(), transaction_timestamp()
  FROM workspace_leases WHERE id = $1`, workspaceLeaseID, sourceWorkspaceLeaseID,
		sourceRuntimeID, sourceMountID, sourceLeaseID, headVersionID)
	dbtest.MustExec(t, ctx, tx, `
INSERT INTO cas_objects (org_id, digest, size_bytes, media_type)
VALUES ($1, $2, 1, $3)`, base.OrgID, checkpointDigest, workspace.ArtifactMediaType)
	dbtest.MustExec(t, ctx, tx, `
INSERT INTO artifacts (
    id, org_id, project_id, environment_id, digest, kind, size_bytes, media_type
) VALUES ($1, $2, $3, $4, $5, 'workspace_version', 1, $6)`,
		checkpointArtifactID, base.OrgID, base.ProjectID, base.EnvironmentID,
		checkpointDigest, workspace.ArtifactMediaType)
	dbtest.MustExec(t, ctx, tx, `
INSERT INTO workspace_versions (
    id, environment_id, workspace_id, parent_version_id,
    artifact_id, artifact_kind, kind, content_digest, size_bytes, entry_count,
    state, source_workspace_lease_id, ownership_generation, writer_generation
) VALUES (
    $1, $2, $3, $4, $5, 'workspace_version', 'user', $6, 1, 1,
    'private', $7, $8, 1
)`, checkpointVersionID, base.EnvironmentID, workspaceID, headVersionID,
		checkpointArtifactID, checkpointDigest, sourceWorkspaceLeaseID, ownershipGeneration)
	dbtest.MustExec(t, ctx, tx, `
INSERT INTO idempotency_claims (
    id, environment_id, operation, slot_hash, request_fingerprint, accepted_at
) VALUES (
    $1, $2, 'task.child.invoke', decode(repeat('62', 32), 'hex'),
    decode(repeat('64', 32), 'hex'), transaction_timestamp()
)`, childClaimID, base.EnvironmentID)
	dbtest.MustExec(t, ctx, tx, `
INSERT INTO runs (
    id, org_id, project_id, environment_id, deployment_id,
    deployment_definition_id, entrypoint_kind, entrypoint_declared_id,
    cause_kind, parent_run_id, parent_owns_lifecycle, workspace_id,
    base_workspace_version_id, payload, queue_name, queue_origin_at,
    queue_score_at, max_active_duration_ms, retry_policy, trace_id,
    root_span_id, claim_id, status, started_at, terminal_at, output
) VALUES (
    $1, $2, $3, $4, $5, $6, 'task', 'test-task',
    'child', $7, true, $8, $9, '{}'::jsonb, 'default', transaction_timestamp(),
    transaction_timestamp(), 300000, '{"enabled":false}'::jsonb,
    '99999999999999999999999999999999', 'aaaaaaaaaaaaaaaa', $10,
    'succeeded', transaction_timestamp(), transaction_timestamp(), '{}'::jsonb
)`, childRunID, base.OrgID, base.ProjectID, base.EnvironmentID, base.DeploymentID,
		base.TaskDefinitionID, work.RunID, workspaceID, checkpointVersionID, childClaimID)
	dbtest.MustExec(t, ctx, tx, `
INSERT INTO run_attempts (
    run_id, number, entrypoint_kind, workspace_id, base_workspace_version_id,
    entrypoint_entered_at, terminal_session_input_sequence,
    terminal_outcome, terminal_reason_code, terminal_at
) VALUES (
    $1, 1, 'task', $2, $3, transaction_timestamp(), NULL,
    'succeeded', 'completed', transaction_timestamp()
)`, childRunID, workspaceID, checkpointVersionID)
	dbtest.MustExec(t, ctx, tx, `
INSERT INTO run_leases (
    id, org_id, project_id, environment_id, run_id, workspace_id, region_id,
    lease_sequence, attempt_number, worker_group_id, worker_instance_id,
    worker_epoch, runtime_instance_id, runtime_identity_id,
    requested_cpu_millis, requested_memory_bytes,
    requested_guest_ephemeral_disk_bytes, requested_execution_slots,
    state, assigned_at, start_deadline_at, claimed_at, started_at, expires_at,
    terminal_at, terminal_reason_code
)
SELECT $2, org_id, project_id, environment_id, $3, workspace_id, region_id,
       1, 1, worker_group_id, worker_instance_id,
       worker_epoch, $4, runtime_identity_id,
       requested_cpu_millis, requested_memory_bytes,
       requested_guest_ephemeral_disk_bytes, requested_execution_slots,
       'completed', assigned_at, start_deadline_at, assigned_at, assigned_at,
       expires_at, transaction_timestamp(), 'completed'
  FROM run_leases WHERE id = $1`, work.LeaseID, childLeaseID, childRunID, sourceRuntimeID)
	dbtest.MustExec(t, ctx, tx, `
INSERT INTO workspace_leases (
    id, org_id, worker_group_id, project_id, environment_id, region_id,
    worker_instance_id, worker_epoch, runtime_instance_id, workspace_id,
    workspace_mount_id, state, owner_run_lease_id, base_version_id,
    ownership_generation, writer_generation, mount_fencing_generation,
    fencing_token_hash, acquired_at, renewed_at, expires_at,
    released_at, terminal_at
)
SELECT $2, org_id, worker_group_id, project_id, environment_id, region_id,
       worker_instance_id, worker_epoch, $3, workspace_id,
       $4, 'released', $5, $6,
       ownership_generation, 2, 1,
       'restored-actor-child', acquired_at, renewed_at, expires_at,
       transaction_timestamp(), transaction_timestamp()
  FROM workspace_leases WHERE id = $1`, workspaceLeaseID, childWorkspaceLeaseID,
		sourceRuntimeID, sourceMountID, childLeaseID, checkpointVersionID)
	dbtest.MustExec(t, ctx, tx, `
INSERT INTO cas_objects (org_id, digest, size_bytes, media_type)
VALUES ($1, $2, 1, $3)`, base.OrgID, privateDigest, workspace.ArtifactMediaType)
	dbtest.MustExec(t, ctx, tx, `
INSERT INTO artifacts (
    id, org_id, project_id, environment_id, digest, kind, size_bytes, media_type
) VALUES ($1, $2, $3, $4, $5, 'workspace_version', 1, $6)`,
		privateArtifactID, base.OrgID, base.ProjectID, base.EnvironmentID,
		privateDigest, workspace.ArtifactMediaType)
	dbtest.MustExec(t, ctx, tx, `
INSERT INTO workspace_versions (
    id, environment_id, workspace_id, parent_version_id,
    artifact_id, artifact_kind, kind, content_digest, size_bytes, entry_count,
    state, source_workspace_lease_id, ownership_generation, writer_generation
) VALUES (
    $1, $2, $3, $4, $5, 'workspace_version', 'user', $6, 1, 1,
    'private', $7, $8, $9
)`, privateVersionID, base.EnvironmentID, workspaceID, checkpointVersionID,
		privateArtifactID, privateDigest, childWorkspaceLeaseID, ownershipGeneration, int64(2))
	dbtest.MustExec(t, ctx, tx, `
INSERT INTO run_waits (
    id, environment_id, run_id, workspace_id, kind,
    child_run_id, child_parent_owned, child_target_declared_id,
    child_claim_id, child_request,
    condition_state, suspension_state, expected_run_state_version,
    attempt_number, prior_run_lease_id, resume_attach_id
) VALUES (
    $1, $2, $3, $4, 'child', $5, true, 'test-task', $6,
    '{"Method":"call"}'::jsonb,
    'pending', 'parked', 1, 1, $7, $8
)`, waitID, base.EnvironmentID, work.RunID, workspaceID, childRunID, childClaimID,
		sourceLeaseID, uuid.NewV7())
	checkpointArtifacts := dbtest.InsertCheckpointArtifacts(t, ctx, tx, work.RunID, checkpointID.String())
	dbtest.MustExec(t, ctx, tx, `
INSERT INTO run_checkpoints (
    id, run_id, attempt_number, run_wait_id, source_run_lease_id,
    source_workspace_lease_id, workspace_id, base_workspace_version_id,
    private_workspace_version_id, actor_speculative_input_sequence,
    runtime_config_artifact_id, vm_state_artifact_id,
    memory_artifact_id, scratch_disk_artifact_id,
    state, restore_manifest,
    ready_request_fingerprint, ready_at
) VALUES (
    $1, $2, 1, $3, $4, $5, $6, $7, $8,
    2, $9, $10, $11, $12, 'ready', '{"kind":"suspend"}'::jsonb, 'restored-actor-ready', transaction_timestamp()
)`, checkpointID, work.RunID, waitID, sourceLeaseID, sourceWorkspaceLeaseID,
		workspaceID, headVersionID, checkpointVersionID,
		checkpointArtifacts.RuntimeConfig, checkpointArtifacts.VMState, checkpointArtifacts.Memory, checkpointArtifacts.ScratchDisk)
	dbtest.MustExec(t, ctx, tx, `
UPDATE run_waits
   SET condition_state = 'completed', condition_result = '{}'::jsonb,
       condition_terminal_at = transaction_timestamp(),
       suspension_state = 'released', suspension_terminal_at = transaction_timestamp(),
       suspend_checkpoint_id = $2,
       checkpoint_request_version = 1, checkpoint_ack_version = 1,
       resume_request_version = 1, resume_ack_version = 1,
       base_workspace_version_id = $3, base_workspace_content_digest = $4,
       resume_workspace_version_id = $5, ownership_generation = $6,
       parent_writer_generation = 1, child_writer_generation = 2,
       resume_writer_generation = 3
 WHERE id = $1`, waitID, checkpointID, checkpointVersionID, checkpointDigest,
		privateVersionID, ownershipGeneration)
	dbtest.MustExec(t, ctx, tx, `
UPDATE runtime_instances SET restore_checkpoint_id = $2 WHERE id = $1`, runtimeID, checkpointID)
	dbtest.MustExec(t, ctx, tx, `
UPDATE runs
   SET status = 'running', started_at = COALESCE(started_at, first_lease_at),
       active_started_at = NULL
 WHERE id = $1`, work.RunID)
	dbtest.MustExec(t, ctx, tx, `
UPDATE run_attempts
   SET entrypoint_entered_at = transaction_timestamp()
 WHERE run_id = $1 AND number = 1`, work.RunID)
	dbtest.MustExec(t, ctx, tx, `
UPDATE run_leases
   SET state = 'finalizing',
       started_at = COALESCE(started_at, claimed_at, assigned_at),
       expires_at = $2,
       finalization_operation_id = $3,
       finalization_kind = $4,
       finalization_started_at = transaction_timestamp(),
       finalization_request_fingerprint = 'pending'
 WHERE id = $1`, work.LeaseID, expiresAt, operationID,
		map[bool]string{false: string(workerapi.RunFinalizationCapture), true: string(workerapi.RunFinalizationReset)}[rollback])
	dbtest.MustExec(t, ctx, tx, `
UPDATE workspace_leases
   SET base_version_id = $2, expires_at = $3
 WHERE id = $1`, workspaceLeaseID, privateVersionID, expiresAt)
	dbtest.MustExec(t, ctx, tx, `
UPDATE workspace_mounts SET materialized_version_id = $2 WHERE id = $1`, mountID, privateVersionID)
	if err := tx.Commit(ctx); err != nil {
		t.Fatal(err)
	}

	assignment := workerapi.RunLeaseAssignment{
		ID: work.LeaseID.String(), RunID: work.RunID.String(), AttemptNumber: 1,
		LeaseSequence: 2, WorkerGroupID: runtest.WorkerGroup,
		WorkerInstanceID: base.WorkerID.String(), WorkerEpoch: 1,
		RuntimeInstanceID: runtimeID.String(), RuntimeIdentityID: base.RuntimeIdentityID,
		WorkspaceID: workspaceID.String(), WorkspaceMountID: mountID.String(),
		WorkspaceLeaseID: workspaceLeaseID.String(), BaseWorkspaceVersionID: privateVersionID.String(),
		OwnershipGeneration: ownershipGeneration, WriterGeneration: 3,
		MountFencingGeneration: mountGeneration, ExpiresAt: expiresAt,
	}
	request := workerapi.CompleteActorRequest{
		Lease: assignment.Fence(),
		Outcome: workerapi.ActorOutcome{
			TerminalInputSequence: 2,
			Succeeded:             &workerapi.ActorSucceeded{},
		},
		Workspace: workerapi.TaskWorkspaceProof{
			Captured: validTaskWorkspaceCapture(t, assignment),
		},
	}
	artifact, cleanupArtifact, err := workspace.CreateEmptyWorkspaceArtifact(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(cleanupArtifact)
	body, err := os.ReadFile(artifact.Path)
	if err != nil {
		t.Fatal(err)
	}
	tree, err := workspace.InspectArtifact(bytes.NewReader(body), artifact)
	if err != nil {
		t.Fatal(err)
	}
	request.Workspace.Captured.Tree = workerapi.WorkspaceTreeIdentity{
		Digest: tree.Digest, SizeBytes: tree.SizeBytes, EntryCount: int32(tree.EntryCount),
	}
	request.Workspace.Captured.Artifact = workerapi.WorkspaceArtifact{
		Digest: artifact.Digest, MediaType: artifact.MediaType, Encoding: artifact.Encoding,
		SizeBytes: artifact.SizeBytes, EntryCount: int32(artifact.EntryCount),
	}
	request.Workspace.Captured.Receipt.OperationID = operationID.String()
	setCaptureFingerprint(t, request.Workspace.Captured)
	finalizationFingerprint := request.Workspace.Captured.Receipt.RequestFingerprint
	if rollback {
		request.Outcome = workerapi.ActorOutcome{
			TerminalInputSequence: 2,
			Failed:                &workerapi.TaskFailure{Message: "actor failed"},
		}
		rolledBack := validTaskWorkspaceRollback(t, request.Workspace.Captured)
		rolledBack.Receipt.OperationID = operationID.String()
		rolledBack.Target.BaseWorkspaceVersionID = headVersionID.String()
		rolledBack.Receipt.RequestFingerprint = ""
		target := workspace.ResetTarget{
			Kind: workspace.ResetTargetEmpty, BaseVersionID: headVersionID.String(),
			Tree: workspace.TreeIdentity{Digest: workspace.CanonicalEmptyTreeDigest},
		}
		fingerprint, err := workspace.FinalizationFingerprint(
			workspace.FinalizationResetKind,
			workspace.FinalizationRequest{
				OperationID: operationID.String(),
				Fence:       testFinalizationFence(rolledBack.Receipt.Fence),
				Target:      target,
			},
		)
		if err != nil {
			t.Fatal(err)
		}
		rolledBack.Receipt.RequestFingerprint = fingerprint
		request.Workspace = workerapi.TaskWorkspaceProof{RolledBack: rolledBack}
		finalizationFingerprint = fingerprint
	}
	dbtest.MustExec(t, ctx, base.Pool, `
UPDATE run_leases SET finalization_request_fingerprint = $2 WHERE id = $1`,
		work.LeaseID, finalizationFingerprint)

	return restoredActorCompletionPostgresFixture{
		server: &Server{
			db: db.New(base.Pool), tx: base.Pool,
			cas: actorTurnCAS{
				object: cas.Object{Digest: artifact.Digest, SizeBytes: artifact.SizeBytes, MediaType: artifact.MediaType},
				body:   body,
			},
		},
		pool: base.Pool,
		worker: workerActor{
			WorkerInstanceID: base.WorkerID, WorkerGroupID: runtest.WorkerGroupID,
			WorkerEpoch: 1, ClaimVersion: 1, GroupClaimVersion: 1,
		},
		request: request, runID: work.RunID, leaseID: work.LeaseID,
		headVersionID: headVersionID, privateVersionID: privateVersionID,
	}
}
