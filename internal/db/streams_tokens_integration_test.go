package db_test

import (
	"context"
	"errors"
	"sort"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/helmrdotdev/helmr/internal/db"
	"github.com/helmrdotdev/helmr/internal/pgvalue"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/jackc/pgx/v5/pgxpool"
)

func TestCurrentRunningRunLeaseAcceptsExecutingRun(t *testing.T) {
	ctx := context.Background()
	pool := newIntegrationDB(t, ctx)
	ids := seedIntegration(t, ctx, pool)
	queries := db.New(pool)
	_, runLeaseID, workerID := seedRunningSessionLease(t, ctx, pool, ids)

	row, err := queries.GetCurrentRunningRunLease(ctx, db.GetCurrentRunningRunLeaseParams{
		OrgID:            pgvalue.UUID(ids.orgID),
		RunID:            pgvalue.UUID(ids.runID),
		RunLeaseID:       pgvalue.UUID(runLeaseID),
		WorkerInstanceID: pgvalue.UUID(workerID),
	})
	if err != nil {
		t.Fatal(err)
	}
	if row.ID != pgvalue.UUID(runLeaseID) || row.RunID != pgvalue.UUID(ids.runID) {
		t.Fatalf("running lease row = %+v, want lease %v run %v", row, runLeaseID, ids.runID)
	}
}

func TestMarkRunWaitResumedAcceptsExecutingRun(t *testing.T) {
	ctx := context.Background()
	pool := newIntegrationDB(t, ctx)
	ids := seedIntegration(t, ctx, pool)
	queries := db.New(pool)
	_, runLeaseID, _ := seedRunningSessionLease(t, ctx, pool, ids)
	runWait := seedRunWait(t, ctx, queries, ids, db.RunWaitKindTimer)
	markRunWaitWaiting(t, ctx, pool, ids, runWait)
	storedWait, err := queries.GetRunWait(ctx, db.GetRunWaitParams{
		OrgID:         pgvalue.UUID(ids.orgID),
		ProjectID:     pgvalue.UUID(ids.projectID),
		EnvironmentID: pgvalue.UUID(ids.environmentID),
		ID:            runWait.ID,
	})
	if err != nil {
		t.Fatal(err)
	}
	if !storedWait.RuntimeCheckpointID.Valid {
		t.Fatal("test setup did not attach a runtime checkpoint")
	}
	if _, err := pool.Exec(ctx, `
		UPDATE run_waits
		   SET state = 'resuming'
		 WHERE org_id = $1
		   AND id = $2
	`, ids.orgID, pgvalue.MustUUIDValue(runWait.ID)); err != nil {
		t.Fatal(err)
	}

	resumed, err := queries.MarkRunWaitResumed(ctx, db.MarkRunWaitResumedParams{
		OrgID:               pgvalue.UUID(ids.orgID),
		ID:                  runWait.ID,
		RunID:               pgvalue.UUID(ids.runID),
		RuntimeCheckpointID: storedWait.RuntimeCheckpointID,
		RunLeaseID:          pgvalue.UUID(runLeaseID),
	})
	if err != nil {
		t.Fatal(err)
	}
	if resumed.ID != pgvalue.UUID(ids.runID) || resumed.ExecutionStatus != db.RunExecutionStatusExecuting {
		t.Fatalf("resumed run = %+v, want run %v executing", resumed, ids.runID)
	}
	after, err := queries.GetRunWait(ctx, db.GetRunWaitParams{
		OrgID:         pgvalue.UUID(ids.orgID),
		ProjectID:     pgvalue.UUID(ids.projectID),
		EnvironmentID: pgvalue.UUID(ids.environmentID),
		ID:            runWait.ID,
	})
	if err != nil {
		t.Fatal(err)
	}
	if after.State != db.RunWaitStateResumed {
		t.Fatalf("run wait state = %s, want resumed", after.State)
	}
}

func TestFailStaleResolvedRunWaitsTerminalizesWorkspaceVersionMismatch(t *testing.T) {
	ctx := context.Background()
	pool := newIntegrationDB(t, ctx)
	ids := seedIntegration(t, ctx, pool)
	queries := db.New(pool)
	_, runLeaseID, workerID := seedRunningSessionLease(t, ctx, pool, ids)
	runWait := seedRunWait(t, ctx, queries, ids, db.RunWaitKindTimer)
	seedActiveWorkspaceLeaseForRun(t, ctx, pool, ids)
	setRunWaitCurrentWorkspaceVersion(t, ctx, pool, ids, runWait)
	checkpointID := uuid.Must(uuid.NewV7())
	if _, err := queries.CreateReadyRuntimeCheckpointForRunWait(ctx, db.CreateReadyRuntimeCheckpointForRunWaitParams{
		OrgID:               pgvalue.UUID(ids.orgID),
		ProjectID:           pgvalue.UUID(ids.projectID),
		EnvironmentID:       pgvalue.UUID(ids.environmentID),
		RunWaitID:           runWait.ID,
		RunID:               pgvalue.UUID(ids.runID),
		RunLeaseID:          pgvalue.UUID(runLeaseID),
		WorkerInstanceID:    pgvalue.UUID(workerID),
		RuntimeCheckpointID: pgvalue.UUID(checkpointID),
		RuntimeBackend:      "test",
		RuntimeID:           "test-runtime",
		RuntimeArch:         "arm64",
		RuntimeABI:          "test-abi",
		KernelDigest:        "sha256:kernel",
		InitramfsDigest:     "sha256:initramfs",
		RootfsDigest:        "sha256:rootfs",
		RuntimeConfigDigest: "sha256:config",
		CniProfile:          "test-cni",
		Manifest:            []byte(`{"runtime":{"backend":"test"}}`),
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := queries.ResolveRunWait(ctx, db.ResolveRunWaitParams{
		OrgID: pgvalue.UUID(ids.orgID),
		ID:    runWait.ID,
	}); err != nil {
		t.Fatal(err)
	}
	nextArtifactID := seedWorkspaceVersionArtifact(t, ctx, pool, ids)
	nextVersionID := uuid.Must(uuid.NewV7())
	if _, err := pool.Exec(ctx, `
		INSERT INTO workspace_versions (
			id, org_id, project_id, environment_id, workspace_id, kind, state,
			artifact_id, artifact_encoding, artifact_entry_count, content_digest, size_bytes, promoted_at
		)
		SELECT $1, $2, $3, $4, $5, 'system', 'ready',
		       artifacts.id, 'tar', 0, artifacts.digest, artifacts.size_bytes, now()
		  FROM artifacts
		 WHERE artifacts.org_id = $2
		   AND artifacts.project_id = $3
		   AND artifacts.environment_id = $4
		   AND artifacts.id = $6
	`, nextVersionID, ids.orgID, ids.projectID, ids.environmentID, ids.workspaceID, nextArtifactID); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `
		UPDATE workspaces
		   SET current_version_id = $1
		 WHERE org_id = $2
		   AND id = $3
	`, nextVersionID, ids.orgID, ids.workspaceID); err != nil {
		t.Fatal(err)
	}

	failed, err := queries.FailStaleResolvedRunWaits(ctx, db.FailStaleResolvedRunWaitsParams{
		OrgID:      pgvalue.UUID(ids.orgID),
		LimitCount: 100,
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(failed) != 1 || failed[0].ID != runWait.ID || failed[0].State != db.RunWaitStateFailed {
		t.Fatalf("failed waits = %+v, want single failed wait %s", failed, pgvalue.MustUUIDValue(runWait.ID))
	}
	requeued, err := queries.RequeueResolvedRunWaits(ctx, db.RequeueResolvedRunWaitsParams{
		OrgID:      pgvalue.UUID(ids.orgID),
		LimitCount: 100,
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(requeued) != 0 {
		t.Fatalf("requeued stale waits = %+v, want none", requeued)
	}
	run, err := queries.GetRun(ctx, db.GetRunParams{
		OrgID: pgvalue.UUID(ids.orgID),
		ID:    pgvalue.UUID(ids.runID),
	})
	if err != nil {
		t.Fatal(err)
	}
	if run.Status != db.RunStatusFailed || run.ExecutionStatus != db.RunExecutionStatusFinished || !run.TerminalOutcome.Valid || run.TerminalOutcome.RunTerminalOutcome != db.RunTerminalOutcomeFailed {
		t.Fatalf("run terminal state = status %s execution %s outcome %+v", run.Status, run.ExecutionStatus, run.TerminalOutcome)
	}
	if run.ErrorMessage.String != "workspace advanced while run was parked" {
		t.Fatalf("run error = %q", run.ErrorMessage.String)
	}
	var sessionStatus db.SessionStatus
	var sessionReason []byte
	if err := pool.QueryRow(ctx, `
		SELECT status, terminal_reason
		  FROM sessions
		 WHERE org_id = $1
		   AND id = $2
	`, ids.orgID, pgvalue.MustUUIDValue(run.SessionID)).Scan(&sessionStatus, &sessionReason); err != nil {
		t.Fatal(err)
	}
	if sessionStatus != db.SessionStatusOpen || string(sessionReason) != "{}" {
		t.Fatalf("session status=%s reason=%s", sessionStatus, string(sessionReason))
	}
	var queueStatus db.RunQueueStatus
	if err := pool.QueryRow(ctx, `
		SELECT status
		  FROM run_queue_items
		 WHERE org_id = $1
		   AND run_id = $2
	`, ids.orgID, ids.runID).Scan(&queueStatus); err != nil {
		t.Fatal(err)
	}
	if queueStatus != db.RunQueueStatusCompleted {
		t.Fatalf("queue status = %s, want completed", queueStatus)
	}
}

func TestFailStaleResolvedRunWaitsTerminalizesQueuedWorkspaceVersionMismatch(t *testing.T) {
	ctx := context.Background()
	pool := newIntegrationDB(t, ctx)
	ids := seedIntegration(t, ctx, pool)
	queries := db.New(pool)
	_, runLeaseID, workerID := seedRunningSessionLease(t, ctx, pool, ids)
	runWait := seedRunWait(t, ctx, queries, ids, db.RunWaitKindTimer)
	seedActiveWorkspaceLeaseForRun(t, ctx, pool, ids)
	setRunWaitCurrentWorkspaceVersion(t, ctx, pool, ids, runWait)
	checkpointID := uuid.Must(uuid.NewV7())
	if _, err := queries.CreateReadyRuntimeCheckpointForRunWait(ctx, db.CreateReadyRuntimeCheckpointForRunWaitParams{
		OrgID:               pgvalue.UUID(ids.orgID),
		ProjectID:           pgvalue.UUID(ids.projectID),
		EnvironmentID:       pgvalue.UUID(ids.environmentID),
		RunWaitID:           runWait.ID,
		RunID:               pgvalue.UUID(ids.runID),
		RunLeaseID:          pgvalue.UUID(runLeaseID),
		WorkerInstanceID:    pgvalue.UUID(workerID),
		RuntimeCheckpointID: pgvalue.UUID(checkpointID),
		RuntimeBackend:      "test",
		RuntimeID:           "test-runtime",
		RuntimeArch:         "arm64",
		RuntimeABI:          "test-abi",
		KernelDigest:        "sha256:kernel",
		InitramfsDigest:     "sha256:initramfs",
		RootfsDigest:        "sha256:rootfs",
		RuntimeConfigDigest: "sha256:config",
		CniProfile:          "test-cni",
		Manifest:            []byte(`{"runtime":{"backend":"test"}}`),
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := queries.ResolveRunWait(ctx, db.ResolveRunWaitParams{
		OrgID: pgvalue.UUID(ids.orgID),
		ID:    runWait.ID,
	}); err != nil {
		t.Fatal(err)
	}
	requeued, err := queries.RequeueResolvedRunWaits(ctx, db.RequeueResolvedRunWaitsParams{
		OrgID:      pgvalue.UUID(ids.orgID),
		LimitCount: 100,
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(requeued) != 1 {
		t.Fatalf("requeued waits = %+v, want one", requeued)
	}
	advanceWorkspaceCurrentVersion(t, ctx, pool, ids)

	failed, err := queries.FailStaleResolvedRunWaits(ctx, db.FailStaleResolvedRunWaitsParams{
		OrgID:      pgvalue.UUID(ids.orgID),
		LimitCount: 100,
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(failed) != 1 || failed[0].ID != runWait.ID || failed[0].State != db.RunWaitStateFailed {
		t.Fatalf("failed waits = %+v, want queued resuming wait failed", failed)
	}
	run, err := queries.GetRun(ctx, db.GetRunParams{
		OrgID: pgvalue.UUID(ids.orgID),
		ID:    pgvalue.UUID(ids.runID),
	})
	if err != nil {
		t.Fatal(err)
	}
	if run.Status != db.RunStatusFailed || run.ExecutionStatus != db.RunExecutionStatusFinished {
		t.Fatalf("run status=%s execution=%s, want failed/finished", run.Status, run.ExecutionStatus)
	}
}

func TestFailStaleResolvedRunWaitsRejectsExpiredRuntimeCheckpoint(t *testing.T) {
	ctx := context.Background()
	pool := newIntegrationDB(t, ctx)
	ids := seedIntegration(t, ctx, pool)
	queries := db.New(pool)
	_, runLeaseID, workerID := seedRunningSessionLease(t, ctx, pool, ids)
	runWait := seedRunWait(t, ctx, queries, ids, db.RunWaitKindTimer)
	seedActiveWorkspaceLeaseForRun(t, ctx, pool, ids)
	setRunWaitCurrentWorkspaceVersion(t, ctx, pool, ids, runWait)
	checkpointID := uuid.Must(uuid.NewV7())
	if _, err := queries.CreateReadyRuntimeCheckpointForRunWait(ctx, db.CreateReadyRuntimeCheckpointForRunWaitParams{
		OrgID:               pgvalue.UUID(ids.orgID),
		ProjectID:           pgvalue.UUID(ids.projectID),
		EnvironmentID:       pgvalue.UUID(ids.environmentID),
		RunWaitID:           runWait.ID,
		RunID:               pgvalue.UUID(ids.runID),
		RunLeaseID:          pgvalue.UUID(runLeaseID),
		WorkerInstanceID:    pgvalue.UUID(workerID),
		RuntimeCheckpointID: pgvalue.UUID(checkpointID),
		RuntimeBackend:      "test",
		RuntimeID:           "test-runtime",
		RuntimeArch:         "arm64",
		RuntimeABI:          "test-abi",
		KernelDigest:        "sha256:kernel",
		InitramfsDigest:     "sha256:initramfs",
		RootfsDigest:        "sha256:rootfs",
		RuntimeConfigDigest: "sha256:config",
		CniProfile:          "test-cni",
		Manifest:            []byte(`{"runtime":{"backend":"test"}}`),
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := queries.ResolveRunWait(ctx, db.ResolveRunWaitParams{
		OrgID: pgvalue.UUID(ids.orgID),
		ID:    runWait.ID,
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `
		UPDATE runtime_checkpoints
		   SET expires_at = now() - interval '1 second'
		 WHERE org_id = $1
		   AND id = $2
	`, ids.orgID, checkpointID); err != nil {
		t.Fatal(err)
	}

	requeued, err := queries.RequeueResolvedRunWaits(ctx, db.RequeueResolvedRunWaitsParams{
		OrgID:      pgvalue.UUID(ids.orgID),
		LimitCount: 100,
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(requeued) != 0 {
		t.Fatalf("requeued expired checkpoint waits = %+v, want none", requeued)
	}
	failed, err := queries.FailStaleResolvedRunWaits(ctx, db.FailStaleResolvedRunWaitsParams{
		OrgID:      pgvalue.UUID(ids.orgID),
		LimitCount: 100,
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(failed) != 1 || failed[0].ID != runWait.ID || failed[0].State != db.RunWaitStateFailed {
		t.Fatalf("failed waits = %+v, want expired checkpoint wait failed", failed)
	}
	run, err := queries.GetRun(ctx, db.GetRunParams{
		OrgID: pgvalue.UUID(ids.orgID),
		ID:    pgvalue.UUID(ids.runID),
	})
	if err != nil {
		t.Fatal(err)
	}
	if run.Status != db.RunStatusFailed || run.ErrorMessage.String != "runtime checkpoint expired while run was parked" {
		t.Fatalf("run status=%s error=%q, want expired checkpoint failure", run.Status, run.ErrorMessage.String)
	}
	var checkpointState db.RuntimeCheckpointState
	if err := pool.QueryRow(ctx, `
		SELECT state
		  FROM runtime_checkpoints
		 WHERE org_id = $1
		   AND id = $2
	`, ids.orgID, checkpointID).Scan(&checkpointState); err != nil {
		t.Fatal(err)
	}
	if checkpointState != db.RuntimeCheckpointStateInvalid {
		t.Fatalf("checkpoint state = %s, want invalid", checkpointState)
	}
	var currentVersion uuid.UUID
	if err := pool.QueryRow(ctx, `
		SELECT current_version_id
		  FROM workspaces
		 WHERE org_id = $1
		   AND id = $2
	`, ids.orgID, ids.workspaceID).Scan(&currentVersion); err != nil {
		t.Fatal(err)
	}
	storedWait, err := queries.GetRunWait(ctx, db.GetRunWaitParams{
		OrgID:         pgvalue.UUID(ids.orgID),
		ProjectID:     pgvalue.UUID(ids.projectID),
		EnvironmentID: pgvalue.UUID(ids.environmentID),
		ID:            runWait.ID,
	})
	if err != nil {
		t.Fatal(err)
	}
	if pgvalue.MustUUIDValue(storedWait.WorkspaceVersionID) != currentVersion {
		t.Fatalf("workspace current version changed from parked truth: wait=%s current=%s", pgvalue.MustUUIDValue(storedWait.WorkspaceVersionID), currentVersion)
	}
	var sessionReason []byte
	if err := pool.QueryRow(ctx, `
		SELECT terminal_reason
		  FROM sessions
		 WHERE org_id = $1
		   AND id = $2
	`, ids.orgID, pgvalue.MustUUIDValue(run.SessionID)).Scan(&sessionReason); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(sessionReason), "runtime_checkpoint_expired") || !strings.Contains(string(sessionReason), checkpointID.String()) {
		t.Fatalf("session terminal reason = %s, want expired checkpoint audit details", string(sessionReason))
	}
}

func TestExpiredRunningLeaseCancelsParkingRunWait(t *testing.T) {
	ctx := context.Background()
	pool := newIntegrationDB(t, ctx)
	ids := seedIntegration(t, ctx, pool)
	queries := db.New(pool)
	_, runLeaseID, _ := seedRunningSessionLease(t, ctx, pool, ids)
	runWait := seedRunWait(t, ctx, queries, ids, db.RunWaitKindTimer)
	if runWait.State != db.RunWaitStateParking {
		t.Fatalf("run wait state = %s, want parking", runWait.State)
	}
	if _, err := pool.Exec(ctx, `
		UPDATE run_leases
		   SET started_at = now() - interval '2 seconds',
		       leased_at = now() - interval '2 seconds',
		       lease_expires_at = now() - interval '1 second'
		 WHERE org_id = $1
		   AND id = $2
	`, ids.orgID, runLeaseID); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `
		UPDATE runs
		   SET active_elapsed_ms = 500,
		       active_started_at = now() - interval '2 seconds'
		 WHERE org_id = $1
		   AND id = $2
	`, ids.orgID, ids.runID); err != nil {
		t.Fatal(err)
	}

	if err := queries.FailExpiredRunningRunLeases(ctx, pgvalue.UUID(ids.orgID)); err != nil {
		t.Fatal(err)
	}

	after, err := queries.GetRunWait(ctx, db.GetRunWaitParams{
		OrgID:         pgvalue.UUID(ids.orgID),
		ProjectID:     pgvalue.UUID(ids.projectID),
		EnvironmentID: pgvalue.UUID(ids.environmentID),
		ID:            runWait.ID,
	})
	if err != nil {
		t.Fatal(err)
	}
	if after.State != db.RunWaitStateCancelled {
		t.Fatalf("run wait state = %s, want cancelled", after.State)
	}
	run, err := queries.GetRun(ctx, db.GetRunParams{
		OrgID: pgvalue.UUID(ids.orgID),
		ID:    pgvalue.UUID(ids.runID),
	})
	if err != nil {
		t.Fatal(err)
	}
	if run.ActiveStartedAt.Valid {
		t.Fatalf("active_started_at = %+v, want closed active interval", run.ActiveStartedAt)
	}
	if run.ActiveElapsedMs < 2000 || run.ActiveElapsedMs >= run.MaxActiveDurationMs {
		t.Fatalf("active elapsed ms = %d max = %d, want DB lease-clock elapsed below max", run.ActiveElapsedMs, run.MaxActiveDurationMs)
	}
	var activeTimeUsageMs int64
	if err := pool.QueryRow(ctx, `
		SELECT COALESCE(SUM(quantity), 0)
		  FROM run_usage_events
		 WHERE org_id = $1
		   AND run_id = $2
		   AND kind = 'active_time'
	`, ids.orgID, ids.runID).Scan(&activeTimeUsageMs); err != nil {
		t.Fatal(err)
	}
	if activeTimeUsageMs != run.ActiveElapsedMs {
		t.Fatalf("active time usage ms = %d, want run active elapsed %d", activeTimeUsageMs, run.ActiveElapsedMs)
	}
}

func TestExpiredParkingLeaseMarksDirtyWorkspaceRecoveryRequired(t *testing.T) {
	ctx := context.Background()
	pool := newIntegrationDB(t, ctx)
	ids := seedIntegration(t, ctx, pool)
	queries := db.New(pool)
	_, runLeaseID, _ := seedRunningSessionLease(t, ctx, pool, ids)
	runWait := seedRunWait(t, ctx, queries, ids, db.RunWaitKindTimer)
	seedActiveWorkspaceLeaseForRun(t, ctx, pool, ids)
	if _, err := pool.Exec(ctx, `
		UPDATE workspace_materializations
		   SET dirty_generation = 1
		 WHERE org_id = $1
		   AND workspace_id = $2
		   AND state = 'running'
	`, ids.orgID, ids.workspaceID); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `
		UPDATE workspaces
		   SET dirty_state = 'dirty'
		 WHERE org_id = $1
		   AND id = $2
	`, ids.orgID, ids.workspaceID); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `
		UPDATE run_leases
		   SET lease_expires_at = now() - interval '1 second'
		 WHERE org_id = $1
		   AND id = $2
	`, ids.orgID, runLeaseID); err != nil {
		t.Fatal(err)
	}

	if err := queries.FailExpiredRunningRunLeases(ctx, pgvalue.UUID(ids.orgID)); err != nil {
		t.Fatal(err)
	}

	after, err := queries.GetRunWait(ctx, db.GetRunWaitParams{
		OrgID:         pgvalue.UUID(ids.orgID),
		ProjectID:     pgvalue.UUID(ids.projectID),
		EnvironmentID: pgvalue.UUID(ids.environmentID),
		ID:            runWait.ID,
	})
	if err != nil {
		t.Fatal(err)
	}
	if after.State != db.RunWaitStateCancelled {
		t.Fatalf("run wait state = %s, want cancelled", after.State)
	}
	run, err := queries.GetRun(ctx, db.GetRunParams{
		OrgID: pgvalue.UUID(ids.orgID),
		ID:    pgvalue.UUID(ids.runID),
	})
	if err != nil {
		t.Fatal(err)
	}
	if run.Status != db.RunStatusFailed {
		t.Fatalf("run status = %s, want failed instead of unsafe retry", run.Status)
	}
	var workspaceState db.WorkspaceState
	var dirtyState db.WorkspaceDirtyState
	if err := pool.QueryRow(ctx, `
		SELECT state, dirty_state
		  FROM workspaces
		 WHERE org_id = $1
		   AND id = $2
	`, ids.orgID, ids.workspaceID).Scan(&workspaceState, &dirtyState); err != nil {
		t.Fatal(err)
	}
	if workspaceState != db.WorkspaceStateRecoveryRequired || dirtyState != db.WorkspaceDirtyStateDirtyStateLost {
		t.Fatalf("workspace state=%s dirty_state=%s, want recovery_required/dirty_state_lost", workspaceState, dirtyState)
	}
	var activeLeases int
	if err := pool.QueryRow(ctx, `
		SELECT count(*)
		  FROM workspace_leases
		 WHERE org_id = $1
		   AND workspace_id = $2
		   AND owner_run_id = $3
		   AND released_at IS NULL
	`, ids.orgID, ids.workspaceID, ids.runID).Scan(&activeLeases); err != nil {
		t.Fatal(err)
	}
	if activeLeases != 0 {
		t.Fatalf("active workspace leases = %d, want released after recovery marking", activeLeases)
	}
}

func TestExpiredParkingLeaseAfterWorkspaceCaptureKeepsVersionAndInvalidatesCheckpoint(t *testing.T) {
	ctx := context.Background()
	pool := newIntegrationDB(t, ctx)
	ids := seedIntegration(t, ctx, pool)
	queries := db.New(pool)
	_, runLeaseID, _ := seedRunningSessionLease(t, ctx, pool, ids)
	runWait := seedRunWait(t, ctx, queries, ids, db.RunWaitKindTimer)
	seedActiveWorkspaceLeaseForRun(t, ctx, pool, ids)
	setRunWaitCurrentWorkspaceVersion(t, ctx, pool, ids, runWait)
	checkpointID := uuid.Must(uuid.NewV7())
	var workspaceLeaseID uuid.UUID
	var materializationID uuid.UUID
	var currentVersionID uuid.UUID
	if err := pool.QueryRow(ctx, `
		SELECT workspace_leases.id, workspace_leases.materialization_id, workspaces.current_version_id
		  FROM workspace_leases
		  JOIN workspaces
		    ON workspaces.org_id = workspace_leases.org_id
		   AND workspaces.id = workspace_leases.workspace_id
		 WHERE workspace_leases.org_id = $1
		   AND workspace_leases.workspace_id = $2
		   AND workspace_leases.owner_run_id = $3
		   AND workspace_leases.state = 'active'
	`, ids.orgID, ids.workspaceID, ids.runID).Scan(&workspaceLeaseID, &materializationID, &currentVersionID); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `
		INSERT INTO runtime_checkpoints (
			id, org_id, project_id, environment_id, workspace_id, run_id,
			source_workspace_lease_id, materialization_id, base_workspace_version_id,
			state, runtime_backend, runtime_id, runtime_arch, runtime_abi, kernel_digest,
			initramfs_digest, rootfs_digest, runtime_config_digest, cni_profile, manifest
		)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9,
		        'creating', 'test', 'test-runtime', 'arm64', 'test-abi', 'sha256:kernel',
		        'sha256:initramfs', 'sha256:rootfs', 'sha256:config', 'test-cni', '{}')
	`, checkpointID, ids.orgID, ids.projectID, ids.environmentID, ids.workspaceID, ids.runID, workspaceLeaseID, materializationID, currentVersionID); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `
		UPDATE run_waits
		   SET runtime_checkpoint_id = $1
		 WHERE org_id = $2
		   AND id = $3
	`, checkpointID, ids.orgID, pgvalue.MustUUIDValue(runWait.ID)); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `
		UPDATE run_leases
		   SET lease_expires_at = now() - interval '1 second'
		 WHERE org_id = $1
		   AND id = $2
	`, ids.orgID, runLeaseID); err != nil {
		t.Fatal(err)
	}

	if err := queries.FailExpiredRunningRunLeases(ctx, pgvalue.UUID(ids.orgID)); err != nil {
		t.Fatal(err)
	}

	var workspaceState db.WorkspaceState
	var dirtyState db.WorkspaceDirtyState
	var afterCurrentVersion uuid.UUID
	if err := pool.QueryRow(ctx, `
		SELECT state, dirty_state, current_version_id
		  FROM workspaces
		 WHERE org_id = $1
		   AND id = $2
	`, ids.orgID, ids.workspaceID).Scan(&workspaceState, &dirtyState, &afterCurrentVersion); err != nil {
		t.Fatal(err)
	}
	if workspaceState != db.WorkspaceStateActive || dirtyState != db.WorkspaceDirtyStateClean || afterCurrentVersion != currentVersionID {
		t.Fatalf("workspace state=%s dirty=%s current=%s, want active/clean/current %s", workspaceState, dirtyState, afterCurrentVersion, currentVersionID)
	}
	var checkpointState db.RuntimeCheckpointState
	var checkpointError pgtype.Text
	if err := pool.QueryRow(ctx, `
		SELECT state, error_message
		  FROM runtime_checkpoints
		 WHERE org_id = $1
		   AND id = $2
	`, ids.orgID, checkpointID).Scan(&checkpointState, &checkpointError); err != nil {
		t.Fatal(err)
	}
	if checkpointState != db.RuntimeCheckpointStateInvalid || checkpointError.String != "worker lease expired" {
		t.Fatalf("checkpoint state=%s error=%q, want invalid worker lease expired", checkpointState, checkpointError.String)
	}
}

func TestCreateReadyRuntimeCheckpointForRunWaitDetachesCurrentRunLease(t *testing.T) {
	ctx := context.Background()
	pool := newIntegrationDB(t, ctx)
	ids := seedIntegration(t, ctx, pool)
	queries := db.New(pool)
	_, runLeaseID, workerID := seedRunningSessionLease(t, ctx, pool, ids)
	runWait := seedRunWait(t, ctx, queries, ids, db.RunWaitKindTimer)
	seedActiveWorkspaceLeaseForRun(t, ctx, pool, ids)
	setRunWaitCurrentWorkspaceVersion(t, ctx, pool, ids, runWait)
	if _, err := pool.Exec(ctx, `
		UPDATE runs
		   SET active_elapsed_ms = 500,
		       active_started_at = now() - interval '1 second'
		 WHERE org_id = $1
		   AND id = $2
	`, ids.orgID, ids.runID); err != nil {
		t.Fatal(err)
	}

	_, err := queries.CreateReadyRuntimeCheckpointForRunWait(ctx, db.CreateReadyRuntimeCheckpointForRunWaitParams{
		OrgID:               pgvalue.UUID(ids.orgID),
		ProjectID:           pgvalue.UUID(ids.projectID),
		EnvironmentID:       pgvalue.UUID(ids.environmentID),
		RunWaitID:           runWait.ID,
		RunID:               pgvalue.UUID(ids.runID),
		RunLeaseID:          pgvalue.UUID(runLeaseID),
		WorkerInstanceID:    pgvalue.UUID(workerID),
		RuntimeCheckpointID: pgvalue.UUID(uuid.Must(uuid.NewV7())),
		RuntimeBackend:      "test",
		RuntimeID:           "test-runtime",
		RuntimeArch:         "arm64",
		RuntimeABI:          "test-abi",
		KernelDigest:        "sha256:kernel",
		InitramfsDigest:     "sha256:initramfs",
		RootfsDigest:        "sha256:rootfs",
		RuntimeConfigDigest: "sha256:config",
		CniProfile:          "test-cni",
		Manifest:            []byte(`{"runtime":{"backend":"test"}}`),
	})
	if err != nil {
		t.Fatal(err)
	}
	storedWait, err := queries.GetRunWait(ctx, db.GetRunWaitParams{
		OrgID:         pgvalue.UUID(ids.orgID),
		ProjectID:     pgvalue.UUID(ids.projectID),
		EnvironmentID: pgvalue.UUID(ids.environmentID),
		ID:            runWait.ID,
	})
	if err != nil {
		t.Fatal(err)
	}
	run, err := queries.GetRun(ctx, db.GetRunParams{OrgID: pgvalue.UUID(ids.orgID), ID: pgvalue.UUID(ids.runID)})
	if err != nil {
		t.Fatal(err)
	}
	var status db.RunLeaseStatus
	var activeDurationMs int64
	if err := pool.QueryRow(ctx, `SELECT status, active_duration_ms FROM run_leases WHERE org_id = $1 AND id = $2`, ids.orgID, runLeaseID).Scan(&status, &activeDurationMs); err != nil {
		t.Fatal(err)
	}
	if status != db.RunLeaseStatusDetached {
		t.Fatalf("run lease status = %s, want detached", status)
	}
	if storedWait.ActiveElapsedMsAtPark.Int64 <= 500 || activeDurationMs != storedWait.ActiveElapsedMsAtPark.Int64 || run.ActiveElapsedMs != storedWait.ActiveElapsedMsAtPark.Int64 {
		t.Fatalf("active time mismatch: wait=%d lease=%d run=%d", storedWait.ActiveElapsedMsAtPark.Int64, activeDurationMs, run.ActiveElapsedMs)
	}
	if run.CurrentRunLeaseID.Valid {
		t.Fatalf("current_run_lease_id still set: %v", run.CurrentRunLeaseID)
	}
	var materializationState db.WorkspaceMaterializationState
	var reservedCPU int32
	if err := pool.QueryRow(ctx, `
		SELECT state, reserved_cpu_millis
		  FROM workspace_materializations
		 WHERE org_id = $1
		   AND workspace_id = $2
	`, ids.orgID, ids.workspaceID).Scan(&materializationState, &reservedCPU); err != nil {
		t.Fatal(err)
	}
	if materializationState != db.WorkspaceMaterializationStateStopped || reservedCPU != 0 {
		t.Fatalf("materialization state=%s reserved_cpu=%d, want stopped/0", materializationState, reservedCPU)
	}
}

func TestCreateReadyRuntimeCheckpointRejectsSharedActiveMaterialization(t *testing.T) {
	ctx := context.Background()
	pool := newIntegrationDB(t, ctx)
	ids := seedIntegration(t, ctx, pool)
	queries := db.New(pool)
	_, runLeaseID, workerID := seedRunningSessionLease(t, ctx, pool, ids)
	runWait := seedRunWait(t, ctx, queries, ids, db.RunWaitKindTimer)
	seedActiveWorkspaceLeaseForRun(t, ctx, pool, ids)
	setRunWaitCurrentWorkspaceVersion(t, ctx, pool, ids, runWait)
	var materializationID uuid.UUID
	if err := pool.QueryRow(ctx, `
		SELECT id
		  FROM workspace_materializations
		 WHERE org_id = $1
		   AND workspace_id = $2
	`, ids.orgID, ids.workspaceID).Scan(&materializationID); err != nil {
		t.Fatal(err)
	}
	execID := uuid.Must(uuid.NewV7())
	if _, err := pool.Exec(ctx, `
		INSERT INTO workspace_execs (
			id, org_id, project_id, environment_id, workspace_id, materialization_id,
			command, state, detached, created_by_subject_type, created_by_subject_id
		)
		SELECT $1, org_id, project_id, environment_id, id, $2,
		       '["true"]'::jsonb, 'running', true, 'test', 'test'
		  FROM workspaces
		 WHERE org_id = $3
		   AND id = $4
	`, execID, materializationID, ids.orgID, ids.workspaceID); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `
		INSERT INTO workspace_leases (
			id, org_id, project_id, environment_id, workspace_id, materialization_id,
			lease_kind, state, owner_exec_id, base_version_id, acquired_version_id,
			acquired_fencing_generation, fencing_token, expires_at
		)
		SELECT $1, org_id, project_id, environment_id, id, $2,
		       'instance', 'active', $3, current_version_id, current_version_id,
		       1, 'exec-instance-token', now() + interval '1 hour'
		  FROM workspaces
		 WHERE org_id = $4
		   AND id = $5
	`, uuid.Must(uuid.NewV7()), materializationID, execID, ids.orgID, ids.workspaceID); err != nil {
		t.Fatal(err)
	}

	_, err := queries.CreateReadyRuntimeCheckpointForRunWait(ctx, db.CreateReadyRuntimeCheckpointForRunWaitParams{
		OrgID:               pgvalue.UUID(ids.orgID),
		ProjectID:           pgvalue.UUID(ids.projectID),
		EnvironmentID:       pgvalue.UUID(ids.environmentID),
		RunWaitID:           runWait.ID,
		RunID:               pgvalue.UUID(ids.runID),
		RunLeaseID:          pgvalue.UUID(runLeaseID),
		WorkerInstanceID:    pgvalue.UUID(workerID),
		RuntimeCheckpointID: pgvalue.UUID(uuid.Must(uuid.NewV7())),
		RuntimeBackend:      "test",
		RuntimeID:           "test-runtime",
		RuntimeArch:         "arm64",
		RuntimeABI:          "test-abi",
		KernelDigest:        "sha256:kernel",
		InitramfsDigest:     "sha256:initramfs",
		RootfsDigest:        "sha256:rootfs",
		RuntimeConfigDigest: "sha256:config",
		CniProfile:          "test-cni",
		Manifest:            []byte(`{"runtime":{"backend":"test"}}`),
	})
	if !errors.Is(err, pgx.ErrNoRows) {
		t.Fatalf("err = %v, want no rows for shared active materialization", err)
	}
}

func TestCreateReadyRuntimeCheckpointForRunWaitRejectsDirtyCleanPath(t *testing.T) {
	ctx := context.Background()
	pool := newIntegrationDB(t, ctx)
	ids := seedIntegration(t, ctx, pool)
	queries := db.New(pool)
	_, runLeaseID, workerID := seedRunningSessionLease(t, ctx, pool, ids)
	runWait := seedRunWait(t, ctx, queries, ids, db.RunWaitKindTimer)
	seedActiveWorkspaceLeaseForRun(t, ctx, pool, ids)
	setRunWaitCurrentWorkspaceVersion(t, ctx, pool, ids, runWait)
	if _, err := pool.Exec(ctx, `
		UPDATE workspace_materializations
		   SET dirty_generation = 1
		 WHERE org_id = $1
		   AND workspace_id = $2
		   AND state = 'running'
	`, ids.orgID, ids.workspaceID); err != nil {
		t.Fatal(err)
	}

	_, err := queries.CreateReadyRuntimeCheckpointForRunWait(ctx, db.CreateReadyRuntimeCheckpointForRunWaitParams{
		OrgID:               pgvalue.UUID(ids.orgID),
		ProjectID:           pgvalue.UUID(ids.projectID),
		EnvironmentID:       pgvalue.UUID(ids.environmentID),
		RunWaitID:           runWait.ID,
		RunID:               pgvalue.UUID(ids.runID),
		RunLeaseID:          pgvalue.UUID(runLeaseID),
		WorkerInstanceID:    pgvalue.UUID(workerID),
		RuntimeCheckpointID: pgvalue.UUID(uuid.Must(uuid.NewV7())),
		RuntimeBackend:      "test",
		RuntimeID:           "test-runtime",
		RuntimeArch:         "arm64",
		RuntimeABI:          "test-abi",
		KernelDigest:        "sha256:kernel",
		InitramfsDigest:     "sha256:initramfs",
		RootfsDigest:        "sha256:rootfs",
		RuntimeConfigDigest: "sha256:config",
		CniProfile:          "test-cni",
		Manifest:            []byte(`{"runtime":{"backend":"test"}}`),
	})
	if !errors.Is(err, pgx.ErrNoRows) {
		t.Fatalf("err = %v, want no rows for dirty clean-path checkpoint", err)
	}
	storedWait, err := queries.GetRunWait(ctx, db.GetRunWaitParams{
		OrgID:         pgvalue.UUID(ids.orgID),
		ProjectID:     pgvalue.UUID(ids.projectID),
		EnvironmentID: pgvalue.UUID(ids.environmentID),
		ID:            runWait.ID,
	})
	if err != nil {
		t.Fatal(err)
	}
	if storedWait.State != db.RunWaitStateParking {
		t.Fatalf("run wait state = %s, want parking after rejected checkpoint", storedWait.State)
	}
}

func TestDirtyRunWaitCapturePromotesSystemVersionBeforeCheckpointReady(t *testing.T) {
	ctx := context.Background()
	pool := newIntegrationDB(t, ctx)
	ids := seedIntegration(t, ctx, pool)
	queries := db.New(pool)
	_, runLeaseID, workerID := seedRunningSessionLease(t, ctx, pool, ids)
	runWait := seedRunWait(t, ctx, queries, ids, db.RunWaitKindTimer)
	seedActiveWorkspaceLeaseForRun(t, ctx, pool, ids)
	if _, err := pool.Exec(ctx, `
		UPDATE workspace_materializations
		   SET dirty_generation = 1
		 WHERE org_id = $1
		   AND workspace_id = $2
		   AND state = 'running'
	`, ids.orgID, ids.workspaceID); err != nil {
		t.Fatal(err)
	}
	captureArtifactID := uuid.Must(uuid.NewV7())
	captureVersionID := uuid.Must(uuid.NewV7())
	captureDigest := "sha256:" + strings.Repeat("b", 64)
	if _, err := pool.Exec(ctx, `
		INSERT INTO cas_objects (digest, size_bytes, media_type)
		VALUES ($1, 42, 'application/vnd.helmr.workspace.v0.tar')
	`, captureDigest); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `
		INSERT INTO artifacts (id, org_id, project_id, environment_id, digest, kind, size_bytes, media_type)
		VALUES ($1, $2, $3, $4, $5, 'workspace_version', 42, 'application/vnd.helmr.workspace.v0.tar')
	`, captureArtifactID, ids.orgID, ids.projectID, ids.environmentID, captureDigest); err != nil {
		t.Fatal(err)
	}
	var workspaceLeaseID uuid.UUID
	var fencingToken string
	if err := pool.QueryRow(ctx, `
		SELECT id, fencing_token
		  FROM workspace_leases
		 WHERE org_id = $1
		   AND workspace_id = $2
		   AND owner_run_id = $3
		   AND state = 'active'
	`, ids.orgID, ids.workspaceID, ids.runID).Scan(&workspaceLeaseID, &fencingToken); err != nil {
		t.Fatal(err)
	}
	version, err := queries.PromoteWorkspaceCapture(ctx, db.PromoteWorkspaceCaptureParams{
		OrgID:              pgvalue.UUID(ids.orgID),
		WriteLeaseID:       pgvalue.UUID(workspaceLeaseID),
		FencingToken:       fencingToken,
		DirtyGeneration:    1,
		ArtifactID:         pgvalue.UUID(captureArtifactID),
		SizeBytes:          42,
		ArtifactEncoding:   "tar",
		ContentDigest:      captureDigest,
		VersionID:          pgvalue.UUID(captureVersionID),
		Kind:               db.WorkspaceVersionKindSystem,
		ArtifactEntryCount: 2,
		Message:            "system capture before parked wait",
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := queries.SetRunWaitWorkspaceVersion(ctx, db.SetRunWaitWorkspaceVersionParams{
		OrgID:              pgvalue.UUID(ids.orgID),
		ProjectID:          pgvalue.UUID(ids.projectID),
		EnvironmentID:      pgvalue.UUID(ids.environmentID),
		ID:                 runWait.ID,
		RunID:              pgvalue.UUID(ids.runID),
		WorkspaceVersionID: version.ID,
	}); err != nil {
		t.Fatal(err)
	}
	_, err = queries.CreateReadyRuntimeCheckpointForRunWait(ctx, db.CreateReadyRuntimeCheckpointForRunWaitParams{
		OrgID:               pgvalue.UUID(ids.orgID),
		ProjectID:           pgvalue.UUID(ids.projectID),
		EnvironmentID:       pgvalue.UUID(ids.environmentID),
		RunWaitID:           runWait.ID,
		RunID:               pgvalue.UUID(ids.runID),
		RunLeaseID:          pgvalue.UUID(runLeaseID),
		WorkerInstanceID:    pgvalue.UUID(workerID),
		RuntimeCheckpointID: pgvalue.UUID(uuid.Must(uuid.NewV7())),
		RuntimeBackend:      "test",
		RuntimeID:           "test-runtime",
		RuntimeArch:         "arm64",
		RuntimeABI:          "test-abi",
		KernelDigest:        "sha256:kernel",
		InitramfsDigest:     "sha256:initramfs",
		RootfsDigest:        "sha256:rootfs",
		RuntimeConfigDigest: "sha256:config",
		CniProfile:          "test-cni",
		Manifest:            []byte(`{"runtime":{"backend":"test"}}`),
	})
	if err != nil {
		t.Fatal(err)
	}
	var currentVersionID uuid.UUID
	var dirtyGeneration int64
	if err := pool.QueryRow(ctx, `
		SELECT workspaces.current_version_id, workspace_materializations.dirty_generation
		  FROM workspaces
		  JOIN workspace_materializations
		    ON workspace_materializations.org_id = workspaces.org_id
		   AND workspace_materializations.workspace_id = workspaces.id
		 WHERE workspaces.org_id = $1
		   AND workspaces.id = $2
	`, ids.orgID, ids.workspaceID).Scan(&currentVersionID, &dirtyGeneration); err != nil {
		t.Fatal(err)
	}
	if currentVersionID != captureVersionID || dirtyGeneration != 0 {
		t.Fatalf("workspace current=%s dirty=%d, want current=%s dirty=0", currentVersionID, dirtyGeneration, captureVersionID)
	}
	storedWait, err := queries.GetRunWait(ctx, db.GetRunWaitParams{
		OrgID:         pgvalue.UUID(ids.orgID),
		ProjectID:     pgvalue.UUID(ids.projectID),
		EnvironmentID: pgvalue.UUID(ids.environmentID),
		ID:            runWait.ID,
	})
	if err != nil {
		t.Fatal(err)
	}
	if storedWait.State != db.RunWaitStateWaiting || pgvalue.MustUUIDValue(storedWait.WorkspaceVersionID) != captureVersionID || !storedWait.RuntimeCheckpointID.Valid {
		t.Fatalf("run wait = %+v, want waiting captured version and checkpoint", storedWait)
	}
	var checkpointBase uuid.UUID
	if err := pool.QueryRow(ctx, `
		SELECT base_workspace_version_id
		  FROM runtime_checkpoints
		 WHERE org_id = $1
		   AND id = $2
	`, ids.orgID, pgvalue.MustUUIDValue(storedWait.RuntimeCheckpointID)).Scan(&checkpointBase); err != nil {
		t.Fatal(err)
	}
	if checkpointBase != captureVersionID {
		t.Fatalf("checkpoint base = %s, want %s", checkpointBase, captureVersionID)
	}
}

func TestCreateReadyRuntimeCheckpointDoesNotRegressActiveTimeWhenClockMovesBackward(t *testing.T) {
	ctx := context.Background()
	pool := newIntegrationDB(t, ctx)
	ids := seedIntegration(t, ctx, pool)
	queries := db.New(pool)
	_, runLeaseID, workerID := seedRunningSessionLease(t, ctx, pool, ids)
	runWait := seedRunWait(t, ctx, queries, ids, db.RunWaitKindTimer)
	seedActiveWorkspaceLeaseForRun(t, ctx, pool, ids)
	setRunWaitCurrentWorkspaceVersion(t, ctx, pool, ids, runWait)
	if _, err := pool.Exec(ctx, `
		UPDATE runs
		   SET active_elapsed_ms = 500,
		       active_started_at = now() + interval '2 seconds'
		 WHERE org_id = $1
		   AND id = $2
	`, ids.orgID, ids.runID); err != nil {
		t.Fatal(err)
	}

	_, err := queries.CreateReadyRuntimeCheckpointForRunWait(ctx, db.CreateReadyRuntimeCheckpointForRunWaitParams{
		OrgID:               pgvalue.UUID(ids.orgID),
		ProjectID:           pgvalue.UUID(ids.projectID),
		EnvironmentID:       pgvalue.UUID(ids.environmentID),
		RunWaitID:           runWait.ID,
		RunID:               pgvalue.UUID(ids.runID),
		RunLeaseID:          pgvalue.UUID(runLeaseID),
		WorkerInstanceID:    pgvalue.UUID(workerID),
		RuntimeCheckpointID: pgvalue.UUID(uuid.Must(uuid.NewV7())),
		RuntimeBackend:      "test",
		RuntimeID:           "test-runtime",
		RuntimeArch:         "arm64",
		RuntimeABI:          "test-abi",
		KernelDigest:        "sha256:kernel",
		InitramfsDigest:     "sha256:initramfs",
		RootfsDigest:        "sha256:rootfs",
		RuntimeConfigDigest: "sha256:config",
		CniProfile:          "test-cni",
		Manifest:            []byte(`{"runtime":{"backend":"test"}}`),
	})
	if err != nil {
		t.Fatal(err)
	}
	storedWait, err := queries.GetRunWait(ctx, db.GetRunWaitParams{
		OrgID:         pgvalue.UUID(ids.orgID),
		ProjectID:     pgvalue.UUID(ids.projectID),
		EnvironmentID: pgvalue.UUID(ids.environmentID),
		ID:            runWait.ID,
	})
	if err != nil {
		t.Fatal(err)
	}
	run, err := queries.GetRun(ctx, db.GetRunParams{OrgID: pgvalue.UUID(ids.orgID), ID: pgvalue.UUID(ids.runID)})
	if err != nil {
		t.Fatal(err)
	}
	if storedWait.ActiveElapsedMsAtPark.Int64 != 500 || run.ActiveElapsedMs != 500 {
		t.Fatalf("active time regressed or advanced under negative elapsed interval: wait=%d run=%d", storedWait.ActiveElapsedMsAtPark.Int64, run.ActiveElapsedMs)
	}
}

func TestStreamRecordAppendUsesStreamSequenceAndIdempotency(t *testing.T) {
	ctx := context.Background()
	pool := newIntegrationDB(t, ctx)
	ids := seedIntegration(t, ctx, pool)
	queries := db.New(pool)
	stream := seedSessionStream(t, ctx, queries, ids, db.StreamDirectionInput, "approval")

	firstData := []byte(`{"approved":true}`)
	first, err := queries.AppendStreamRecord(ctx, db.AppendStreamRecordParams{
		ID:                     pgvalue.UUID(uuid.Must(uuid.NewV7())),
		OrgID:                  pgvalue.UUID(ids.orgID),
		ProjectID:              pgvalue.UUID(ids.projectID),
		EnvironmentID:          pgvalue.UUID(ids.environmentID),
		StreamID:               stream.ID,
		Direction:              db.StreamDirectionInput,
		Data:                   firstData,
		CorrelationID:          "thread-1",
		ContentType:            "application/json",
		IdempotencyKey:         "provider-event-1",
		IdempotencyFingerprint: canonicalFingerprint(t, firstData),
		SourceType:             db.StreamRecordSourceTypeApiKey,
		SourceID:               "test",
	})
	if err != nil {
		t.Fatal(err)
	}
	if first.Sequence != 1 || first.IsCached || first.IdempotencyFingerprintMismatch {
		t.Fatalf("first append = seq %d cached %v mismatch %v", first.Sequence, first.IsCached, first.IdempotencyFingerprintMismatch)
	}
	replay, err := queries.AppendStreamRecord(ctx, db.AppendStreamRecordParams{
		ID:                     pgvalue.UUID(uuid.Must(uuid.NewV7())),
		OrgID:                  pgvalue.UUID(ids.orgID),
		ProjectID:              pgvalue.UUID(ids.projectID),
		EnvironmentID:          pgvalue.UUID(ids.environmentID),
		StreamID:               stream.ID,
		Direction:              db.StreamDirectionInput,
		Data:                   []byte(`{"approved":true}`),
		ContentType:            "application/json",
		IdempotencyKey:         "provider-event-1",
		IdempotencyFingerprint: canonicalFingerprint(t, []byte(`{"approved":true}`)),
		SourceType:             db.StreamRecordSourceTypeApiKey,
		SourceID:               "test",
	})
	if err != nil {
		t.Fatal(err)
	}
	if replay.ID != first.ID || !replay.IsCached || replay.IdempotencyFingerprintMismatch {
		t.Fatalf("replay = id %v cached %v mismatch %v, want cached original", replay.ID, replay.IsCached, replay.IdempotencyFingerprintMismatch)
	}
	conflict, err := queries.AppendStreamRecord(ctx, db.AppendStreamRecordParams{
		ID:                     pgvalue.UUID(uuid.Must(uuid.NewV7())),
		OrgID:                  pgvalue.UUID(ids.orgID),
		ProjectID:              pgvalue.UUID(ids.projectID),
		EnvironmentID:          pgvalue.UUID(ids.environmentID),
		StreamID:               stream.ID,
		Direction:              db.StreamDirectionInput,
		Data:                   []byte(`{"approved":false}`),
		ContentType:            "application/json",
		IdempotencyKey:         "provider-event-1",
		IdempotencyFingerprint: canonicalFingerprint(t, []byte(`{"approved":false}`)),
		SourceType:             db.StreamRecordSourceTypeApiKey,
		SourceID:               "test",
	})
	if err != nil {
		t.Fatal(err)
	}
	if conflict.ID != first.ID || !conflict.IsCached || !conflict.IdempotencyFingerprintMismatch {
		t.Fatalf("conflict = id %v cached %v mismatch %v", conflict.ID, conflict.IsCached, conflict.IdempotencyFingerprintMismatch)
	}
	records, err := queries.ListStreamRecords(ctx, db.ListStreamRecordsParams{
		OrgID:         pgvalue.UUID(ids.orgID),
		ProjectID:     pgvalue.UUID(ids.projectID),
		EnvironmentID: pgvalue.UUID(ids.environmentID),
		StreamID:      stream.ID,
		Direction:     db.StreamDirectionInput,
		AfterSequence: 0,
		CorrelationID: pgtype.Text{String: "thread-1", Valid: true},
		LimitCount:    10,
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(records) != 1 || records[0].Sequence != 1 {
		t.Fatalf("records = %+v, want one sequence 1", records)
	}
	if records[0].CorrelationID != "thread-1" {
		t.Fatalf("correlation_id = %q, want thread-1", records[0].CorrelationID)
	}
	if records[0].SessionID != stream.SessionID || records[0].Direction != db.StreamDirectionInput {
		t.Fatalf("record scope = session %v direction %s, want stream session %v direction %s", records[0].SessionID, records[0].Direction, stream.SessionID, db.StreamDirectionInput)
	}
	records, err = queries.ListStreamRecords(ctx, db.ListStreamRecordsParams{
		OrgID:         pgvalue.UUID(ids.orgID),
		ProjectID:     pgvalue.UUID(ids.projectID),
		EnvironmentID: pgvalue.UUID(ids.environmentID),
		StreamID:      stream.ID,
		Direction:     db.StreamDirectionInput,
		AfterSequence: 0,
		CorrelationID: pgtype.Text{String: "thread-2", Valid: true},
		LimitCount:    10,
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(records) != 0 {
		t.Fatalf("thread-2 records = %+v, want none", records)
	}
	_, err = queries.AppendStreamRecord(ctx, db.AppendStreamRecordParams{
		ID:                     pgvalue.UUID(uuid.Must(uuid.NewV7())),
		OrgID:                  pgvalue.UUID(ids.orgID),
		ProjectID:              pgvalue.UUID(ids.projectID),
		EnvironmentID:          pgvalue.UUID(ids.environmentID),
		StreamID:               stream.ID,
		Direction:              db.StreamDirectionOutput,
		Data:                   []byte(`{"approved":true}`),
		ContentType:            "application/json",
		IdempotencyKey:         "wrong-direction",
		IdempotencyFingerprint: canonicalFingerprint(t, []byte(`{"approved":true}`)),
		SourceType:             db.StreamRecordSourceTypeApiKey,
		SourceID:               "test",
	})
	if !errors.Is(err, pgx.ErrNoRows) {
		t.Fatalf("wrong-direction append err = %v, want no rows", err)
	}
}

func TestConcurrentStreamRecordAppendAllocatesContiguousSequences(t *testing.T) {
	ctx := context.Background()
	pool := newIntegrationDB(t, ctx)
	ids := seedIntegration(t, ctx, pool)
	queries := db.New(pool)
	stream := seedSessionStream(t, ctx, queries, ids, db.StreamDirectionInput, "events")

	const appendCount = 16
	var wg sync.WaitGroup
	results := make(chan db.AppendStreamRecordRow, appendCount)
	errs := make(chan error, appendCount)
	for i := 0; i < appendCount; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			data := []byte(`{"index":` + string(rune('0'+i%10)) + `}`)
			row, err := queries.AppendStreamRecord(ctx, db.AppendStreamRecordParams{
				ID:                     pgvalue.UUID(uuid.Must(uuid.NewV7())),
				OrgID:                  pgvalue.UUID(ids.orgID),
				ProjectID:              pgvalue.UUID(ids.projectID),
				EnvironmentID:          pgvalue.UUID(ids.environmentID),
				StreamID:               stream.ID,
				Direction:              db.StreamDirectionInput,
				Data:                   data,
				ContentType:            "application/json",
				IdempotencyKey:         "concurrent-" + uuid.Must(uuid.NewV7()).String(),
				IdempotencyFingerprint: canonicalFingerprint(t, data),
				SourceType:             db.StreamRecordSourceTypeApiKey,
				SourceID:               "test",
			})
			if err != nil {
				errs <- err
				return
			}
			results <- row
		}(i)
	}
	wg.Wait()
	close(results)
	close(errs)
	for err := range errs {
		t.Fatal(err)
	}
	sequences := make([]int64, 0, appendCount)
	for row := range results {
		sequences = append(sequences, row.Sequence)
	}
	sort.Slice(sequences, func(i, j int) bool { return sequences[i] < sequences[j] })
	for i, sequence := range sequences {
		want := int64(i + 1)
		if sequence != want {
			t.Fatalf("sequences = %+v, want contiguous from 1", sequences)
		}
	}
	stored, err := queries.GetStream(ctx, db.GetStreamParams{
		OrgID:         pgvalue.UUID(ids.orgID),
		ProjectID:     pgvalue.UUID(ids.projectID),
		EnvironmentID: pgvalue.UUID(ids.environmentID),
		ID:            stream.ID,
	})
	if err != nil {
		t.Fatal(err)
	}
	if stored.NextSequence != appendCount+1 {
		t.Fatalf("next_sequence = %d, want %d", stored.NextSequence, appendCount+1)
	}
}

func TestResolveStreamWaitsRequiresWaitingState(t *testing.T) {
	ctx := context.Background()
	pool := newIntegrationDB(t, ctx)
	ids := seedIntegration(t, ctx, pool)
	queries := db.New(pool)
	stream := seedSessionStream(t, ctx, queries, ids, db.StreamDirectionInput, "approval")
	record, err := queries.AppendStreamRecord(ctx, db.AppendStreamRecordParams{
		ID:                     pgvalue.UUID(uuid.Must(uuid.NewV7())),
		OrgID:                  pgvalue.UUID(ids.orgID),
		ProjectID:              pgvalue.UUID(ids.projectID),
		EnvironmentID:          pgvalue.UUID(ids.environmentID),
		StreamID:               stream.ID,
		Direction:              db.StreamDirectionInput,
		Data:                   []byte(`{"approved":true}`),
		ContentType:            "application/json",
		IdempotencyKey:         "provider-event-2",
		IdempotencyFingerprint: canonicalFingerprint(t, []byte(`{"approved":true}`)),
		SourceType:             db.StreamRecordSourceTypeApiKey,
		SourceID:               "test",
	})
	if err != nil {
		t.Fatal(err)
	}
	runWait := seedRunWait(t, ctx, queries, ids, db.RunWaitKindStream)
	streamWait, err := queries.CreateStreamWait(ctx, db.CreateStreamWaitParams{
		ID:            pgvalue.UUID(uuid.Must(uuid.NewV7())),
		OrgID:         pgvalue.UUID(ids.orgID),
		ProjectID:     pgvalue.UUID(ids.projectID),
		EnvironmentID: pgvalue.UUID(ids.environmentID),
		RunWaitID:     runWait.ID,
		StreamID:      stream.ID,
		AfterSequence: 0,
	})
	if err != nil {
		t.Fatal(err)
	}
	resolved, err := queries.ResolveStreamWaitsForStream(ctx, db.ResolveStreamWaitsForStreamParams{
		OrgID:         pgvalue.UUID(ids.orgID),
		ProjectID:     pgvalue.UUID(ids.projectID),
		EnvironmentID: pgvalue.UUID(ids.environmentID),
		StreamID:      stream.ID,
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(resolved) != 0 {
		t.Fatalf("resolved parking wait = %+v, want none before checkpoint boundary", resolved)
	}
	markRunWaitWaiting(t, ctx, pool, ids, runWait)
	resolved, err = queries.ResolveStreamWaitsForStream(ctx, db.ResolveStreamWaitsForStreamParams{
		OrgID:         pgvalue.UUID(ids.orgID),
		ProjectID:     pgvalue.UUID(ids.projectID),
		EnvironmentID: pgvalue.UUID(ids.environmentID),
		StreamID:      stream.ID,
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(resolved) != 1 || resolved[0].RecordID != record.ID || resolved[0].RunWaitID != runWait.ID {
		t.Fatalf("resolved = %+v, want waiting run wait %v matched to record %v", resolved, runWait.ID, record.ID)
	}
	storedStreamWait, err := queries.GetStreamWaitForRunWait(ctx, db.GetStreamWaitForRunWaitParams{
		OrgID:         pgvalue.UUID(ids.orgID),
		ProjectID:     pgvalue.UUID(ids.projectID),
		EnvironmentID: pgvalue.UUID(ids.environmentID),
		RunWaitID:     streamWait.RunWaitID,
	})
	if err != nil {
		t.Fatal(err)
	}
	if storedStreamWait.MatchedRecordID != record.ID {
		t.Fatalf("matched record = %v, want %v", storedStreamWait.MatchedRecordID, record.ID)
	}
	storedWait, err := queries.GetRunWait(ctx, db.GetRunWaitParams{
		OrgID:         pgvalue.UUID(ids.orgID),
		ProjectID:     pgvalue.UUID(ids.projectID),
		EnvironmentID: pgvalue.UUID(ids.environmentID),
		ID:            runWait.ID,
	})
	if err != nil {
		t.Fatal(err)
	}
	if storedWait.State != db.RunWaitStateResolved {
		t.Fatalf("run wait state = %s, want resolved", storedWait.State)
	}
}

func TestResolveStreamWaitsBroadcastsRecordToMatchingWaiters(t *testing.T) {
	ctx := context.Background()
	pool := newIntegrationDB(t, ctx)
	ids := seedIntegration(t, ctx, pool)
	queries := db.New(pool)
	sessionID := seedSessionForRun(t, ctx, pool, ids)
	secondRunID := uuid.Must(uuid.NewV7())
	if _, err := pool.Exec(ctx, `
		INSERT INTO runs (
			id, org_id, project_id, environment_id, deployment_id, deployment_task_id, workspace_id, task_id,
			session_id, status, execution_status, payload, queue_name, max_active_duration_ms, trace_id, root_span_id
		)
		VALUES ($1, $2, $3, $4, $5, $6, $7, 'approval-task', $8, 'waiting', 'waiting', '{}', 'default', 300000,
			'11111111111111111111111111111111', '2222222222222222')
	`, secondRunID, ids.orgID, ids.projectID, ids.environmentID, ids.deploymentID, ids.taskID, ids.workspaceID, sessionID); err != nil {
		t.Fatal(err)
	}
	secondIDs := ids
	secondIDs.runID = secondRunID
	stream := seedSessionStream(t, ctx, queries, ids, db.StreamDirectionInput, "approval")
	firstWait := seedRunWait(t, ctx, queries, ids, db.RunWaitKindStream)
	secondWait := seedRunWait(t, ctx, queries, secondIDs, db.RunWaitKindStream)
	for _, item := range []struct {
		ids     integrationIDs
		runWait db.RunWait
	}{
		{ids: ids, runWait: firstWait},
		{ids: secondIDs, runWait: secondWait},
	} {
		markRunWaitWaiting(t, ctx, pool, item.ids, item.runWait)
		if _, err := queries.CreateStreamWait(ctx, db.CreateStreamWaitParams{
			ID:            pgvalue.UUID(uuid.Must(uuid.NewV7())),
			OrgID:         pgvalue.UUID(ids.orgID),
			ProjectID:     pgvalue.UUID(ids.projectID),
			EnvironmentID: pgvalue.UUID(ids.environmentID),
			RunWaitID:     item.runWait.ID,
			StreamID:      stream.ID,
			AfterSequence: 0,
			CorrelationID: "thread-a",
		}); err != nil {
			t.Fatal(err)
		}
	}
	record, err := queries.AppendStreamRecord(ctx, db.AppendStreamRecordParams{
		ID:                     pgvalue.UUID(uuid.Must(uuid.NewV7())),
		OrgID:                  pgvalue.UUID(ids.orgID),
		ProjectID:              pgvalue.UUID(ids.projectID),
		EnvironmentID:          pgvalue.UUID(ids.environmentID),
		StreamID:               stream.ID,
		Direction:              db.StreamDirectionInput,
		Data:                   []byte(`{"approved":true}`),
		CorrelationID:          "thread-a",
		ContentType:            "application/json",
		IdempotencyKey:         "provider-event-broadcast",
		IdempotencyFingerprint: canonicalFingerprint(t, []byte(`{"approved":true}`)),
		SourceType:             db.StreamRecordSourceTypeApiKey,
		SourceID:               "test",
	})
	if err != nil {
		t.Fatal(err)
	}

	resolved, err := queries.ResolveStreamWaitsForStream(ctx, db.ResolveStreamWaitsForStreamParams{
		OrgID:         pgvalue.UUID(ids.orgID),
		ProjectID:     pgvalue.UUID(ids.projectID),
		EnvironmentID: pgvalue.UUID(ids.environmentID),
		StreamID:      stream.ID,
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(resolved) != 2 {
		t.Fatalf("resolved count = %d rows = %+v, want 2", len(resolved), resolved)
	}
	got := map[pgtype.UUID]pgtype.UUID{}
	for _, row := range resolved {
		got[row.RunWaitID] = row.RecordID
	}
	for _, waitID := range []pgtype.UUID{firstWait.ID, secondWait.ID} {
		if got[waitID] != record.ID {
			t.Fatalf("resolved records = %+v, want wait %v matched to record %v", got, waitID, record.ID)
		}
	}
}

func TestResolveStreamWaitForRunWaitDoesNotFanOutToOtherWaiters(t *testing.T) {
	ctx := context.Background()
	pool := newIntegrationDB(t, ctx)
	ids := seedIntegration(t, ctx, pool)
	queries := db.New(pool)
	sessionID := seedSessionForRun(t, ctx, pool, ids)
	secondRunID := uuid.Must(uuid.NewV7())
	if _, err := pool.Exec(ctx, `
		INSERT INTO runs (
			id, org_id, project_id, environment_id, deployment_id, deployment_task_id, workspace_id, task_id,
			session_id, status, execution_status, payload, queue_name, max_active_duration_ms, trace_id, root_span_id
		)
		VALUES ($1, $2, $3, $4, $5, $6, $7, 'approval-task', $8, 'waiting', 'waiting', '{}', 'default', 300000,
			'11111111111111111111111111111111', '2222222222222222')
	`, secondRunID, ids.orgID, ids.projectID, ids.environmentID, ids.deploymentID, ids.taskID, ids.workspaceID, sessionID); err != nil {
		t.Fatal(err)
	}
	secondIDs := ids
	secondIDs.runID = secondRunID
	stream := seedSessionStream(t, ctx, queries, ids, db.StreamDirectionInput, "approval")
	firstWait := seedRunWait(t, ctx, queries, ids, db.RunWaitKindStream)
	secondWait := seedRunWait(t, ctx, queries, secondIDs, db.RunWaitKindStream)
	for _, item := range []struct {
		ids     integrationIDs
		runWait db.RunWait
	}{
		{ids: ids, runWait: firstWait},
		{ids: secondIDs, runWait: secondWait},
	} {
		markRunWaitWaiting(t, ctx, pool, item.ids, item.runWait)
		if _, err := queries.CreateStreamWait(ctx, db.CreateStreamWaitParams{
			ID:            pgvalue.UUID(uuid.Must(uuid.NewV7())),
			OrgID:         pgvalue.UUID(ids.orgID),
			ProjectID:     pgvalue.UUID(ids.projectID),
			EnvironmentID: pgvalue.UUID(ids.environmentID),
			RunWaitID:     item.runWait.ID,
			StreamID:      stream.ID,
			AfterSequence: 0,
			CorrelationID: "thread-a",
		}); err != nil {
			t.Fatal(err)
		}
	}
	record, err := queries.AppendStreamRecord(ctx, db.AppendStreamRecordParams{
		ID:                     pgvalue.UUID(uuid.Must(uuid.NewV7())),
		OrgID:                  pgvalue.UUID(ids.orgID),
		ProjectID:              pgvalue.UUID(ids.projectID),
		EnvironmentID:          pgvalue.UUID(ids.environmentID),
		StreamID:               stream.ID,
		Direction:              db.StreamDirectionInput,
		Data:                   []byte(`{"approved":true}`),
		CorrelationID:          "thread-a",
		ContentType:            "application/json",
		IdempotencyKey:         "provider-event-scoped",
		IdempotencyFingerprint: canonicalFingerprint(t, []byte(`{"approved":true}`)),
		SourceType:             db.StreamRecordSourceTypeApiKey,
		SourceID:               "test",
	})
	if err != nil {
		t.Fatal(err)
	}

	resolved, err := queries.ResolveStreamWaitForRunWait(ctx, db.ResolveStreamWaitForRunWaitParams{
		OrgID:         pgvalue.UUID(ids.orgID),
		ProjectID:     pgvalue.UUID(ids.projectID),
		EnvironmentID: pgvalue.UUID(ids.environmentID),
		RunWaitID:     firstWait.ID,
	})
	if err != nil {
		t.Fatal(err)
	}
	if resolved.RunWaitID != firstWait.ID || resolved.RecordID != record.ID {
		t.Fatalf("resolved = %+v, want first wait matched to record %v", resolved, record.ID)
	}
	storedSecond, err := queries.GetRunWait(ctx, db.GetRunWaitParams{
		OrgID:         pgvalue.UUID(ids.orgID),
		ProjectID:     pgvalue.UUID(ids.projectID),
		EnvironmentID: pgvalue.UUID(ids.environmentID),
		ID:            secondWait.ID,
	})
	if err != nil {
		t.Fatal(err)
	}
	if storedSecond.State != db.RunWaitStateWaiting {
		t.Fatalf("second wait state = %s, want waiting", storedSecond.State)
	}
	secondStreamWait, err := queries.GetStreamWaitForRunWait(ctx, db.GetStreamWaitForRunWaitParams{
		OrgID:         pgvalue.UUID(ids.orgID),
		ProjectID:     pgvalue.UUID(ids.projectID),
		EnvironmentID: pgvalue.UUID(ids.environmentID),
		RunWaitID:     secondWait.ID,
	})
	if err != nil {
		t.Fatal(err)
	}
	if secondStreamWait.MatchedRecordID.Valid {
		t.Fatalf("second stream wait matched record = %v, want none", secondStreamWait.MatchedRecordID)
	}
}

func TestResolveStreamWaitsDoesNotRematchResolvedWait(t *testing.T) {
	ctx := context.Background()
	pool := newIntegrationDB(t, ctx)
	ids := seedIntegration(t, ctx, pool)
	queries := db.New(pool)
	stream := seedSessionStream(t, ctx, queries, ids, db.StreamDirectionInput, "approval")
	firstRecord, err := queries.AppendStreamRecord(ctx, db.AppendStreamRecordParams{
		ID:                     pgvalue.UUID(uuid.Must(uuid.NewV7())),
		OrgID:                  pgvalue.UUID(ids.orgID),
		ProjectID:              pgvalue.UUID(ids.projectID),
		EnvironmentID:          pgvalue.UUID(ids.environmentID),
		StreamID:               stream.ID,
		Direction:              db.StreamDirectionInput,
		Data:                   []byte(`{"approved":"first"}`),
		ContentType:            "application/json",
		IdempotencyKey:         "approval-first",
		IdempotencyFingerprint: canonicalFingerprint(t, []byte(`{"approved":"first"}`)),
		SourceType:             db.StreamRecordSourceTypeApiKey,
		SourceID:               "test",
	})
	if err != nil {
		t.Fatal(err)
	}
	runWait := seedRunWait(t, ctx, queries, ids, db.RunWaitKindStream)
	markRunWaitWaiting(t, ctx, pool, ids, runWait)
	if _, err := queries.CreateStreamWait(ctx, db.CreateStreamWaitParams{
		ID:            pgvalue.UUID(uuid.Must(uuid.NewV7())),
		OrgID:         pgvalue.UUID(ids.orgID),
		ProjectID:     pgvalue.UUID(ids.projectID),
		EnvironmentID: pgvalue.UUID(ids.environmentID),
		RunWaitID:     runWait.ID,
		StreamID:      stream.ID,
		AfterSequence: 0,
	}); err != nil {
		t.Fatal(err)
	}
	resolved, err := queries.ResolveStreamWaitsForStream(ctx, db.ResolveStreamWaitsForStreamParams{
		OrgID:         pgvalue.UUID(ids.orgID),
		ProjectID:     pgvalue.UUID(ids.projectID),
		EnvironmentID: pgvalue.UUID(ids.environmentID),
		StreamID:      stream.ID,
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(resolved) != 1 || resolved[0].RecordID != firstRecord.ID {
		t.Fatalf("first resolve = %+v, want record %v", resolved, firstRecord.ID)
	}
	if _, err := queries.AppendStreamRecord(ctx, db.AppendStreamRecordParams{
		ID:                     pgvalue.UUID(uuid.Must(uuid.NewV7())),
		OrgID:                  pgvalue.UUID(ids.orgID),
		ProjectID:              pgvalue.UUID(ids.projectID),
		EnvironmentID:          pgvalue.UUID(ids.environmentID),
		StreamID:               stream.ID,
		Direction:              db.StreamDirectionInput,
		Data:                   []byte(`{"approved":"second"}`),
		ContentType:            "application/json",
		IdempotencyKey:         "approval-second",
		IdempotencyFingerprint: canonicalFingerprint(t, []byte(`{"approved":"second"}`)),
		SourceType:             db.StreamRecordSourceTypeApiKey,
		SourceID:               "test",
	}); err != nil {
		t.Fatal(err)
	}
	resolved, err = queries.ResolveStreamWaitsForStream(ctx, db.ResolveStreamWaitsForStreamParams{
		OrgID:         pgvalue.UUID(ids.orgID),
		ProjectID:     pgvalue.UUID(ids.projectID),
		EnvironmentID: pgvalue.UUID(ids.environmentID),
		StreamID:      stream.ID,
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(resolved) != 0 {
		t.Fatalf("second resolve = %+v, want no rematch for already resolved wait", resolved)
	}
	streamWait, err := queries.GetStreamWaitForRunWait(ctx, db.GetStreamWaitForRunWaitParams{
		OrgID:         pgvalue.UUID(ids.orgID),
		ProjectID:     pgvalue.UUID(ids.projectID),
		EnvironmentID: pgvalue.UUID(ids.environmentID),
		RunWaitID:     runWait.ID,
	})
	if err != nil {
		t.Fatal(err)
	}
	if streamWait.MatchedRecordID != firstRecord.ID {
		t.Fatalf("matched record = %v, want first record %v", streamWait.MatchedRecordID, firstRecord.ID)
	}
}

func TestResolveStreamWaitsMatchesCorrelationLane(t *testing.T) {
	ctx := context.Background()
	pool := newIntegrationDB(t, ctx)
	ids := seedIntegration(t, ctx, pool)
	queries := db.New(pool)
	stream := seedSessionStream(t, ctx, queries, ids, db.StreamDirectionInput, "replies")
	first, err := queries.AppendStreamRecord(ctx, db.AppendStreamRecordParams{
		ID:                     pgvalue.UUID(uuid.Must(uuid.NewV7())),
		OrgID:                  pgvalue.UUID(ids.orgID),
		ProjectID:              pgvalue.UUID(ids.projectID),
		EnvironmentID:          pgvalue.UUID(ids.environmentID),
		StreamID:               stream.ID,
		Direction:              db.StreamDirectionInput,
		Data:                   []byte(`{"text":"a"}`),
		CorrelationID:          "thread-a",
		ContentType:            "application/json",
		IdempotencyKey:         "reply-a",
		IdempotencyFingerprint: canonicalFingerprint(t, []byte(`{"text":"a"}`)),
		SourceType:             db.StreamRecordSourceTypeApiKey,
		SourceID:               "test",
	})
	if err != nil {
		t.Fatal(err)
	}
	second, err := queries.AppendStreamRecord(ctx, db.AppendStreamRecordParams{
		ID:                     pgvalue.UUID(uuid.Must(uuid.NewV7())),
		OrgID:                  pgvalue.UUID(ids.orgID),
		ProjectID:              pgvalue.UUID(ids.projectID),
		EnvironmentID:          pgvalue.UUID(ids.environmentID),
		StreamID:               stream.ID,
		Direction:              db.StreamDirectionInput,
		Data:                   []byte(`{"text":"b"}`),
		CorrelationID:          "thread-b",
		ContentType:            "application/json",
		IdempotencyKey:         "reply-b",
		IdempotencyFingerprint: canonicalFingerprint(t, []byte(`{"text":"b"}`)),
		SourceType:             db.StreamRecordSourceTypeApiKey,
		SourceID:               "test",
	})
	if err != nil {
		t.Fatal(err)
	}
	if first.Sequence >= second.Sequence {
		t.Fatalf("test setup expected thread-a before thread-b: first=%d second=%d", first.Sequence, second.Sequence)
	}
	runWait := seedRunWait(t, ctx, queries, ids, db.RunWaitKindStream)
	markRunWaitWaiting(t, ctx, pool, ids, runWait)
	streamWait, err := queries.CreateStreamWait(ctx, db.CreateStreamWaitParams{
		ID:            pgvalue.UUID(uuid.Must(uuid.NewV7())),
		OrgID:         pgvalue.UUID(ids.orgID),
		ProjectID:     pgvalue.UUID(ids.projectID),
		EnvironmentID: pgvalue.UUID(ids.environmentID),
		RunWaitID:     runWait.ID,
		StreamID:      stream.ID,
		AfterSequence: 0,
		CorrelationID: "thread-b",
	})
	if err != nil {
		t.Fatal(err)
	}
	resolved, err := queries.ResolveStreamWaitsForStream(ctx, db.ResolveStreamWaitsForStreamParams{
		OrgID:         pgvalue.UUID(ids.orgID),
		ProjectID:     pgvalue.UUID(ids.projectID),
		EnvironmentID: pgvalue.UUID(ids.environmentID),
		StreamID:      stream.ID,
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(resolved) != 1 || resolved[0].RecordID != second.ID {
		t.Fatalf("resolved = %+v, want thread-b record %v", resolved, second.ID)
	}
	storedStreamWait, err := queries.GetStreamWaitForRunWait(ctx, db.GetStreamWaitForRunWaitParams{
		OrgID:         pgvalue.UUID(ids.orgID),
		ProjectID:     pgvalue.UUID(ids.projectID),
		EnvironmentID: pgvalue.UUID(ids.environmentID),
		RunWaitID:     streamWait.RunWaitID,
	})
	if err != nil {
		t.Fatal(err)
	}
	if storedStreamWait.MatchedRecordID != second.ID {
		t.Fatalf("matched record = %v, want thread-b record %v", storedStreamWait.MatchedRecordID, second.ID)
	}
}

func TestResolveStreamWaitsDoesNotBlockIndependentCorrelationLane(t *testing.T) {
	ctx := context.Background()
	pool := newIntegrationDB(t, ctx)
	ids := seedIntegration(t, ctx, pool)
	queries := db.New(pool)
	stream := seedSessionStream(t, ctx, queries, ids, db.StreamDirectionInput, "replies")

	threadAWait := seedRunWait(t, ctx, queries, ids, db.RunWaitKindStream)
	if _, err := queries.CreateStreamWait(ctx, db.CreateStreamWaitParams{
		ID:            pgvalue.UUID(uuid.Must(uuid.NewV7())),
		OrgID:         pgvalue.UUID(ids.orgID),
		ProjectID:     pgvalue.UUID(ids.projectID),
		EnvironmentID: pgvalue.UUID(ids.environmentID),
		RunWaitID:     threadAWait.ID,
		StreamID:      stream.ID,
		AfterSequence: 0,
		CorrelationID: "thread-a",
	}); err != nil {
		t.Fatal(err)
	}
	threadBWait := seedRunWait(t, ctx, queries, ids, db.RunWaitKindStream)
	if _, err := queries.CreateStreamWait(ctx, db.CreateStreamWaitParams{
		ID:            pgvalue.UUID(uuid.Must(uuid.NewV7())),
		OrgID:         pgvalue.UUID(ids.orgID),
		ProjectID:     pgvalue.UUID(ids.projectID),
		EnvironmentID: pgvalue.UUID(ids.environmentID),
		RunWaitID:     threadBWait.ID,
		StreamID:      stream.ID,
		AfterSequence: 0,
		CorrelationID: "thread-b",
	}); err != nil {
		t.Fatal(err)
	}
	markRunWaitWaiting(t, ctx, pool, ids, threadAWait)
	markRunWaitWaiting(t, ctx, pool, ids, threadBWait)
	record, err := queries.AppendStreamRecord(ctx, db.AppendStreamRecordParams{
		ID:                     pgvalue.UUID(uuid.Must(uuid.NewV7())),
		OrgID:                  pgvalue.UUID(ids.orgID),
		ProjectID:              pgvalue.UUID(ids.projectID),
		EnvironmentID:          pgvalue.UUID(ids.environmentID),
		StreamID:               stream.ID,
		Direction:              db.StreamDirectionInput,
		Data:                   []byte(`{"text":"b"}`),
		CorrelationID:          "thread-b",
		ContentType:            "application/json",
		IdempotencyKey:         "reply-b",
		IdempotencyFingerprint: canonicalFingerprint(t, []byte(`{"text":"b"}`)),
		SourceType:             db.StreamRecordSourceTypeApiKey,
		SourceID:               "test",
	})
	if err != nil {
		t.Fatal(err)
	}
	resolved, err := queries.ResolveStreamWaitsForStream(ctx, db.ResolveStreamWaitsForStreamParams{
		OrgID:         pgvalue.UUID(ids.orgID),
		ProjectID:     pgvalue.UUID(ids.projectID),
		EnvironmentID: pgvalue.UUID(ids.environmentID),
		StreamID:      stream.ID,
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(resolved) != 1 || resolved[0].RunWaitID != threadBWait.ID || resolved[0].RecordID != record.ID {
		t.Fatalf("resolved = %+v, want only thread-b wait %v record %v", resolved, threadBWait.ID, record.ID)
	}
	storedThreadA, err := queries.GetRunWait(ctx, db.GetRunWaitParams{
		OrgID:         pgvalue.UUID(ids.orgID),
		ProjectID:     pgvalue.UUID(ids.projectID),
		EnvironmentID: pgvalue.UUID(ids.environmentID),
		ID:            threadAWait.ID,
	})
	if err != nil {
		t.Fatal(err)
	}
	if storedThreadA.State != db.RunWaitStateWaiting {
		t.Fatalf("thread-a wait state = %s, want waiting", storedThreadA.State)
	}
}

func TestDeploymentStreamNameResolutionAndSessionEnsure(t *testing.T) {
	ctx := context.Background()
	pool := newIntegrationDB(t, ctx)
	ids := seedIntegration(t, ctx, pool)
	queries := db.New(pool)
	stream := seedSessionStream(t, ctx, queries, ids, db.StreamDirectionOutput, "progress")

	deployed, err := queries.GetDeploymentStreamByName(ctx, db.GetDeploymentStreamByNameParams{
		OrgID:         pgvalue.UUID(ids.orgID),
		ProjectID:     pgvalue.UUID(ids.projectID),
		EnvironmentID: pgvalue.UUID(ids.environmentID),
		DeploymentID:  pgvalue.UUID(ids.deploymentID),
		Name:          "progress",
		Direction:     db.StreamDirectionOutput,
	})
	if err != nil {
		t.Fatal(err)
	}
	if deployed.ID != stream.DeploymentStreamID || deployed.Direction != db.StreamDirectionOutput {
		t.Fatalf("deployed stream = %+v, session stream = %+v", deployed, stream)
	}
	byName, err := queries.GetSessionStreamByName(ctx, db.GetSessionStreamByNameParams{
		OrgID:         pgvalue.UUID(ids.orgID),
		ProjectID:     pgvalue.UUID(ids.projectID),
		EnvironmentID: pgvalue.UUID(ids.environmentID),
		SessionID:     stream.SessionID,
		Name:          "progress",
		Direction:     db.StreamDirectionOutput,
	})
	if err != nil {
		t.Fatal(err)
	}
	if byName.ID != stream.ID {
		t.Fatalf("session stream by name = %v, want %v", byName.ID, stream.ID)
	}
}

func TestTokenCreateAndCompletionIdempotency(t *testing.T) {
	ctx := context.Background()
	pool := newIntegrationDB(t, ctx)
	ids := seedIntegration(t, ctx, pool)
	queries := db.New(pool)
	createFingerprint := "create-" + canonicalFingerprint(t, []byte(`{"prompt":"approve"}`))
	tokenID := uuid.Must(uuid.NewV7())
	token, err := queries.CreateToken(ctx, db.CreateTokenParams{
		ID:                        pgvalue.UUID(tokenID),
		OrgID:                     pgvalue.UUID(ids.orgID),
		ProjectID:                 pgvalue.UUID(ids.projectID),
		EnvironmentID:             pgvalue.UUID(ids.environmentID),
		TimeoutAt:                 pgvalue.Timestamptz(time.Now().Add(time.Hour)),
		IdempotencyKey:            "token-create-1",
		CreateRequestFingerprint:  createFingerprint,
		CallbackKeyID:             "callback-key-1",
		CallbackSecretFingerprint: "callback-secret-fp-1",
		CallbackSecretCreatedAt:   pgvalue.Timestamptz(time.Now()),
		Metadata:                  []byte(`{}`),
	})
	if err != nil {
		t.Fatal(err)
	}
	replay, err := queries.CreateToken(ctx, db.CreateTokenParams{
		ID:                       pgvalue.UUID(uuid.Must(uuid.NewV7())),
		OrgID:                    pgvalue.UUID(ids.orgID),
		ProjectID:                pgvalue.UUID(ids.projectID),
		EnvironmentID:            pgvalue.UUID(ids.environmentID),
		TimeoutAt:                pgvalue.Timestamptz(time.Now().Add(time.Hour)),
		IdempotencyKey:           "token-create-1",
		CreateRequestFingerprint: createFingerprint,
		Metadata:                 []byte(`{}`),
	})
	if err != nil {
		t.Fatal(err)
	}
	if replay.ID != token.ID || !replay.IsCached || replay.IdempotencyFingerprintMismatch {
		t.Fatalf("token replay = id %v cached %v mismatch %v", replay.ID, replay.IsCached, replay.IdempotencyFingerprintMismatch)
	}
	conflict, err := queries.CreateToken(ctx, db.CreateTokenParams{
		ID:                       pgvalue.UUID(uuid.Must(uuid.NewV7())),
		OrgID:                    pgvalue.UUID(ids.orgID),
		ProjectID:                pgvalue.UUID(ids.projectID),
		EnvironmentID:            pgvalue.UUID(ids.environmentID),
		TimeoutAt:                pgvalue.Timestamptz(time.Now().Add(time.Hour)),
		IdempotencyKey:           "token-create-1",
		CreateRequestFingerprint: "different",
		Metadata:                 []byte(`{}`),
	})
	if err != nil {
		t.Fatal(err)
	}
	if conflict.ID != token.ID || !conflict.IsCached || !conflict.IdempotencyFingerprintMismatch {
		t.Fatalf("token conflict = id %v cached %v mismatch %v", conflict.ID, conflict.IsCached, conflict.IdempotencyFingerprintMismatch)
	}
	runWait := seedRunWait(t, ctx, queries, ids, db.RunWaitKindToken)
	if _, err := queries.CreateTokenWait(ctx, db.CreateTokenWaitParams{
		ID:            pgvalue.UUID(uuid.Must(uuid.NewV7())),
		OrgID:         pgvalue.UUID(ids.orgID),
		ProjectID:     pgvalue.UUID(ids.projectID),
		EnvironmentID: pgvalue.UUID(ids.environmentID),
		RunWaitID:     runWait.ID,
		TokenID:       token.ID,
	}); err != nil {
		t.Fatal(err)
	}
	markRunWaitWaiting(t, ctx, pool, ids, runWait)
	completionData := []byte(`{"approved":true}`)
	completed, err := queries.CompleteToken(ctx, db.CompleteTokenParams{
		OrgID:                 pgvalue.UUID(ids.orgID),
		ProjectID:             pgvalue.UUID(ids.projectID),
		EnvironmentID:         pgvalue.UUID(ids.environmentID),
		ID:                    token.ID,
		CompletionData:        completionData,
		CompletionContentType: "application/json",
		CompletionFingerprint: canonicalFingerprint(t, completionData),
	})
	if err != nil {
		t.Fatal(err)
	}
	if completed.State != db.TokenStateCompleted || completed.ResolvedWaitCount != 1 {
		t.Fatalf("completed token = state %s resolved %d", completed.State, completed.ResolvedWaitCount)
	}
	again, err := queries.CompleteToken(ctx, db.CompleteTokenParams{
		OrgID:                 pgvalue.UUID(ids.orgID),
		ProjectID:             pgvalue.UUID(ids.projectID),
		EnvironmentID:         pgvalue.UUID(ids.environmentID),
		ID:                    token.ID,
		CompletionData:        []byte(`{"approved":true}`),
		CompletionContentType: "application/json",
		CompletionFingerprint: canonicalFingerprint(t, []byte(`{"approved":true}`)),
	})
	if err != nil {
		t.Fatal(err)
	}
	if !again.AlreadyCompleted || again.CompletionConflict {
		t.Fatalf("duplicate completion = already %v conflict %v", again.AlreadyCompleted, again.CompletionConflict)
	}
	different, err := queries.CompleteToken(ctx, db.CompleteTokenParams{
		OrgID:                 pgvalue.UUID(ids.orgID),
		ProjectID:             pgvalue.UUID(ids.projectID),
		EnvironmentID:         pgvalue.UUID(ids.environmentID),
		ID:                    token.ID,
		CompletionData:        []byte(`{"approved":false}`),
		CompletionContentType: "application/json",
		CompletionFingerprint: canonicalFingerprint(t, []byte(`{"approved":false}`)),
	})
	if err != nil {
		t.Fatal(err)
	}
	if !different.CompletionConflict {
		t.Fatal("different duplicate completion should report a conflict")
	}
}

func TestConcurrentTokenCompletionIsFirstResolveWins(t *testing.T) {
	ctx := context.Background()
	pool := newIntegrationDB(t, ctx)
	ids := seedIntegration(t, ctx, pool)
	queries := db.New(pool)
	token, err := queries.CreateToken(ctx, db.CreateTokenParams{
		ID:                       pgvalue.UUID(uuid.Must(uuid.NewV7())),
		OrgID:                    pgvalue.UUID(ids.orgID),
		ProjectID:                pgvalue.UUID(ids.projectID),
		EnvironmentID:            pgvalue.UUID(ids.environmentID),
		TimeoutAt:                pgvalue.Timestamptz(time.Now().Add(time.Hour)),
		CreateRequestFingerprint: "concurrent-token",
		Metadata:                 []byte(`{}`),
	})
	if err != nil {
		t.Fatal(err)
	}
	const completionCount = 8
	var wg sync.WaitGroup
	results := make(chan db.CompleteTokenRow, completionCount)
	errs := make(chan error, completionCount)
	for i := 0; i < completionCount; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			data := []byte(`{"winner":` + string(rune('0'+i)) + `}`)
			row, err := queries.CompleteToken(ctx, db.CompleteTokenParams{
				OrgID:                 pgvalue.UUID(ids.orgID),
				ProjectID:             pgvalue.UUID(ids.projectID),
				EnvironmentID:         pgvalue.UUID(ids.environmentID),
				ID:                    token.ID,
				CompletionData:        data,
				CompletionContentType: "application/json",
				CompletionFingerprint: canonicalFingerprint(t, data),
			})
			if err != nil {
				errs <- err
				return
			}
			results <- row
		}(i)
	}
	wg.Wait()
	close(results)
	close(errs)
	for err := range errs {
		t.Fatal(err)
	}
	winners := 0
	conflicts := 0
	for row := range results {
		if !row.WasAlreadyCompleted {
			winners++
		}
		if row.CompletionConflict {
			conflicts++
		}
	}
	if winners != 1 || conflicts != completionCount-1 {
		t.Fatalf("completion results: winners=%d conflicts=%d", winners, conflicts)
	}
}

func TestTokenAndTimerWaitExpiryBoundaries(t *testing.T) {
	ctx := context.Background()
	pool := newIntegrationDB(t, ctx)
	ids := seedIntegration(t, ctx, pool)
	queries := db.New(pool)
	token, err := queries.CreateToken(ctx, db.CreateTokenParams{
		ID:                       pgvalue.UUID(uuid.Must(uuid.NewV7())),
		OrgID:                    pgvalue.UUID(ids.orgID),
		ProjectID:                pgvalue.UUID(ids.projectID),
		EnvironmentID:            pgvalue.UUID(ids.environmentID),
		TimeoutAt:                pgvalue.Timestamptz(time.Now().Add(-time.Second)),
		CreateRequestFingerprint: "expired-token",
		Metadata:                 []byte(`{}`),
	})
	if err != nil {
		t.Fatal(err)
	}
	tokenRunWait := seedRunWait(t, ctx, queries, ids, db.RunWaitKindToken)
	if _, err := queries.CreateTokenWait(ctx, db.CreateTokenWaitParams{
		ID:            pgvalue.UUID(uuid.Must(uuid.NewV7())),
		OrgID:         pgvalue.UUID(ids.orgID),
		ProjectID:     pgvalue.UUID(ids.projectID),
		EnvironmentID: pgvalue.UUID(ids.environmentID),
		RunWaitID:     tokenRunWait.ID,
		TokenID:       token.ID,
	}); err != nil {
		t.Fatal(err)
	}
	markRunWaitWaiting(t, ctx, pool, ids, tokenRunWait)
	expired, err := queries.ExpireDueTokens(ctx, pgvalue.UUID(ids.orgID))
	if err != nil {
		t.Fatal(err)
	}
	if len(expired) != 1 || expired[0].ID != token.ID || expired[0].State != db.TokenStateExpired {
		t.Fatalf("expired tokens = %+v", expired)
	}
	storedTokenWait, err := queries.GetRunWait(ctx, db.GetRunWaitParams{
		OrgID:         pgvalue.UUID(ids.orgID),
		ProjectID:     pgvalue.UUID(ids.projectID),
		EnvironmentID: pgvalue.UUID(ids.environmentID),
		ID:            tokenRunWait.ID,
	})
	if err != nil {
		t.Fatal(err)
	}
	if storedTokenWait.State != db.RunWaitStateExpired {
		t.Fatalf("token run wait state = %s, want expired", storedTokenWait.State)
	}

	timerRunWait := seedRunWait(t, ctx, queries, ids, db.RunWaitKindTimer)
	if _, err := queries.CreateTimerWait(ctx, db.CreateTimerWaitParams{
		ID:            pgvalue.UUID(uuid.Must(uuid.NewV7())),
		OrgID:         pgvalue.UUID(ids.orgID),
		ProjectID:     pgvalue.UUID(ids.projectID),
		EnvironmentID: pgvalue.UUID(ids.environmentID),
		RunWaitID:     timerRunWait.ID,
		FireAt:        pgvalue.Timestamptz(time.Now().Add(-time.Second)),
	}); err != nil {
		t.Fatal(err)
	}
	markRunWaitWaiting(t, ctx, pool, ids, timerRunWait)
	resolved, err := queries.ResolveDueTimerWaits(ctx, db.ResolveDueTimerWaitsParams{
		OrgID:      pgvalue.UUID(ids.orgID),
		LimitCount: 10,
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(resolved) != 1 || resolved[0].ID != timerRunWait.ID || resolved[0].State != db.RunWaitStateResolved {
		t.Fatalf("resolved timer waits = %+v", resolved)
	}
}

func TestResolveDueTimerWaitForRunWaitDoesNotFanOutToOtherTimers(t *testing.T) {
	ctx := context.Background()
	pool := newIntegrationDB(t, ctx)
	ids := seedIntegration(t, ctx, pool)
	queries := db.New(pool)
	sessionID := seedSessionForRun(t, ctx, pool, ids)
	secondRunID := uuid.Must(uuid.NewV7())
	if _, err := pool.Exec(ctx, `
		INSERT INTO runs (
			id, org_id, project_id, environment_id, deployment_id, deployment_task_id, workspace_id, task_id,
			session_id, status, execution_status, payload, queue_name, max_active_duration_ms, trace_id, root_span_id
		)
		VALUES ($1, $2, $3, $4, $5, $6, $7, 'approval-task', $8, 'waiting', 'waiting', '{}', 'default', 300000,
			'11111111111111111111111111111111', '2222222222222222')
	`, secondRunID, ids.orgID, ids.projectID, ids.environmentID, ids.deploymentID, ids.taskID, ids.workspaceID, sessionID); err != nil {
		t.Fatal(err)
	}
	secondIDs := ids
	secondIDs.runID = secondRunID
	firstWait := seedRunWait(t, ctx, queries, ids, db.RunWaitKindTimer)
	secondWait := seedRunWait(t, ctx, queries, secondIDs, db.RunWaitKindTimer)
	for _, item := range []struct {
		ids     integrationIDs
		runWait db.RunWait
	}{
		{ids: ids, runWait: firstWait},
		{ids: secondIDs, runWait: secondWait},
	} {
		markRunWaitWaiting(t, ctx, pool, item.ids, item.runWait)
		if _, err := queries.CreateTimerWait(ctx, db.CreateTimerWaitParams{
			ID:            pgvalue.UUID(uuid.Must(uuid.NewV7())),
			OrgID:         pgvalue.UUID(ids.orgID),
			ProjectID:     pgvalue.UUID(ids.projectID),
			EnvironmentID: pgvalue.UUID(ids.environmentID),
			RunWaitID:     item.runWait.ID,
			FireAt:        pgvalue.Timestamptz(time.Now().Add(-time.Second)),
		}); err != nil {
			t.Fatal(err)
		}
	}

	resolved, err := queries.ResolveDueTimerWaitForRunWait(ctx, db.ResolveDueTimerWaitForRunWaitParams{
		OrgID:         pgvalue.UUID(ids.orgID),
		ProjectID:     pgvalue.UUID(ids.projectID),
		EnvironmentID: pgvalue.UUID(ids.environmentID),
		RunWaitID:     firstWait.ID,
	})
	if err != nil {
		t.Fatal(err)
	}
	if resolved.ID != firstWait.ID || resolved.State != db.RunWaitStateResolved {
		t.Fatalf("resolved timer wait = %+v, want first wait resolved", resolved)
	}
	storedSecond, err := queries.GetRunWait(ctx, db.GetRunWaitParams{
		OrgID:         pgvalue.UUID(ids.orgID),
		ProjectID:     pgvalue.UUID(ids.projectID),
		EnvironmentID: pgvalue.UUID(ids.environmentID),
		ID:            secondWait.ID,
	})
	if err != nil {
		t.Fatal(err)
	}
	if storedSecond.State != db.RunWaitStateWaiting {
		t.Fatalf("second timer state = %s, want waiting", storedSecond.State)
	}
}

func TestPublicAccessTokenScopesUseTypedResourceFKs(t *testing.T) {
	ctx := context.Background()
	pool := newIntegrationDB(t, ctx)
	ids := seedIntegration(t, ctx, pool)
	queries := db.New(pool)
	inputStream := seedSessionStream(t, ctx, queries, ids, db.StreamDirectionInput, "approval")
	outputStream := seedSessionStream(t, ctx, queries, ids, db.StreamDirectionOutput, "updates")
	token, err := queries.CreateToken(ctx, db.CreateTokenParams{
		ID:                       pgvalue.UUID(uuid.Must(uuid.NewV7())),
		OrgID:                    pgvalue.UUID(ids.orgID),
		ProjectID:                pgvalue.UUID(ids.projectID),
		EnvironmentID:            pgvalue.UUID(ids.environmentID),
		TimeoutAt:                pgvalue.Timestamptz(time.Now().Add(time.Hour)),
		CreateRequestFingerprint: "token-scope",
		Metadata:                 []byte(`{}`),
	})
	if err != nil {
		t.Fatal(err)
	}
	publicToken, err := queries.CreatePublicAccessToken(ctx, db.CreatePublicAccessTokenParams{
		ID:            pgvalue.UUID(uuid.Must(uuid.NewV7())),
		OrgID:         pgvalue.UUID(ids.orgID),
		ProjectID:     pgvalue.UUID(ids.projectID),
		EnvironmentID: pgvalue.UUID(ids.environmentID),
		TokenHash:     []byte("public-token-hash"),
		ExpiresAt:     pgvalue.Timestamptz(time.Now().Add(time.Hour)),
		Metadata:      []byte(`{}`),
		CreatedBy:     []byte(`{}`),
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := queries.CreatePublicAccessTokenScope(ctx, db.CreatePublicAccessTokenScopeParams{
		ID:                  pgvalue.UUID(uuid.Must(uuid.NewV7())),
		OrgID:               pgvalue.UUID(ids.orgID),
		ProjectID:           pgvalue.UUID(ids.projectID),
		EnvironmentID:       pgvalue.UUID(ids.environmentID),
		PublicAccessTokenID: publicToken.ID,
		ScopeType:           db.PublicAccessTokenScopeTypeTokencomplete,
		TokenID:             token.ID,
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := queries.CreatePublicAccessTokenScope(ctx, db.CreatePublicAccessTokenScopeParams{
		ID:                  pgvalue.UUID(uuid.Must(uuid.NewV7())),
		OrgID:               pgvalue.UUID(ids.orgID),
		ProjectID:           pgvalue.UUID(ids.projectID),
		EnvironmentID:       pgvalue.UUID(ids.environmentID),
		PublicAccessTokenID: publicToken.ID,
		ScopeType:           db.PublicAccessTokenScopeTypeSessioninputsend,
		StreamID:            inputStream.ID,
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := queries.CreatePublicAccessTokenScope(ctx, db.CreatePublicAccessTokenScopeParams{
		ID:                  pgvalue.UUID(uuid.Must(uuid.NewV7())),
		OrgID:               pgvalue.UUID(ids.orgID),
		ProjectID:           pgvalue.UUID(ids.projectID),
		EnvironmentID:       pgvalue.UUID(ids.environmentID),
		PublicAccessTokenID: publicToken.ID,
		ScopeType:           db.PublicAccessTokenScopeTypeSessionoutputread,
		StreamID:            outputStream.ID,
	}); err != nil {
		t.Fatal(err)
	}
	_, err = queries.CreatePublicAccessTokenScope(ctx, db.CreatePublicAccessTokenScopeParams{
		ID:                  pgvalue.UUID(uuid.Must(uuid.NewV7())),
		OrgID:               pgvalue.UUID(ids.orgID),
		ProjectID:           pgvalue.UUID(ids.projectID),
		EnvironmentID:       pgvalue.UUID(ids.environmentID),
		PublicAccessTokenID: publicToken.ID,
		ScopeType:           db.PublicAccessTokenScopeTypeSessionoutputread,
		StreamID:            inputStream.ID,
	})
	if !errors.Is(err, pgx.ErrNoRows) {
		t.Fatalf("input stream read scope err = %v, want no rows", err)
	}
	_, err = queries.CreatePublicAccessTokenScope(ctx, db.CreatePublicAccessTokenScopeParams{
		ID:                  pgvalue.UUID(uuid.Must(uuid.NewV7())),
		OrgID:               pgvalue.UUID(ids.orgID),
		ProjectID:           pgvalue.UUID(ids.projectID),
		EnvironmentID:       pgvalue.UUID(ids.environmentID),
		PublicAccessTokenID: publicToken.ID,
		ScopeType:           db.PublicAccessTokenScopeTypeSessioninputsend,
		TokenID:             token.ID,
	})
	if !errors.Is(err, pgx.ErrNoRows) {
		t.Fatalf("invalid stream scope err = %v, want no rows", err)
	}
	_, err = pool.Exec(ctx, `
		INSERT INTO public_access_token_scopes (
			id, org_id, project_id, environment_id, public_access_token_id, scope_type, token_id, stream_id
		)
		VALUES ($1, $2, $3, $4, $5, 'session.input.send', $6, NULL)
	`, uuid.Must(uuid.NewV7()), ids.orgID, ids.projectID, ids.environmentID, pgvalue.MustUUIDValue(publicToken.ID), pgvalue.MustUUIDValue(token.ID))
	var pgErr *pgconn.PgError
	if !errors.As(err, &pgErr) || pgErr.Code != "23514" {
		t.Fatalf("invalid direct scope err = %v, want check violation", err)
	}
}

func seedSessionStream(t *testing.T, ctx context.Context, queries *db.Queries, ids integrationIDs, direction db.StreamDirection, name string) db.Stream {
	t.Helper()
	deploymentStream, err := queries.UpsertDeploymentStream(ctx, db.UpsertDeploymentStreamParams{
		ID:                pgvalue.UUID(uuid.Must(uuid.NewV7())),
		OrgID:             pgvalue.UUID(ids.orgID),
		ProjectID:         pgvalue.UUID(ids.projectID),
		EnvironmentID:     pgvalue.UUID(ids.environmentID),
		DeploymentID:      pgvalue.UUID(ids.deploymentID),
		Name:              name,
		Direction:         direction,
		SchemaFingerprint: "schema-" + name,
		SchemaJson:        []byte(`{"type":"object"}`),
		Metadata:          []byte(`{}`),
	})
	if err != nil {
		t.Fatal(err)
	}
	run, err := queries.GetRun(ctx, db.GetRunParams{
		OrgID: pgvalue.UUID(ids.orgID),
		ID:    pgvalue.UUID(ids.runID),
	})
	if err != nil {
		t.Fatal(err)
	}
	stream, err := queries.EnsureSessionStream(ctx, db.EnsureSessionStreamParams{
		ID:                 pgvalue.UUID(uuid.Must(uuid.NewV7())),
		OrgID:              pgvalue.UUID(ids.orgID),
		ProjectID:          pgvalue.UUID(ids.projectID),
		EnvironmentID:      pgvalue.UUID(ids.environmentID),
		SessionID:          run.SessionID,
		DeploymentStreamID: deploymentStream.ID,
		Metadata:           []byte(`{}`),
	})
	if err != nil {
		t.Fatal(err)
	}
	return stream
}

func seedRunWait(t *testing.T, ctx context.Context, queries *db.Queries, ids integrationIDs, kind db.RunWaitKind) db.RunWait {
	t.Helper()
	runWait, err := queries.CreateRunWait(ctx, db.CreateRunWaitParams{
		ID:            pgvalue.UUID(uuid.Must(uuid.NewV7())),
		OrgID:         pgvalue.UUID(ids.orgID),
		ProjectID:     pgvalue.UUID(ids.projectID),
		EnvironmentID: pgvalue.UUID(ids.environmentID),
		RunID:         pgvalue.UUID(ids.runID),
		Kind:          kind,
	})
	if err != nil {
		t.Fatal(err)
	}
	return runWait
}

func ensureRunningWorkspaceMaterialization(t *testing.T, ctx context.Context, pool interface {
	Exec(context.Context, string, ...any) (pgconn.CommandTag, error)
	QueryRow(context.Context, string, ...any) pgx.Row
}, ids integrationIDs) {
	t.Helper()
	materializationID := uuid.Must(uuid.NewV7())
	if _, err := pool.Exec(ctx, `
		INSERT INTO runtime_releases (
			runtime_id, runtime_arch, runtime_abi, kernel_digest, initramfs_digest, rootfs_digest, cni_profile
		)
		VALUES ('test-runtime', 'arm64', 'test-abi', 'sha256:kernel', 'sha256:initramfs', 'sha256:rootfs', 'test-cni')
		ON CONFLICT (runtime_id) DO NOTHING
	`); err != nil {
		t.Fatal(err)
	}
	var id uuid.UUID
	if err := pool.QueryRow(ctx, `
		WITH existing AS (
			SELECT id
			  FROM workspace_materializations
			 WHERE org_id = $2
			   AND workspace_id = $3
			   AND state IN ('requested', 'materializing', 'restoring', 'running', 'pausing', 'paused', 'capturing', 'stopping')
			 LIMIT 1
		),
		inserted AS (
		INSERT INTO workspace_materializations (
			id, org_id, project_id, environment_id, workspace_id, deployment_sandbox_id, sandbox_fingerprint,
			requested_cpu_millis, requested_memory_mib, requested_disk_mib, requested_execution_slots,
			image_artifact_id, image_artifact_format, rootfs_digest, image_digest, image_format,
			workspace_artifact_id, workspace_artifact_encoding, workspace_artifact_entry_count,
			workspace_artifact_digest, workspace_artifact_size_bytes, workspace_artifact_media_type,
			workspace_mount_path, runtime_abi, guestd_abi, adapter_abi, state
		)
		SELECT $1, workspaces.org_id, workspaces.project_id, workspaces.environment_id, workspaces.id,
		       deployment_sandboxes.id, workspaces.sandbox_fingerprint,
		       1000, 512, 1024, 1,
		       image_artifact.id, deployment_sandboxes.image_artifact_format, deployment_sandboxes.rootfs_digest,
		       deployment_sandboxes.image_digest, deployment_sandboxes.image_format,
		       workspace_artifact.id, workspace_versions.artifact_encoding, workspace_versions.artifact_entry_count,
		       workspace_artifact.digest, workspace_artifact.size_bytes, workspace_artifact.media_type,
		       deployment_sandboxes.workspace_mount_path, deployment_sandboxes.runtime_abi,
		       deployment_sandboxes.guestd_abi, deployment_sandboxes.adapter_abi, 'running'
		  FROM workspaces
		  JOIN deployment_sandboxes
		    ON deployment_sandboxes.org_id = workspaces.org_id
		   AND deployment_sandboxes.project_id = workspaces.project_id
		   AND deployment_sandboxes.environment_id = workspaces.environment_id
		   AND deployment_sandboxes.id = workspaces.deployment_sandbox_id
		  JOIN artifacts AS image_artifact
		    ON image_artifact.org_id = deployment_sandboxes.org_id
		   AND image_artifact.project_id = deployment_sandboxes.project_id
		   AND image_artifact.environment_id = deployment_sandboxes.environment_id
		   AND image_artifact.id = deployment_sandboxes.image_artifact_id
		  JOIN workspace_versions
		    ON workspace_versions.org_id = workspaces.org_id
		   AND workspace_versions.project_id = workspaces.project_id
		   AND workspace_versions.environment_id = workspaces.environment_id
		   AND workspace_versions.workspace_id = workspaces.id
		   AND workspace_versions.id = workspaces.current_version_id
		  JOIN artifacts AS workspace_artifact
		    ON workspace_artifact.org_id = workspace_versions.org_id
		   AND workspace_artifact.project_id = workspace_versions.project_id
		   AND workspace_artifact.environment_id = workspace_versions.environment_id
		   AND workspace_artifact.id = workspace_versions.artifact_id
		 WHERE workspaces.org_id = $2
		   AND workspaces.id = $3
		   AND NOT EXISTS (SELECT 1 FROM existing)
		RETURNING id
		)
		SELECT id FROM inserted
		UNION ALL
		SELECT id FROM existing
		LIMIT 1
	`, materializationID, ids.orgID, ids.workspaceID).Scan(&id); err != nil {
		t.Fatal(err)
	}
}

func seedActiveWorkspaceLeaseForRun(t *testing.T, ctx context.Context, pool interface {
	Exec(context.Context, string, ...any) (pgconn.CommandTag, error)
	QueryRow(context.Context, string, ...any) pgx.Row
}, ids integrationIDs) {
	t.Helper()
	ensureRunningWorkspaceMaterialization(t, ctx, pool, ids)
	leaseID := uuid.Must(uuid.NewV7())
	var materializationID uuid.UUID
	if err := pool.QueryRow(ctx, `
		SELECT id
		  FROM workspace_materializations
		 WHERE org_id = $1
		   AND workspace_id = $2
		   AND state IN ('requested', 'materializing', 'restoring', 'running', 'pausing', 'paused', 'capturing', 'stopping')
		 LIMIT 1
	`, ids.orgID, ids.workspaceID).Scan(&materializationID); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `
		INSERT INTO workspace_leases (
			id, org_id, project_id, environment_id, workspace_id, materialization_id,
			lease_kind, state, owner_run_id, base_version_id, acquired_version_id,
			acquired_fencing_generation, fencing_token, expires_at
		)
		SELECT $1, org_id, project_id, environment_id, id, $2,
		       'write', 'active', $3, current_version_id, current_version_id,
		       1, 'test-fencing-token', now() + interval '1 hour'
		  FROM workspaces
		 WHERE org_id = $4
		   AND id = $5
	`, leaseID, materializationID, ids.runID, ids.orgID, ids.workspaceID); err != nil {
		t.Fatal(err)
	}
}

func setRunWaitCurrentWorkspaceVersion(t *testing.T, ctx context.Context, pool interface {
	Exec(context.Context, string, ...any) (pgconn.CommandTag, error)
}, ids integrationIDs, runWait db.RunWait) {
	t.Helper()
	if _, err := pool.Exec(ctx, `
		UPDATE run_waits
		   SET workspace_version_id = (
		           SELECT current_version_id FROM workspaces WHERE org_id = $1 AND id = $2
		       )
		 WHERE org_id = $1
		   AND id = $3
	`, ids.orgID, ids.workspaceID, pgvalue.MustUUIDValue(runWait.ID)); err != nil {
		t.Fatal(err)
	}
}

func advanceWorkspaceCurrentVersion(t *testing.T, ctx context.Context, pool *pgxpool.Pool, ids integrationIDs) uuid.UUID {
	t.Helper()
	nextArtifactID := seedWorkspaceVersionArtifact(t, ctx, pool, ids)
	nextVersionID := uuid.Must(uuid.NewV7())
	if _, err := pool.Exec(ctx, `
		INSERT INTO workspace_versions (
			id, org_id, project_id, environment_id, workspace_id, kind, state,
			artifact_id, artifact_encoding, artifact_entry_count, content_digest, size_bytes, promoted_at
		)
		SELECT $1, $2, $3, $4, $5, 'system', 'ready',
		       artifacts.id, 'tar', 0, artifacts.digest, artifacts.size_bytes, now()
		  FROM artifacts
		 WHERE artifacts.org_id = $2
		   AND artifacts.project_id = $3
		   AND artifacts.environment_id = $4
		   AND artifacts.id = $6
	`, nextVersionID, ids.orgID, ids.projectID, ids.environmentID, ids.workspaceID, nextArtifactID); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `
		UPDATE workspaces
		   SET current_version_id = $1
		 WHERE org_id = $2
		   AND id = $3
	`, nextVersionID, ids.orgID, ids.workspaceID); err != nil {
		t.Fatal(err)
	}
	return nextVersionID
}

func markRunWaitWaiting(t *testing.T, ctx context.Context, pool interface {
	Exec(context.Context, string, ...any) (pgconn.CommandTag, error)
	QueryRow(context.Context, string, ...any) pgx.Row
}, ids integrationIDs, runWait db.RunWait) {
	t.Helper()
	leaseID := uuid.Must(uuid.NewV7())
	checkpointID := uuid.Must(uuid.NewV7())
	ensureRunningWorkspaceMaterialization(t, ctx, pool, ids)
	var materializationID uuid.UUID
	if err := pool.QueryRow(ctx, `
		SELECT id
		  FROM workspace_materializations
		 WHERE org_id = $1
		   AND workspace_id = $2
		   AND state IN ('requested', 'materializing', 'restoring', 'running', 'pausing', 'paused', 'capturing', 'stopping')
		 LIMIT 1
	`, ids.orgID, ids.workspaceID).Scan(&materializationID); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `
		INSERT INTO workspace_leases (
			id, org_id, project_id, environment_id, workspace_id, materialization_id,
			lease_kind, state, owner_run_id, base_version_id, acquired_version_id,
			acquired_fencing_generation, fencing_token, expires_at
		)
		SELECT $1, org_id, project_id, environment_id, id, $2,
		       'write', 'released', $3, current_version_id, current_version_id,
		       1, 'test-fencing-token', now() + interval '1 hour'
		  FROM workspaces
		 WHERE org_id = $4
		   AND id = $5
	`, leaseID, materializationID, ids.runID, ids.orgID, ids.workspaceID); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `
		INSERT INTO runtime_checkpoints (
			id, org_id, project_id, environment_id, workspace_id, run_id,
			source_workspace_lease_id, materialization_id, base_workspace_version_id,
			state, runtime_backend, runtime_id, runtime_arch, runtime_abi, kernel_digest,
			initramfs_digest, rootfs_digest, runtime_config_digest, cni_profile, manifest, ready_at
		)
		SELECT $1, workspaces.org_id, workspaces.project_id, workspaces.environment_id,
		       workspaces.id, $2, $3, $4, workspaces.current_version_id,
		       'ready', 'test', 'test-runtime', 'arm64', 'test-abi', 'sha256:kernel',
		       'sha256:initramfs', 'sha256:rootfs', 'sha256:config', 'test-cni', '{}', now()
		  FROM workspaces
		 WHERE workspaces.org_id = $5
		   AND workspaces.id = $6
	`, checkpointID, ids.runID, leaseID, materializationID, ids.orgID, ids.workspaceID); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `
		UPDATE run_waits
		   SET state = 'waiting',
		       workspace_version_id = (
		           SELECT current_version_id FROM workspaces WHERE org_id = $1 AND id = $2
		       ),
		       runtime_checkpoint_id = $3,
		       active_elapsed_ms_at_park = 10
		 WHERE org_id = $1 AND id = $4
	`, ids.orgID, ids.workspaceID, checkpointID, pgvalue.MustUUIDValue(runWait.ID)); err != nil {
		t.Fatal(err)
	}
}
