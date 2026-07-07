package db_test

import (
	"context"
	"encoding/json"
	"errors"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/helmrdotdev/helmr/internal/db"
	"github.com/helmrdotdev/helmr/internal/db/dbtest"
	"github.com/helmrdotdev/helmr/internal/pgvalue"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
)

func TestResolveImmediateTokenWaitForRunWaitCompletesNewWait(t *testing.T) {
	ctx := context.Background()
	pool := newIntegrationDB(t, ctx)
	ids := seedIntegration(t, ctx, pool)
	queries := db.New(pool)
	_, runLeaseID, workerID := seedRunningSessionLease(t, ctx, pool, ids)
	tokenID := uuid.Must(uuid.NewV7())
	waitID := uuid.Must(uuid.NewV7())
	runWaitID := uuid.Must(uuid.NewV7())

	if _, err := pool.Exec(ctx, `
		INSERT INTO tokens (
			id, public_id, org_id, project_id, environment_id, state,
			timeout_at, completion_data, completion_fingerprint, completed_at
		)
		VALUES ($1, $5, $2, $3, $4, 'completed', now() + interval '1 hour', '{"ok":true}'::jsonb, 'done', now())
	`, tokenID, ids.orgID, ids.projectID, ids.environmentID, testTokenPublicID(t)); err != nil {
		t.Fatal(err)
	}
	if _, err := queries.CreateHotRunWait(ctx, db.CreateHotRunWaitParams{
		OrgID:            pgvalue.UUID(ids.orgID),
		ProjectID:        pgvalue.UUID(ids.projectID),
		EnvironmentID:    pgvalue.UUID(ids.environmentID),
		RunID:            pgvalue.UUID(ids.runID),
		RunLeaseID:       pgvalue.UUID(runLeaseID),
		WorkerInstanceID: pgvalue.UUID(workerID),
		WaitID:           pgvalue.UUID(waitID),
		PublicID:         testWaitPublicID(t),
		Kind:             db.WaitKindToken,
		TokenID:          pgvalue.UUID(tokenID),
		ExpiresAt:        timestamptz(time.Now().Add(time.Hour)),
		RunWaitID:        pgvalue.UUID(runWaitID),
		CheckpointDelay:  interval(time.Minute),
	}); err != nil {
		t.Fatal(err)
	}

	resolved, err := queries.ResolveImmediateTokenWaitForRunWait(ctx, db.ResolveImmediateTokenWaitForRunWaitParams{
		OrgID:         pgvalue.UUID(ids.orgID),
		WorkerGroupID: dbtest.DefaultWorkerGroupID,
		ProjectID:     pgvalue.UUID(ids.projectID),
		EnvironmentID: pgvalue.UUID(ids.environmentID),
		RunWaitID:     pgvalue.UUID(runWaitID),
	})
	if err != nil {
		t.Fatal(err)
	}
	if resolved.State != db.RunWaitStateResuming {
		t.Fatalf("run wait state = %s, want resuming", resolved.State)
	}

	var waitState db.WaitState
	var result []byte
	if err := pool.QueryRow(ctx, `
		SELECT state, result
		  FROM waits
		 WHERE org_id = $1
		   AND id = $2
	`, ids.orgID, waitID).Scan(&waitState, &result); err != nil {
		t.Fatal(err)
	}
	if waitState != db.WaitStateCompleted {
		t.Fatalf("wait state = %s, want completed", waitState)
	}
	var payload map[string]bool
	if err := json.Unmarshal(result, &payload); err != nil {
		t.Fatal(err)
	}
	if !payload["ok"] {
		t.Fatalf("wait result = %s, want completed token payload", string(result))
	}
}

func TestResolveImmediateTokenWaitForRunWaitResumesTerminalWait(t *testing.T) {
	ctx := context.Background()
	pool := newIntegrationDB(t, ctx)
	ids := seedIntegration(t, ctx, pool)
	queries := db.New(pool)
	_, runLeaseID, workerID := seedRunningSessionLease(t, ctx, pool, ids)
	tokenID := uuid.Must(uuid.NewV7())
	waitID := uuid.Must(uuid.NewV7())
	runWaitID := uuid.Must(uuid.NewV7())

	if _, err := pool.Exec(ctx, `
		INSERT INTO tokens (
			id, public_id, org_id, project_id, environment_id, state,
			timeout_at, completion_data, completion_fingerprint, completed_at
		)
		VALUES ($1, $5, $2, $3, $4, 'completed', now() + interval '1 hour', '{"ok":true}'::jsonb, 'done', now())
	`, tokenID, ids.orgID, ids.projectID, ids.environmentID, testTokenPublicID(t)); err != nil {
		t.Fatal(err)
	}
	if _, err := queries.CreateHotRunWait(ctx, db.CreateHotRunWaitParams{
		OrgID:            pgvalue.UUID(ids.orgID),
		ProjectID:        pgvalue.UUID(ids.projectID),
		EnvironmentID:    pgvalue.UUID(ids.environmentID),
		RunID:            pgvalue.UUID(ids.runID),
		RunLeaseID:       pgvalue.UUID(runLeaseID),
		WorkerInstanceID: pgvalue.UUID(workerID),
		WaitID:           pgvalue.UUID(waitID),
		PublicID:         testWaitPublicID(t),
		Kind:             db.WaitKindToken,
		TokenID:          pgvalue.UUID(tokenID),
		ExpiresAt:        timestamptz(time.Now().Add(time.Hour)),
		RunWaitID:        pgvalue.UUID(runWaitID),
		CheckpointDelay:  interval(time.Minute),
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `
		UPDATE waits
		   SET state = 'completed',
		       result = '{"ok":true}'::jsonb,
		       completed_at = now()
		 WHERE org_id = $1
		   AND id = $2
	`, ids.orgID, waitID); err != nil {
		t.Fatal(err)
	}

	resolved, err := queries.ResolveImmediateTokenWaitForRunWait(ctx, db.ResolveImmediateTokenWaitForRunWaitParams{
		OrgID:         pgvalue.UUID(ids.orgID),
		WorkerGroupID: dbtest.DefaultWorkerGroupID,
		ProjectID:     pgvalue.UUID(ids.projectID),
		EnvironmentID: pgvalue.UUID(ids.environmentID),
		RunWaitID:     pgvalue.UUID(runWaitID),
	})
	if err != nil {
		t.Fatal(err)
	}
	if resolved.State != db.RunWaitStateResuming {
		t.Fatalf("run wait state = %s, want resuming", resolved.State)
	}
	assertWaitAndRunWaitState(t, ctx, pool, ids.orgID, waitID, db.WaitStateCompleted, runWaitID, db.RunWaitStateResuming)
}

func TestExpireDueRunWaitsIsScopedByWorkerGroup(t *testing.T) {
	ctx := context.Background()
	pool := newIntegrationDB(t, ctx)
	ids := seedIntegration(t, ctx, pool)
	queries := db.New(pool)
	_, runLeaseID, workerID := seedRunningSessionLease(t, ctx, pool, ids)
	waitID := uuid.Must(uuid.NewV7())
	runWaitID := uuid.Must(uuid.NewV7())
	otherWorkerGroupID := dbtest.DefaultWorkerGroupID + "-expiry-other"

	if _, err := pool.Exec(ctx, `
		INSERT INTO worker_groups (id, region_id, name, state, health_state, routing_fresh_until)
		VALUES ($1, $2, $1, 'active', 'healthy', now() + interval '5 minutes')
	`, otherWorkerGroupID, dbtest.DefaultRegionID); err != nil {
		t.Fatal(err)
	}
	if _, err := queries.CreateHotRunWait(ctx, db.CreateHotRunWaitParams{
		OrgID:            pgvalue.UUID(ids.orgID),
		ProjectID:        pgvalue.UUID(ids.projectID),
		EnvironmentID:    pgvalue.UUID(ids.environmentID),
		RunID:            pgvalue.UUID(ids.runID),
		RunLeaseID:       pgvalue.UUID(runLeaseID),
		WorkerInstanceID: pgvalue.UUID(workerID),
		WaitID:           pgvalue.UUID(waitID),
		PublicID:         testWaitPublicID(t),
		Kind:             db.WaitKindTimer,
		CompletedAfter:   timestamptz(time.Now().Add(time.Hour)),
		ExpiresAt:        timestamptz(time.Now().Add(-time.Minute)),
		RunWaitID:        pgvalue.UUID(runWaitID),
		CheckpointDelay:  interval(time.Minute),
	}); err != nil {
		t.Fatal(err)
	}

	wrongGroupRows, err := queries.ExpireDueRunWaits(ctx, db.ExpireDueRunWaitsParams{
		OrgID:         pgvalue.UUID(ids.orgID),
		WorkerGroupID: otherWorkerGroupID,
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(wrongGroupRows) != 0 {
		t.Fatalf("wrong worker group expired %d waits, want 0", len(wrongGroupRows))
	}
	assertWaitAndRunWaitState(t, ctx, pool, ids.orgID, waitID, db.WaitStatePending, runWaitID, db.RunWaitStateHotWaiting)

	defaultRows, err := queries.ExpireDueRunWaits(ctx, db.ExpireDueRunWaitsParams{
		OrgID:         pgvalue.UUID(ids.orgID),
		WorkerGroupID: dbtest.DefaultWorkerGroupID,
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(defaultRows) != 1 {
		t.Fatalf("default worker group expired %d waits, want 1", len(defaultRows))
	}
	assertWaitAndRunWaitState(t, ctx, pool, ids.orgID, waitID, db.WaitStateExpired, runWaitID, db.RunWaitStateResuming)
}

func TestCancelRunCancelsPendingWaits(t *testing.T) {
	ctx := context.Background()
	pool := newIntegrationDB(t, ctx)
	ids := seedIntegration(t, ctx, pool)
	queries := db.New(pool)
	_, runLeaseID, workerID := seedRunningSessionLease(t, ctx, pool, ids)
	waitID := uuid.Must(uuid.NewV7())
	runWaitID := uuid.Must(uuid.NewV7())
	operation := seedCancelOperation(t, ctx, queries, ids, "interrupt")

	if _, err := queries.CreateHotRunWait(ctx, db.CreateHotRunWaitParams{
		OrgID:            pgvalue.UUID(ids.orgID),
		ProjectID:        pgvalue.UUID(ids.projectID),
		EnvironmentID:    pgvalue.UUID(ids.environmentID),
		RunID:            pgvalue.UUID(ids.runID),
		RunLeaseID:       pgvalue.UUID(runLeaseID),
		WorkerInstanceID: pgvalue.UUID(workerID),
		WaitID:           pgvalue.UUID(waitID),
		PublicID:         testWaitPublicID(t),
		Kind:             db.WaitKindTimer,
		CompletedAfter:   timestamptz(time.Now().Add(time.Hour)),
		ExpiresAt:        timestamptz(time.Now().Add(2 * time.Hour)),
		RunWaitID:        pgvalue.UUID(runWaitID),
		CheckpointDelay:  interval(time.Minute),
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := queries.CancelRun(ctx, db.CancelRunParams{
		OrgID:         pgvalue.UUID(ids.orgID),
		WorkerGroupID: dbtest.DefaultWorkerGroupID,
		RunID:         pgvalue.UUID(ids.runID),
		Reason:        "interrupt",
		Force:         false,
		OperationID:   operation.ID,
	}); err != nil {
		t.Fatal(err)
	}

	assertWaitAndRunWaitState(t, ctx, pool, ids.orgID, waitID, db.WaitStateCancelled, runWaitID, db.RunWaitStateCancelled)
}

func TestCheckpointRunWaitRestoreLifecycle(t *testing.T) {
	ctx := context.Background()
	pool := newIntegrationDB(t, ctx)
	ids := seedIntegration(t, ctx, pool)
	queries := db.New(pool)
	_, runLeaseID, workerID := seedRunningSessionLease(t, ctx, pool, ids)
	seedActiveWorkspaceLeaseForRun(t, ctx, pool, ids)
	checkpointed := createCheckpointedRunWait(t, ctx, queries, ids, runLeaseID, workerID)
	assertCheckpointedRunWaitParked(t, ctx, pool, ids.orgID, ids.runID, checkpointed.runWaitID, runLeaseID, checkpointed.runtimeInstanceID, checkpointed.runCheckpointID)
	assertLatestRunTransition(t, ctx, pool, ids.orgID, ids.runID, "run.waiting")

	if _, err := queries.ResolveRunWait(ctx, db.ResolveRunWaitParams{
		OrgID:  pgvalue.UUID(ids.orgID),
		ID:     pgvalue.UUID(checkpointed.runWaitID),
		Result: []byte(`{"timer":true}`),
	}); err != nil {
		t.Fatal(err)
	}
	requeued, err := queries.RequeueResolvedRunWaits(ctx, db.RequeueResolvedRunWaitsParams{
		OrgID:         pgvalue.UUID(ids.orgID),
		WorkerGroupID: dbtest.DefaultWorkerGroupID,
		LimitCount:    10,
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(requeued) != 1 {
		t.Fatalf("requeued waits = %d, want 1", len(requeued))
	}
	assertWaitAndRunWaitState(t, ctx, pool, ids.orgID, checkpointed.waitID, db.WaitStateCompleted, checkpointed.runWaitID, db.RunWaitStateResuming)
	assertRunQueuedAfterWaitResume(t, ctx, pool, ids.orgID, ids.runID)
	assertLatestRunTransition(t, ctx, pool, ids.orgID, ids.runID, "run.resumed")

	mount, err := queries.EnsureWorkspaceMountRequested(ctx, db.EnsureWorkspaceMountRequestedParams{
		OrgID:           pgvalue.UUID(ids.orgID),
		ProjectID:       pgvalue.UUID(ids.projectID),
		EnvironmentID:   pgvalue.UUID(ids.environmentID),
		WorkspaceID:     pgvalue.UUID(ids.workspaceID),
		ID:              pgvalue.UUID(uuid.Must(uuid.NewV7())),
		RequestPriority: requeued[0].Priority,
		Request:         []byte(`{"reason":"checkpoint_restore_test"}`),
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := queries.SetQueuedRunWorkspaceMount(ctx, db.SetQueuedRunWorkspaceMountParams{
		WorkspaceMountID: mount.ID,
		OrgID:            pgvalue.UUID(ids.orgID),
		RunID:            pgvalue.UUID(ids.runID),
		WorkspaceID:      pgvalue.UUID(ids.workspaceID),
	}); err != nil {
		t.Fatal(err)
	}
	dispatchGeneration := currentRunDispatchGeneration(t, ctx, pool, ids.orgID, ids.runID)
	leased, err := queries.LeaseRunLease(ctx, leaseRunLeaseParamsWithGeneration(ids.orgID, ids.runID, workerID, "restore", dispatchGeneration))
	if err != nil {
		t.Fatal(err)
	}
	if got := pgvalue.MustUUIDValue(leased.RunLeaseRestoreRunCheckpointID); got != checkpointed.runCheckpointID {
		t.Fatalf("restore run checkpoint id = %s, want %s", got, checkpointed.runCheckpointID)
	}
	assertRunCheckpointRestore(t, ctx, pool, ids.orgID, ids.runID, checkpointed.runWaitID, pgvalue.MustUUIDValue(leased.RunLeaseID), workerID, db.RunCheckpointRestoreStatusRestoring)

	payload, err := queries.GetRunRestorePayload(ctx, db.GetRunRestorePayloadParams{
		OrgID:            pgvalue.UUID(ids.orgID),
		RunID:            pgvalue.UUID(ids.runID),
		RunLeaseID:       leased.RunLeaseID,
		WorkerInstanceID: pgvalue.UUID(workerID),
	})
	if err != nil {
		t.Fatal(err)
	}
	if got := pgvalue.MustUUIDValue(payload.RunCheckpointID); got != checkpointed.runCheckpointID {
		t.Fatalf("restore payload checkpoint id = %s, want %s", got, checkpointed.runCheckpointID)
	}
	if got := pgvalue.MustUUIDValue(payload.RunWaitID); got != checkpointed.runWaitID {
		t.Fatalf("restore payload run wait id = %s, want %s", got, checkpointed.runWaitID)
	}
	if payload.WaitState != db.WaitStateCompleted {
		t.Fatalf("restore payload wait state = %s, want completed", payload.WaitState)
	}
	var payloadResult map[string]bool
	if err := json.Unmarshal(payload.WaitResult, &payloadResult); err != nil {
		t.Fatal(err)
	}
	if !payloadResult["timer"] {
		t.Fatalf("restore payload wait result = %s, want timer payload", string(payload.WaitResult))
	}

	started, err := queries.StartRunLease(ctx, db.StartRunLeaseParams{
		OrgID:             pgvalue.UUID(ids.orgID),
		RunID:             pgvalue.UUID(ids.runID),
		RunLeaseID:        leased.RunLeaseID,
		WorkerInstanceID:  pgvalue.UUID(workerID),
		DispatchMessageID: leased.RunLeaseDispatchMessageID,
		DispatchLeaseID:   leased.RunLeaseDispatchLeaseID,
	})
	if err != nil {
		t.Fatal(err)
	}
	if started.Status != db.RunLeaseStatusRunning {
		t.Fatalf("started run lease status = %s, want running", started.Status)
	}
	resumed, err := queries.MarkRunResumeWaitResumed(ctx, db.MarkRunResumeWaitResumedParams{
		OrgID:           pgvalue.UUID(ids.orgID),
		ID:              pgvalue.UUID(checkpointed.runWaitID),
		RunID:           pgvalue.UUID(ids.runID),
		RunCheckpointID: pgvalue.UUID(checkpointed.runCheckpointID),
		RunLeaseID:      leased.RunLeaseID,
		RestorePhases:   []byte(`[{"name":"load","duration_ms":12}]`),
	})
	if err != nil {
		t.Fatal(err)
	}
	if resumed.State != db.RunWaitStateReleased {
		t.Fatalf("resumed run wait state = %s, want released", resumed.State)
	}
	assertRunCheckpointRestore(t, ctx, pool, ids.orgID, ids.runID, checkpointed.runWaitID, pgvalue.MustUUIDValue(leased.RunLeaseID), workerID, db.RunCheckpointRestoreStatusRestored)
}

func TestRequeueResolvedRunWaitsRequiresLatestRunCheckpoint(t *testing.T) {
	ctx := context.Background()
	pool := newIntegrationDB(t, ctx)
	ids := seedIntegration(t, ctx, pool)
	queries := db.New(pool)
	_, runLeaseID, workerID := seedRunningSessionLease(t, ctx, pool, ids)
	seedActiveWorkspaceLeaseForRun(t, ctx, pool, ids)
	otherRunCheckpointID := uuid.Must(uuid.NewV7())
	checkpointed := createCheckpointedRunWait(t, ctx, queries, ids, runLeaseID, workerID)
	if _, err := pool.Exec(ctx, `
		INSERT INTO run_checkpoints (
			id, org_id, worker_group_id, project_id, environment_id, workspace_id, run_id,
			source_workspace_lease_id, workspace_mount_id, base_workspace_version_id,
			state, runtime_backend, runtime_identity_id, runtime_arch, runtime_abi, kernel_digest,
			initramfs_digest, rootfs_digest, runtime_config_digest, owner_runtime_instance_id,
			owner_runtime_epoch, owner_run_id, owner_run_wait_id, owner_run_lease_id,
			owner_worker_instance_id, source_worker_instance_id, cni_profile, manifest, ready_at
		)
		SELECT $1, org_id, worker_group_id, project_id, environment_id, workspace_id, run_id,
		       source_workspace_lease_id, workspace_mount_id, base_workspace_version_id,
		       'ready', runtime_backend, runtime_identity_id, runtime_arch, runtime_abi, kernel_digest,
		       initramfs_digest, rootfs_digest, runtime_config_digest, owner_runtime_instance_id,
		       owner_runtime_epoch, owner_run_id, owner_run_wait_id, owner_run_lease_id,
		       owner_worker_instance_id, source_worker_instance_id, cni_profile, '{"checkpoint":"other"}'::jsonb, now()
		  FROM run_checkpoints
		 WHERE org_id = $2
		   AND run_id = $3
		   AND id = $4
	`, otherRunCheckpointID, ids.orgID, ids.runID, checkpointed.runCheckpointID); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `
		UPDATE runs
		   SET latest_run_checkpoint_id = $3
		 WHERE org_id = $1
		   AND id = $2
	`, ids.orgID, ids.runID, otherRunCheckpointID); err != nil {
		t.Fatal(err)
	}
	if _, err := queries.ResolveRunWait(ctx, db.ResolveRunWaitParams{
		OrgID:  pgvalue.UUID(ids.orgID),
		ID:     pgvalue.UUID(checkpointed.runWaitID),
		Result: []byte(`{"timer":true}`),
	}); err != nil {
		t.Fatal(err)
	}
	requeued, err := queries.RequeueResolvedRunWaits(ctx, db.RequeueResolvedRunWaitsParams{
		OrgID:         pgvalue.UUID(ids.orgID),
		WorkerGroupID: dbtest.DefaultWorkerGroupID,
		LimitCount:    10,
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(requeued) != 0 {
		t.Fatalf("requeued waits = %d, want 0", len(requeued))
	}
	failed, err := queries.FailStaleResolvedRunWaits(ctx, db.FailStaleResolvedRunWaitsParams{
		OrgID:         pgvalue.UUID(ids.orgID),
		WorkerGroupID: dbtest.DefaultWorkerGroupID,
		LimitCount:    10,
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(failed) != 1 {
		t.Fatalf("failed stale waits = %d, want 1", len(failed))
	}
	assertRunFailedAfterStaleWaitResume(t, ctx, pool, ids.orgID, ids.runID, checkpointed.runWaitID, checkpointed.runCheckpointID, otherRunCheckpointID, checkpointed.runtimeInstanceID)
}

func TestAcknowledgeWorkerCommandDoesNotAckCurrentRunCheckpointWait(t *testing.T) {
	ctx := context.Background()
	pool := newIntegrationDB(t, ctx)
	ids := seedIntegration(t, ctx, pool)
	queries := db.New(pool)
	_, runLeaseID, workerID := seedRunningSessionLease(t, ctx, pool, ids)
	waitID := uuid.Must(uuid.NewV7())
	runWaitID := uuid.Must(uuid.NewV7())

	runWait, command := createAcceptedHotRunCheckpointCommand(t, ctx, queries, ids, runLeaseID, workerID, waitID, runWaitID)

	if _, err := queries.AcknowledgeWorkerCommand(ctx, db.AcknowledgeWorkerCommandParams{
		WorkerInstanceID: pgvalue.UUID(workerID),
		WorkerGroupID:    dbtest.DefaultWorkerGroupID,
		ID:               command.ID,
	}); !errors.Is(err, pgx.ErrNoRows) {
		t.Fatalf("ack current checkpoint command err = %v, want no rows", err)
	}
	assertWorkerCommandAcknowledged(t, ctx, pool, ids.orgID, command.ID, false)

	if _, err := pool.Exec(ctx, `
		UPDATE runtime_instances
		   SET owner_run_wait_id = NULL,
		       updated_at = now()
		 WHERE org_id = $1
		   AND id = $2
	`, ids.orgID, pgvalue.MustUUIDValue(runWait.OwnerRuntimeInstanceID)); err != nil {
		t.Fatal(err)
	}
	if _, err := queries.AcknowledgeWorkerCommand(ctx, db.AcknowledgeWorkerCommandParams{
		WorkerInstanceID: pgvalue.UUID(workerID),
		WorkerGroupID:    dbtest.DefaultWorkerGroupID,
		ID:               command.ID,
	}); err != nil {
		t.Fatalf("ack stale checkpoint command: %v", err)
	}
	assertWorkerCommandAcknowledged(t, ctx, pool, ids.orgID, command.ID, true)
}

func TestAcknowledgeWorkerCommandForRunWaitRequiresTerminalCheckpointRecord(t *testing.T) {
	ctx := context.Background()
	pool := newIntegrationDB(t, ctx)
	ids := seedIntegration(t, ctx, pool)
	queries := db.New(pool)
	_, runLeaseID, workerID := seedRunningSessionLease(t, ctx, pool, ids)
	seedActiveWorkspaceLeaseForRun(t, ctx, pool, ids)
	waitID := uuid.Must(uuid.NewV7())
	runWaitID := uuid.Must(uuid.NewV7())
	runCheckpointID := uuid.Must(uuid.NewV7())

	_, command := createAcceptedHotRunCheckpointCommand(t, ctx, queries, ids, runLeaseID, workerID, waitID, runWaitID)
	if _, err := queries.SetRunWaitWorkspaceVersion(ctx, db.SetRunWaitWorkspaceVersionParams{
		OrgID:              pgvalue.UUID(ids.orgID),
		ProjectID:          pgvalue.UUID(ids.projectID),
		EnvironmentID:      pgvalue.UUID(ids.environmentID),
		ID:                 pgvalue.UUID(runWaitID),
		RunID:              pgvalue.UUID(ids.runID),
		WorkspaceVersionID: currentWorkspaceVersionID(t, ctx, queries, ids),
	}); err != nil {
		t.Fatalf("set run wait workspace version: %v", err)
	}

	if _, err := queries.AcknowledgeWorkerCommandForRunWait(ctx, db.AcknowledgeWorkerCommandForRunWaitParams{
		WorkerInstanceID: pgvalue.UUID(workerID),
		ID:               command.ID,
		OrgID:            pgvalue.UUID(ids.orgID),
		WorkerGroupID:    dbtest.DefaultWorkerGroupID,
		RunID:            pgvalue.UUID(ids.runID),
		RunWaitID:        pgvalue.UUID(runWaitID),
		RunLeaseID:       pgvalue.UUID(runLeaseID),
		Kind:             db.WorkerCommandKindRunCheckpointWait,
		RunCheckpointID:  pgvalue.UUID(runCheckpointID),
	}); !errors.Is(err, pgx.ErrNoRows) {
		t.Fatalf("ack checkpoint command before terminal checkpoint err = %v, want no rows", err)
	}

	if _, err := queries.ClaimRunCheckpointWait(ctx, db.ClaimRunCheckpointWaitParams{
		OrgID:            pgvalue.UUID(ids.orgID),
		ProjectID:        pgvalue.UUID(ids.projectID),
		EnvironmentID:    pgvalue.UUID(ids.environmentID),
		RunID:            pgvalue.UUID(ids.runID),
		RunWaitID:        pgvalue.UUID(runWaitID),
		RunLeaseID:       pgvalue.UUID(runLeaseID),
		WorkerInstanceID: pgvalue.UUID(workerID),
		RunCheckpointID:  pgvalue.UUID(runCheckpointID),
	}); err != nil {
		t.Fatalf("claim run checkpoint wait: %v", err)
	}
	if _, err := queries.AcknowledgeWorkerCommandForRunWait(ctx, db.AcknowledgeWorkerCommandForRunWaitParams{
		WorkerInstanceID: pgvalue.UUID(workerID),
		ID:               command.ID,
		OrgID:            pgvalue.UUID(ids.orgID),
		WorkerGroupID:    dbtest.DefaultWorkerGroupID,
		RunID:            pgvalue.UUID(ids.runID),
		RunWaitID:        pgvalue.UUID(runWaitID),
		RunLeaseID:       pgvalue.UUID(runLeaseID),
		Kind:             db.WorkerCommandKindRunCheckpointWait,
		RunCheckpointID:  pgvalue.UUID(runCheckpointID),
	}); !errors.Is(err, pgx.ErrNoRows) {
		t.Fatalf("ack checkpoint command while checkpointing err = %v, want no rows", err)
	}

	failed, err := queries.FailRunCheckpointAttempt(ctx, db.FailRunCheckpointAttemptParams{
		RunCheckpointID:  pgvalue.UUID(runCheckpointID),
		WorkerCommandID:  command.ID,
		OrgID:            pgvalue.UUID(ids.orgID),
		ProjectID:        pgvalue.UUID(ids.projectID),
		EnvironmentID:    pgvalue.UUID(ids.environmentID),
		RunID:            pgvalue.UUID(ids.runID),
		RunWaitID:        pgvalue.UUID(runWaitID),
		RunLeaseID:       pgvalue.UUID(runLeaseID),
		WorkerInstanceID: pgvalue.UUID(workerID),
		ErrorMessage:     "snapshot failed",
	})
	if err != nil {
		t.Fatalf("fail run checkpoint attempt: %v", err)
	}
	if failed.State != db.RunWaitStateHotWaiting {
		t.Fatalf("failed checkpoint run wait state = %s, want hot_waiting", failed.State)
	}
	if _, err := queries.AcknowledgeWorkerCommandForRunWait(ctx, db.AcknowledgeWorkerCommandForRunWaitParams{
		WorkerInstanceID: pgvalue.UUID(workerID),
		ID:               command.ID,
		OrgID:            pgvalue.UUID(ids.orgID),
		WorkerGroupID:    dbtest.DefaultWorkerGroupID,
		RunID:            pgvalue.UUID(ids.runID),
		RunWaitID:        pgvalue.UUID(runWaitID),
		RunLeaseID:       pgvalue.UUID(runLeaseID),
		Kind:             db.WorkerCommandKindRunCheckpointWait,
		RunCheckpointID:  pgvalue.UUID(runCheckpointID),
	}); err != nil {
		t.Fatalf("ack checkpoint command after failed checkpoint attempt: %v", err)
	}
	assertWorkerCommandAcknowledged(t, ctx, pool, ids.orgID, command.ID, true)

	if _, err := pool.Exec(ctx, `
		UPDATE run_waits
		   SET run_checkpoint_due_at = now(),
		       updated_at = now()
		 WHERE org_id = $1
		   AND id = $2
	`, ids.orgID, runWaitID); err != nil {
		t.Fatal(err)
	}
	retryCommands, err := queries.CreateDueLiveRunCheckpointWaitCommandsForWorker(ctx, db.CreateDueLiveRunCheckpointWaitCommandsForWorkerParams{
		WorkerInstanceID: pgvalue.UUID(workerID),
		LimitCount:       10,
	})
	if err != nil {
		t.Fatalf("create retry checkpoint wait command: %v", err)
	}
	if len(retryCommands) != 1 {
		t.Fatalf("retry checkpoint wait commands = %d, want 1", len(retryCommands))
	}
	retryCommand := retryCommands[0]
	if _, err := queries.AcceptWorkerCommand(ctx, db.AcceptWorkerCommandParams{
		WorkerInstanceID: pgvalue.UUID(workerID),
		ID:               retryCommand.ID,
		WorkerGroupID:    dbtest.DefaultWorkerGroupID,
	}); err != nil {
		t.Fatalf("accept retry checkpoint wait command: %v", err)
	}
	if _, err := queries.AcknowledgeWorkerCommandForRunWait(ctx, db.AcknowledgeWorkerCommandForRunWaitParams{
		WorkerInstanceID: pgvalue.UUID(workerID),
		ID:               retryCommand.ID,
		OrgID:            pgvalue.UUID(ids.orgID),
		WorkerGroupID:    dbtest.DefaultWorkerGroupID,
		RunID:            pgvalue.UUID(ids.runID),
		RunWaitID:        pgvalue.UUID(runWaitID),
		RunLeaseID:       pgvalue.UUID(runLeaseID),
		Kind:             db.WorkerCommandKindRunCheckpointWait,
		RunCheckpointID:  pgvalue.UUID(runCheckpointID),
	}); !errors.Is(err, pgx.ErrNoRows) {
		t.Fatalf("ack retry checkpoint command with old checkpoint err = %v, want no rows", err)
	}
	assertWorkerCommandAcknowledged(t, ctx, pool, ids.orgID, retryCommand.ID, false)
}

type checkpointedRunWaitFixture struct {
	waitID            uuid.UUID
	runWaitID         uuid.UUID
	runCheckpointID   uuid.UUID
	runtimeInstanceID uuid.UUID
}

func createAcceptedHotRunCheckpointCommand(t *testing.T, ctx context.Context, queries *db.Queries, ids integrationIDs, runLeaseID uuid.UUID, workerID uuid.UUID, waitID uuid.UUID, runWaitID uuid.UUID) (db.CreateHotRunWaitRow, db.WorkerCommand) {
	t.Helper()
	runWait, err := queries.CreateHotRunWait(ctx, db.CreateHotRunWaitParams{
		OrgID:            pgvalue.UUID(ids.orgID),
		ProjectID:        pgvalue.UUID(ids.projectID),
		EnvironmentID:    pgvalue.UUID(ids.environmentID),
		RunID:            pgvalue.UUID(ids.runID),
		RunLeaseID:       pgvalue.UUID(runLeaseID),
		WorkerInstanceID: pgvalue.UUID(workerID),
		WaitID:           pgvalue.UUID(waitID),
		PublicID:         testWaitPublicID(t),
		Kind:             db.WaitKindTimer,
		CompletedAfter:   timestamptz(time.Now().Add(time.Hour)),
		ExpiresAt:        timestamptz(time.Now().Add(2 * time.Hour)),
		RunWaitID:        pgvalue.UUID(runWaitID),
		CheckpointDelay:  interval(0),
	})
	if err != nil {
		t.Fatalf("create hot run wait: %v", err)
	}
	commands, err := queries.CreateDueLiveRunCheckpointWaitCommandsForWorker(ctx, db.CreateDueLiveRunCheckpointWaitCommandsForWorkerParams{
		WorkerInstanceID: pgvalue.UUID(workerID),
		LimitCount:       10,
	})
	if err != nil {
		t.Fatalf("create checkpoint wait command: %v", err)
	}
	if len(commands) != 1 {
		t.Fatalf("checkpoint wait commands = %d, want 1", len(commands))
	}
	command := commands[0]
	if _, err := queries.AcceptWorkerCommand(ctx, db.AcceptWorkerCommandParams{
		WorkerInstanceID: pgvalue.UUID(workerID),
		ID:               command.ID,
		WorkerGroupID:    dbtest.DefaultWorkerGroupID,
	}); err != nil {
		t.Fatalf("accept checkpoint wait command: %v", err)
	}
	return runWait, command
}

func createCheckpointedRunWait(t *testing.T, ctx context.Context, queries *db.Queries, ids integrationIDs, runLeaseID uuid.UUID, workerID uuid.UUID) checkpointedRunWaitFixture {
	t.Helper()
	waitID := uuid.Must(uuid.NewV7())
	runWaitID := uuid.Must(uuid.NewV7())
	runCheckpointID := uuid.Must(uuid.NewV7())

	if _, err := queries.CreateHotRunWait(ctx, db.CreateHotRunWaitParams{
		OrgID:            pgvalue.UUID(ids.orgID),
		ProjectID:        pgvalue.UUID(ids.projectID),
		EnvironmentID:    pgvalue.UUID(ids.environmentID),
		RunID:            pgvalue.UUID(ids.runID),
		RunLeaseID:       pgvalue.UUID(runLeaseID),
		WorkerInstanceID: pgvalue.UUID(workerID),
		WaitID:           pgvalue.UUID(waitID),
		PublicID:         testWaitPublicID(t),
		Kind:             db.WaitKindTimer,
		CompletedAfter:   timestamptz(time.Now().Add(time.Hour)),
		ExpiresAt:        timestamptz(time.Now().Add(2 * time.Hour)),
		RunWaitID:        pgvalue.UUID(runWaitID),
		CheckpointDelay:  interval(0),
	}); err != nil {
		t.Fatalf("create hot run wait: %v", err)
	}
	if _, err := queries.SetRunWaitWorkspaceVersion(ctx, db.SetRunWaitWorkspaceVersionParams{
		OrgID:              pgvalue.UUID(ids.orgID),
		ProjectID:          pgvalue.UUID(ids.projectID),
		EnvironmentID:      pgvalue.UUID(ids.environmentID),
		ID:                 pgvalue.UUID(runWaitID),
		RunID:              pgvalue.UUID(ids.runID),
		WorkspaceVersionID: currentWorkspaceVersionID(t, ctx, queries, ids),
	}); err != nil {
		t.Fatalf("set run wait workspace version: %v", err)
	}
	commands, err := queries.CreateDueLiveRunCheckpointWaitCommandsForWorker(ctx, db.CreateDueLiveRunCheckpointWaitCommandsForWorkerParams{
		WorkerInstanceID: pgvalue.UUID(workerID),
		LimitCount:       10,
	})
	if err != nil {
		t.Fatalf("create checkpoint wait command: %v", err)
	}
	if len(commands) != 1 {
		t.Fatalf("checkpoint wait commands = %d, want 1", len(commands))
	}
	command := commands[0]
	if command.Kind != db.WorkerCommandKindRunCheckpointWait {
		t.Fatalf("checkpoint command kind = %s, want run_checkpoint_wait", command.Kind)
	}
	if _, err := queries.AcceptWorkerCommand(ctx, db.AcceptWorkerCommandParams{
		WorkerInstanceID: pgvalue.UUID(workerID),
		ID:               command.ID,
		WorkerGroupID:    dbtest.DefaultWorkerGroupID,
	}); err != nil {
		t.Fatalf("accept checkpoint wait command: %v", err)
	}
	claimed, err := queries.ClaimRunCheckpointWait(ctx, db.ClaimRunCheckpointWaitParams{
		OrgID:            pgvalue.UUID(ids.orgID),
		ProjectID:        pgvalue.UUID(ids.projectID),
		EnvironmentID:    pgvalue.UUID(ids.environmentID),
		RunID:            pgvalue.UUID(ids.runID),
		RunWaitID:        pgvalue.UUID(runWaitID),
		RunLeaseID:       pgvalue.UUID(runLeaseID),
		WorkerInstanceID: pgvalue.UUID(workerID),
		RunCheckpointID:  pgvalue.UUID(runCheckpointID),
	})
	if err != nil {
		t.Fatalf("claim run checkpoint wait: %v", err)
	}
	checkpoint, err := queries.CreateReadyRunCheckpointForRunWait(ctx, db.CreateReadyRunCheckpointForRunWaitParams{
		WorkerCommandID:     command.ID,
		OrgID:               pgvalue.UUID(ids.orgID),
		ProjectID:           pgvalue.UUID(ids.projectID),
		EnvironmentID:       pgvalue.UUID(ids.environmentID),
		RunWaitID:           pgvalue.UUID(runWaitID),
		RunID:               pgvalue.UUID(ids.runID),
		RunCheckpointID:     pgvalue.UUID(runCheckpointID),
		RunLeaseID:          pgvalue.UUID(runLeaseID),
		WorkerInstanceID:    pgvalue.UUID(workerID),
		RuntimeBackend:      "firecracker",
		RuntimeID:           "test-runtime",
		RuntimeArch:         "arm64",
		RuntimeABI:          "test",
		KernelDigest:        "sha256:kernel",
		InitramfsDigest:     "sha256:initramfs",
		RootfsDigest:        "sha256:rootfs",
		RuntimeConfigDigest: "sha256:runtime-config",
		CniProfile:          "default",
		Manifest:            []byte(`{"checkpoint":"ready"}`),
	})
	if err != nil {
		t.Fatalf("create ready run checkpoint for run wait: %v", err)
	}
	if checkpoint.State != db.RunCheckpointStateReady {
		t.Fatalf("run checkpoint state = %s, want ready", checkpoint.State)
	}
	if _, err := queries.AcknowledgeWorkerCommandForRunWait(ctx, db.AcknowledgeWorkerCommandForRunWaitParams{
		WorkerInstanceID: pgvalue.UUID(workerID),
		ID:               command.ID,
		OrgID:            pgvalue.UUID(ids.orgID),
		WorkerGroupID:    dbtest.DefaultWorkerGroupID,
		RunID:            pgvalue.UUID(ids.runID),
		RunWaitID:        pgvalue.UUID(runWaitID),
		RunLeaseID:       pgvalue.UUID(runLeaseID),
		Kind:             db.WorkerCommandKindRunCheckpointWait,
		RunCheckpointID:  pgvalue.UUID(runCheckpointID),
	}); err != nil {
		t.Fatalf("acknowledge checkpoint wait command: %v", err)
	}
	return checkpointedRunWaitFixture{
		waitID:            waitID,
		runWaitID:         runWaitID,
		runCheckpointID:   runCheckpointID,
		runtimeInstanceID: pgvalue.MustUUIDValue(claimed.RuntimeInstanceID),
	}
}

func currentWorkspaceVersionID(t *testing.T, ctx context.Context, queries *db.Queries, ids integrationIDs) pgtype.UUID {
	t.Helper()
	workspace, err := queries.GetWorkspace(ctx, db.GetWorkspaceParams{
		OrgID:         pgvalue.UUID(ids.orgID),
		ProjectID:     pgvalue.UUID(ids.projectID),
		EnvironmentID: pgvalue.UUID(ids.environmentID),
		ID:            pgvalue.UUID(ids.workspaceID),
	})
	if err != nil {
		t.Fatalf("get workspace: %v", err)
	}
	if !workspace.CurrentVersionID.Valid {
		t.Fatalf("workspace current version id is null")
	}
	return workspace.CurrentVersionID
}

func assertWaitAndRunWaitState(t *testing.T, ctx context.Context, pool interface {
	QueryRow(context.Context, string, ...any) pgx.Row
}, orgID uuid.UUID, waitID uuid.UUID, wantWaitState db.WaitState, runWaitID uuid.UUID, wantRunWaitState db.RunWaitState) {
	t.Helper()
	var waitState db.WaitState
	var runWaitState db.RunWaitState
	if err := pool.QueryRow(ctx, `
		SELECT waits.state, run_waits.state
		  FROM waits
		  JOIN run_waits ON run_waits.org_id = waits.org_id
		                AND run_waits.wait_id = waits.id
		 WHERE waits.org_id = $1
		   AND waits.id = $2
		   AND run_waits.id = $3
	`, orgID, waitID, runWaitID).Scan(&waitState, &runWaitState); err != nil {
		t.Fatal(err)
	}
	if waitState != wantWaitState {
		t.Fatalf("wait state = %s, want %s", waitState, wantWaitState)
	}
	if runWaitState != wantRunWaitState {
		t.Fatalf("run wait state = %s, want %s", runWaitState, wantRunWaitState)
	}
}

func assertCheckpointedRunWaitParked(t *testing.T, ctx context.Context, pool interface {
	QueryRow(context.Context, string, ...any) pgx.Row
}, orgID uuid.UUID, runID uuid.UUID, runWaitID uuid.UUID, runLeaseID uuid.UUID, runtimeInstanceID uuid.UUID, runCheckpointID uuid.UUID) {
	t.Helper()
	var runStatus db.RunStatus
	var executionStatus db.RunExecutionStatus
	var currentRunLeaseID pgtype.UUID
	var latestRunCheckpointID pgtype.UUID
	var runWaitState db.RunWaitState
	var runLeaseStatus db.RunLeaseStatus
	var runtimeInstanceState db.RuntimeInstanceState
	var activeWorkspaceLeases int
	if err := pool.QueryRow(ctx, `
		SELECT runs.status,
		       runs.execution_status,
		       runs.current_run_lease_id,
		       runs.latest_run_checkpoint_id,
		       run_waits.state,
		       run_leases.status,
		       runtime_instances.state,
		       (
		           SELECT count(*)
		             FROM workspace_leases
		            WHERE workspace_leases.org_id = runs.org_id
		              AND workspace_leases.owner_run_id = runs.id
		              AND workspace_leases.state = 'active'
		       )::int
		  FROM runs
		  JOIN run_waits ON run_waits.org_id = runs.org_id
		                AND run_waits.run_id = runs.id
		                AND run_waits.id = $3
		  JOIN run_leases ON run_leases.org_id = runs.org_id
		                 AND run_leases.run_id = runs.id
		                 AND run_leases.id = $4
		  JOIN runtime_instances ON runtime_instances.org_id = runs.org_id
		                        AND runtime_instances.id = $5
		 WHERE runs.org_id = $1
		   AND runs.id = $2
	`, orgID, runID, runWaitID, runLeaseID, runtimeInstanceID).Scan(
		&runStatus,
		&executionStatus,
		&currentRunLeaseID,
		&latestRunCheckpointID,
		&runWaitState,
		&runLeaseStatus,
		&runtimeInstanceState,
		&activeWorkspaceLeases,
	); err != nil {
		t.Fatal(err)
	}
	if runStatus != db.RunStatusWaiting {
		t.Fatalf("run status = %s, want waiting", runStatus)
	}
	if executionStatus != db.RunExecutionStatusWaiting {
		t.Fatalf("run execution status = %s, want waiting", executionStatus)
	}
	if currentRunLeaseID.Valid {
		t.Fatalf("current run lease id is valid, want null")
	}
	if got := pgvalue.MustUUIDValue(latestRunCheckpointID); got != runCheckpointID {
		t.Fatalf("latest run checkpoint id = %s, want %s", got, runCheckpointID)
	}
	if runWaitState != db.RunWaitStateCheckpointedWaiting {
		t.Fatalf("run wait state = %s, want checkpointed_waiting", runWaitState)
	}
	if runLeaseStatus != db.RunLeaseStatusDetached {
		t.Fatalf("run lease status = %s, want detached", runLeaseStatus)
	}
	if runtimeInstanceState != db.RuntimeInstanceStateClosed {
		t.Fatalf("runtime instance state = %s, want closed", runtimeInstanceState)
	}
	if activeWorkspaceLeases != 0 {
		t.Fatalf("active workspace leases = %d, want 0", activeWorkspaceLeases)
	}
}

func assertRunQueuedAfterWaitResume(t *testing.T, ctx context.Context, pool interface {
	QueryRow(context.Context, string, ...any) pgx.Row
}, orgID uuid.UUID, runID uuid.UUID) {
	t.Helper()
	var runStatus db.RunStatus
	var executionStatus db.RunExecutionStatus
	var currentRunLeaseID pgtype.UUID
	if err := pool.QueryRow(ctx, `
		SELECT status, execution_status, current_run_lease_id
		  FROM runs
		 WHERE org_id = $1
		   AND id = $2
	`, orgID, runID).Scan(&runStatus, &executionStatus, &currentRunLeaseID); err != nil {
		t.Fatal(err)
	}
	if runStatus != db.RunStatusQueued {
		t.Fatalf("run status = %s, want queued", runStatus)
	}
	if executionStatus != db.RunExecutionStatusQueued {
		t.Fatalf("run execution status = %s, want queued", executionStatus)
	}
	if currentRunLeaseID.Valid {
		t.Fatalf("current run lease id is valid, want null")
	}
}

func assertLatestRunTransition(t *testing.T, ctx context.Context, pool interface {
	QueryRow(context.Context, string, ...any) pgx.Row
}, orgID uuid.UUID, runID uuid.UUID, wantTransition string) {
	t.Helper()
	var snapshotTransition string
	var eventKind string
	var outboxCount int
	if err := pool.QueryRow(ctx, `
		SELECT (
		           SELECT transition
		             FROM run_state_snapshots
		            WHERE org_id = $1
		              AND run_id = $2
		            ORDER BY version DESC
		            LIMIT 1
		       ),
		       (
		           SELECT kind
			             FROM telemetry_outbox
			            WHERE org_id = $1
			              AND run_id = $2
			              AND stream_kind = 'event'
			            ORDER BY id DESC
		            LIMIT 1
		       ),
		       (
		           SELECT count(*)
		             FROM telemetry_outbox
		            WHERE org_id = $1
		              AND source_kind = 'run'
		              AND source_id = $2
		       )::int
	`, orgID, runID).Scan(&snapshotTransition, &eventKind, &outboxCount); err != nil {
		t.Fatal(err)
	}
	if snapshotTransition != wantTransition {
		t.Fatalf("latest run snapshot transition = %s, want %s", snapshotTransition, wantTransition)
	}
	if eventKind != wantTransition {
		t.Fatalf("latest run event kind = %s, want %s", eventKind, wantTransition)
	}
	if outboxCount == 0 {
		t.Fatalf("telemetry outbox count = 0, want at least 1")
	}
}

func currentRunDispatchGeneration(t *testing.T, ctx context.Context, pool interface {
	QueryRow(context.Context, string, ...any) pgx.Row
}, orgID uuid.UUID, runID uuid.UUID) int64 {
	t.Helper()
	var dispatchGeneration int64
	if err := pool.QueryRow(ctx, `
		SELECT dispatch_generation
		  FROM runs
		 WHERE org_id = $1
		   AND id = $2
	`, orgID, runID).Scan(&dispatchGeneration); err != nil {
		t.Fatal(err)
	}
	return dispatchGeneration
}

func assertRunCheckpointRestore(t *testing.T, ctx context.Context, pool interface {
	QueryRow(context.Context, string, ...any) pgx.Row
}, orgID uuid.UUID, runID uuid.UUID, runWaitID uuid.UUID, runLeaseID uuid.UUID, workerID uuid.UUID, wantStatus db.RunCheckpointRestoreStatus) {
	t.Helper()
	var status db.RunCheckpointRestoreStatus
	var gotRunWaitID pgtype.UUID
	var gotRunLeaseID pgtype.UUID
	var gotWorkerID pgtype.UUID
	var acknowledgedAt pgtype.Timestamptz
	var finishedAt pgtype.Timestamptz
	var phases []byte
	if err := pool.QueryRow(ctx, `
		SELECT status,
		       run_wait_id,
		       run_lease_id,
		       worker_instance_id,
		       acknowledged_at,
		       finished_at,
		       phases
		  FROM run_checkpoint_restores
		 WHERE org_id = $1
		   AND run_id = $2
		   AND run_wait_id = $3
		   AND run_lease_id = $4
	`, orgID, runID, runWaitID, runLeaseID).Scan(
		&status,
		&gotRunWaitID,
		&gotRunLeaseID,
		&gotWorkerID,
		&acknowledgedAt,
		&finishedAt,
		&phases,
	); err != nil {
		t.Fatal(err)
	}
	if status != wantStatus {
		t.Fatalf("restore status = %s, want %s", status, wantStatus)
	}
	if got := pgvalue.MustUUIDValue(gotRunWaitID); got != runWaitID {
		t.Fatalf("restore run wait id = %s, want %s", got, runWaitID)
	}
	if got := pgvalue.MustUUIDValue(gotRunLeaseID); got != runLeaseID {
		t.Fatalf("restore run lease id = %s, want %s", got, runLeaseID)
	}
	if got := pgvalue.MustUUIDValue(gotWorkerID); got != workerID {
		t.Fatalf("restore worker id = %s, want %s", got, workerID)
	}
	if wantStatus != db.RunCheckpointRestoreStatusRestored {
		return
	}
	if !acknowledgedAt.Valid {
		t.Fatalf("restore acknowledged_at is null, want set")
	}
	if !finishedAt.Valid {
		t.Fatalf("restore finished_at is null, want set")
	}
	var phasePayload []map[string]any
	if err := json.Unmarshal(phases, &phasePayload); err != nil {
		t.Fatal(err)
	}
	if len(phasePayload) != 1 || phasePayload[0]["name"] != "load" {
		t.Fatalf("restore phases = %s, want load phase", string(phases))
	}
}

func assertWorkerCommandAcknowledged(t *testing.T, ctx context.Context, pool interface {
	QueryRow(context.Context, string, ...any) pgx.Row
}, orgID uuid.UUID, commandID int64, wantAcknowledged bool) {
	t.Helper()
	var acknowledgedAt pgtype.Timestamptz
	if err := pool.QueryRow(ctx, `
		SELECT acknowledged_at
		  FROM worker_commands
		 WHERE org_id = $1
		   AND id = $2
	`, orgID, commandID).Scan(&acknowledgedAt); err != nil {
		t.Fatal(err)
	}
	if acknowledgedAt.Valid != wantAcknowledged {
		t.Fatalf("worker command acknowledged = %t, want %t", acknowledgedAt.Valid, wantAcknowledged)
	}
}

func assertRunFailedAfterStaleWaitResume(t *testing.T, ctx context.Context, pool interface {
	QueryRow(context.Context, string, ...any) pgx.Row
}, orgID uuid.UUID, runID uuid.UUID, runWaitID uuid.UUID, runCheckpointID uuid.UUID, latestRunCheckpointID uuid.UUID, runtimeInstanceID uuid.UUID) {
	t.Helper()
	var runStatus db.RunStatus
	var executionStatus db.RunExecutionStatus
	var terminalOutcome db.NullRunTerminalOutcome
	var errorMessage string
	var currentRunLeaseID pgtype.UUID
	var gotLatestRunCheckpointID pgtype.UUID
	var runWaitState db.RunWaitState
	var gotRunWaitRunCheckpointID pgtype.UUID
	var runtimeInstanceState db.RuntimeInstanceState
	var staleCheckpointState db.RunCheckpointState
	var latestCheckpointState db.RunCheckpointState
	if err := pool.QueryRow(ctx, `
		SELECT runs.status,
		       runs.execution_status,
		       runs.terminal_outcome,
		       runs.error_message,
		       runs.current_run_lease_id,
		       runs.latest_run_checkpoint_id,
		       run_waits.state,
		       run_waits.run_checkpoint_id,
		       runtime_instances.state,
		       stale_checkpoint.state,
		       latest_checkpoint.state
		  FROM runs
		  JOIN run_waits ON run_waits.org_id = runs.org_id
		                AND run_waits.run_id = runs.id
		                AND run_waits.id = $3
		  JOIN runtime_instances ON runtime_instances.org_id = runs.org_id
		                        AND runtime_instances.id = $6
		  JOIN run_checkpoints stale_checkpoint
		    ON stale_checkpoint.org_id = runs.org_id
		   AND stale_checkpoint.run_id = runs.id
		   AND stale_checkpoint.id = $4
		  JOIN run_checkpoints latest_checkpoint
		    ON latest_checkpoint.org_id = runs.org_id
		   AND latest_checkpoint.run_id = runs.id
		   AND latest_checkpoint.id = $5
		 WHERE runs.org_id = $1
		   AND runs.id = $2
	`, orgID, runID, runWaitID, runCheckpointID, latestRunCheckpointID, runtimeInstanceID).Scan(
		&runStatus,
		&executionStatus,
		&terminalOutcome,
		&errorMessage,
		&currentRunLeaseID,
		&gotLatestRunCheckpointID,
		&runWaitState,
		&gotRunWaitRunCheckpointID,
		&runtimeInstanceState,
		&staleCheckpointState,
		&latestCheckpointState,
	); err != nil {
		t.Fatal(err)
	}
	if runStatus != db.RunStatusFailed {
		t.Fatalf("run status = %s, want failed", runStatus)
	}
	if executionStatus != db.RunExecutionStatusFinished {
		t.Fatalf("run execution status = %s, want finished", executionStatus)
	}
	if !terminalOutcome.Valid || terminalOutcome.RunTerminalOutcome != db.RunTerminalOutcomeFailed {
		t.Fatalf("terminal outcome = %+v, want failed", terminalOutcome)
	}
	if errorMessage != "resolved wait is not attached to the latest run checkpoint" {
		t.Fatalf("error message = %q, want non-latest checkpoint message", errorMessage)
	}
	if currentRunLeaseID.Valid {
		t.Fatalf("current run lease id is valid, want null")
	}
	if got := pgvalue.MustUUIDValue(gotLatestRunCheckpointID); got != latestRunCheckpointID {
		t.Fatalf("latest run checkpoint id = %s, want %s", got, latestRunCheckpointID)
	}
	if runWaitState != db.RunWaitStateFailed {
		t.Fatalf("run wait state = %s, want failed", runWaitState)
	}
	if got := pgvalue.MustUUIDValue(gotRunWaitRunCheckpointID); got != runCheckpointID {
		t.Fatalf("run wait run checkpoint id = %s, want %s", got, runCheckpointID)
	}
	if runtimeInstanceState != db.RuntimeInstanceStateClosed {
		t.Fatalf("runtime instance state = %s, want closed", runtimeInstanceState)
	}
	if staleCheckpointState != db.RunCheckpointStateInvalid {
		t.Fatalf("stale checkpoint state = %s, want invalid", staleCheckpointState)
	}
	if latestCheckpointState != db.RunCheckpointStateReady {
		t.Fatalf("latest checkpoint state = %s, want ready", latestCheckpointState)
	}
}

func timestamptz(t time.Time) pgtype.Timestamptz {
	return pgtype.Timestamptz{Time: t.UTC(), Valid: true}
}

func interval(d time.Duration) pgtype.Interval {
	return pgtype.Interval{Microseconds: d.Microseconds(), Valid: true}
}
