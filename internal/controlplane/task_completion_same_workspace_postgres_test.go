package controlplane

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
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

func TestSameWorkspaceTaskCompletionRequestsDiscardBeforeSuccessOrRetry(t *testing.T) {
	tests := []struct {
		name  string
		retry bool
	}{
		{name: "success"},
		{name: "retryable failure", retry: true},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			fixture := newSameWorkspaceCompletionPostgresFixture(t, test.retry)
			completion, err := parseTaskCompletionRequest(fixture.request)
			if err != nil {
				t.Fatal(err)
			}
			if err := fixture.server.completeTask(
				t.Context(), fixture.worker, fixture.request, completion,
			); err != nil {
				if point, ok := staleAuthorityPointOf(err); ok {
					t.Fatalf("completion failed at %s: %v", point, err)
				}
				t.Fatal(err)
			}

			var runtimeDesired, mountState, workspaceLeaseState string
			if err := fixture.pool.QueryRow(t.Context(), `
SELECT runtime_instances.desired_state,
       workspace_mounts.state,
       workspace_leases.state
  FROM runtime_instances
  JOIN workspace_mounts
    ON workspace_mounts.id = $2
   AND workspace_mounts.runtime_instance_id = runtime_instances.id
  JOIN workspace_leases
    ON workspace_leases.id = $3
   AND workspace_leases.workspace_mount_id = workspace_mounts.id
 WHERE runtime_instances.id = $1`,
				fixture.runtimeID, fixture.mountID, fixture.workspaceLeaseID,
			).Scan(&runtimeDesired, &mountState, &workspaceLeaseState); err != nil {
				t.Fatal(err)
			}
			if runtimeDesired != "closed" || mountState != "unmounting" || workspaceLeaseState != "released" {
				t.Fatalf("completion lifecycle = runtime:%s mount:%s lease:%s",
					runtimeDesired, mountState, workspaceLeaseState)
			}

			var runStatus string
			var currentAttempt int32
			if err := fixture.pool.QueryRow(t.Context(), `
SELECT status, current_attempt_number FROM runs WHERE id = $1`, fixture.childRunID,
			).Scan(&runStatus, &currentAttempt); err != nil {
				t.Fatal(err)
			}
			if test.retry {
				if runStatus != "retry_delayed" || currentAttempt != 2 {
					t.Fatalf("retry state = %s attempt %d", runStatus, currentAttempt)
				}
				var parentStatus, waitCondition, waitSuspension string
				if err := fixture.pool.QueryRow(t.Context(), `
SELECT parent.status, edge.condition_state, edge.suspension_state
  FROM runs AS parent
  JOIN run_waits AS edge ON edge.run_id = parent.id
 WHERE parent.id = $1 AND edge.id = $2`, fixture.parentRunID, fixture.waitID,
				).Scan(&parentStatus, &waitCondition, &waitSuspension); err != nil {
					t.Fatal(err)
				}
				if parentStatus != "waiting" || waitCondition != "pending" || waitSuspension != "parked" {
					t.Fatalf("retry changed parent edge = %s %s/%s", parentStatus, waitCondition, waitSuspension)
				}
			} else if runStatus != "succeeded" || currentAttempt != 1 {
				t.Fatalf("success state = %s attempt %d", runStatus, currentAttempt)
			}
		})
	}
}

func TestWorkerCompleteTaskRejectsInvalidPinnedRetryPolicyPermanently(t *testing.T) {
	fixture := newSameWorkspaceCompletionPostgresFixture(t, true)
	dbtest.MustExec(t, t.Context(), fixture.pool, `
UPDATE runs SET retry_policy = '{"enabled":true}'::jsonb WHERE id = $1`, fixture.childRunID)
	body, err := json.Marshal(fixture.request)
	if err != nil {
		t.Fatal(err)
	}
	request := httptest.NewRequest(
		http.MethodPost,
		"/worker/v1/run/tasks/complete",
		bytes.NewReader(body),
	)
	request = request.WithContext(context.WithValue(
		request.Context(), workerContextKey{}, fixture.worker,
	))
	response := httptest.NewRecorder()
	fixture.server.log = taskCompletionTestLogger()

	fixture.server.workerCompleteTask(response, request)

	if response.Code != http.StatusUnprocessableEntity {
		t.Fatalf("status = %d body=%s", response.Code, response.Body.String())
	}
	var envelope struct {
		Error struct {
			Code string `json:"code"`
		} `json:"error"`
	}
	if err := json.Unmarshal(response.Body.Bytes(), &envelope); err != nil {
		t.Fatal(err)
	}
	if envelope.Error.Code != "unprocessable_entity" {
		t.Fatalf("error code = %q", envelope.Error.Code)
	}
}

type sameWorkspaceCompletionPostgresFixture struct {
	server           *Server
	pool             db.DBTX
	worker           workerActor
	request          workerapi.CompleteTaskRequest
	childRunID       uuid.UUID
	parentRunID      uuid.UUID
	waitID           uuid.UUID
	runtimeID        uuid.UUID
	mountID          uuid.UUID
	workspaceLeaseID uuid.UUID
}

func newSameWorkspaceCompletionPostgresFixture(
	t *testing.T,
	retry bool,
) sameWorkspaceCompletionPostgresFixture {
	t.Helper()
	base := runtest.New(t)
	work := base.AddRunLease(t, "starting", time.Now().Add(-time.Minute))
	ctx := t.Context()
	var workspaceID, baseVersionID, runtimeID, mountID, workspaceLeaseID uuid.UUID
	if err := base.Pool.QueryRow(ctx, `
SELECT runs.workspace_id,
       runs.base_workspace_version_id,
       run_leases.runtime_instance_id,
       workspace_leases.workspace_mount_id,
       workspace_leases.id
  FROM runs
  JOIN run_leases ON run_leases.id = $2 AND run_leases.run_id = runs.id
  JOIN workspace_leases ON workspace_leases.owner_run_lease_id = run_leases.id
 WHERE runs.id = $1`, work.RunID, work.LeaseID,
	).Scan(&workspaceID, &baseVersionID, &runtimeID, &mountID, &workspaceLeaseID); err != nil {
		t.Fatal(err)
	}

	parentRunID := uuid.NewV7()
	parentLeaseID := uuid.NewV7()
	parentWorkspaceLeaseID := uuid.NewV7()
	waitID := uuid.NewV7()
	checkpointID := uuid.NewV7()
	claimID := uuid.NewV7()
	operationID := uuid.NewV7()
	expiresAt := time.Now().Add(10 * time.Minute).UTC().Truncate(time.Microsecond)
	retryPolicy := `{"enabled":false}`
	if retry {
		retryPolicy = `{"enabled":true,"maxAttempts":3,"backoff":{"minMs":1,"maxMs":1,"factor":1,"jitter":"none"}}`
	}

	tx, err := base.Pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = tx.Rollback(context.Background()) }()
	dbtest.MustExec(t, ctx, tx, `SET CONSTRAINTS ALL DEFERRED`)
	dbtest.MustExec(t, ctx, tx, `
INSERT INTO idempotency_claims (
    id, environment_id, operation, slot_hash, request_fingerprint, accepted_at
) VALUES (
    $1, $2, 'task.child.invoke', decode(repeat('52', 32), 'hex'),
    decode(repeat('54', 32), 'hex'), transaction_timestamp()
)`, claimID, base.EnvironmentID)
	dbtest.MustExec(t, ctx, tx, `
INSERT INTO runs (
    id, org_id, project_id, environment_id, deployment_id,
    deployment_definition_id, entrypoint_kind, entrypoint_declared_id,
    cause_kind, workspace_id, base_workspace_version_id, payload,
    queue_name, queue_origin_at, queue_score_at, max_active_duration_ms,
    retry_policy, trace_id, root_span_id, status, state_version,
    current_run_lease_id, first_lease_at
)
SELECT $1, org_id, project_id, environment_id, deployment_id,
       deployment_definition_id, entrypoint_kind, entrypoint_declared_id,
       'api', workspace_id, base_workspace_version_id, payload,
       queue_name, queue_origin_at, queue_score_at, max_active_duration_ms,
       '{"enabled":false}'::jsonb, '77777777777777777777777777777777',
       '8888888888888888', 'waiting', 3, NULL, first_lease_at
  FROM runs WHERE id = $2`, parentRunID, work.RunID)
	dbtest.MustExec(t, ctx, tx, `
INSERT INTO run_attempts (
    run_id, number, entrypoint_kind, workspace_id, base_workspace_version_id,
    entrypoint_entered_at
) VALUES ($1, 1, 'task', $2, $3, transaction_timestamp())`,
		parentRunID, workspaceID, baseVersionID)
	dbtest.MustExec(t, ctx, tx, `
INSERT INTO run_leases (
    id, org_id, project_id, environment_id, run_id, workspace_id, region_id,
    lease_sequence, attempt_number, worker_group_id, worker_instance_id,
    worker_epoch, runtime_instance_id, runtime_identity_id,
    requested_cpu_millis, requested_memory_bytes,
    requested_guest_ephemeral_disk_bytes, requested_execution_slots,
    trace_id, span_id, state, assigned_at, start_deadline_at,
    claimed_at, started_at, expires_at, checkpointed_at,
    terminal_at, terminal_reason_code
)
SELECT $1, org_id, project_id, environment_id, $2, workspace_id, region_id,
       1, 1, worker_group_id, worker_instance_id, worker_epoch,
       runtime_instance_id, runtime_identity_id, requested_cpu_millis,
       requested_memory_bytes, requested_guest_ephemeral_disk_bytes,
       requested_execution_slots, '77777777777777777777777777777777',
       '8888888888888888', 'checkpointed', assigned_at, start_deadline_at,
       assigned_at, assigned_at, expires_at, transaction_timestamp(),
       transaction_timestamp(), 'checkpointed'
  FROM run_leases WHERE id = $3`, parentLeaseID, parentRunID, work.LeaseID)
	dbtest.MustExec(t, ctx, tx, `
UPDATE workspace_leases SET writer_generation = 2 WHERE id = $1`, workspaceLeaseID)
	dbtest.MustExec(t, ctx, tx, `
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
       workspace_mount_id, 'released', $2, base_version_id,
       1, 1, mount_fencing_generation, 'parent-fence', acquired_at, renewed_at,
       expires_at, transaction_timestamp(), transaction_timestamp()
  FROM workspace_leases WHERE id = $3`,
		parentWorkspaceLeaseID, parentLeaseID, workspaceLeaseID)
	dbtest.MustExec(t, ctx, tx, `
UPDATE runs
   SET cause_kind = 'child', parent_run_id = $2,
       parent_owns_lifecycle = true, claim_id = $3,
       status = 'running', started_at = COALESCE(started_at, first_lease_at),
       active_started_at = NULL,
       retry_policy = $4::jsonb
 WHERE id = $1`, work.RunID, parentRunID, claimID, retryPolicy)
	dbtest.MustExec(t, ctx, tx, `
INSERT INTO run_waits (
    id, environment_id, run_id, workspace_id, kind, condition_state,
    child_run_id, child_parent_owned, child_target_declared_id,
	child_claim_id, child_request, expected_run_state_version,
	attempt_number, prior_run_lease_id, checkpoint_request_version,
	checkpoint_ack_version, resume_attach_id,
	suspension_state
) VALUES (
	$1, $2, $3, $4, 'child', 'pending', $5, true, 'test-task',
	$6, '{"Method":"call"}'::jsonb, 3, 1, $7, 1, 1, $8,
	'parked'
)`, waitID, base.EnvironmentID, parentRunID, workspaceID, work.RunID,
		claimID, parentLeaseID, uuid.NewV7())
	checkpointArtifacts := dbtest.InsertCheckpointArtifacts(t, ctx, tx, parentRunID, checkpointID.String())
	dbtest.MustExec(t, ctx, tx, `
INSERT INTO run_checkpoints (
    id, run_id, attempt_number, run_wait_id, source_run_lease_id,
    source_workspace_lease_id, workspace_id, base_workspace_version_id,
    private_workspace_version_id, runtime_config_artifact_id, vm_state_artifact_id,
    memory_artifact_id, scratch_disk_artifact_id, state, restore_manifest,
    ready_request_fingerprint, ready_at
) VALUES (
    $1, $2, 1, $3, $4, $5, $6, $7, $7, $8, $9, $10, $11, 'ready',
    '{"kind":"suspend"}'::jsonb, 'completion-test-ready', transaction_timestamp()
)`, checkpointID, parentRunID, waitID, parentLeaseID,
		parentWorkspaceLeaseID, workspaceID, baseVersionID,
		checkpointArtifacts.RuntimeConfig, checkpointArtifacts.VMState, checkpointArtifacts.Memory, checkpointArtifacts.ScratchDisk)
	dbtest.MustExec(t, ctx, tx, `
UPDATE run_waits
   SET suspend_checkpoint_id = $2,
       base_workspace_version_id = $3,
       base_workspace_content_digest = $4,
       ownership_generation = 1,
       parent_writer_generation = 1,
       child_writer_generation = 2
 WHERE id = $1`, waitID, checkpointID, baseVersionID,
		workspace.CanonicalEmptyTreeDigest)
	dbtest.MustExec(t, ctx, tx, `
UPDATE workspaces
   SET owner_run_id = $2, owner_session_id = NULL,
       ownership_generation = 1, writer_generation = 2
 WHERE id = $1`, workspaceID, parentRunID)
	dbtest.MustExec(t, ctx, tx, `
UPDATE run_attempts
   SET entrypoint_entered_at = transaction_timestamp()
 WHERE run_id = $1 AND number = 1`, work.RunID)
	dbtest.MustExec(t, ctx, tx, `
UPDATE run_leases
   SET state = 'finalizing', claimed_at = COALESCE(claimed_at, assigned_at),
       started_at = COALESCE(started_at, claimed_at, assigned_at), expires_at = $2,
       finalization_operation_id = $3, finalization_kind = $4,
       finalization_started_at = transaction_timestamp(),
       finalization_request_fingerprint = 'completion-test-finalization'
 WHERE id = $1`, work.LeaseID, expiresAt, operationID,
		map[bool]string{true: string(workerapi.RunFinalizationReset), false: string(workerapi.RunFinalizationCapture)}[retry])
	dbtest.MustExec(t, ctx, tx, `
UPDATE workspace_leases
   SET writer_generation = 2, expires_at = $2
 WHERE id = $1`, workspaceLeaseID, expiresAt)
	if err := tx.Commit(ctx); err != nil {
		t.Fatal(err)
	}

	assignment := workerapi.RunLeaseAssignment{
		ID: work.LeaseID.String(), RunID: work.RunID.String(), AttemptNumber: 1,
		LeaseSequence: 1, WorkerGroupID: runtest.WorkerGroup,
		WorkerInstanceID: base.WorkerID.String(), WorkerEpoch: 1,
		RuntimeInstanceID: runtimeID.String(), RuntimeIdentityID: base.RuntimeIdentityID,
		WorkspaceID: workspaceID.String(), WorkspaceMountID: mountID.String(),
		WorkspaceLeaseID: workspaceLeaseID.String(), BaseWorkspaceVersionID: baseVersionID.String(),
		OwnershipGeneration: 1, WriterGeneration: 2, MountFencingGeneration: 2,
		ExpiresAt: expiresAt,
	}
	request := workerapi.CompleteTaskRequest{
		Lease: assignment.Fence(),
		Outcome: workerapi.TaskOutcome{Succeeded: &workerapi.TaskSucceeded{
			Output: json.RawMessage(`{"ok":true}`),
		}},
		Workspace: workerapi.TaskWorkspaceProof{
			Captured: validTaskWorkspaceCapture(t, assignment),
		},
	}
	artifact, cleanupArtifact, err := workspace.CreateEmptyWorkspaceArtifact(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer cleanupArtifact()
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
		Digest: artifact.Digest, MediaType: artifact.MediaType,
		Encoding: artifact.Encoding, SizeBytes: artifact.SizeBytes,
		EntryCount: int32(artifact.EntryCount),
	}
	request.Workspace.Captured.Receipt.OperationID = operationID.String()
	setCaptureFingerprint(t, request.Workspace.Captured)
	finalizationFingerprint := request.Workspace.Captured.Receipt.RequestFingerprint
	if retry {
		request.Outcome = workerapi.TaskOutcome{Failed: &workerapi.TaskFailure{Message: "retry"}}
		request.Workspace = workerapi.TaskWorkspaceProof{
			RolledBack: validTaskWorkspaceRollback(t, request.Workspace.Captured),
		}
		request.Workspace.RolledBack.Receipt.OperationID = operationID.String()
		request.Workspace.RolledBack.Receipt.RequestFingerprint = ""
		target := workspace.ResetTarget{
			Kind: workspace.ResetTargetEmpty, BaseVersionID: baseVersionID.String(),
			Tree: workspace.TreeIdentity{Digest: workspace.CanonicalEmptyTreeDigest},
		}
		fingerprint, err := workspace.FinalizationFingerprint(
			workspace.FinalizationResetKind,
			workspace.FinalizationRequest{
				OperationID: operationID.String(),
				Fence:       testFinalizationFence(request.Workspace.RolledBack.Receipt.Fence),
				Target:      target,
			},
		)
		if err != nil {
			t.Fatal(err)
		}
		request.Workspace.RolledBack.Receipt.RequestFingerprint = fingerprint
		finalizationFingerprint = fingerprint
	}
	dbtest.MustExec(t, ctx, base.Pool, `
UPDATE run_leases
   SET finalization_request_fingerprint = $2
 WHERE id = $1`, work.LeaseID, finalizationFingerprint)

	queries := db.New(base.Pool)
	return sameWorkspaceCompletionPostgresFixture{
		server: &Server{
			db: queries, tx: base.Pool,
			cas: actorTurnCAS{
				object: cas.Object{
					Digest: artifact.Digest, SizeBytes: artifact.SizeBytes,
					MediaType: artifact.MediaType,
				},
				body: body,
			},
		}, pool: base.Pool,
		worker: workerActor{
			WorkerInstanceID: base.WorkerID, WorkerGroupID: runtest.WorkerGroup,
			WorkerEpoch: 1, ClaimVersion: 1, GroupClaimVersion: 1,
		},
		request: request, childRunID: work.RunID, parentRunID: parentRunID,
		waitID: waitID, runtimeID: runtimeID, mountID: mountID,
		workspaceLeaseID: workspaceLeaseID,
	}
}
