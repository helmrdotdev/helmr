package run

import (
	"context"
	"encoding/json"
	"errors"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/helmrdotdev/helmr/internal/db"
	"github.com/helmrdotdev/helmr/internal/pgvalue"
)

func TestCancelerTerminalizesAuthorityAndLeavesDetachedChildren(t *testing.T) {
	ctx := context.Background()
	fixture := newPostgresFixture(t)
	parent := fixture.addRun(t, "assigned", time.Now().Add(-time.Minute))
	detached := fixture.addRun(t, "assigned", time.Now().Add(-time.Minute))
	mustExec(t, ctx, fixture.pool, `
UPDATE runs
   SET cause_kind = 'child',
       parent_run_id = $1,
       parent_owns_lifecycle = false
 WHERE id = $2`, parent.runID, detached.runID)

	canceler, err := NewCanceler(fixture.pool)
	if err != nil {
		t.Fatal(err)
	}
	result, err := canceler.Cancel(ctx, CancellationRequest{
		OrgID: fixture.orgID, ProjectID: fixture.projectID,
		EnvironmentID: fixture.environmentID,
		RunPublicID:   fixture.runPublicID(t, parent.runID),
	})
	if err != nil {
		t.Fatal(err)
	}
	if !result.Changed || result.CancelledRuns != 1 || result.RunID != parent.runID {
		t.Fatalf("cancellation result = %+v", result)
	}

	assertCancellationState(t, fixture, parent, "cancelled", "cancelled", "fenced")
	var detachedStatus db.RunStatus
	if err := fixture.pool.QueryRow(ctx,
		`SELECT status FROM runs WHERE id = $1`, detached.runID,
	).Scan(&detachedStatus); err != nil {
		t.Fatal(err)
	}
	if detachedStatus != db.RunStatusQueued {
		t.Fatalf("detached child status = %s, want queued", detachedStatus)
	}
	var ownerRunID *uuid.UUID
	if err := fixture.pool.QueryRow(ctx, `
SELECT owner_run_id
  FROM workspaces
 WHERE id = (SELECT workspace_id FROM runs WHERE id = $1)`, parent.runID,
	).Scan(&ownerRunID); err != nil {
		t.Fatal(err)
	}
	if ownerRunID != nil {
		t.Fatalf("cancelled root retained Workspace ownership: %s", *ownerRunID)
	}
	var desiredState, mountState string
	if err := fixture.pool.QueryRow(ctx, `
SELECT runtime_instances.desired_state, workspace_mounts.state
  FROM run_leases
  JOIN runtime_instances ON runtime_instances.id = run_leases.runtime_instance_id
  JOIN workspace_mounts ON workspace_mounts.runtime_instance_id = runtime_instances.id
 WHERE run_leases.id = $1`, parent.leaseID,
	).Scan(&desiredState, &mountState); err != nil {
		t.Fatal(err)
	}
	if desiredState != "closed" || mountState != "unmounting" {
		t.Fatalf("cleanup state = runtime:%s mount:%s", desiredState, mountState)
	}

	replay, err := canceler.Cancel(ctx, CancellationRequest{
		OrgID: fixture.orgID, ProjectID: fixture.projectID,
		EnvironmentID: fixture.environmentID,
		RunPublicID:   result.RunPublicID,
	})
	if err != nil {
		t.Fatal(err)
	}
	if replay.Changed || replay.CancelledRuns != 0 || replay.RunID != parent.runID {
		t.Fatalf("cancellation replay = %+v", replay)
	}
}

func TestOwnedFinalizationFailsSecretRevokedRun(t *testing.T) {
	ctx := context.Background()
	fixture := newPostgresFixture(t)
	work := fixture.addRun(t, "assigned", time.Now().Add(-time.Minute))
	tx, err := fixture.pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = tx.Rollback(context.Background()) }()
	graph, err := LockOwnedFinalization(
		ctx,
		tx,
		OwnedFinalizationRequest{
			OrgID:         fixture.orgID,
			ProjectID:     fixture.projectID,
			EnvironmentID: fixture.environmentID,
			RunID:         work.runID,
		},
	)
	if err != nil {
		t.Fatal(err)
	}
	terminalized, err := graph.FailCurrentForSecretRevocation(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if terminalized != 1 {
		t.Fatalf("terminalized Runs = %d, want 1", terminalized)
	}
	if err := tx.Commit(ctx); err != nil {
		t.Fatal(err)
	}
	var runStatus db.RunStatus
	var runReason string
	var runError []byte
	var attemptOutcome, attemptReason string
	var runLeaseState, workspaceLeaseState string
	if err := fixture.pool.QueryRow(ctx, `
SELECT runs.status,
       runs.terminal_reason_code,
       runs.error,
       run_attempts.terminal_outcome,
       run_attempts.terminal_reason_code,
       run_leases.state,
       workspace_leases.state
  FROM runs
  JOIN run_attempts
    ON run_attempts.run_id = runs.id
   AND run_attempts.number = runs.current_attempt_number
  JOIN run_leases
    ON run_leases.run_id = runs.id
   AND run_leases.id = $2
  JOIN workspace_leases
    ON workspace_leases.owner_run_lease_id = run_leases.id
 WHERE runs.id = $1`,
		work.runID,
		work.leaseID,
	).Scan(
		&runStatus,
		&runReason,
		&runError,
		&attemptOutcome,
		&attemptReason,
		&runLeaseState,
		&workspaceLeaseState,
	); err != nil {
		t.Fatal(err)
	}
	if runStatus != db.RunStatusFailed || runReason != "secret_revoked" ||
		attemptOutcome != "failed" || attemptReason != "secret_revoked" ||
		runLeaseState != "rejected" || workspaceLeaseState != "fenced" {
		t.Fatalf(
			"Secret-revoked authority = run:%s/%s attempt:%s/%s leases:%s/%s",
			runStatus,
			runReason,
			attemptOutcome,
			attemptReason,
			runLeaseState,
			workspaceLeaseState,
		)
	}
	var errorPayload map[string]any
	if err := json.Unmarshal(runError, &errorPayload); err != nil {
		t.Fatal(err)
	}
	if errorPayload["code"] != "secret_revoked" ||
		errorPayload["retryable"] != false {
		t.Fatalf("Run error = %#v", errorPayload)
	}
}

func TestCancelerRejectsAnotherTerminalOutcome(t *testing.T) {
	ctx := context.Background()
	fixture := newPostgresFixture(t)
	work := fixture.addRun(t, "assigned", time.Now().Add(-time.Minute))
	mustExec(t, ctx, fixture.pool, `
UPDATE run_attempts
   SET terminal_outcome = 'succeeded',
       terminal_reason_code = 'task_succeeded',
       terminal_at = now()
 WHERE run_id = $1
   AND number = 1`, work.runID)
	mustExec(t, ctx, fixture.pool, `
UPDATE runs
   SET status = 'succeeded',
       output = '{}'::jsonb,
       terminal_at = now()
 WHERE id = $1`, work.runID)
	canceler, err := NewCanceler(fixture.pool)
	if err != nil {
		t.Fatal(err)
	}

	_, err = canceler.Cancel(ctx, CancellationRequest{
		OrgID: fixture.orgID, ProjectID: fixture.projectID,
		EnvironmentID: fixture.environmentID,
		RunPublicID:   fixture.runPublicID(t, work.runID),
	})
	if !errors.Is(err, ErrCancellationConflict) {
		t.Fatalf("cancel succeeded Run error = %v", err)
	}
	var status db.RunStatus
	if err := fixture.pool.QueryRow(ctx,
		`SELECT status FROM runs WHERE id = $1`, work.runID,
	).Scan(&status); err != nil {
		t.Fatal(err)
	}
	if status != db.RunStatusSucceeded {
		t.Fatalf("succeeded Run changed to %s", status)
	}
}

func TestCancelerCascadesOwnedHandoffGraph(t *testing.T) {
	ctx := context.Background()
	fixture := newPostgresFixture(t)
	leaf := fixture.addRun(t, "assigned", time.Now().Add(-time.Minute))
	chain := fixture.addHandoffChain(t, ctx, leaf)
	canceler, err := NewCanceler(fixture.pool)
	if err != nil {
		t.Fatal(err)
	}

	result, err := canceler.Cancel(ctx, CancellationRequest{
		OrgID: fixture.orgID, ProjectID: fixture.projectID,
		EnvironmentID: fixture.environmentID,
		RunPublicID:   fixture.runPublicID(t, chain.outerRunID),
	})
	if err != nil {
		t.Fatal(err)
	}
	if !result.Changed || result.CancelledRuns != 3 {
		t.Fatalf("cancellation result = %+v", result)
	}
	for _, runID := range []uuid.UUID{chain.outerRunID, chain.parentRunID, leaf.runID} {
		var status db.RunStatus
		var attemptOutcome string
		if err := fixture.pool.QueryRow(ctx, `
SELECT runs.status, run_attempts.terminal_outcome
  FROM runs
  JOIN run_attempts
    ON run_attempts.run_id = runs.id
   AND run_attempts.number = runs.current_attempt_number
 WHERE runs.id = $1`, runID).Scan(&status, &attemptOutcome); err != nil {
			t.Fatal(err)
		}
		if status != db.RunStatusCancelled || attemptOutcome != "cancelled" {
			t.Fatalf("Run %s = status:%s attempt:%s", runID, status, attemptOutcome)
		}
	}
	var activeWaits int
	if err := fixture.pool.QueryRow(ctx, `
SELECT count(*)
  FROM run_waits
 WHERE run_id = ANY($1::uuid[])
   AND suspension_state IN ('hot', 'checkpointing', 'parked', 'resume_pending', 'resuming')`,
		[]uuid.UUID{chain.outerRunID, chain.parentRunID},
	).Scan(&activeWaits); err != nil {
		t.Fatal(err)
	}
	if activeWaits != 0 {
		t.Fatalf("active waits after cascade = %d", activeWaits)
	}
	var resumeMessages int
	if err := fixture.pool.QueryRow(ctx,
		`SELECT count(*) FROM outbox_messages WHERE topic = 'run.resume'`,
	).Scan(&resumeMessages); err != nil {
		t.Fatal(err)
	}
	if resumeMessages != 0 {
		t.Fatalf("parent cascade published %d resume messages", resumeMessages)
	}
}

func TestOwnedActorFinalizationCancelsOnlyDescendants(t *testing.T) {
	ctx := context.Background()
	fixture := newPostgresFixture(t)
	leaf := fixture.addRun(t, "assigned", time.Now().Add(-time.Minute))
	chain := fixture.addHandoffChain(t, ctx, leaf)
	var outerLeaseID uuid.UUID
	if err := fixture.pool.QueryRow(ctx,
		`SELECT current_run_lease_id FROM runs WHERE id = $1`,
		chain.outerRunID,
	).Scan(&outerLeaseID); err != nil {
		t.Fatal(err)
	}

	fixture.convertToActor(
		t,
		ctx,

		leasedRun{leaseID: outerLeaseID, runID: chain.outerRunID},
		`{"enabled":false}`)

	tx, err := fixture.pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = tx.Rollback(context.Background()) }()
	graph, err := LockOwnedFinalization(
		ctx,
		tx,
		OwnedFinalizationRequest{
			OrgID:         fixture.orgID,
			ProjectID:     fixture.projectID,
			EnvironmentID: fixture.environmentID,
			RunID:         chain.outerRunID,
		},
	)
	if err != nil {
		t.Fatal(err)
	}
	cancelled, err := graph.CancelDescendants(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if cancelled != 2 {
		t.Fatalf("cancelled descendants = %d, want 2", cancelled)
	}
	if err := tx.Commit(ctx); err != nil {
		t.Fatal(err)
	}

	var outerStatus, parentStatus, childStatus db.RunStatus
	var outerCondition db.WaitState
	var outerSuspension db.RunWaitState
	if err := fixture.pool.QueryRow(ctx, `
SELECT outer_run.status,
       parent_run.status,
       child_run.status,
       outer_wait.condition_state,
       outer_wait.suspension_state
  FROM runs AS outer_run
  JOIN runs AS parent_run ON parent_run.parent_run_id = outer_run.id
  JOIN runs AS child_run ON child_run.parent_run_id = parent_run.id
  JOIN run_waits AS outer_wait
    ON outer_wait.run_id = outer_run.id
   AND outer_wait.child_run_id = parent_run.id
 WHERE outer_run.id = $1
   AND outer_wait.id = $2`,
		chain.outerRunID,
		chain.outerWaitID,
	).Scan(
		&outerStatus,
		&parentStatus,
		&childStatus,
		&outerCondition,
		&outerSuspension,
	); err != nil {
		t.Fatal(err)
	}
	if outerStatus != db.RunStatusWaiting ||
		parentStatus != db.RunStatusCancelled ||
		childStatus != db.RunStatusCancelled ||
		outerCondition != db.WaitStatePending ||
		outerSuspension != db.RunWaitStateParked {
		t.Fatalf(
			"finalization graph = outer:%s parent:%s child:%s wait:%s/%s",
			outerStatus,
			parentStatus,
			childStatus,
			outerCondition,
			outerSuspension,
		)
	}
}

func TestCancelerUnwindsDirectOwnedChildCancellation(t *testing.T) {
	ctx := context.Background()
	fixture := newPostgresFixture(t)
	leaf := fixture.addRun(t, "assigned", time.Now().Add(-time.Minute))
	chain := fixture.addHandoffChain(t, ctx, leaf)
	canceler, err := NewCanceler(fixture.pool)
	if err != nil {
		t.Fatal(err)
	}

	result, err := canceler.Cancel(ctx, CancellationRequest{
		OrgID: fixture.orgID, ProjectID: fixture.projectID,
		EnvironmentID: fixture.environmentID,
		RunPublicID:   fixture.runPublicID(t, leaf.runID),
	})
	if err != nil {
		t.Fatal(err)
	}
	if !result.Changed || result.CancelledRuns != 1 {
		t.Fatalf("cancellation result = %+v", result)
	}
	var childStatus, parentStatus, outerStatus db.RunStatus
	var condition db.WaitState
	var suspension db.RunWaitState
	var resumeVersion, baseVersion uuid.UUID
	if err := fixture.pool.QueryRow(ctx, `
SELECT child_run.status,
       parent_run.status,
       outer_run.status,
       wait.condition_state,
       wait.suspension_state,
       wait.resume_workspace_version_id,
       wait.base_workspace_version_id
  FROM runs AS child_run
  JOIN runs AS parent_run ON parent_run.id = child_run.parent_run_id
  JOIN runs AS outer_run ON outer_run.id = parent_run.parent_run_id
  JOIN run_waits AS wait
    ON wait.run_id = parent_run.id
   AND wait.child_run_id = child_run.id
 WHERE child_run.id = $1`, leaf.runID,
	).Scan(
		&childStatus, &parentStatus, &outerStatus, &condition, &suspension,
		&resumeVersion, &baseVersion,
	); err != nil {
		t.Fatal(err)
	}
	if childStatus != db.RunStatusCancelled || parentStatus != db.RunStatusQueued ||
		outerStatus != db.RunStatusWaiting ||
		condition != db.WaitStateCancelled || suspension != db.RunWaitStateResumePending ||
		resumeVersion != baseVersion {
		t.Fatalf(
			"unwind = child:%s parent:%s outer:%s condition:%s suspension:%s resume:%s base:%s",
			childStatus, parentStatus, outerStatus, condition, suspension,
			resumeVersion, baseVersion,
		)
	}
	var runtimeState, mountState string
	var outerRuntimeID, parentRuntimeID, outerMountID, parentMountID uuid.UUID
	if err := fixture.pool.QueryRow(ctx, `
SELECT runtime_instances.desired_state,
       workspace_mounts.state,
       outer_wait.handoff_runtime_instance_id,
       parent_wait.handoff_runtime_instance_id,
       outer_wait.handoff_workspace_mount_id,
       parent_wait.handoff_workspace_mount_id
  FROM runtime_instances
  JOIN workspace_mounts
    ON workspace_mounts.runtime_instance_id = runtime_instances.id
  JOIN run_waits AS outer_wait
    ON outer_wait.id = $1
   AND outer_wait.handoff_runtime_instance_id = runtime_instances.id
   AND outer_wait.handoff_workspace_mount_id = workspace_mounts.id
  JOIN run_waits AS parent_wait
    ON parent_wait.id = $2
   AND parent_wait.handoff_runtime_instance_id = runtime_instances.id
   AND parent_wait.handoff_workspace_mount_id = workspace_mounts.id`,
		chain.outerWaitID,
		chain.enclosingWaitID,
	).Scan(
		&runtimeState,
		&mountState,
		&outerRuntimeID,
		&parentRuntimeID,
		&outerMountID,
		&parentMountID,
	); err != nil {
		t.Fatal(err)
	}
	if runtimeState != "ready" || mountState != "mounted" ||
		outerRuntimeID != chain.runtimeID || parentRuntimeID != chain.runtimeID ||
		outerMountID != chain.mountID || parentMountID != chain.mountID {
		t.Fatalf(
			"retained handoff = runtime:%s mount:%s outer:%s/%s parent:%s/%s",
			runtimeState,
			mountState,
			outerRuntimeID,
			outerMountID,
			parentRuntimeID,
			parentMountID,
		)
	}
	var resumeMessages int
	if err := fixture.pool.QueryRow(ctx, `
SELECT count(*)
  FROM outbox_messages
 WHERE topic = 'run.resume'
   AND (payload->>'runId')::uuid = $1`, chain.parentRunID,
	).Scan(&resumeMessages); err != nil {
		t.Fatal(err)
	}
	if resumeMessages != 1 {
		t.Fatalf("parent resume messages = %d, want 1", resumeMessages)
	}
	resumeLeaseID := grantCancelledChildParentResume(
		t,
		ctx,
		fixture,
		chain.parentRunID,
		chain.enclosingWaitID,
		leaf.runID,
		leaf.leaseID,
		chain.versionID,
	)
	locators, err := fixture.queries.GetRunLeaseClaimLocators(
		ctx,
		db.GetRunLeaseClaimLocatorsParams{
			ID:                    pgvalue.UUID(resumeLeaseID),
			LeaseSequence:         2,
			WorkerGroupID:         testWorkerGroup,
			WorkerInstanceID:      pgvalue.UUID(fixture.workerID),
			WorkerEpoch:           1,
			WorkerProtocolVersion: testWorkerProtocol,
		},
	)
	if err != nil {
		t.Fatal(err)
	}
	if pgvalue.MustUUIDValue(locators.RunID) != chain.parentRunID ||
		pgvalue.MustUUIDValue(locators.RunWaitID) != chain.enclosingWaitID ||
		pgvalue.MustUUIDValue(locators.ResumeChildRunID) != leaf.runID ||
		pgvalue.MustUUIDValue(locators.SuspendCheckpointID) != chain.enclosingCheckpoint ||
		pgvalue.MustUUIDValue(locators.ResumeHandoffRuntimeInstanceID) != chain.runtimeID ||
		pgvalue.MustUUIDValue(locators.ResumeHandoffWorkspaceMountID) != chain.mountID ||
		pgvalue.MustUUIDValue(locators.EnclosingWaitID) != chain.outerWaitID ||
		locators.ResumeHandoffResumeWriterGeneration.Int64 != 4 {
		t.Fatalf("retained parent resume locators = %+v", locators)
	}
}

func TestCancelerRetainsBoundaryHandoffAcrossNestedCascade(t *testing.T) {
	ctx := context.Background()
	fixture := newPostgresFixture(t)
	leaf := fixture.addRun(t, "assigned", time.Now().Add(-time.Minute))
	chain := fixture.addHandoffChain(t, ctx, leaf)
	canceler, err := NewCanceler(fixture.pool)
	if err != nil {
		t.Fatal(err)
	}

	result, err := canceler.Cancel(ctx, CancellationRequest{
		OrgID: fixture.orgID, ProjectID: fixture.projectID,
		EnvironmentID: fixture.environmentID,
		RunPublicID:   fixture.runPublicID(t, chain.parentRunID),
	})
	if err != nil {
		t.Fatal(err)
	}
	if !result.Changed || result.CancelledRuns != 2 {
		t.Fatalf("cancellation result = %+v", result)
	}
	var outerStatus, parentStatus, childStatus db.RunStatus
	var outerCondition db.WaitState
	var outerSuspension db.RunWaitState
	var runtimeState, mountState string
	if err := fixture.pool.QueryRow(ctx, `
SELECT outer_run.status,
       parent_run.status,
       child_run.status,
       outer_wait.condition_state,
       outer_wait.suspension_state,
       runtime_instances.desired_state,
       workspace_mounts.state
  FROM runs AS outer_run
  JOIN runs AS parent_run ON parent_run.parent_run_id = outer_run.id
  JOIN runs AS child_run ON child_run.parent_run_id = parent_run.id
  JOIN run_waits AS outer_wait
    ON outer_wait.run_id = outer_run.id
   AND outer_wait.child_run_id = parent_run.id
  JOIN runtime_instances
    ON runtime_instances.id = outer_wait.handoff_runtime_instance_id
  JOIN workspace_mounts
    ON workspace_mounts.id = outer_wait.handoff_workspace_mount_id
   AND workspace_mounts.runtime_instance_id = runtime_instances.id
 WHERE outer_run.id = $1`,
		chain.outerRunID,
	).Scan(
		&outerStatus,
		&parentStatus,
		&childStatus,
		&outerCondition,
		&outerSuspension,
		&runtimeState,
		&mountState,
	); err != nil {
		t.Fatal(err)
	}
	if outerStatus != db.RunStatusQueued ||
		parentStatus != db.RunStatusCancelled ||
		childStatus != db.RunStatusCancelled ||
		outerCondition != db.WaitStateCancelled ||
		outerSuspension != db.RunWaitStateResumePending ||
		runtimeState != "ready" ||
		mountState != "mounted" {
		t.Fatalf(
			"nested boundary = outer:%s parent:%s child:%s wait:%s/%s runtime:%s mount:%s",
			outerStatus,
			parentStatus,
			childStatus,
			outerCondition,
			outerSuspension,
			runtimeState,
			mountState,
		)
	}

	resumeLeaseID := grantCancelledChildParentResume(
		t,
		ctx,
		fixture,
		chain.outerRunID,
		chain.outerWaitID,
		chain.parentRunID,
		leaf.leaseID,
		chain.versionID,
	)
	locators, err := fixture.queries.GetRunLeaseClaimLocators(
		ctx,
		db.GetRunLeaseClaimLocatorsParams{
			ID:                    pgvalue.UUID(resumeLeaseID),
			LeaseSequence:         2,
			WorkerGroupID:         testWorkerGroup,
			WorkerInstanceID:      pgvalue.UUID(fixture.workerID),
			WorkerEpoch:           1,
			WorkerProtocolVersion: testWorkerProtocol,
		},
	)
	if err != nil {
		t.Fatal(err)
	}
	if pgvalue.MustUUIDValue(locators.RunID) != chain.outerRunID ||
		pgvalue.MustUUIDValue(locators.RunWaitID) != chain.outerWaitID ||
		pgvalue.MustUUIDValue(locators.ResumeChildRunID) != chain.parentRunID ||
		pgvalue.MustUUIDValue(locators.SuspendCheckpointID) != chain.outerCheckpoint ||
		pgvalue.MustUUIDValue(locators.ResumeHandoffRuntimeInstanceID) != chain.runtimeID ||
		pgvalue.MustUUIDValue(locators.ResumeHandoffWorkspaceMountID) != chain.mountID ||
		locators.EnclosingWaitID.Valid ||
		locators.ResumeHandoffResumeWriterGeneration.Int64 != 4 {
		t.Fatalf("nested boundary parent resume locators = %+v", locators)
	}
}

func grantCancelledChildParentResume(
	t *testing.T,
	ctx context.Context,
	fixture postgresFixture,
	parentRunID uuid.UUID,
	waitID uuid.UUID,
	childRunID uuid.UUID,
	templateLeaseID uuid.UUID,
	versionID uuid.UUID,
) uuid.UUID {
	t.Helper()
	runLeaseID := uuid.Must(uuid.NewV7())
	workspaceLeaseID := uuid.Must(uuid.NewV7())
	tx, err := fixture.pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = tx.Rollback(context.Background()) }()
	if _, err := tx.Exec(ctx, `SET CONSTRAINTS ALL DEFERRED`); err != nil {
		t.Fatal(err)
	}
	mustExec(t, ctx, tx, `
INSERT INTO run_leases (
    id, org_id, project_id, environment_id, run_id, workspace_id, region_id,
    lease_sequence, attempt_number, worker_group_id, worker_instance_id,
    worker_epoch, runtime_instance_id, network_slot_id, network_slot_generation,
    runtime_identity_id, worker_protocol_version, requested_cpu_millis,
    requested_memory_bytes, requested_workload_disk_bytes, requested_scratch_bytes,
    requested_execution_slots, state, assigned_at, start_deadline_at, expires_at
)
SELECT $1, org_id, project_id, environment_id, $2, workspace_id, region_id,
       2, attempt_number, worker_group_id, worker_instance_id,
       worker_epoch, runtime_instance_id, network_slot_id, network_slot_generation,
       runtime_identity_id, worker_protocol_version, requested_cpu_millis,
       requested_memory_bytes, requested_workload_disk_bytes, requested_scratch_bytes,
       requested_execution_slots, 'assigned', now(), now() + interval '5 minutes',
       now() + interval '10 minutes'
  FROM run_leases
 WHERE id = $3`,
		runLeaseID,
		parentRunID,
		templateLeaseID,
	)
	mustExec(t, ctx, tx, `
UPDATE workspaces
   SET writer_generation = 4,
       updated_at = transaction_timestamp()
 WHERE id = (SELECT workspace_id FROM runs WHERE id = $1)
   AND writer_generation = 3`,
		parentRunID,
	)
	mustExec(t, ctx, tx, `
INSERT INTO workspace_leases (
    id, org_id, worker_group_id, project_id, environment_id, region_id,
    worker_instance_id, worker_epoch, runtime_instance_id, workspace_id,
    workspace_mount_id, owner_run_lease_id, base_version_id,
    ownership_generation, writer_generation, mount_fencing_generation,
    fencing_key_fingerprint, fencing_token_hash, expires_at
)
SELECT $1, org_id, worker_group_id, project_id, environment_id, region_id,
       worker_instance_id, worker_epoch, runtime_instance_id, workspace_id,
       workspace_mount_id, $2, $3, ownership_generation, 4,
       mount_fencing_generation, fencing_key_fingerprint, $4,
       now() + interval '10 minutes'
  FROM workspace_leases
 WHERE owner_run_lease_id = $5`,
		workspaceLeaseID,
		runLeaseID,
		versionID,
		"resume-"+runLeaseID.String(),
		templateLeaseID,
	)
	mustExec(t, ctx, tx, `
UPDATE runs
   SET current_run_lease_id = $1,
       state_version = state_version + 1,
       updated_at = transaction_timestamp()
 WHERE id = $2
   AND status = 'queued'
   AND current_run_lease_id IS NULL`,
		runLeaseID,
		parentRunID,
	)
	mustExec(t, ctx, tx, `
UPDATE run_waits
   SET suspension_state = 'resuming',
       current_run_lease_id = $1,
       resume_writer_generation = 4,
       updated_at = transaction_timestamp()
 WHERE id = $2
   AND child_run_id = $3
   AND condition_state = 'cancelled'
   AND suspension_state = 'resume_pending'`,
		runLeaseID,
		waitID,
		childRunID,
	)
	if err := tx.Commit(ctx); err != nil {
		t.Fatal(err)
	}
	return runLeaseID
}

func assertCancellationState(
	t *testing.T,
	fixture postgresFixture,
	work leasedRun,
	wantRun string,
	wantLease string,
	wantWorkspaceLease string,
) {
	t.Helper()
	var runStatus, leaseState, workspaceLeaseState string
	var attemptOutcome string
	if err := fixture.pool.QueryRow(t.Context(), `
SELECT runs.status,
       run_attempts.terminal_outcome,
       run_leases.state,
       workspace_leases.state
  FROM runs
  JOIN run_attempts
    ON run_attempts.run_id = runs.id
   AND run_attempts.number = runs.current_attempt_number
  JOIN run_leases ON run_leases.id = $2
  JOIN workspace_leases ON workspace_leases.owner_run_lease_id = run_leases.id
 WHERE runs.id = $1`, work.runID, work.leaseID,
	).Scan(&runStatus, &attemptOutcome, &leaseState, &workspaceLeaseState); err != nil {
		t.Fatal(err)
	}
	if runStatus != wantRun || attemptOutcome != "cancelled" ||
		leaseState != wantLease || workspaceLeaseState != wantWorkspaceLease {
		t.Fatalf(
			"authority state = run:%s attempt:%s lease:%s workspace-lease:%s",
			runStatus, attemptOutcome, leaseState, workspaceLeaseState,
		)
	}
}
