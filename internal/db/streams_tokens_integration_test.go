package db_test

import (
	"context"
	"errors"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/helmrdotdev/helmr/internal/db"
	"github.com/helmrdotdev/helmr/internal/db/dbtest"
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
		CellID:           testCellID,
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

func TestMarkRuntimeResumeWaitResumedAcceptsExecutingRun(t *testing.T) {
	ctx := context.Background()
	pool := newIntegrationDB(t, ctx)
	ids := seedIntegration(t, ctx, pool)
	queries := db.New(pool)
	_, runLeaseID, workerID := seedRunningSessionLease(t, ctx, pool, ids)
	runWait := seedRunWait(t, ctx, pool, ids, db.RunWaitKindTimer)
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
	restoreID := uuid.Must(uuid.NewV7())
	if _, err := pool.Exec(ctx, `
			INSERT INTO runtime_checkpoint_restores (
				id, org_id, cell_id, project_id, environment_id, run_id, runtime_checkpoint_id,
				run_wait_id, run_lease_id, worker_instance_id
			)
			VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10)
		`, restoreID, ids.orgID, dbtest.DefaultCellID, ids.projectID, ids.environmentID, ids.runID, pgvalue.MustUUIDValue(storedWait.RuntimeCheckpointID), pgvalue.MustUUIDValue(runWait.ID), runLeaseID, workerID); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `
		UPDATE run_waits
		   SET state = 'resuming'
		 WHERE org_id = $1
		   AND id = $2
	`, ids.orgID, pgvalue.MustUUIDValue(runWait.ID)); err != nil {
		t.Fatal(err)
	}

	resumed, err := queries.MarkRuntimeResumeWaitResumed(ctx, db.MarkRuntimeResumeWaitResumedParams{
		OrgID:               pgvalue.UUID(ids.orgID),
		ID:                  runWait.ID,
		RunID:               pgvalue.UUID(ids.runID),
		RuntimeCheckpointID: storedWait.RuntimeCheckpointID,
		RunLeaseID:          pgvalue.UUID(runLeaseID),
		RestorePhases: []byte(`[
			{
				"name": "restore_materialize_memory_filepack",
				"duration_ms": 1234,
				"role": "memory",
				"media_type": "application/vnd.helmr.checkpoint-memory.v0.filepack",
				"filepack": {
					"logical_bytes": 104857600,
					"allocated_bytes": 8388608,
					"sparse_supported": true,
					"sparse_data_ranges": 2,
					"sparse_data_bytes": 8388608,
					"zero_chunks_skipped": 1024,
					"encoded_chunks": 64,
					"compressed_bytes": 2097152,
					"unpack_written_bytes": 8388608
				}
			},
			{
				"name": "restore_attach_guest_resume",
				"duration_ms": 45
			},
			{
				"name": "restore_malformed_telemetry",
				"duration_ms": -1,
				"filepack": {
					"logical_bytes": -10,
					"sparse_supported": "unknown"
				}
			}
		]`),
	})
	if err != nil {
		t.Fatal(err)
	}
	if resumed.ID != runWait.ID || resumed.RunID != pgvalue.UUID(ids.runID) || resumed.State != db.RunWaitStateResumed {
		t.Fatalf("resumed wait = %+v, want wait %v for run %v resumed", resumed, pgvalue.MustUUIDValue(runWait.ID), ids.runID)
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
	var restoreStatus db.RuntimeCheckpointRestoreStatus
	var acknowledgedAt pgtype.Timestamptz
	var finishedAt pgtype.Timestamptz
	var restorePhases []byte
	if err := pool.QueryRow(ctx, `
		SELECT status, acknowledged_at, finished_at, phases
		  FROM runtime_checkpoint_restores
		 WHERE org_id = $1
		   AND id = $2
	`, ids.orgID, restoreID).Scan(&restoreStatus, &acknowledgedAt, &finishedAt, &restorePhases); err != nil {
		t.Fatal(err)
	}
	if restoreStatus != db.RuntimeCheckpointRestoreStatusRestored {
		t.Fatalf("restore status = %s, want restored", restoreStatus)
	}
	if !acknowledgedAt.Valid || !finishedAt.Valid {
		t.Fatalf("restore acknowledged_at=%+v finished_at=%+v, want both set", acknowledgedAt, finishedAt)
	}
	if got := string(restorePhases); !strings.Contains(got, "restore_materialize_memory_filepack") || !strings.Contains(got, "restore_attach_guest_resume") {
		t.Fatalf("restore phases = %s", got)
	}
}

func TestMarkRuntimeResumeWaitResumedRequiresRestoreAttempt(t *testing.T) {
	ctx := context.Background()
	pool := newIntegrationDB(t, ctx)
	ids := seedIntegration(t, ctx, pool)
	queries := db.New(pool)
	_, runLeaseID, _ := seedRunningSessionLease(t, ctx, pool, ids)
	runWait := seedRunWait(t, ctx, pool, ids, db.RunWaitKindTimer)
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
	if _, err := pool.Exec(ctx, `
		UPDATE run_waits
		   SET state = 'resuming'
		 WHERE org_id = $1
		   AND id = $2
	`, ids.orgID, pgvalue.MustUUIDValue(runWait.ID)); err != nil {
		t.Fatal(err)
	}

	_, err = queries.MarkRuntimeResumeWaitResumed(ctx, db.MarkRuntimeResumeWaitResumedParams{
		OrgID:               pgvalue.UUID(ids.orgID),
		ID:                  runWait.ID,
		RunID:               pgvalue.UUID(ids.runID),
		RuntimeCheckpointID: storedWait.RuntimeCheckpointID,
		RunLeaseID:          pgvalue.UUID(runLeaseID),
		RestorePhases:       []byte(`[]`),
	})
	if !errors.Is(err, pgx.ErrNoRows) {
		t.Fatalf("MarkRuntimeResumeWaitResumed without restore attempt error = %v, want pgx.ErrNoRows", err)
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
	if after.State != db.RunWaitStateResuming {
		t.Fatalf("run wait state = %s, want resuming after failed restore ack", after.State)
	}
}

func TestLeaseRunLeaseCreatesRuntimeCheckpointRestoreAttempt(t *testing.T) {
	ctx := context.Background()
	pool := newIntegrationDB(t, ctx)
	ids := seedIntegration(t, ctx, pool)
	queries := db.New(pool)
	_, sourceRunLeaseID, workerID := seedRunningSessionLease(t, ctx, pool, ids)
	seedActiveWorkspaceLeaseForRun(t, ctx, pool, ids)
	runWait, err := queries.CreateHotRunWait(ctx, db.CreateHotRunWaitParams{
		PublicID:         testWaitPublicID(t),
		ID:               pgvalue.UUID(uuid.Must(uuid.NewV7())),
		Kind:             db.RunWaitKindTimer,
		OrgID:            pgvalue.UUID(ids.orgID),
		ProjectID:        pgvalue.UUID(ids.projectID),
		EnvironmentID:    pgvalue.UUID(ids.environmentID),
		RunID:            pgvalue.UUID(ids.runID),
		RunLeaseID:       pgvalue.UUID(sourceRunLeaseID),
		WorkerInstanceID: pgvalue.UUID(workerID),
		CheckpointDelay:  pgvalue.Interval(0),
	})
	if err != nil {
		t.Fatal(err)
	}
	setRunWaitCurrentWorkspaceVersion(t, ctx, pool, ids, runWaitFromCreateHotRunWaitRow(runWait))
	checkpointID := uuid.Must(uuid.NewV7())
	if _, err := queries.ClaimRuntimeCheckpointWait(ctx, db.ClaimRuntimeCheckpointWaitParams{
		OrgID:               pgvalue.UUID(ids.orgID),
		ProjectID:           pgvalue.UUID(ids.projectID),
		EnvironmentID:       pgvalue.UUID(ids.environmentID),
		RunID:               pgvalue.UUID(ids.runID),
		RunWaitID:           runWait.ID,
		RunLeaseID:          pgvalue.UUID(sourceRunLeaseID),
		WorkerInstanceID:    pgvalue.UUID(workerID),
		RuntimeCheckpointID: pgvalue.UUID(checkpointID),
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := queries.CreateReadyRuntimeCheckpointForRunWait(ctx, readyRuntimeCheckpointParamsForRun(t, ctx, pool, ids, runWaitFromCreateHotRunWaitRow(runWait), sourceRunLeaseID, workerID, checkpointID)); err != nil {
		t.Fatal(err)
	}
	var sourceWorkerID uuid.UUID
	if err := pool.QueryRow(ctx, `
		SELECT source_worker_instance_id
		  FROM runtime_checkpoints
		 WHERE org_id = $1
		   AND run_id = $2
		   AND id = $3
	`, ids.orgID, ids.runID, checkpointID).Scan(&sourceWorkerID); err != nil {
		t.Fatal(err)
	}
	if sourceWorkerID != workerID {
		t.Fatalf("checkpoint source worker = %s, want %s", sourceWorkerID, workerID)
	}
	resolveCheckpointedRunWaitForTest(t, ctx, pool, ids, runWaitFromCreateHotRunWaitRow(runWait))
	requeued, err := queries.RequeueResolvedRunWaits(ctx, db.RequeueResolvedRunWaitsParams{
		OrgID:      pgvalue.UUID(ids.orgID),
		CellID:     dbtest.DefaultCellID,
		LimitCount: 100,
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(requeued) != 1 || requeued[0].ID != runWait.ID || requeued[0].State != db.RunWaitStateResuming {
		t.Fatalf("requeued waits = %+v, want resuming wait %s", requeued, pgvalue.MustUUIDValue(runWait.ID))
	}
	if !requeued[0].ResumingAt.Valid {
		t.Fatalf("requeued resuming_at is invalid")
	}
	var queuedStatus db.RunQueueStatus
	var queuedDispatchMessageID pgtype.Text
	var queuedReservationExpiresAt pgtype.Timestamptz
	if err := pool.QueryRow(ctx, `
		SELECT status, dispatch_message_id, reservation_expires_at
		  FROM run_queue_items
		 WHERE org_id = $1
		   AND run_id = $2
	`, ids.orgID, ids.runID).Scan(&queuedStatus, &queuedDispatchMessageID, &queuedReservationExpiresAt); err != nil {
		t.Fatal(err)
	}
	if queuedStatus != db.RunQueueStatusQueued || queuedDispatchMessageID.Valid || queuedReservationExpiresAt.Valid {
		t.Fatalf("queued item status=%s dispatch=%+v reservation=%+v, want queued without dispatch reservation", queuedStatus, queuedDispatchMessageID, queuedReservationExpiresAt)
	}
	seedRestoreReadyWorkspaceMount(t, ctx, pool, ids, workerID)
	restoreRunLeaseID := uuid.Must(uuid.NewV7())
	reserved, err := queries.ReserveCheckpointRestoreRunQueueItemForWorker(ctx, db.ReserveCheckpointRestoreRunQueueItemForWorkerParams{
		WorkerInstanceID:     pgvalue.UUID(workerID),
		ReservationExpiresAt: pgtype.Timestamptz{Time: time.Now().Add(time.Hour), Valid: true},
	})
	if err != nil {
		t.Fatal(err)
	}
	if reserved.RunID != pgvalue.UUID(ids.runID) ||
		reserved.ReservedByWorkerInstanceID != pgvalue.UUID(workerID) ||
		!strings.HasPrefix(reserved.DispatchMessageID.String, "restore-source:") {
		t.Fatalf("reserved source restore queue item = %+v, want run %s on source worker %s", reserved, ids.runID, workerID)
	}
	restoreDispatchMessageID := reserved.DispatchMessageID.String
	leased, err := queries.LeaseRunLease(ctx, db.LeaseRunLeaseParams{
		OrgID:             pgvalue.UUID(ids.orgID),
		RunID:             pgvalue.UUID(ids.runID),
		WorkerInstanceID:  pgvalue.UUID(workerID),
		RunLeaseID:        pgvalue.UUID(restoreRunLeaseID),
		DispatchMessageID: pgtype.Text{String: restoreDispatchMessageID, Valid: true},
		DispatchLeaseID:   "lease-restore-" + shortUUID(restoreRunLeaseID),
		DispatchAttempt:   1,
		LeaseExpiresAt:    pgtype.Timestamptz{Time: time.Now().Add(time.Hour), Valid: true},
		RunLeaseSpanID:    "4444444444444444",
	})
	if err != nil {
		t.Fatal(err)
	}
	if leased.RunLeaseRestoreRuntimeCheckpointID != pgvalue.UUID(checkpointID) {
		t.Fatalf("restore checkpoint id = %+v, want %s", leased.RunLeaseRestoreRuntimeCheckpointID, checkpointID)
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
	if checkpointState != db.RuntimeCheckpointStateReady {
		t.Fatalf("checkpoint state=%s, want ready", checkpointState)
	}
	var restoreCount int
	var restoreStatus db.RuntimeCheckpointRestoreStatus
	var restoreStartedAt pgtype.Timestamptz
	if err := pool.QueryRow(ctx, `
		SELECT count(*), max(status), max(started_at)
		  FROM runtime_checkpoint_restores
		 WHERE org_id = $1
		   AND run_id = $2
		   AND runtime_checkpoint_id = $3
		   AND run_wait_id = $4
		   AND run_lease_id = $5
		   AND worker_instance_id = $6
	`, ids.orgID, ids.runID, checkpointID, pgvalue.MustUUIDValue(runWait.ID), restoreRunLeaseID, workerID).Scan(&restoreCount, &restoreStatus, &restoreStartedAt); err != nil {
		t.Fatal(err)
	}
	if restoreCount != 1 || restoreStatus != db.RuntimeCheckpointRestoreStatusRestoring || !restoreStartedAt.Valid {
		t.Fatalf("restore attempt count=%d status=%s started_at=%+v, want one restoring attempt", restoreCount, restoreStatus, restoreStartedAt)
	}
}

func TestClaimRuntimeCheckpointWaitRejectsStaleRuntimeEpoch(t *testing.T) {
	ctx := context.Background()
	pool := newIntegrationDB(t, ctx)
	ids := seedIntegration(t, ctx, pool)
	queries := db.New(pool)
	_, runLeaseID, workerID := seedRunningSessionLease(t, ctx, pool, ids)
	seedActiveWorkspaceLeaseForRun(t, ctx, pool, ids)
	runWait, err := queries.CreateHotRunWait(ctx, db.CreateHotRunWaitParams{
		PublicID:         testWaitPublicID(t),
		ID:               pgvalue.UUID(uuid.Must(uuid.NewV7())),
		Kind:             db.RunWaitKindTimer,
		OrgID:            pgvalue.UUID(ids.orgID),
		ProjectID:        pgvalue.UUID(ids.projectID),
		EnvironmentID:    pgvalue.UUID(ids.environmentID),
		RunID:            pgvalue.UUID(ids.runID),
		RunLeaseID:       pgvalue.UUID(runLeaseID),
		WorkerInstanceID: pgvalue.UUID(workerID),
		CheckpointDelay:  pgvalue.Interval(0),
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `
		UPDATE runtime_instances
		   SET runtime_epoch = runtime_epoch + 1
		 WHERE org_id = $1
		   AND id = $2
	`, ids.orgID, pgvalue.MustUUIDValue(runWait.OwnerRuntimeInstanceID)); err != nil {
		t.Fatal(err)
	}

	_, err = queries.ClaimRuntimeCheckpointWait(ctx, db.ClaimRuntimeCheckpointWaitParams{
		OrgID:               pgvalue.UUID(ids.orgID),
		ProjectID:           pgvalue.UUID(ids.projectID),
		EnvironmentID:       pgvalue.UUID(ids.environmentID),
		RunID:               pgvalue.UUID(ids.runID),
		RunWaitID:           runWait.ID,
		RunLeaseID:          pgvalue.UUID(runLeaseID),
		WorkerInstanceID:    pgvalue.UUID(workerID),
		RuntimeCheckpointID: pgvalue.UUID(uuid.Must(uuid.NewV7())),
	})
	if !errors.Is(err, pgx.ErrNoRows) {
		t.Fatalf("claim stale runtime epoch error = %v, want ErrNoRows", err)
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
	if after.State != db.RunWaitStateLiveWaiting || after.RuntimeCheckpointID.Valid {
		t.Fatalf("run wait after stale claim = state %s checkpoint %+v, want live_waiting without checkpoint", after.State, after.RuntimeCheckpointID)
	}
}

func TestAcknowledgeWorkerCommandMarksHotWaitResumed(t *testing.T) {
	ctx := context.Background()
	pool := newIntegrationDB(t, ctx)
	ids := seedIntegration(t, ctx, pool)
	queries := db.New(pool)
	_, runLeaseID, workerID := seedRunningSessionLease(t, ctx, pool, ids)
	runWait, err := queries.CreateHotRunWait(ctx, db.CreateHotRunWaitParams{
		PublicID:         testWaitPublicID(t),
		ID:               pgvalue.UUID(uuid.Must(uuid.NewV7())),
		Kind:             db.RunWaitKindTimer,
		OrgID:            pgvalue.UUID(ids.orgID),
		ProjectID:        pgvalue.UUID(ids.projectID),
		EnvironmentID:    pgvalue.UUID(ids.environmentID),
		RunID:            pgvalue.UUID(ids.runID),
		RunLeaseID:       pgvalue.UUID(runLeaseID),
		WorkerInstanceID: pgvalue.UUID(workerID),
		CheckpointDelay:  pgvalue.Interval(5 * time.Second),
	})
	if err != nil {
		t.Fatal(err)
	}
	state, currentWaitID := runtimeStateForRun(t, ctx, pool, ids)
	if state != db.RuntimeInstanceStateWaitingHot || currentWaitID != pgvalue.MustUUIDValue(runWait.ID) {
		t.Fatalf("runtime after hot wait = state %s wait %s, want checkpointed_waiting_hot/%s", state, currentWaitID, pgvalue.MustUUIDValue(runWait.ID))
	}
	if _, err := pool.Exec(ctx, `
		UPDATE run_waits
		   SET state = 'resolved_live',
		       resolved_at = now()
		 WHERE org_id = $1
		   AND id = $2
	`, ids.orgID, pgvalue.MustUUIDValue(runWait.ID)); err != nil {
		t.Fatal(err)
	}
	command, err := queries.CreateWorkerCommand(ctx, db.CreateWorkerCommandParams{
		OrgID:             pgvalue.UUID(ids.orgID),
		CellID:            dbtest.DefaultCellID,
		RouteGeneration:   1,
		ProjectID:         pgvalue.UUID(ids.projectID),
		EnvironmentID:     pgvalue.UUID(ids.environmentID),
		RunID:             pgvalue.UUID(ids.runID),
		RunWaitID:         runWait.ID,
		RunLeaseID:        pgvalue.UUID(runLeaseID),
		WorkerInstanceID:  pgvalue.UUID(workerID),
		RuntimeInstanceID: runWait.OwnerRuntimeInstanceID,
		RuntimeEpoch:      runWait.OwnerRuntimeEpoch,
		RunStateVersion:   runWait.OwnerRunStateVersion,
		Kind:              db.WorkerCommandKindRuntimeResumeWait,
		Payload:           []byte(`{"resume_kind":"completed","resume_payload":null}`),
	})
	if err != nil {
		t.Fatal(err)
	}

	acked, err := queries.AcknowledgeWorkerCommand(ctx, db.AcknowledgeWorkerCommandParams{
		WorkerInstanceID: pgvalue.UUID(workerID),
		CellID:           dbtest.DefaultCellID,
		ID:               command.ID,
	})
	if err != nil {
		t.Fatal(err)
	}
	if acked.ID != command.ID || !acked.AcknowledgedAt.Valid {
		t.Fatalf("acked command = %+v", acked)
	}
	if !acked.AcceptedAt.Valid || !acked.CompletedAt.Valid {
		t.Fatalf("command lifecycle after ack = accepted_at %+v completed_at %+v, want both set", acked.AcceptedAt, acked.CompletedAt)
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
	if after.State != db.RunWaitStateResumed || !after.ResumedAt.Valid {
		t.Fatalf("run wait after ack = state %s resumed_at %+v, want resumed", after.State, after.ResumedAt)
	}
	state, currentWaitID = runtimeStateForRun(t, ctx, pool, ids)
	if state != db.RuntimeInstanceStateRunning || currentWaitID != uuid.Nil {
		t.Fatalf("runtime after resume ack = state %s wait %s, want running/no wait", state, currentWaitID)
	}
}

func TestClaimRuntimeCheckpointWaitReplaysExistingCreatingCheckpoint(t *testing.T) {
	ctx := context.Background()
	pool := newIntegrationDB(t, ctx)
	ids := seedIntegration(t, ctx, pool)
	queries := db.New(pool)
	_, runLeaseID, workerID := seedRunningSessionLease(t, ctx, pool, ids)
	seedActiveWorkspaceLeaseForRun(t, ctx, pool, ids)
	runWaitRow, err := queries.CreateHotRunWait(ctx, db.CreateHotRunWaitParams{
		PublicID:         testWaitPublicID(t),
		ID:               pgvalue.UUID(uuid.Must(uuid.NewV7())),
		Kind:             db.RunWaitKindTimer,
		OrgID:            pgvalue.UUID(ids.orgID),
		ProjectID:        pgvalue.UUID(ids.projectID),
		EnvironmentID:    pgvalue.UUID(ids.environmentID),
		RunID:            pgvalue.UUID(ids.runID),
		RunLeaseID:       pgvalue.UUID(runLeaseID),
		WorkerInstanceID: pgvalue.UUID(workerID),
		CheckpointDelay:  pgvalue.Interval(0),
	})
	if err != nil {
		t.Fatal(err)
	}
	runWait := runWaitFromCreateHotRunWaitRow(runWaitRow)
	setRunWaitCurrentWorkspaceVersion(t, ctx, pool, ids, runWait)
	firstCheckpointID := uuid.Must(uuid.NewV7())
	first, err := queries.ClaimRuntimeCheckpointWait(ctx, db.ClaimRuntimeCheckpointWaitParams{
		OrgID:               pgvalue.UUID(ids.orgID),
		ProjectID:           pgvalue.UUID(ids.projectID),
		EnvironmentID:       pgvalue.UUID(ids.environmentID),
		RunID:               pgvalue.UUID(ids.runID),
		RunWaitID:           runWait.ID,
		RunLeaseID:          pgvalue.UUID(runLeaseID),
		WorkerInstanceID:    pgvalue.UUID(workerID),
		RuntimeCheckpointID: pgvalue.UUID(firstCheckpointID),
	})
	if err != nil {
		t.Fatal(err)
	}
	replayed, err := queries.ClaimRuntimeCheckpointWait(ctx, db.ClaimRuntimeCheckpointWaitParams{
		OrgID:               pgvalue.UUID(ids.orgID),
		ProjectID:           pgvalue.UUID(ids.projectID),
		EnvironmentID:       pgvalue.UUID(ids.environmentID),
		RunID:               pgvalue.UUID(ids.runID),
		RunWaitID:           runWait.ID,
		RunLeaseID:          pgvalue.UUID(runLeaseID),
		WorkerInstanceID:    pgvalue.UUID(workerID),
		RuntimeCheckpointID: pgvalue.UUID(uuid.Must(uuid.NewV7())),
	})
	if err != nil {
		t.Fatal(err)
	}
	if replayed.RuntimeCheckpointID != first.RuntimeCheckpointID {
		t.Fatalf("replayed checkpoint id = %s, want original %s", pgvalue.MustUUIDValue(replayed.RuntimeCheckpointID), pgvalue.MustUUIDValue(first.RuntimeCheckpointID))
	}
	var checkpointCount int
	if err := pool.QueryRow(ctx, `
		SELECT count(*)
		  FROM runtime_checkpoints
		 WHERE org_id = $1
		   AND run_id = $2
		   AND owner_run_wait_id = $3
	`, ids.orgID, ids.runID, pgvalue.MustUUIDValue(runWait.ID)).Scan(&checkpointCount); err != nil {
		t.Fatal(err)
	}
	if checkpointCount != 1 {
		t.Fatalf("checkpoint count = %d, want one idempotent creating checkpoint", checkpointCount)
	}
}

func TestAcknowledgeWorkerCommandRetiresStaleHotResumeAfterRuntimeCheckpointing(t *testing.T) {
	ctx := context.Background()
	pool := newIntegrationDB(t, ctx)
	ids := seedIntegration(t, ctx, pool)
	queries := db.New(pool)
	_, runLeaseID, workerID := seedRunningSessionLease(t, ctx, pool, ids)
	runWait, err := queries.CreateHotRunWait(ctx, db.CreateHotRunWaitParams{
		PublicID:         testWaitPublicID(t),
		ID:               pgvalue.UUID(uuid.Must(uuid.NewV7())),
		Kind:             db.RunWaitKindTimer,
		OrgID:            pgvalue.UUID(ids.orgID),
		ProjectID:        pgvalue.UUID(ids.projectID),
		EnvironmentID:    pgvalue.UUID(ids.environmentID),
		RunID:            pgvalue.UUID(ids.runID),
		RunLeaseID:       pgvalue.UUID(runLeaseID),
		WorkerInstanceID: pgvalue.UUID(workerID),
		CheckpointDelay:  pgvalue.Interval(5 * time.Second),
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `
		UPDATE run_waits
		   SET state = 'resolved_live',
		       resolved_at = now()
		 WHERE org_id = $1
		   AND id = $2
	`, ids.orgID, pgvalue.MustUUIDValue(runWait.ID)); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `
		UPDATE runtime_instances
		   SET state = 'checkpointing',
		       checkpointing_at = now()
		 WHERE org_id = $1
		   AND id = $2
		   AND runtime_epoch = $3
	`, ids.orgID, pgvalue.MustUUIDValue(runWait.OwnerRuntimeInstanceID), runWait.OwnerRuntimeEpoch.Int64); err != nil {
		t.Fatal(err)
	}
	command, err := queries.CreateWorkerCommand(ctx, db.CreateWorkerCommandParams{
		OrgID:             pgvalue.UUID(ids.orgID),
		CellID:            dbtest.DefaultCellID,
		RouteGeneration:   1,
		ProjectID:         pgvalue.UUID(ids.projectID),
		EnvironmentID:     pgvalue.UUID(ids.environmentID),
		RunID:             pgvalue.UUID(ids.runID),
		RunWaitID:         runWait.ID,
		RunLeaseID:        pgvalue.UUID(runLeaseID),
		WorkerInstanceID:  pgvalue.UUID(workerID),
		RuntimeInstanceID: runWait.OwnerRuntimeInstanceID,
		RuntimeEpoch:      runWait.OwnerRuntimeEpoch,
		RunStateVersion:   runWait.OwnerRunStateVersion,
		Kind:              db.WorkerCommandKindRuntimeResumeWait,
		Payload:           []byte(`{"resume_kind":"completed","resume_payload":null}`),
	})
	if err != nil {
		t.Fatal(err)
	}

	acked, err := queries.AcknowledgeWorkerCommand(ctx, db.AcknowledgeWorkerCommandParams{
		WorkerInstanceID: pgvalue.UUID(workerID),
		CellID:           dbtest.DefaultCellID,
		ID:               command.ID,
	})
	if err != nil {
		t.Fatal(err)
	}
	if acked.ID != command.ID || !acked.AcknowledgedAt.Valid || !acked.CompletedAt.Valid {
		t.Fatalf("stale acked command = %+v, want acknowledged and completed", acked)
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
	if after.State != db.RunWaitStateResolvedLive || after.ResumedAt.Valid {
		t.Fatalf("run wait after rejected ack = state %s resumed_at %+v, want resolved_live without resume", after.State, after.ResumedAt)
	}
	var acknowledgedAt pgtype.Timestamptz
	if err := pool.QueryRow(ctx, `
		SELECT acknowledged_at
		  FROM worker_commands
		 WHERE id = $1
	`, command.ID).Scan(&acknowledgedAt); err != nil {
		t.Fatal(err)
	}
	if !acknowledgedAt.Valid {
		t.Fatal("command acknowledged_at is null after stale resume ack")
	}
}

func TestCreateResolvedLiveRuntimeResumeWaitCommandsSkipsCheckpointingRuntime(t *testing.T) {
	ctx := context.Background()
	pool := newIntegrationDB(t, ctx)
	ids := seedIntegration(t, ctx, pool)
	queries := db.New(pool)
	_, runLeaseID, workerID := seedRunningSessionLease(t, ctx, pool, ids)
	runWait, err := queries.CreateHotRunWait(ctx, db.CreateHotRunWaitParams{
		PublicID:         testWaitPublicID(t),
		ID:               pgvalue.UUID(uuid.Must(uuid.NewV7())),
		Kind:             db.RunWaitKindTimer,
		OrgID:            pgvalue.UUID(ids.orgID),
		ProjectID:        pgvalue.UUID(ids.projectID),
		EnvironmentID:    pgvalue.UUID(ids.environmentID),
		RunID:            pgvalue.UUID(ids.runID),
		RunLeaseID:       pgvalue.UUID(runLeaseID),
		WorkerInstanceID: pgvalue.UUID(workerID),
		CheckpointDelay:  pgvalue.Interval(5 * time.Second),
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `
		UPDATE run_waits
		   SET state = 'resolved_live',
		       resolved_at = now()
		 WHERE org_id = $1
		   AND id = $2
	`, ids.orgID, pgvalue.MustUUIDValue(runWait.ID)); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `
		UPDATE runtime_instances
		   SET state = 'checkpointing',
		       checkpointing_at = now()
		 WHERE org_id = $1
		   AND id = $2
		   AND runtime_epoch = $3
	`, ids.orgID, pgvalue.MustUUIDValue(runWait.OwnerRuntimeInstanceID), runWait.OwnerRuntimeEpoch.Int64); err != nil {
		t.Fatal(err)
	}

	commands, err := queries.CreateResolvedLiveRuntimeResumeWaitCommandsForOrg(ctx, db.CreateResolvedLiveRuntimeResumeWaitCommandsForOrgParams{
		OrgID:      pgvalue.UUID(ids.orgID),
		CellID:     dbtest.DefaultCellID,
		LimitCount: 10,
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(commands) != 0 {
		t.Fatalf("resume commands = %+v, want none after runtime moved to checkpointing", commands)
	}
}

func TestResolvedTimerWorkerCommandCompletesEvenWhenLegacyTimeoutMatchesFireTime(t *testing.T) {
	ctx := context.Background()
	pool := newIntegrationDB(t, ctx)
	ids := seedIntegration(t, ctx, pool)
	queries := db.New(pool)
	_, runLeaseID, workerID := seedRunningSessionLease(t, ctx, pool, ids)
	runWait, err := queries.CreateHotRunWait(ctx, db.CreateHotRunWaitParams{
		PublicID:         testWaitPublicID(t),
		ID:               pgvalue.UUID(uuid.Must(uuid.NewV7())),
		Kind:             db.RunWaitKindTimer,
		OrgID:            pgvalue.UUID(ids.orgID),
		ProjectID:        pgvalue.UUID(ids.projectID),
		EnvironmentID:    pgvalue.UUID(ids.environmentID),
		RunID:            pgvalue.UUID(ids.runID),
		RunLeaseID:       pgvalue.UUID(runLeaseID),
		WorkerInstanceID: pgvalue.UUID(workerID),
		TimeoutAt:        pgvalue.Timestamptz(time.Now().Add(-time.Second)),
		CheckpointDelay:  pgvalue.Interval(5 * time.Second),
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `
		UPDATE run_waits
		   SET state = 'resolved_live',
		       resolved_at = now()
		 WHERE org_id = $1
		   AND id = $2
	`, ids.orgID, pgvalue.MustUUIDValue(runWait.ID)); err != nil {
		t.Fatal(err)
	}

	commands, err := queries.CreateResolvedLiveRuntimeResumeWaitCommandsForOrg(ctx, db.CreateResolvedLiveRuntimeResumeWaitCommandsForOrgParams{
		OrgID:      pgvalue.UUID(ids.orgID),
		CellID:     dbtest.DefaultCellID,
		LimitCount: 10,
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(commands) != 1 || commands[0].RunWaitID != runWait.ID {
		t.Fatalf("resume commands = %+v, want single command for %s", commands, pgvalue.MustUUIDValue(runWait.ID))
	}
	payload := string(commands[0].Payload)
	if !strings.Contains(payload, `"resume_kind": "completed"`) && !strings.Contains(payload, `"resume_kind":"completed"`) {
		t.Fatalf("resume payload = %s, want completed timer resume", payload)
	}
}

func TestCheckpointWorkerCommandSkipsDueTimerWait(t *testing.T) {
	ctx := context.Background()
	pool := newIntegrationDB(t, ctx)
	ids := seedIntegration(t, ctx, pool)
	queries := db.New(pool)
	_, runLeaseID, workerID := seedRunningSessionLease(t, ctx, pool, ids)
	runWait, err := queries.CreateHotRunWait(ctx, db.CreateHotRunWaitParams{
		PublicID:         testWaitPublicID(t),
		ID:               pgvalue.UUID(uuid.Must(uuid.NewV7())),
		Kind:             db.RunWaitKindTimer,
		OrgID:            pgvalue.UUID(ids.orgID),
		ProjectID:        pgvalue.UUID(ids.projectID),
		EnvironmentID:    pgvalue.UUID(ids.environmentID),
		RunID:            pgvalue.UUID(ids.runID),
		RunLeaseID:       pgvalue.UUID(runLeaseID),
		WorkerInstanceID: pgvalue.UUID(workerID),
		CheckpointDelay:  pgvalue.Interval(0),
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := queries.CreateTimerWait(ctx, db.CreateTimerWaitParams{
		ID:            pgvalue.UUID(uuid.Must(uuid.NewV7())),
		OrgID:         pgvalue.UUID(ids.orgID),
		ProjectID:     pgvalue.UUID(ids.projectID),
		EnvironmentID: pgvalue.UUID(ids.environmentID),
		RunWaitID:     runWait.ID,
		FireAt:        pgvalue.Timestamptz(time.Now().Add(-time.Second)),
	}); err != nil {
		t.Fatal(err)
	}

	checkpointCommands, err := queries.CreateDueLiveRuntimeCheckpointWaitCommandsForOrg(ctx, db.CreateDueLiveRuntimeCheckpointWaitCommandsForOrgParams{
		OrgID:      pgvalue.UUID(ids.orgID),
		CellID:     dbtest.DefaultCellID,
		LimitCount: 10,
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(checkpointCommands) != 0 {
		t.Fatalf("checkpoint commands = %+v, want none for due timer wait", checkpointCommands)
	}
}

func TestCheckpointWorkerCommandCanRetryAfterAcknowledgedFailure(t *testing.T) {
	ctx := context.Background()
	pool := newIntegrationDB(t, ctx)
	ids := seedIntegration(t, ctx, pool)
	queries := db.New(pool)
	_, runLeaseID, workerID := seedRunningSessionLease(t, ctx, pool, ids)
	runWait, err := queries.CreateHotRunWait(ctx, db.CreateHotRunWaitParams{
		PublicID:         testWaitPublicID(t),
		ID:               pgvalue.UUID(uuid.Must(uuid.NewV7())),
		Kind:             db.RunWaitKindToken,
		OrgID:            pgvalue.UUID(ids.orgID),
		ProjectID:        pgvalue.UUID(ids.projectID),
		EnvironmentID:    pgvalue.UUID(ids.environmentID),
		RunID:            pgvalue.UUID(ids.runID),
		RunLeaseID:       pgvalue.UUID(runLeaseID),
		WorkerInstanceID: pgvalue.UUID(workerID),
		CheckpointDelay:  pgvalue.Interval(0),
	})
	if err != nil {
		t.Fatal(err)
	}
	first, err := queries.CreateDueLiveRuntimeCheckpointWaitCommandsForOrg(ctx, db.CreateDueLiveRuntimeCheckpointWaitCommandsForOrgParams{
		OrgID:      pgvalue.UUID(ids.orgID),
		CellID:     dbtest.DefaultCellID,
		LimitCount: 10,
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(first) != 1 || first[0].RunWaitID != runWait.ID {
		t.Fatalf("first checkpoint commands = %+v, want one for %s", first, pgvalue.UUIDString(runWait.ID))
	}
	if _, err := queries.AcknowledgeWorkerCommand(ctx, db.AcknowledgeWorkerCommandParams{
		ID:               first[0].ID,
		WorkerInstanceID: pgvalue.UUID(workerID),
		CellID:           dbtest.DefaultCellID,
	}); err != nil {
		t.Fatal(err)
	}

	second, err := queries.CreateDueLiveRuntimeCheckpointWaitCommandsForOrg(ctx, db.CreateDueLiveRuntimeCheckpointWaitCommandsForOrgParams{
		OrgID:      pgvalue.UUID(ids.orgID),
		CellID:     dbtest.DefaultCellID,
		LimitCount: 10,
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(second) != 1 || second[0].ID == first[0].ID || second[0].RunWaitID != runWait.ID {
		t.Fatalf("second checkpoint commands = %+v, want new command for %s after acknowledged failure", second, pgvalue.UUIDString(runWait.ID))
	}
}

func TestDueCheckpointWorkerCommandCanBeCreatedFromWorkerScope(t *testing.T) {
	ctx := context.Background()
	pool := newIntegrationDB(t, ctx)
	ids := seedIntegration(t, ctx, pool)
	queries := db.New(pool)
	_, runLeaseID, workerID := seedRunningSessionLease(t, ctx, pool, ids)
	runWait, err := queries.CreateHotRunWait(ctx, db.CreateHotRunWaitParams{
		PublicID:         testWaitPublicID(t),
		ID:               pgvalue.UUID(uuid.Must(uuid.NewV7())),
		Kind:             db.RunWaitKindToken,
		OrgID:            pgvalue.UUID(ids.orgID),
		ProjectID:        pgvalue.UUID(ids.projectID),
		EnvironmentID:    pgvalue.UUID(ids.environmentID),
		RunID:            pgvalue.UUID(ids.runID),
		RunLeaseID:       pgvalue.UUID(runLeaseID),
		WorkerInstanceID: pgvalue.UUID(workerID),
		CheckpointDelay:  pgvalue.Interval(0),
	})
	if err != nil {
		t.Fatal(err)
	}

	commands, err := queries.CreateDueLiveRuntimeCheckpointWaitCommandsForWorker(ctx, db.CreateDueLiveRuntimeCheckpointWaitCommandsForWorkerParams{
		WorkerInstanceID: pgvalue.UUID(workerID),
		LimitCount:       10,
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(commands) != 1 || commands[0].RunWaitID != runWait.ID || commands[0].WorkerInstanceID != pgvalue.UUID(workerID) {
		t.Fatalf("checkpoint commands = %+v, want one worker-scoped command for %s", commands, pgvalue.UUIDString(runWait.ID))
	}
}

func TestClaimWorkerCommandsKeepsRetryingNotifyAttempts(t *testing.T) {
	ctx := context.Background()
	pool := newIntegrationDB(t, ctx)
	ids := seedIntegration(t, ctx, pool)
	queries := db.New(pool)
	_, runLeaseID, workerID := seedRunningSessionLease(t, ctx, pool, ids)
	runWait, err := queries.CreateHotRunWait(ctx, db.CreateHotRunWaitParams{
		PublicID:         testWaitPublicID(t),
		ID:               pgvalue.UUID(uuid.Must(uuid.NewV7())),
		Kind:             db.RunWaitKindTimer,
		OrgID:            pgvalue.UUID(ids.orgID),
		ProjectID:        pgvalue.UUID(ids.projectID),
		EnvironmentID:    pgvalue.UUID(ids.environmentID),
		RunID:            pgvalue.UUID(ids.runID),
		RunLeaseID:       pgvalue.UUID(runLeaseID),
		WorkerInstanceID: pgvalue.UUID(workerID),
		CheckpointDelay:  pgvalue.Interval(5 * time.Second),
	})
	if err != nil {
		t.Fatal(err)
	}
	command, err := queries.CreateWorkerCommand(ctx, db.CreateWorkerCommandParams{
		OrgID:             pgvalue.UUID(ids.orgID),
		CellID:            dbtest.DefaultCellID,
		RouteGeneration:   1,
		ProjectID:         pgvalue.UUID(ids.projectID),
		EnvironmentID:     pgvalue.UUID(ids.environmentID),
		RunID:             pgvalue.UUID(ids.runID),
		RunWaitID:         runWait.ID,
		RunLeaseID:        pgvalue.UUID(runLeaseID),
		WorkerInstanceID:  pgvalue.UUID(workerID),
		RuntimeInstanceID: runWait.OwnerRuntimeInstanceID,
		RuntimeEpoch:      runWait.OwnerRuntimeEpoch,
		RunStateVersion:   runWait.OwnerRunStateVersion,
		Kind:              db.WorkerCommandKindRuntimeCheckpointWait,
		Payload:           []byte(`{"reason":"test"}`),
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `
		UPDATE worker_commands
		   SET delivery_attempts = 25,
		       delivery_locked_until = now() - interval '1 second'
		 WHERE id = $1
	`, command.ID); err != nil {
		t.Fatal(err)
	}

	claimed, err := queries.ClaimWorkerCommands(ctx, db.ClaimWorkerCommandsParams{
		CellID:        dbtest.DefaultCellID,
		RowLimit:      1,
		LeaseDuration: pgvalue.Interval(30 * time.Second),
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(claimed) != 1 || claimed[0].ID != command.ID || claimed[0].DeliveryAttempts != 26 {
		t.Fatalf("claimed = %+v, want command %d with incremented notify attempts", claimed, command.ID)
	}
}

func TestMarkWorkerCommandDeliveredRequiresCommandOwnerScope(t *testing.T) {
	ctx := context.Background()
	pool := newIntegrationDB(t, ctx)
	ids := seedIntegration(t, ctx, pool)
	queries := db.New(pool)
	_, runLeaseID, workerID := seedRunningSessionLease(t, ctx, pool, ids)
	command := seedWorkerCommandForScopeTest(t, ctx, queries, ids, runLeaseID, workerID)

	_, err := queries.MarkWorkerCommandDelivered(ctx, db.MarkWorkerCommandDeliveredParams{
		ID:               command.ID,
		OrgID:            pgvalue.UUID(ids.orgID),
		WorkerInstanceID: pgvalue.UUID(uuid.Must(uuid.NewV7())),
	})
	if !errors.Is(err, pgx.ErrNoRows) {
		t.Fatalf("mark delivered with wrong worker error = %v, want ErrNoRows", err)
	}
	_, err = queries.MarkWorkerCommandDelivered(ctx, db.MarkWorkerCommandDeliveredParams{
		ID:               command.ID,
		OrgID:            pgvalue.UUID(uuid.Must(uuid.NewV7())),
		WorkerInstanceID: pgvalue.UUID(workerID),
	})
	if !errors.Is(err, pgx.ErrNoRows) {
		t.Fatalf("mark delivered with wrong org error = %v, want ErrNoRows", err)
	}
	var deliveredAt pgtype.Timestamptz
	if err := pool.QueryRow(ctx, `
		SELECT delivered_at
		  FROM worker_commands
		 WHERE id = $1
	`, command.ID).Scan(&deliveredAt); err != nil {
		t.Fatal(err)
	}
	if deliveredAt.Valid {
		t.Fatalf("delivered_at = %v after wrong scope marks, want null", deliveredAt.Time)
	}

	delivered, err := queries.MarkWorkerCommandDelivered(ctx, db.MarkWorkerCommandDeliveredParams{
		ID:               command.ID,
		OrgID:            pgvalue.UUID(ids.orgID),
		WorkerInstanceID: pgvalue.UUID(workerID),
	})
	if err != nil {
		t.Fatal(err)
	}
	if !delivered.DeliveredAt.Valid {
		t.Fatal("delivered_at is null after correct scoped mark")
	}
}

func TestAcceptWorkerCommandRequiresCommandOwnerScope(t *testing.T) {
	ctx := context.Background()
	pool := newIntegrationDB(t, ctx)
	ids := seedIntegration(t, ctx, pool)
	queries := db.New(pool)
	_, runLeaseID, workerID := seedRunningSessionLease(t, ctx, pool, ids)
	command := seedWorkerCommandForScopeTest(t, ctx, queries, ids, runLeaseID, workerID)

	_, err := queries.AcceptWorkerCommand(ctx, db.AcceptWorkerCommandParams{
		ID:               command.ID,
		WorkerInstanceID: pgvalue.UUID(uuid.Must(uuid.NewV7())),
		CellID:           dbtest.DefaultCellID,
	})
	if !errors.Is(err, pgx.ErrNoRows) {
		t.Fatalf("accept with wrong worker error = %v, want ErrNoRows", err)
	}
	var acceptedAt pgtype.Timestamptz
	if err := pool.QueryRow(ctx, `
		SELECT accepted_at
		  FROM worker_commands
		 WHERE id = $1
	`, command.ID).Scan(&acceptedAt); err != nil {
		t.Fatal(err)
	}
	if acceptedAt.Valid {
		t.Fatalf("accepted_at = %v after wrong scope accept, want null", acceptedAt.Time)
	}

	accepted, err := queries.AcceptWorkerCommand(ctx, db.AcceptWorkerCommandParams{
		ID:               command.ID,
		WorkerInstanceID: pgvalue.UUID(workerID),
		CellID:           dbtest.DefaultCellID,
	})
	if err != nil {
		t.Fatal(err)
	}
	if !accepted.AcceptedAt.Valid || accepted.CompletedAt.Valid || accepted.AcknowledgedAt.Valid {
		t.Fatalf("accepted command lifecycle = accepted %+v completed %+v acked %+v", accepted.AcceptedAt, accepted.CompletedAt, accepted.AcknowledgedAt)
	}
}

func TestWorkerCommandQueriesRejectDisabledSourceRoute(t *testing.T) {
	ctx := context.Background()
	pool := newIntegrationDB(t, ctx)
	ids := seedIntegration(t, ctx, pool)
	queries := db.New(pool)
	_, runLeaseID, workerID := seedRunningSessionLease(t, ctx, pool, ids)
	command := seedWorkerCommandForScopeTest(t, ctx, queries, ids, runLeaseID, workerID)
	disableDefaultEnvironmentRoute(t, ctx, pool, ids)

	claimed, err := queries.ClaimWorkerCommands(ctx, db.ClaimWorkerCommandsParams{
		CellID:        dbtest.DefaultCellID,
		RowLimit:      10,
		LeaseDuration: pgvalue.Interval(time.Minute),
	})
	if err != nil {
		t.Fatal(err)
	}
	for _, row := range claimed {
		if row.ID == command.ID {
			t.Fatalf("ClaimWorkerCommands returned disabled-route command %+v", row)
		}
	}

	listed, err := queries.ListWorkerCommandsAfter(ctx, db.ListWorkerCommandsAfterParams{
		WorkerInstanceID: pgvalue.UUID(workerID),
		CellID:           dbtest.DefaultCellID,
		AfterID:          0,
		LimitCount:       10,
	})
	if err != nil {
		t.Fatal(err)
	}
	for _, row := range listed {
		if row.ID == command.ID {
			t.Fatalf("ListWorkerCommandsAfter returned disabled-route command %+v", row)
		}
	}

	_, err = queries.AcceptWorkerCommand(ctx, db.AcceptWorkerCommandParams{
		WorkerInstanceID: pgvalue.UUID(workerID),
		CellID:           dbtest.DefaultCellID,
		ID:               command.ID,
	})
	if !errors.Is(err, pgx.ErrNoRows) {
		t.Fatalf("AcceptWorkerCommand err = %v, want ErrNoRows", err)
	}
	_, err = queries.AcknowledgeWorkerCommand(ctx, db.AcknowledgeWorkerCommandParams{
		WorkerInstanceID: pgvalue.UUID(workerID),
		CellID:           dbtest.DefaultCellID,
		ID:               command.ID,
	})
	if !errors.Is(err, pgx.ErrNoRows) {
		t.Fatalf("AcknowledgeWorkerCommand err = %v, want ErrNoRows", err)
	}
	_, err = queries.AcknowledgeWorkerCommandForRunWait(ctx, db.AcknowledgeWorkerCommandForRunWaitParams{
		WorkerInstanceID: pgvalue.UUID(workerID),
		ID:               command.ID,
		OrgID:            pgvalue.UUID(ids.orgID),
		CellID:           dbtest.DefaultCellID,
		RunID:            pgvalue.UUID(ids.runID),
		RunWaitID:        command.RunWaitID,
		RunLeaseID:       pgvalue.UUID(runLeaseID),
		Kind:             command.Kind,
	})
	if !errors.Is(err, pgx.ErrNoRows) {
		t.Fatalf("AcknowledgeWorkerCommandForRunWait err = %v, want ErrNoRows", err)
	}

	var acceptedAt, acknowledgedAt pgtype.Timestamptz
	if err := pool.QueryRow(ctx, `
		SELECT accepted_at, acknowledged_at
		  FROM worker_commands
		 WHERE id = $1
	`, command.ID).Scan(&acceptedAt, &acknowledgedAt); err != nil {
		t.Fatal(err)
	}
	if acceptedAt.Valid || acknowledgedAt.Valid {
		t.Fatalf("stale-route command mutated: accepted=%v acknowledged=%v", acceptedAt.Time, acknowledgedAt.Time)
	}
}

func TestWorkerCommandTargetShapeRejectsPrepareRunTargets(t *testing.T) {
	ctx := context.Background()
	pool := newIntegrationDB(t, ctx)
	ids := seedIntegration(t, ctx, pool)
	queries := db.New(pool)
	_, runLeaseID, workerID := seedRunningSessionLease(t, ctx, pool, ids)
	runWait, err := queries.CreateHotRunWait(ctx, db.CreateHotRunWaitParams{
		PublicID:         testWaitPublicID(t),
		ID:               pgvalue.UUID(uuid.Must(uuid.NewV7())),
		Kind:             db.RunWaitKindTimer,
		OrgID:            pgvalue.UUID(ids.orgID),
		ProjectID:        pgvalue.UUID(ids.projectID),
		EnvironmentID:    pgvalue.UUID(ids.environmentID),
		RunID:            pgvalue.UUID(ids.runID),
		RunLeaseID:       pgvalue.UUID(runLeaseID),
		WorkerInstanceID: pgvalue.UUID(workerID),
		CheckpointDelay:  pgvalue.Interval(5 * time.Second),
	})
	if err != nil {
		t.Fatal(err)
	}

	_, err = queries.CreateWorkerCommand(ctx, db.CreateWorkerCommandParams{
		OrgID:             pgvalue.UUID(ids.orgID),
		CellID:            dbtest.DefaultCellID,
		RouteGeneration:   1,
		ProjectID:         pgvalue.UUID(ids.projectID),
		EnvironmentID:     pgvalue.UUID(ids.environmentID),
		RunID:             pgvalue.UUID(ids.runID),
		RunWaitID:         runWait.ID,
		RunLeaseID:        pgvalue.UUID(runLeaseID),
		WorkerInstanceID:  pgvalue.UUID(workerID),
		RuntimeInstanceID: runWait.OwnerRuntimeInstanceID,
		RuntimeEpoch:      runWait.OwnerRuntimeEpoch,
		RunStateVersion:   runWait.OwnerRunStateVersion,
		Kind:              db.WorkerCommandKindRuntimePrepare,
		Payload:           []byte(`{}`),
	})
	if err == nil {
		t.Fatal("runtime_prepare with run targets succeeded, want target shape violation")
	}

	command, err := queries.CreateWorkerCommand(ctx, db.CreateWorkerCommandParams{
		OrgID:             pgvalue.UUID(ids.orgID),
		CellID:            dbtest.DefaultCellID,
		RouteGeneration:   1,
		ProjectID:         pgvalue.UUID(ids.projectID),
		EnvironmentID:     pgvalue.UUID(ids.environmentID),
		WorkerInstanceID:  pgvalue.UUID(workerID),
		RuntimeInstanceID: runWait.OwnerRuntimeInstanceID,
		RuntimeEpoch:      runWait.OwnerRuntimeEpoch,
		Kind:              db.WorkerCommandKindRuntimePrepare,
		Payload:           []byte(`{}`),
	})
	if err != nil {
		t.Fatal(err)
	}
	if command.RunID.Valid || command.RunWaitID.Valid || command.RunLeaseID.Valid || command.RunStateVersion.Valid {
		t.Fatalf("prepare command carried run targets: %+v", command)
	}

	_, err = queries.CreateWorkerCommand(ctx, db.CreateWorkerCommandParams{
		OrgID:            pgvalue.UUID(ids.orgID),
		CellID:           dbtest.DefaultCellID,
		RouteGeneration:  1,
		ProjectID:        pgvalue.UUID(ids.projectID),
		EnvironmentID:    pgvalue.UUID(ids.environmentID),
		WorkerInstanceID: pgvalue.UUID(workerID),
		Kind:             db.WorkerCommandKindRuntimeSubstratePrepare,
		Payload:          []byte(`{}`),
	})
	if err == nil {
		t.Fatal("runtime_substrate_prepare without deployment_sandbox_id succeeded, want target shape violation")
	}

	substrateCommand, err := queries.CreateWorkerCommand(ctx, db.CreateWorkerCommandParams{
		OrgID:               pgvalue.UUID(ids.orgID),
		CellID:              dbtest.DefaultCellID,
		RouteGeneration:     1,
		ProjectID:           pgvalue.UUID(ids.projectID),
		EnvironmentID:       pgvalue.UUID(ids.environmentID),
		WorkerInstanceID:    pgvalue.UUID(workerID),
		DeploymentSandboxID: pgvalue.UUID(ids.deploymentSandboxID),
		Kind:                db.WorkerCommandKindRuntimeSubstratePrepare,
		Payload:             []byte(`{"deployment_sandbox_id":"` + ids.deploymentSandboxID.String() + `"}`),
	})
	if err != nil {
		t.Fatal(err)
	}
	if !substrateCommand.DeploymentSandboxID.Valid || pgvalue.MustUUIDValue(substrateCommand.DeploymentSandboxID) != ids.deploymentSandboxID {
		t.Fatalf("substrate deployment sandbox id = %+v, want %s", substrateCommand.DeploymentSandboxID, ids.deploymentSandboxID)
	}
}

func TestMarkWorkerCommandDeliveryFailedRequiresCommandOwnerScope(t *testing.T) {
	ctx := context.Background()
	pool := newIntegrationDB(t, ctx)
	ids := seedIntegration(t, ctx, pool)
	queries := db.New(pool)
	_, runLeaseID, workerID := seedRunningSessionLease(t, ctx, pool, ids)
	command := seedWorkerCommandForScopeTest(t, ctx, queries, ids, runLeaseID, workerID)

	if err := queries.MarkWorkerCommandDeliveryFailed(ctx, db.MarkWorkerCommandDeliveryFailedParams{
		ID:                command.ID,
		OrgID:             pgvalue.UUID(ids.orgID),
		WorkerInstanceID:  pgvalue.UUID(uuid.Must(uuid.NewV7())),
		LastDeliveryError: "wrong worker",
		RetryAfter:        pgvalue.Interval(time.Minute),
	}); err != nil {
		t.Fatal(err)
	}
	if err := queries.MarkWorkerCommandDeliveryFailed(ctx, db.MarkWorkerCommandDeliveryFailedParams{
		ID:                command.ID,
		OrgID:             pgvalue.UUID(uuid.Must(uuid.NewV7())),
		WorkerInstanceID:  pgvalue.UUID(workerID),
		LastDeliveryError: "wrong org",
		RetryAfter:        pgvalue.Interval(time.Minute),
	}); err != nil {
		t.Fatal(err)
	}
	var lockedUntil pgtype.Timestamptz
	var lastDeliveryError string
	if err := pool.QueryRow(ctx, `
		SELECT delivery_locked_until, last_delivery_error
		  FROM worker_commands
		 WHERE id = $1
	`, command.ID).Scan(&lockedUntil, &lastDeliveryError); err != nil {
		t.Fatal(err)
	}
	if lockedUntil.Valid || lastDeliveryError != "" {
		t.Fatalf("failed mark changed wrong-scope command: locked_until=%v last_error=%q", lockedUntil, lastDeliveryError)
	}

	if err := queries.MarkWorkerCommandDeliveryFailed(ctx, db.MarkWorkerCommandDeliveryFailedParams{
		ID:                command.ID,
		OrgID:             pgvalue.UUID(ids.orgID),
		WorkerInstanceID:  pgvalue.UUID(workerID),
		LastDeliveryError: "expected retry",
		RetryAfter:        pgvalue.Interval(time.Minute),
	}); err != nil {
		t.Fatal(err)
	}
	if err := pool.QueryRow(ctx, `
		SELECT delivery_locked_until, last_delivery_error
		  FROM worker_commands
		 WHERE id = $1
	`, command.ID).Scan(&lockedUntil, &lastDeliveryError); err != nil {
		t.Fatal(err)
	}
	if !lockedUntil.Valid || lastDeliveryError != "expected retry" {
		t.Fatalf("failed mark after correct scope: locked_until=%v last_error=%q", lockedUntil, lastDeliveryError)
	}
}

func seedWorkerCommandForScopeTest(t *testing.T, ctx context.Context, queries *db.Queries, ids integrationIDs, runLeaseID uuid.UUID, workerID uuid.UUID) db.WorkerCommand {
	t.Helper()
	runWait, err := queries.CreateHotRunWait(ctx, db.CreateHotRunWaitParams{
		PublicID:         testWaitPublicID(t),
		ID:               pgvalue.UUID(uuid.Must(uuid.NewV7())),
		Kind:             db.RunWaitKindTimer,
		OrgID:            pgvalue.UUID(ids.orgID),
		ProjectID:        pgvalue.UUID(ids.projectID),
		EnvironmentID:    pgvalue.UUID(ids.environmentID),
		RunID:            pgvalue.UUID(ids.runID),
		RunLeaseID:       pgvalue.UUID(runLeaseID),
		WorkerInstanceID: pgvalue.UUID(workerID),
		CheckpointDelay:  pgvalue.Interval(5 * time.Second),
	})
	if err != nil {
		t.Fatal(err)
	}
	command, err := queries.CreateWorkerCommand(ctx, db.CreateWorkerCommandParams{
		OrgID:             pgvalue.UUID(ids.orgID),
		CellID:            dbtest.DefaultCellID,
		RouteGeneration:   1,
		ProjectID:         pgvalue.UUID(ids.projectID),
		EnvironmentID:     pgvalue.UUID(ids.environmentID),
		RunID:             pgvalue.UUID(ids.runID),
		RunWaitID:         runWait.ID,
		RunLeaseID:        pgvalue.UUID(runLeaseID),
		WorkerInstanceID:  pgvalue.UUID(workerID),
		RuntimeInstanceID: runWait.OwnerRuntimeInstanceID,
		RuntimeEpoch:      runWait.OwnerRuntimeEpoch,
		RunStateVersion:   runWait.OwnerRunStateVersion,
		Kind:              db.WorkerCommandKindRuntimeCheckpointWait,
		Payload:           []byte(`{"reason":"scope-test"}`),
	})
	if err != nil {
		t.Fatal(err)
	}
	return command
}

func TestWorkerCommandCreationSkipsExistingCommandsBeforeLimit(t *testing.T) {
	ctx := context.Background()
	pool := newIntegrationDB(t, ctx)
	ids := seedIntegration(t, ctx, pool)
	queries := db.New(pool)
	_, runLeaseID, workerID := seedRunningSessionLease(t, ctx, pool, ids)

	createLiveWait := func() db.RunWait {
		t.Helper()
		runWait, err := queries.CreateHotRunWait(ctx, db.CreateHotRunWaitParams{
			PublicID:         testWaitPublicID(t),
			ID:               pgvalue.UUID(uuid.Must(uuid.NewV7())),
			Kind:             db.RunWaitKindTimer,
			OrgID:            pgvalue.UUID(ids.orgID),
			ProjectID:        pgvalue.UUID(ids.projectID),
			EnvironmentID:    pgvalue.UUID(ids.environmentID),
			RunID:            pgvalue.UUID(ids.runID),
			RunLeaseID:       pgvalue.UUID(runLeaseID),
			WorkerInstanceID: pgvalue.UUID(workerID),
			CheckpointDelay:  pgvalue.Interval(0),
		})
		if err != nil {
			t.Fatal(err)
		}
		return runWaitFromCreateHotRunWaitRow(runWait)
	}

	oldResumeWait := createLiveWait()
	newResumeWait := createLiveWait()
	if _, err := pool.Exec(ctx, `
		UPDATE run_waits
		   SET state = 'resolved_live',
		       resolved_at = CASE id
		           WHEN $2 THEN now() - interval '2 seconds'
		           ELSE now() - interval '1 second'
		       END
		 WHERE org_id = $1
		   AND id IN ($2, $3)
	`, ids.orgID, pgvalue.MustUUIDValue(oldResumeWait.ID), pgvalue.MustUUIDValue(newResumeWait.ID)); err != nil {
		t.Fatal(err)
	}
	if _, err := queries.CreateWorkerCommand(ctx, db.CreateWorkerCommandParams{
		OrgID:             pgvalue.UUID(ids.orgID),
		CellID:            dbtest.DefaultCellID,
		RouteGeneration:   1,
		ProjectID:         pgvalue.UUID(ids.projectID),
		EnvironmentID:     pgvalue.UUID(ids.environmentID),
		RunID:             pgvalue.UUID(ids.runID),
		RunWaitID:         oldResumeWait.ID,
		RunLeaseID:        pgvalue.UUID(runLeaseID),
		WorkerInstanceID:  pgvalue.UUID(workerID),
		RuntimeInstanceID: oldResumeWait.OwnerRuntimeInstanceID,
		RuntimeEpoch:      oldResumeWait.OwnerRuntimeEpoch,
		RunStateVersion:   oldResumeWait.OwnerRunStateVersion,
		Kind:              db.WorkerCommandKindRuntimeResumeWait,
		Payload:           []byte(`{"resume_kind":"completed","resume_payload":null}`),
	}); err != nil {
		t.Fatal(err)
	}
	resumeCommands, err := queries.CreateResolvedLiveRuntimeResumeWaitCommandsForOrg(ctx, db.CreateResolvedLiveRuntimeResumeWaitCommandsForOrgParams{
		OrgID:      pgvalue.UUID(ids.orgID),
		CellID:     dbtest.DefaultCellID,
		LimitCount: 1,
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(resumeCommands) != 1 || resumeCommands[0].RunWaitID != newResumeWait.ID {
		t.Fatalf("resume commands = %+v, want command for %s", resumeCommands, pgvalue.MustUUIDValue(newResumeWait.ID))
	}

	oldCheckpointWait := createLiveWait()
	newCheckpointWait := createLiveWait()
	if _, err := pool.Exec(ctx, `
		UPDATE run_waits
		   SET runtime_checkpoint_due_at = CASE id
		           WHEN $2 THEN now() - interval '2 seconds'
		           ELSE now() - interval '1 second'
		       END
		 WHERE org_id = $1
		   AND id IN ($2, $3)
	`, ids.orgID, pgvalue.MustUUIDValue(oldCheckpointWait.ID), pgvalue.MustUUIDValue(newCheckpointWait.ID)); err != nil {
		t.Fatal(err)
	}
	if _, err := queries.CreateWorkerCommand(ctx, db.CreateWorkerCommandParams{
		OrgID:             pgvalue.UUID(ids.orgID),
		CellID:            dbtest.DefaultCellID,
		RouteGeneration:   1,
		ProjectID:         pgvalue.UUID(ids.projectID),
		EnvironmentID:     pgvalue.UUID(ids.environmentID),
		RunID:             pgvalue.UUID(ids.runID),
		RunWaitID:         oldCheckpointWait.ID,
		RunLeaseID:        pgvalue.UUID(runLeaseID),
		WorkerInstanceID:  pgvalue.UUID(workerID),
		RuntimeInstanceID: oldCheckpointWait.OwnerRuntimeInstanceID,
		RuntimeEpoch:      oldCheckpointWait.OwnerRuntimeEpoch,
		RunStateVersion:   oldCheckpointWait.OwnerRunStateVersion,
		Kind:              db.WorkerCommandKindRuntimeCheckpointWait,
		Payload:           []byte(`{}`),
	}); err != nil {
		t.Fatal(err)
	}
	checkpointCommands, err := queries.CreateDueLiveRuntimeCheckpointWaitCommandsForOrg(ctx, db.CreateDueLiveRuntimeCheckpointWaitCommandsForOrgParams{
		OrgID:      pgvalue.UUID(ids.orgID),
		CellID:     dbtest.DefaultCellID,
		LimitCount: 1,
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(checkpointCommands) != 1 || checkpointCommands[0].RunWaitID != newCheckpointWait.ID {
		t.Fatalf("checkpoint commands = %+v, want command for %s", checkpointCommands, pgvalue.MustUUIDValue(newCheckpointWait.ID))
	}
}

func TestFailStaleResolvedRunWaitsTerminalizesWorkspaceVersionMismatch(t *testing.T) {
	ctx := context.Background()
	pool := newIntegrationDB(t, ctx)
	ids := seedIntegration(t, ctx, pool)
	queries := db.New(pool)
	_, runLeaseID, workerID := seedRunningSessionLease(t, ctx, pool, ids)
	seedActiveWorkspaceLeaseForRun(t, ctx, pool, ids)
	runWait, checkpointID := seedCheckpointingRunWait(t, ctx, queries, pool, ids, runLeaseID, workerID, db.RunWaitKindTimer)
	if _, err := queries.CreateReadyRuntimeCheckpointForRunWait(ctx, readyRuntimeCheckpointParamsForRun(t, ctx, pool, ids, runWait, runLeaseID, workerID, checkpointID)); err != nil {
		t.Fatal(err)
	}
	resolveCheckpointedRunWaitForTest(t, ctx, pool, ids, runWait)
	nextArtifactID := seedWorkspaceVersionArtifact(t, ctx, pool, ids)
	nextVersionID := uuid.Must(uuid.NewV7())
	if _, err := pool.Exec(ctx, `
		INSERT INTO workspace_versions (
			id, public_id, org_id, cell_id, project_id, environment_id, workspace_id, kind, state,
			artifact_id, artifact_encoding, artifact_entry_count, content_digest, size_bytes, promoted_at
		)
		SELECT $1, $8, $2, $3, $4, $5, $6, 'system', 'ready',
		       artifacts.id, 'tar', 0, artifacts.digest, artifacts.size_bytes, now()
		  FROM artifacts
		 WHERE artifacts.org_id = $2
		   AND artifacts.cell_id = $3
		   AND artifacts.project_id = $4
		   AND artifacts.environment_id = $5
		   AND artifacts.id = $7
	`, nextVersionID, ids.orgID, dbtest.DefaultCellID, ids.projectID, ids.environmentID, ids.workspaceID, nextArtifactID, testWorkspaceVersionPublicID(t)); err != nil {
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
		CellID:     dbtest.DefaultCellID,
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
		CellID:     dbtest.DefaultCellID,
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

func TestFailStaleResolvedRunWaitsTerminalizesResolvedCheckpointedMismatch(t *testing.T) {
	ctx := context.Background()
	pool := newIntegrationDB(t, ctx)
	ids := seedIntegration(t, ctx, pool)
	queries := db.New(pool)
	_, runLeaseID, workerID := seedRunningSessionLease(t, ctx, pool, ids)
	seedActiveWorkspaceLeaseForRun(t, ctx, pool, ids)
	runWait, checkpointID := seedCheckpointingRunWait(t, ctx, queries, pool, ids, runLeaseID, workerID, db.RunWaitKindTimer)
	if _, err := queries.CreateReadyRuntimeCheckpointForRunWait(ctx, readyRuntimeCheckpointParamsForRun(t, ctx, pool, ids, runWait, runLeaseID, workerID, checkpointID)); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `
		UPDATE run_waits
		   SET state = 'resolved_checkpointed',
		       resolved_at = now()
		 WHERE org_id = $1
		   AND id = $2
	`, ids.orgID, pgvalue.MustUUIDValue(runWait.ID)); err != nil {
		t.Fatal(err)
	}
	advanceWorkspaceCurrentVersion(t, ctx, pool, ids)

	failed, err := queries.FailStaleResolvedRunWaits(ctx, db.FailStaleResolvedRunWaitsParams{
		OrgID:      pgvalue.UUID(ids.orgID),
		CellID:     dbtest.DefaultCellID,
		LimitCount: 100,
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(failed) != 1 || failed[0].ID != runWait.ID || failed[0].State != db.RunWaitStateFailed {
		t.Fatalf("failed waits = %+v, want resolved_live_checkpointed wait failed", failed)
	}
	requeued, err := queries.RequeueResolvedRunWaits(ctx, db.RequeueResolvedRunWaitsParams{
		OrgID:      pgvalue.UUID(ids.orgID),
		CellID:     dbtest.DefaultCellID,
		LimitCount: 100,
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(requeued) != 0 {
		t.Fatalf("requeued stale waits = %+v, want none", requeued)
	}
}

func TestFailStaleResolvedRunWaitsTerminalizesQueuedWorkspaceVersionMismatch(t *testing.T) {
	ctx := context.Background()
	pool := newIntegrationDB(t, ctx)
	ids := seedIntegration(t, ctx, pool)
	queries := db.New(pool)
	_, runLeaseID, workerID := seedRunningSessionLease(t, ctx, pool, ids)
	seedActiveWorkspaceLeaseForRun(t, ctx, pool, ids)
	runWait, checkpointID := seedCheckpointingRunWait(t, ctx, queries, pool, ids, runLeaseID, workerID, db.RunWaitKindTimer)
	if _, err := queries.CreateReadyRuntimeCheckpointForRunWait(ctx, readyRuntimeCheckpointParamsForRun(t, ctx, pool, ids, runWait, runLeaseID, workerID, checkpointID)); err != nil {
		t.Fatal(err)
	}
	resolveCheckpointedRunWaitForTest(t, ctx, pool, ids, runWait)
	requeued, err := queries.RequeueResolvedRunWaits(ctx, db.RequeueResolvedRunWaitsParams{
		OrgID:      pgvalue.UUID(ids.orgID),
		CellID:     dbtest.DefaultCellID,
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
		CellID:     dbtest.DefaultCellID,
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
	seedActiveWorkspaceLeaseForRun(t, ctx, pool, ids)
	runWait, checkpointID := seedCheckpointingRunWait(t, ctx, queries, pool, ids, runLeaseID, workerID, db.RunWaitKindTimer)
	if _, err := queries.CreateReadyRuntimeCheckpointForRunWait(ctx, readyRuntimeCheckpointParamsForRun(t, ctx, pool, ids, runWait, runLeaseID, workerID, checkpointID)); err != nil {
		t.Fatal(err)
	}
	resolveCheckpointedRunWaitForTest(t, ctx, pool, ids, runWait)
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
		CellID:     dbtest.DefaultCellID,
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
		CellID:     dbtest.DefaultCellID,
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
	var snapshotReason []byte
	if err := pool.QueryRow(ctx, `
		SELECT reason
		  FROM run_snapshots
		 WHERE org_id = $1
		   AND run_id = $2
		   AND transition = 'run.failed'
		 ORDER BY version DESC
		 LIMIT 1
	`, ids.orgID, ids.runID).Scan(&snapshotReason); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(snapshotReason), "runtime_checkpoint_expired") || !strings.Contains(string(snapshotReason), checkpointID.String()) {
		t.Fatalf("run snapshot reason = %s, want expired checkpoint audit details", string(snapshotReason))
	}
}

func TestExpiredRunningLeaseCancelsParkingRunWait(t *testing.T) {
	ctx := context.Background()
	pool := newIntegrationDB(t, ctx)
	ids := seedIntegration(t, ctx, pool)
	queries := db.New(pool)
	_, runLeaseID, _ := seedRunningSessionLease(t, ctx, pool, ids)
	runWait := seedRunWait(t, ctx, pool, ids, db.RunWaitKindTimer)
	if runWait.State != db.RunWaitStateLiveWaiting {
		t.Fatalf("run wait state = %s, want live_waiting", runWait.State)
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

	if err := queries.FailExpiredRunningRunLeases(ctx, db.FailExpiredRunningRunLeasesParams{OrgID: pgvalue.UUID(ids.orgID), CellID: dbtest.DefaultCellID}); err != nil {
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
		  FROM usage_ledger_entries
		 WHERE org_id = $1
		   AND run_id = $2
		   AND meter = 'active_time'
	`, ids.orgID, ids.runID).Scan(&activeTimeUsageMs); err != nil {
		t.Fatal(err)
	}
	if activeTimeUsageMs != run.ActiveElapsedMs {
		t.Fatalf("active time usage ms = %d, want run active elapsed %d", activeTimeUsageMs, run.ActiveElapsedMs)
	}
}

func TestExpiredRunningLeaseCancelsCheckpointingRunWait(t *testing.T) {
	ctx := context.Background()
	pool := newIntegrationDB(t, ctx)
	ids := seedIntegration(t, ctx, pool)
	queries := db.New(pool)
	_, runLeaseID, workerID := seedRunningSessionLease(t, ctx, pool, ids)
	runWait, err := queries.CreateHotRunWait(ctx, db.CreateHotRunWaitParams{
		PublicID:         testWaitPublicID(t),
		ID:               pgvalue.UUID(uuid.Must(uuid.NewV7())),
		Kind:             db.RunWaitKindTimer,
		OrgID:            pgvalue.UUID(ids.orgID),
		ProjectID:        pgvalue.UUID(ids.projectID),
		EnvironmentID:    pgvalue.UUID(ids.environmentID),
		RunID:            pgvalue.UUID(ids.runID),
		RunLeaseID:       pgvalue.UUID(runLeaseID),
		WorkerInstanceID: pgvalue.UUID(workerID),
		CheckpointDelay:  pgvalue.Interval(0),
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `
		UPDATE run_waits
		   SET state = 'checkpointing',
		       runtime_checkpoint_started_at = now() - interval '2 seconds'
		 WHERE org_id = $1
		   AND id = $2
	`, ids.orgID, pgvalue.MustUUIDValue(runWait.ID)); err != nil {
		t.Fatal(err)
	}
	command, err := queries.CreateWorkerCommand(ctx, db.CreateWorkerCommandParams{
		OrgID:             pgvalue.UUID(ids.orgID),
		CellID:            dbtest.DefaultCellID,
		RouteGeneration:   1,
		ProjectID:         pgvalue.UUID(ids.projectID),
		EnvironmentID:     pgvalue.UUID(ids.environmentID),
		RunID:             pgvalue.UUID(ids.runID),
		RunWaitID:         runWait.ID,
		RunLeaseID:        pgvalue.UUID(runLeaseID),
		WorkerInstanceID:  pgvalue.UUID(workerID),
		RuntimeInstanceID: runWait.OwnerRuntimeInstanceID,
		RuntimeEpoch:      runWait.OwnerRuntimeEpoch,
		RunStateVersion:   runWait.OwnerRunStateVersion,
		Kind:              db.WorkerCommandKindRuntimeCheckpointWait,
		Payload:           []byte(`{"reason":"test"}`),
	})
	if err != nil {
		t.Fatal(err)
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

	if err := queries.FailExpiredRunningRunLeases(ctx, db.FailExpiredRunningRunLeasesParams{OrgID: pgvalue.UUID(ids.orgID), CellID: dbtest.DefaultCellID}); err != nil {
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
	var acknowledgedAt pgtype.Timestamptz
	if err := pool.QueryRow(ctx, `
		SELECT acknowledged_at
		  FROM worker_commands
		 WHERE id = $1
	`, command.ID).Scan(&acknowledgedAt); err != nil {
		t.Fatal(err)
	}
	if !acknowledgedAt.Valid {
		t.Fatal("worker command acknowledged_at is invalid, want cancelled wait cleanup to acknowledge command")
	}
}

func TestExpiredParkingLeaseMarksDirtyWorkspaceRecoveryRequired(t *testing.T) {
	ctx := context.Background()
	pool := newIntegrationDB(t, ctx)
	ids := seedIntegration(t, ctx, pool)
	queries := db.New(pool)
	_, runLeaseID, _ := seedRunningSessionLease(t, ctx, pool, ids)
	runWait := seedRunWait(t, ctx, pool, ids, db.RunWaitKindTimer)
	seedActiveWorkspaceLeaseForRun(t, ctx, pool, ids)
	if _, err := pool.Exec(ctx, `
		UPDATE workspace_mounts
		   SET dirty_generation = 1
		 WHERE org_id = $1
		   AND workspace_id = $2
		   AND state = 'mounted'
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
		UPDATE run_waits
		   SET state = 'checkpointing',
		       runtime_checkpoint_started_at = now(),
		       workspace_version_id = NULL,
		       updated_at = now()
		 WHERE org_id = $1
		   AND id = $2
	`, ids.orgID, pgvalue.MustUUIDValue(runWait.ID)); err != nil {
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

	if err := queries.FailExpiredRunningRunLeases(ctx, db.FailExpiredRunningRunLeasesParams{OrgID: pgvalue.UUID(ids.orgID), CellID: dbtest.DefaultCellID}); err != nil {
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
	runWait := seedRunWait(t, ctx, pool, ids, db.RunWaitKindTimer)
	seedActiveWorkspaceLeaseForRun(t, ctx, pool, ids)
	setRunWaitCurrentWorkspaceVersion(t, ctx, pool, ids, runWait)
	checkpointID := uuid.Must(uuid.NewV7())
	var workspaceLeaseID uuid.UUID
	var workspaceMountID uuid.UUID
	var currentVersionID uuid.UUID
	if err := pool.QueryRow(ctx, `
		SELECT workspace_leases.id, workspace_leases.workspace_mount_id, workspaces.current_version_id
		  FROM workspace_leases
		  JOIN workspaces
		    ON workspaces.org_id = workspace_leases.org_id
		   AND workspaces.id = workspace_leases.workspace_id
		 WHERE workspace_leases.org_id = $1
		   AND workspace_leases.workspace_id = $2
		   AND workspace_leases.owner_run_id = $3
		   AND workspace_leases.state = 'active'
	`, ids.orgID, ids.workspaceID, ids.runID).Scan(&workspaceLeaseID, &workspaceMountID, &currentVersionID); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `
		INSERT INTO runtime_checkpoints (
			id, org_id, cell_id, project_id, environment_id, workspace_id, run_id,
			source_workspace_lease_id, workspace_mount_id, base_workspace_version_id,
			state, runtime_backend, runtime_id, runtime_arch, runtime_abi, kernel_digest,
			initramfs_digest, rootfs_digest, runtime_config_digest, cni_profile, manifest
		)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10,
		        'creating', 'test', 'test-runtime', 'arm64', 'test-abi', 'sha256:kernel',
		        'sha256:initramfs', 'sha256:rootfs', 'sha256:config', 'test-cni', '{}')
	`, checkpointID, ids.orgID, dbtest.DefaultCellID, ids.projectID, ids.environmentID, ids.workspaceID, ids.runID, workspaceLeaseID, workspaceMountID, currentVersionID); err != nil {
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

	if err := queries.FailExpiredRunningRunLeases(ctx, db.FailExpiredRunningRunLeasesParams{OrgID: pgvalue.UUID(ids.orgID), CellID: dbtest.DefaultCellID}); err != nil {
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
	seedActiveWorkspaceLeaseForRun(t, ctx, pool, ids)
	runWait, checkpointID := seedCheckpointingRunWait(t, ctx, queries, pool, ids, runLeaseID, workerID, db.RunWaitKindTimer)
	if _, err := pool.Exec(ctx, `
		UPDATE runs
		   SET active_elapsed_ms = 500,
		       active_started_at = now() - interval '1 second'
		 WHERE org_id = $1
		   AND id = $2
	`, ids.orgID, ids.runID); err != nil {
		t.Fatal(err)
	}
	var workspaceVersionID uuid.UUID
	if err := pool.QueryRow(ctx, `
		SELECT current_version_id
		  FROM workspaces
		 WHERE org_id = $1
		   AND id = $2
	`, ids.orgID, ids.workspaceID).Scan(&workspaceVersionID); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `
		UPDATE runtime_instances
		   SET owner_workspace_id = $3,
		       owner_workspace_version_id = $4
		  FROM workspace_mounts
		 WHERE runtime_instances.org_id = workspace_mounts.org_id
		   AND runtime_instances.id = workspace_mounts.runtime_instance_id
		   AND workspace_mounts.org_id = $1
		   AND workspace_mounts.workspace_id = $2
	`, ids.orgID, ids.workspaceID, ids.workspaceID, workspaceVersionID); err != nil {
		t.Fatal(err)
	}
	markEnvironmentRouteDrainingWithStaleHealth(t, ctx, pool, ids)

	_, err := queries.CreateReadyRuntimeCheckpointForRunWait(ctx, readyRuntimeCheckpointParamsForRun(t, ctx, pool, ids, runWait, runLeaseID, workerID, checkpointID))
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
	var workspaceMountState db.WorkspaceMountState
	if err := pool.QueryRow(ctx, `
				SELECT state
				  FROM workspace_mounts
				 WHERE org_id = $1
				   AND workspace_id = $2
		`, ids.orgID, ids.workspaceID).Scan(&workspaceMountState); err != nil {
		t.Fatal(err)
	}
	if workspaceMountState != db.WorkspaceMountStateUnmounted {
		t.Fatalf("workspaceMount state=%s, want unmounted", workspaceMountState)
	}
	runtimeState, currentWaitID := runtimeStateForWorkspaceMount(t, ctx, pool, ids)
	if runtimeState != db.RuntimeInstanceStateClosed || currentWaitID != uuid.Nil {
		t.Fatalf("runtime after checkpoint ready = state %s wait %s, want closed/no wait", runtimeState, currentWaitID)
	}
	var ownerWorkspaceID pgtype.UUID
	var ownerWorkspaceVersionID pgtype.UUID
	if err := pool.QueryRow(ctx, `
		SELECT runtime_instances.owner_workspace_id,
		       runtime_instances.owner_workspace_version_id
		  FROM workspace_mounts
		  JOIN runtime_instances
		    ON runtime_instances.org_id = workspace_mounts.org_id
		   AND runtime_instances.id = workspace_mounts.runtime_instance_id
		 WHERE workspace_mounts.org_id = $1
		   AND workspace_mounts.workspace_id = $2
		 ORDER BY workspace_mounts.updated_at DESC
		 LIMIT 1
	`, ids.orgID, ids.workspaceID).Scan(&ownerWorkspaceID, &ownerWorkspaceVersionID); err != nil {
		t.Fatal(err)
	}
	if ownerWorkspaceID.Valid || ownerWorkspaceVersionID.Valid {
		t.Fatalf("closed checkpoint runtime owner workspace = %s/%s, want cleared", pgvalue.UUIDString(ownerWorkspaceID), pgvalue.UUIDString(ownerWorkspaceVersionID))
	}
}

func TestCreateRuntimeCheckpointArtifactRejectsWrongCellArtifact(t *testing.T) {
	ctx := context.Background()
	pool := newIntegrationDB(t, ctx)
	ids := seedIntegration(t, ctx, pool)
	queries := db.New(pool)
	_, runLeaseID, workerID := seedRunningSessionLease(t, ctx, pool, ids)
	seedActiveWorkspaceLeaseForRun(t, ctx, pool, ids)
	runWait, checkpointID := seedCheckpointingRunWait(t, ctx, queries, pool, ids, runLeaseID, workerID, db.RunWaitKindTimer)

	otherCellID := "us-east-1-cell-2"
	if _, err := pool.Exec(ctx, `
		INSERT INTO cells (id, region_id, environment_class)
		VALUES ($1, $2, $3)
		ON CONFLICT (id) DO NOTHING
	`, otherCellID, dbtest.DefaultRegionID, dbtest.DefaultEnvironmentClass); err != nil {
		t.Fatal(err)
	}
	digest := testDigest("wrong-cell-runtime-checkpoint-artifact")
	if _, err := queries.UpsertCasObject(ctx, db.UpsertCasObjectParams{
		OrgID:     pgvalue.UUID(ids.orgID),
		CellID:    otherCellID,
		Digest:    digest,
		SizeBytes: 256,
		MediaType: "application/vnd.helmr.runtime-checkpoint.config.v0+json",
	}); err != nil {
		t.Fatal(err)
	}
	artifact, err := queries.CreateArtifact(ctx, db.CreateArtifactParams{
		ID:              pgvalue.UUID(uuid.Must(uuid.NewV7())),
		OrgID:           pgvalue.UUID(ids.orgID),
		CellID:          otherCellID,
		RouteGeneration: 1,
		ProjectID:       pgvalue.UUID(ids.projectID),
		EnvironmentID:   pgvalue.UUID(ids.environmentID),
		Digest:          digest,
		Kind:            db.ArtifactKindRuntimeCheckpointConfig,
		SizeBytes:       256,
		MediaType:       "application/vnd.helmr.runtime-checkpoint.config.v0+json",
	})
	if err != nil {
		t.Fatal(err)
	}

	_, err = queries.CreateRuntimeCheckpointArtifact(ctx, db.CreateRuntimeCheckpointArtifactParams{
		Role:                db.RuntimeCheckpointArtifactRoleRuntimeConfig,
		Ordinal:             0,
		ArtifactID:          artifact.ID,
		Digest:              digest,
		OrgID:               pgvalue.UUID(ids.orgID),
		ProjectID:           pgvalue.UUID(ids.projectID),
		EnvironmentID:       pgvalue.UUID(ids.environmentID),
		RunID:               pgvalue.UUID(ids.runID),
		RuntimeCheckpointID: pgvalue.UUID(checkpointID),
	})
	if !errors.Is(err, pgx.ErrNoRows) {
		t.Fatalf("CreateRuntimeCheckpointArtifact wrong-cell artifact for wait %s error = %v, want pgx.ErrNoRows", pgvalue.UUIDString(runWait.ID), err)
	}
}

func TestFailRuntimeCheckpointAttemptRestoresLiveWaitAndInvalidatesCheckpoint(t *testing.T) {
	ctx := context.Background()
	pool := newIntegrationDB(t, ctx)
	ids := seedIntegration(t, ctx, pool)
	queries := db.New(pool)
	_, runLeaseID, workerID := seedRunningSessionLease(t, ctx, pool, ids)
	seedActiveWorkspaceLeaseForRun(t, ctx, pool, ids)
	runWait, checkpointID := seedCheckpointingRunWait(t, ctx, queries, pool, ids, runLeaseID, workerID, db.RunWaitKindTimer)
	workerCommandID := seedAcceptedRuntimeCheckpointWorkerCommand(t, ctx, pool, ids, runWait, runLeaseID, workerID)
	markEnvironmentRouteDrainingWithStaleHealth(t, ctx, pool, ids)

	failed, err := queries.FailRuntimeCheckpointAttempt(ctx, db.FailRuntimeCheckpointAttemptParams{
		OrgID:               pgvalue.UUID(ids.orgID),
		ProjectID:           pgvalue.UUID(ids.projectID),
		EnvironmentID:       pgvalue.UUID(ids.environmentID),
		RunID:               pgvalue.UUID(ids.runID),
		RunWaitID:           runWait.ID,
		RunLeaseID:          pgvalue.UUID(runLeaseID),
		WorkerInstanceID:    pgvalue.UUID(workerID),
		RuntimeCheckpointID: pgvalue.UUID(checkpointID),
		WorkerCommandID:     workerCommandID,
		ErrorMessage:        "snapshot failed",
	})
	if err != nil {
		t.Fatal(err)
	}
	if failed.State != db.RunWaitStateLiveWaiting || failed.RuntimeCheckpointID.Valid || failed.RuntimeCheckpointStartedAt.Valid || !failed.RuntimeCheckpointDueAt.Valid {
		t.Fatalf("failed checkpoint wait = state %s checkpoint %s started %+v due %+v, want live_waiting with checkpoint cleared and retry due", failed.State, pgvalue.UUIDString(failed.RuntimeCheckpointID), failed.RuntimeCheckpointStartedAt, failed.RuntimeCheckpointDueAt)
	}

	var checkpointState db.RuntimeCheckpointState
	var checkpointError string
	var invalidatedAt pgtype.Timestamptz
	if err := pool.QueryRow(ctx, `
		SELECT state, error_message, invalidated_at
		  FROM runtime_checkpoints
		 WHERE org_id = $1
		   AND id = $2
	`, ids.orgID, checkpointID).Scan(&checkpointState, &checkpointError, &invalidatedAt); err != nil {
		t.Fatal(err)
	}
	if checkpointState != db.RuntimeCheckpointStateInvalid || checkpointError != "snapshot failed" || !invalidatedAt.Valid {
		t.Fatalf("checkpoint after failure = state %s error %q invalidated %+v, want invalid snapshot failed", checkpointState, checkpointError, invalidatedAt)
	}
	runtimeState, currentWaitID := runtimeStateForRun(t, ctx, pool, ids)
	if runtimeState != db.RuntimeInstanceStateWaitingHot || currentWaitID != pgvalue.MustUUIDValue(runWait.ID) {
		t.Fatalf("runtime after checkpoint failure = state %s wait %s, want waiting_hot wait %s", runtimeState, currentWaitID, pgvalue.MustUUIDValue(runWait.ID))
	}
}

func TestFailRuntimeCheckpointAttemptRejectsAcknowledgedWorkerCommand(t *testing.T) {
	ctx := context.Background()
	pool := newIntegrationDB(t, ctx)
	ids := seedIntegration(t, ctx, pool)
	queries := db.New(pool)
	_, runLeaseID, workerID := seedRunningSessionLease(t, ctx, pool, ids)
	seedActiveWorkspaceLeaseForRun(t, ctx, pool, ids)
	runWait, checkpointID := seedCheckpointingRunWait(t, ctx, queries, pool, ids, runLeaseID, workerID, db.RunWaitKindTimer)
	workerCommandID := seedAcceptedRuntimeCheckpointWorkerCommand(t, ctx, pool, ids, runWait, runLeaseID, workerID)
	if _, err := pool.Exec(ctx, `
		UPDATE worker_commands
		   SET acknowledged_at = now(),
		       completed_at = now(),
		       updated_at = now()
		 WHERE id = $1
	`, workerCommandID); err != nil {
		t.Fatal(err)
	}

	_, err := queries.FailRuntimeCheckpointAttempt(ctx, db.FailRuntimeCheckpointAttemptParams{
		OrgID:               pgvalue.UUID(ids.orgID),
		ProjectID:           pgvalue.UUID(ids.projectID),
		EnvironmentID:       pgvalue.UUID(ids.environmentID),
		RunID:               pgvalue.UUID(ids.runID),
		RunWaitID:           runWait.ID,
		RunLeaseID:          pgvalue.UUID(runLeaseID),
		WorkerInstanceID:    pgvalue.UUID(workerID),
		RuntimeCheckpointID: pgvalue.UUID(checkpointID),
		WorkerCommandID:     workerCommandID,
		ErrorMessage:        "stale failure",
	})
	if !errors.Is(err, pgx.ErrNoRows) {
		t.Fatalf("FailRuntimeCheckpointAttempt error = %v, want no rows", err)
	}
	var waitState db.RunWaitState
	var checkpointState db.RuntimeCheckpointState
	if err := pool.QueryRow(ctx, `
		SELECT run_waits.state, runtime_checkpoints.state
		  FROM run_waits
		  JOIN runtime_checkpoints
		    ON runtime_checkpoints.org_id = run_waits.org_id
		   AND runtime_checkpoints.id = run_waits.runtime_checkpoint_id
		 WHERE run_waits.org_id = $1
		   AND run_waits.id = $2
	`, ids.orgID, runWait.ID).Scan(&waitState, &checkpointState); err != nil {
		t.Fatal(err)
	}
	if waitState != db.RunWaitStateCheckpointing || checkpointState != db.RuntimeCheckpointStateCreating {
		t.Fatalf("state after acknowledged command failure = wait %s checkpoint %s, want checkpointing/creating", waitState, checkpointState)
	}
}

func TestCreateReadyRuntimeCheckpointRejectsSharedActiveWorkspaceMount(t *testing.T) {
	ctx := context.Background()
	pool := newIntegrationDB(t, ctx)
	ids := seedIntegration(t, ctx, pool)
	queries := db.New(pool)
	_, runLeaseID, workerID := seedRunningSessionLease(t, ctx, pool, ids)
	seedActiveWorkspaceLeaseForRun(t, ctx, pool, ids)
	runWait, checkpointID := seedCheckpointingRunWait(t, ctx, queries, pool, ids, runLeaseID, workerID, db.RunWaitKindTimer)
	var workspaceMountID uuid.UUID
	if err := pool.QueryRow(ctx, `
		SELECT id
		  FROM workspace_mounts
		 WHERE org_id = $1
		   AND workspace_id = $2
	`, ids.orgID, ids.workspaceID).Scan(&workspaceMountID); err != nil {
		t.Fatal(err)
	}
	execID := uuid.Must(uuid.NewV7())
	if _, err := pool.Exec(ctx, `
		INSERT INTO workspace_execs (
			id, org_id, cell_id, project_id, environment_id, workspace_id, workspace_mount_id,
			command, state, detached, created_by_subject_type, created_by_subject_id
		)
		SELECT $1, org_id, cell_id, project_id, environment_id, id, $2,
		       '["true"]'::jsonb, 'running', true, 'test', 'test'
		  FROM workspaces
		 WHERE org_id = $3
		   AND id = $4
	`, execID, workspaceMountID, ids.orgID, ids.workspaceID); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `
		INSERT INTO workspace_leases (
			id, org_id, cell_id, project_id, environment_id, workspace_id, workspace_mount_id,
			lease_kind, state, owner_exec_id, base_version_id, acquired_version_id,
			acquired_fencing_generation, fencing_token, expires_at
		)
		SELECT $1, org_id, cell_id, project_id, environment_id, id, $2,
		       'instance', 'active', $3, current_version_id, current_version_id,
		       1, 'exec-instance-token', now() + interval '1 hour'
		  FROM workspaces
		 WHERE org_id = $4
		   AND id = $5
	`, uuid.Must(uuid.NewV7()), workspaceMountID, execID, ids.orgID, ids.workspaceID); err != nil {
		t.Fatal(err)
	}

	_, err := queries.CreateReadyRuntimeCheckpointForRunWait(ctx, readyRuntimeCheckpointParamsForRun(t, ctx, pool, ids, runWait, runLeaseID, workerID, checkpointID))
	if !errors.Is(err, pgx.ErrNoRows) {
		t.Fatalf("err = %v, want no rows for shared active workspaceMount", err)
	}
}

func TestCreateReadyRuntimeCheckpointForRunWaitRejectsDirtyCleanPath(t *testing.T) {
	ctx := context.Background()
	pool := newIntegrationDB(t, ctx)
	ids := seedIntegration(t, ctx, pool)
	queries := db.New(pool)
	_, runLeaseID, workerID := seedRunningSessionLease(t, ctx, pool, ids)
	seedActiveWorkspaceLeaseForRun(t, ctx, pool, ids)
	runWait, checkpointID := seedCheckpointingRunWait(t, ctx, queries, pool, ids, runLeaseID, workerID, db.RunWaitKindTimer)
	if _, err := pool.Exec(ctx, `
		UPDATE workspace_mounts
		   SET dirty_generation = 1
		 WHERE org_id = $1
		   AND workspace_id = $2
		   AND state = 'mounted'
	`, ids.orgID, ids.workspaceID); err != nil {
		t.Fatal(err)
	}

	_, err := queries.CreateReadyRuntimeCheckpointForRunWait(ctx, readyRuntimeCheckpointParamsForRun(t, ctx, pool, ids, runWait, runLeaseID, workerID, checkpointID))
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
	if storedWait.State != db.RunWaitStateCheckpointing {
		t.Fatalf("run wait state = %s, want checkpointing after rejected checkpoint", storedWait.State)
	}
}

func TestDirtyRunWaitCapturePromotesSystemVersionBeforeCheckpointReady(t *testing.T) {
	ctx := context.Background()
	pool := newIntegrationDB(t, ctx)
	ids := seedIntegration(t, ctx, pool)
	queries := db.New(pool)
	_, runLeaseID, workerID := seedRunningSessionLease(t, ctx, pool, ids)
	seedActiveWorkspaceLeaseForRun(t, ctx, pool, ids)
	hotWait, err := queries.CreateHotRunWait(ctx, db.CreateHotRunWaitParams{
		PublicID:         testWaitPublicID(t),
		ID:               pgvalue.UUID(uuid.Must(uuid.NewV7())),
		Kind:             db.RunWaitKindTimer,
		OrgID:            pgvalue.UUID(ids.orgID),
		ProjectID:        pgvalue.UUID(ids.projectID),
		EnvironmentID:    pgvalue.UUID(ids.environmentID),
		RunID:            pgvalue.UUID(ids.runID),
		RunLeaseID:       pgvalue.UUID(runLeaseID),
		WorkerInstanceID: pgvalue.UUID(workerID),
		CheckpointDelay:  pgvalue.Interval(0),
	})
	if err != nil {
		t.Fatal(err)
	}
	runWait := runWaitFromCreateHotRunWaitRow(hotWait)
	if _, err := pool.Exec(ctx, `
		UPDATE workspace_mounts
		   SET dirty_generation = 1
		 WHERE org_id = $1
		   AND workspace_id = $2
		   AND state = 'mounted'
	`, ids.orgID, ids.workspaceID); err != nil {
		t.Fatal(err)
	}
	captureArtifactID := uuid.Must(uuid.NewV7())
	captureVersionID := uuid.Must(uuid.NewV7())
	captureDigest := "sha256:" + strings.Repeat("b", 64)
	if _, err := pool.Exec(ctx, `
		INSERT INTO cas_objects (org_id, cell_id, digest, size_bytes, media_type)
		VALUES ($1, $2, $3, 42, 'application/vnd.helmr.workspace.v0.tar')
	`, ids.orgID, dbtest.DefaultCellID, captureDigest); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `
		INSERT INTO artifacts (id, org_id, cell_id, project_id, environment_id, digest, kind, size_bytes, media_type)
		VALUES ($1, $2, $3, $4, $5, $6, 'workspace_version', 42, 'application/vnd.helmr.workspace.v0.tar')
	`, captureArtifactID, ids.orgID, dbtest.DefaultCellID, ids.projectID, ids.environmentID, captureDigest); err != nil {
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
		VersionPublicID:    testWorkspaceVersionPublicID(t),
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
	checkpointID := uuid.Must(uuid.NewV7())
	claimRuntimeCheckpointWaitForTest(t, ctx, pool, queries, ids, runWait, runLeaseID, workerID, checkpointID)
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
	_, err = queries.CreateReadyRuntimeCheckpointForRunWait(ctx, readyRuntimeCheckpointParamsForRun(t, ctx, pool, ids, runWait, runLeaseID, workerID, checkpointID))
	if err != nil {
		t.Fatal(err)
	}
	var currentVersionID uuid.UUID
	var dirtyGeneration int64
	if err := pool.QueryRow(ctx, `
		SELECT workspaces.current_version_id, workspace_mounts.dirty_generation
		  FROM workspaces
		  JOIN workspace_mounts
		    ON workspace_mounts.org_id = workspaces.org_id
		   AND workspace_mounts.workspace_id = workspaces.id
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
	if storedWait.State != db.RunWaitStateCheckpointedWaiting || pgvalue.MustUUIDValue(storedWait.WorkspaceVersionID) != captureVersionID || !storedWait.RuntimeCheckpointID.Valid {
		t.Fatalf("run wait = %+v, want checkpointed_waiting captured version and checkpoint", storedWait)
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
	seedActiveWorkspaceLeaseForRun(t, ctx, pool, ids)
	runWait, checkpointID := seedCheckpointingRunWait(t, ctx, queries, pool, ids, runLeaseID, workerID, db.RunWaitKindTimer)
	if _, err := pool.Exec(ctx, `
		UPDATE runs
		   SET active_elapsed_ms = 500,
		       active_started_at = now() + interval '2 seconds'
		 WHERE org_id = $1
		   AND id = $2
	`, ids.orgID, ids.runID); err != nil {
		t.Fatal(err)
	}

	_, err := queries.CreateReadyRuntimeCheckpointForRunWait(ctx, readyRuntimeCheckpointParamsForRun(t, ctx, pool, ids, runWait, runLeaseID, workerID, checkpointID))
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
		PublicID:               testStreamRecordPublicID(t),
		ID:                     pgvalue.UUID(uuid.Must(uuid.NewV7())),
		OrgID:                  pgvalue.UUID(ids.orgID),
		CellID:                 dbtest.DefaultCellID,
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
		PublicID:               testStreamRecordPublicID(t),
		ID:                     pgvalue.UUID(uuid.Must(uuid.NewV7())),
		OrgID:                  pgvalue.UUID(ids.orgID),
		CellID:                 dbtest.DefaultCellID,
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
		PublicID:               testStreamRecordPublicID(t),
		ID:                     pgvalue.UUID(uuid.Must(uuid.NewV7())),
		OrgID:                  pgvalue.UUID(ids.orgID),
		CellID:                 dbtest.DefaultCellID,
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
		CellID:        dbtest.DefaultCellID,
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
		CellID:        dbtest.DefaultCellID,
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
		PublicID:               testStreamRecordPublicID(t),
		ID:                     pgvalue.UUID(uuid.Must(uuid.NewV7())),
		OrgID:                  pgvalue.UUID(ids.orgID),
		CellID:                 dbtest.DefaultCellID,
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
	for i := range appendCount {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			data := []byte(`{"index":` + string(rune('0'+i%10)) + `}`)
			row, err := queries.AppendStreamRecord(ctx, db.AppendStreamRecordParams{
				PublicID:               testStreamRecordPublicID(t),
				ID:                     pgvalue.UUID(uuid.Must(uuid.NewV7())),
				OrgID:                  pgvalue.UUID(ids.orgID),
				CellID:                 dbtest.DefaultCellID,
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
	slices.Sort(sequences)
	for i, sequence := range sequences {
		want := int64(i + 1)
		if sequence != want {
			t.Fatalf("sequences = %+v, want contiguous from 1", sequences)
		}
	}
	stored, err := queries.GetStream(ctx, db.GetStreamParams{
		OrgID:         pgvalue.UUID(ids.orgID),
		CellID:        dbtest.DefaultCellID,
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
		PublicID:               testStreamRecordPublicID(t),
		ID:                     pgvalue.UUID(uuid.Must(uuid.NewV7())),
		OrgID:                  pgvalue.UUID(ids.orgID),
		CellID:                 dbtest.DefaultCellID,
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
	runWait := seedRunWait(t, ctx, pool, ids, db.RunWaitKindStream)
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
		CellID:        dbtest.DefaultCellID,
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
	markRunWaitLiveWaiting(t, ctx, pool, ids, runWait)
	resolved, err = queries.ResolveStreamWaitsForStream(ctx, db.ResolveStreamWaitsForStreamParams{
		OrgID:         pgvalue.UUID(ids.orgID),
		CellID:        dbtest.DefaultCellID,
		ProjectID:     pgvalue.UUID(ids.projectID),
		EnvironmentID: pgvalue.UUID(ids.environmentID),
		StreamID:      stream.ID,
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(resolved) != 1 || resolved[0].RecordID != record.ID || resolved[0].RunWaitID != runWait.ID {
		t.Fatalf("resolved = %+v, want checkpointed_waiting run wait %v matched to record %v", resolved, runWait.ID, record.ID)
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
	if storedWait.State != db.RunWaitStateResolvedLive {
		t.Fatalf("run wait state = %s, want resolved_live", storedWait.State)
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
			id, public_id, org_id, cell_id, project_id, environment_id, deployment_id, deployment_task_id, workspace_id, task_id,
			session_id, status, execution_status, payload, queue_name, max_active_duration_ms, trace_id, root_span_id
		)
		VALUES ($1, $10, $2, $3, $4, $5, $6, $7, $8, 'approval-task', $9, 'waiting', 'waiting', '{}', 'default', 300000,
			'11111111111111111111111111111111', '2222222222222222')
	`, secondRunID, ids.orgID, dbtest.DefaultCellID, ids.projectID, ids.environmentID, ids.deploymentID, ids.taskID, ids.workspaceID, sessionID, testRunPublicID(t)); err != nil {
		t.Fatal(err)
	}
	secondIDs := ids
	secondIDs.runID = secondRunID
	stream := seedSessionStream(t, ctx, queries, ids, db.StreamDirectionInput, "approval")
	firstWait := seedRunWait(t, ctx, pool, ids, db.RunWaitKindStream)
	secondWait := seedRunWait(t, ctx, pool, secondIDs, db.RunWaitKindStream)
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
		PublicID:               testStreamRecordPublicID(t),
		ID:                     pgvalue.UUID(uuid.Must(uuid.NewV7())),
		OrgID:                  pgvalue.UUID(ids.orgID),
		CellID:                 dbtest.DefaultCellID,
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
		CellID:        dbtest.DefaultCellID,
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
			id, public_id, org_id, cell_id, project_id, environment_id, deployment_id, deployment_task_id, workspace_id, task_id,
			session_id, status, execution_status, payload, queue_name, max_active_duration_ms, trace_id, root_span_id
		)
		VALUES ($1, $10, $2, $3, $4, $5, $6, $7, $8, 'approval-task', $9, 'waiting', 'waiting', '{}', 'default', 300000,
			'11111111111111111111111111111111', '2222222222222222')
	`, secondRunID, ids.orgID, dbtest.DefaultCellID, ids.projectID, ids.environmentID, ids.deploymentID, ids.taskID, ids.workspaceID, sessionID, testRunPublicID(t)); err != nil {
		t.Fatal(err)
	}
	secondIDs := ids
	secondIDs.runID = secondRunID
	stream := seedSessionStream(t, ctx, queries, ids, db.StreamDirectionInput, "approval")
	firstWait := seedRunWait(t, ctx, pool, ids, db.RunWaitKindStream)
	secondWait := seedRunWait(t, ctx, pool, secondIDs, db.RunWaitKindStream)
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
		PublicID:               testStreamRecordPublicID(t),
		ID:                     pgvalue.UUID(uuid.Must(uuid.NewV7())),
		OrgID:                  pgvalue.UUID(ids.orgID),
		CellID:                 dbtest.DefaultCellID,
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
		CellID:        dbtest.DefaultCellID,
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
	if storedSecond.State != db.RunWaitStateCheckpointedWaiting {
		t.Fatalf("second wait state = %s, want checkpointed_waiting", storedSecond.State)
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
		PublicID:               testStreamRecordPublicID(t),
		ID:                     pgvalue.UUID(uuid.Must(uuid.NewV7())),
		OrgID:                  pgvalue.UUID(ids.orgID),
		CellID:                 dbtest.DefaultCellID,
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
	runWait := seedRunWait(t, ctx, pool, ids, db.RunWaitKindStream)
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
		CellID:        dbtest.DefaultCellID,
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
		PublicID:               testStreamRecordPublicID(t),
		ID:                     pgvalue.UUID(uuid.Must(uuid.NewV7())),
		OrgID:                  pgvalue.UUID(ids.orgID),
		CellID:                 dbtest.DefaultCellID,
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
		CellID:        dbtest.DefaultCellID,
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
		PublicID:               testStreamRecordPublicID(t),
		ID:                     pgvalue.UUID(uuid.Must(uuid.NewV7())),
		OrgID:                  pgvalue.UUID(ids.orgID),
		CellID:                 dbtest.DefaultCellID,
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
		PublicID:               testStreamRecordPublicID(t),
		ID:                     pgvalue.UUID(uuid.Must(uuid.NewV7())),
		OrgID:                  pgvalue.UUID(ids.orgID),
		CellID:                 dbtest.DefaultCellID,
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
	runWait := seedRunWait(t, ctx, pool, ids, db.RunWaitKindStream)
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
		CellID:        dbtest.DefaultCellID,
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

	threadAWait := seedRunWait(t, ctx, pool, ids, db.RunWaitKindStream)
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
	threadBWait := seedRunWait(t, ctx, pool, ids, db.RunWaitKindStream)
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
		PublicID:               testStreamRecordPublicID(t),
		ID:                     pgvalue.UUID(uuid.Must(uuid.NewV7())),
		OrgID:                  pgvalue.UUID(ids.orgID),
		CellID:                 dbtest.DefaultCellID,
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
		CellID:        dbtest.DefaultCellID,
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
	if storedThreadA.State != db.RunWaitStateCheckpointedWaiting {
		t.Fatalf("thread-a wait state = %s, want checkpointed_waiting", storedThreadA.State)
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
		CellID:        dbtest.DefaultCellID,
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
		CellID:        dbtest.DefaultCellID,
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
		PublicID:                  testTokenPublicID(t),
		ID:                        pgvalue.UUID(tokenID),
		OrgID:                     pgvalue.UUID(ids.orgID),
		CellID:                    dbtest.DefaultCellID,
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
		PublicID:                 testTokenPublicID(t),
		ID:                       pgvalue.UUID(uuid.Must(uuid.NewV7())),
		OrgID:                    pgvalue.UUID(ids.orgID),
		CellID:                   dbtest.DefaultCellID,
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
		PublicID:                 testTokenPublicID(t),
		ID:                       pgvalue.UUID(uuid.Must(uuid.NewV7())),
		OrgID:                    pgvalue.UUID(ids.orgID),
		CellID:                   dbtest.DefaultCellID,
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
	runWait := seedRunWait(t, ctx, pool, ids, db.RunWaitKindToken)
	if _, err := queries.CreateTokenWait(ctx, db.CreateTokenWaitParams{
		ID:            pgvalue.UUID(uuid.Must(uuid.NewV7())),
		OrgID:         pgvalue.UUID(ids.orgID),
		CellID:        dbtest.DefaultCellID,
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
		CellID:                dbtest.DefaultCellID,
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
		CellID:                dbtest.DefaultCellID,
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
		CellID:                dbtest.DefaultCellID,
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

func TestTokenQueriesRejectWrongCell(t *testing.T) {
	ctx := context.Background()
	pool := newIntegrationDB(t, ctx)
	ids := seedIntegration(t, ctx, pool)
	queries := db.New(pool)
	wrongCellID := "wrong-cell"
	token, err := queries.CreateToken(ctx, db.CreateTokenParams{
		PublicID:                 testTokenPublicID(t),
		ID:                       pgvalue.UUID(uuid.Must(uuid.NewV7())),
		OrgID:                    pgvalue.UUID(ids.orgID),
		CellID:                   dbtest.DefaultCellID,
		ProjectID:                pgvalue.UUID(ids.projectID),
		EnvironmentID:            pgvalue.UUID(ids.environmentID),
		TimeoutAt:                pgvalue.Timestamptz(time.Now().Add(time.Hour)),
		CreateRequestFingerprint: "wrong-cell-token",
		Metadata:                 []byte(`{}`),
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := queries.GetToken(ctx, db.GetTokenParams{
		OrgID:         pgvalue.UUID(ids.orgID),
		CellID:        wrongCellID,
		ProjectID:     pgvalue.UUID(ids.projectID),
		EnvironmentID: pgvalue.UUID(ids.environmentID),
		ID:            token.ID,
	}); !errors.Is(err, pgx.ErrNoRows) {
		t.Fatalf("GetToken wrong cell err = %v, want no rows", err)
	}
	listed, err := queries.ListTokens(ctx, db.ListTokensParams{
		OrgID:         pgvalue.UUID(ids.orgID),
		CellID:        wrongCellID,
		ProjectID:     pgvalue.UUID(ids.projectID),
		EnvironmentID: pgvalue.UUID(ids.environmentID),
		LimitCount:    10,
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(listed) != 0 {
		t.Fatalf("ListTokens wrong cell returned %d rows", len(listed))
	}
	runWait := seedRunWait(t, ctx, pool, ids, db.RunWaitKindToken)
	if _, err := queries.CreateTokenWait(ctx, db.CreateTokenWaitParams{
		ID:            pgvalue.UUID(uuid.Must(uuid.NewV7())),
		OrgID:         pgvalue.UUID(ids.orgID),
		CellID:        wrongCellID,
		ProjectID:     pgvalue.UUID(ids.projectID),
		EnvironmentID: pgvalue.UUID(ids.environmentID),
		RunWaitID:     runWait.ID,
		TokenID:       token.ID,
	}); !errors.Is(err, pgx.ErrNoRows) {
		t.Fatalf("CreateTokenWait wrong cell err = %v, want no rows", err)
	}
	if _, err := queries.CompleteToken(ctx, db.CompleteTokenParams{
		OrgID:                 pgvalue.UUID(ids.orgID),
		CellID:                wrongCellID,
		ProjectID:             pgvalue.UUID(ids.projectID),
		EnvironmentID:         pgvalue.UUID(ids.environmentID),
		ID:                    token.ID,
		CompletionData:        []byte(`{"ok":true}`),
		CompletionContentType: "application/json",
		CompletionFingerprint: canonicalFingerprint(t, []byte(`{"ok":true}`)),
	}); !errors.Is(err, pgx.ErrNoRows) {
		t.Fatalf("CompleteToken wrong cell err = %v, want no rows", err)
	}
	if _, err := queries.CancelToken(ctx, db.CancelTokenParams{
		OrgID:         pgvalue.UUID(ids.orgID),
		CellID:        wrongCellID,
		ProjectID:     pgvalue.UUID(ids.projectID),
		EnvironmentID: pgvalue.UUID(ids.environmentID),
		ID:            token.ID,
	}); !errors.Is(err, pgx.ErrNoRows) {
		t.Fatalf("CancelToken wrong cell err = %v, want no rows", err)
	}
	publicToken, err := queries.CreatePublicAccessToken(ctx, db.CreatePublicAccessTokenParams{
		PublicID:      testPublicAccessTokenPublicID(t),
		ID:            pgvalue.UUID(uuid.Must(uuid.NewV7())),
		OrgID:         pgvalue.UUID(ids.orgID),
		CellID:        dbtest.DefaultCellID,
		ProjectID:     pgvalue.UUID(ids.projectID),
		EnvironmentID: pgvalue.UUID(ids.environmentID),
		TokenHash:     []byte("wrong-cell-public-token-hash"),
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
		CellID:              dbtest.DefaultCellID,
		ProjectID:           pgvalue.UUID(ids.projectID),
		EnvironmentID:       pgvalue.UUID(ids.environmentID),
		PublicAccessTokenID: publicToken.ID,
		ScopeType:           db.PublicAccessTokenScopeTypeTokencomplete,
		TokenID:             token.ID,
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := queries.GetPublicAccessTokenTokenScope(ctx, db.GetPublicAccessTokenTokenScopeParams{
		OrgID:               pgvalue.UUID(ids.orgID),
		CellID:              wrongCellID,
		ProjectID:           pgvalue.UUID(ids.projectID),
		EnvironmentID:       pgvalue.UUID(ids.environmentID),
		PublicAccessTokenID: publicToken.ID,
		TokenID:             token.ID,
	}); !errors.Is(err, pgx.ErrNoRows) {
		t.Fatalf("GetPublicAccessTokenTokenScope wrong cell err = %v, want no rows", err)
	}
	inputStream := seedSessionStream(t, ctx, queries, ids, db.StreamDirectionInput, "wrong-cell-input")
	if _, err := queries.CreatePublicAccessTokenScope(ctx, db.CreatePublicAccessTokenScopeParams{
		ID:                  pgvalue.UUID(uuid.Must(uuid.NewV7())),
		OrgID:               pgvalue.UUID(ids.orgID),
		CellID:              dbtest.DefaultCellID,
		ProjectID:           pgvalue.UUID(ids.projectID),
		EnvironmentID:       pgvalue.UUID(ids.environmentID),
		PublicAccessTokenID: publicToken.ID,
		ScopeType:           db.PublicAccessTokenScopeTypeSessioninputsend,
		StreamID:            inputStream.ID,
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := queries.GetPublicAccessTokenStreamScope(ctx, db.GetPublicAccessTokenStreamScopeParams{
		OrgID:               pgvalue.UUID(ids.orgID),
		CellID:              wrongCellID,
		ProjectID:           pgvalue.UUID(ids.projectID),
		EnvironmentID:       pgvalue.UUID(ids.environmentID),
		PublicAccessTokenID: publicToken.ID,
		ScopeType:           db.PublicAccessTokenScopeTypeSessioninputsend,
		StreamID:            inputStream.ID,
	}); !errors.Is(err, pgx.ErrNoRows) {
		t.Fatalf("GetPublicAccessTokenStreamScope wrong cell err = %v, want no rows", err)
	}
	if _, err := queries.ConsumePublicAccessToken(ctx, db.ConsumePublicAccessTokenParams{
		OrgID:  pgvalue.UUID(ids.orgID),
		CellID: wrongCellID,
		ID:     publicToken.ID,
	}); !errors.Is(err, pgx.ErrNoRows) {
		t.Fatalf("ConsumePublicAccessToken wrong cell err = %v, want no rows", err)
	}
}

func TestConcurrentTokenCompletionIsFirstResolveWins(t *testing.T) {
	ctx := context.Background()
	pool := newIntegrationDB(t, ctx)
	ids := seedIntegration(t, ctx, pool)
	queries := db.New(pool)
	token, err := queries.CreateToken(ctx, db.CreateTokenParams{
		PublicID:                 testTokenPublicID(t),
		ID:                       pgvalue.UUID(uuid.Must(uuid.NewV7())),
		OrgID:                    pgvalue.UUID(ids.orgID),
		CellID:                   dbtest.DefaultCellID,
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
	for i := range completionCount {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			data := []byte(`{"winner":` + string(rune('0'+i)) + `}`)
			row, err := queries.CompleteToken(ctx, db.CompleteTokenParams{
				OrgID:                 pgvalue.UUID(ids.orgID),
				CellID:                dbtest.DefaultCellID,
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
		PublicID:                 testTokenPublicID(t),
		ID:                       pgvalue.UUID(uuid.Must(uuid.NewV7())),
		OrgID:                    pgvalue.UUID(ids.orgID),
		CellID:                   dbtest.DefaultCellID,
		ProjectID:                pgvalue.UUID(ids.projectID),
		EnvironmentID:            pgvalue.UUID(ids.environmentID),
		TimeoutAt:                pgvalue.Timestamptz(time.Now().Add(-time.Second)),
		CreateRequestFingerprint: "expired-token",
		Metadata:                 []byte(`{}`),
	})
	if err != nil {
		t.Fatal(err)
	}
	tokenRunWait := seedRunWait(t, ctx, pool, ids, db.RunWaitKindToken)
	if _, err := queries.CreateTokenWait(ctx, db.CreateTokenWaitParams{
		ID:            pgvalue.UUID(uuid.Must(uuid.NewV7())),
		OrgID:         pgvalue.UUID(ids.orgID),
		CellID:        dbtest.DefaultCellID,
		ProjectID:     pgvalue.UUID(ids.projectID),
		EnvironmentID: pgvalue.UUID(ids.environmentID),
		RunWaitID:     tokenRunWait.ID,
		TokenID:       token.ID,
	}); err != nil {
		t.Fatal(err)
	}
	markRunWaitLiveWaiting(t, ctx, pool, ids, tokenRunWait)
	expired, err := queries.ExpireDueTokens(ctx, db.ExpireDueTokensParams{
		OrgID:  pgvalue.UUID(ids.orgID),
		CellID: dbtest.DefaultCellID,
	})
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

	timerRunWait := seedRunWait(t, ctx, pool, ids, db.RunWaitKindTimer)
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
	markRunWaitLiveWaiting(t, ctx, pool, ids, timerRunWait)
	resolved, err := queries.ResolveDueTimerWaits(ctx, db.ResolveDueTimerWaitsParams{
		OrgID:      pgvalue.UUID(ids.orgID),
		CellID:     dbtest.DefaultCellID,
		LimitCount: 10,
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(resolved) != 1 || resolved[0].ID != timerRunWait.ID || resolved[0].State != db.RunWaitStateResolvedLive {
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
			id, public_id, org_id, cell_id, project_id, environment_id, deployment_id, deployment_task_id, workspace_id, task_id,
			session_id, status, execution_status, payload, queue_name, max_active_duration_ms, trace_id, root_span_id
		)
		VALUES ($1, $10, $2, $3, $4, $5, $6, $7, $8, 'approval-task', $9, 'waiting', 'waiting', '{}', 'default', 300000,
			'11111111111111111111111111111111', '2222222222222222')
	`, secondRunID, ids.orgID, dbtest.DefaultCellID, ids.projectID, ids.environmentID, ids.deploymentID, ids.taskID, ids.workspaceID, sessionID, testRunPublicID(t)); err != nil {
		t.Fatal(err)
	}
	secondIDs := ids
	secondIDs.runID = secondRunID
	firstWait := seedRunWait(t, ctx, pool, ids, db.RunWaitKindTimer)
	secondWait := seedRunWait(t, ctx, pool, secondIDs, db.RunWaitKindTimer)
	for _, item := range []struct {
		ids     integrationIDs
		runWait db.RunWait
	}{
		{ids: ids, runWait: firstWait},
		{ids: secondIDs, runWait: secondWait},
	} {
		if item.runWait.ID == firstWait.ID {
			markRunWaitLiveWaiting(t, ctx, pool, item.ids, item.runWait)
		} else {
			markRunWaitWaiting(t, ctx, pool, item.ids, item.runWait)
		}
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
	if resolved.ID != firstWait.ID || resolved.State != db.RunWaitStateResolvedLive {
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
	if storedSecond.State != db.RunWaitStateCheckpointedWaiting {
		t.Fatalf("second timer state = %s, want checkpointed_waiting", storedSecond.State)
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
		PublicID:                 testTokenPublicID(t),
		ID:                       pgvalue.UUID(uuid.Must(uuid.NewV7())),
		OrgID:                    pgvalue.UUID(ids.orgID),
		CellID:                   dbtest.DefaultCellID,
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
		PublicID:      testPublicAccessTokenPublicID(t),
		ID:            pgvalue.UUID(uuid.Must(uuid.NewV7())),
		OrgID:         pgvalue.UUID(ids.orgID),
		CellID:        dbtest.DefaultCellID,
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
		CellID:              dbtest.DefaultCellID,
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
		CellID:              dbtest.DefaultCellID,
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
		CellID:              dbtest.DefaultCellID,
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
		CellID:              dbtest.DefaultCellID,
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
		CellID:              dbtest.DefaultCellID,
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
				id, org_id, cell_id, project_id, environment_id, public_access_token_id, scope_type, token_id, stream_id
			)
			VALUES ($1, $2, $3, $4, $5, $6, 'session.input.send', $7, NULL)
		`, uuid.Must(uuid.NewV7()), ids.orgID, dbtest.DefaultCellID, ids.projectID, ids.environmentID, pgvalue.MustUUIDValue(publicToken.ID), pgvalue.MustUUIDValue(token.ID))
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
		CellID:            dbtest.DefaultCellID,
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
		PublicID:           testStreamPublicID(t),
		ID:                 pgvalue.UUID(uuid.Must(uuid.NewV7())),
		OrgID:              pgvalue.UUID(ids.orgID),
		CellID:             dbtest.DefaultCellID,
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

func seedRunWait(t *testing.T, ctx context.Context, pool *pgxpool.Pool, ids integrationIDs, kind db.RunWaitKind) db.RunWait {
	t.Helper()
	runWaitID := uuid.Must(uuid.NewV7())
	if _, err := pool.Exec(ctx, `
		WITH run_scope AS (
			SELECT runs.org_id,
			       runs.cell_id,
			       runs.project_id,
			       runs.environment_id,
			       runs.id AS run_id,
			       runs.state_version,
			       run_leases.id AS run_lease_id,
			       run_leases.worker_instance_id,
			       runtime_instances.id AS runtime_instance_id,
			       runtime_instances.runtime_epoch
			  FROM runs
			  LEFT JOIN run_leases
			    ON run_leases.org_id = runs.org_id
			   AND run_leases.run_id = runs.id
			   AND run_leases.id = runs.current_run_lease_id
			   AND run_leases.status IN ('leased', 'running')
			   AND run_leases.lease_expires_at > now()
			  LEFT JOIN workspace_mounts
			    ON workspace_mounts.org_id = runs.org_id
			   AND workspace_mounts.project_id = runs.project_id
			   AND workspace_mounts.environment_id = runs.environment_id
			   AND workspace_mounts.id = runs.workspace_mount_id
			  LEFT JOIN runtime_instances
			    ON runtime_instances.org_id = workspace_mounts.org_id
			   AND runtime_instances.id = workspace_mounts.runtime_instance_id
			   AND runtime_instances.worker_instance_id = run_leases.worker_instance_id
			   AND runtime_instances.workspace_mount_id = workspace_mounts.id
			   AND runtime_instances.state IN ('running', 'waiting_hot', 'checkpointing')
			 WHERE runs.org_id = $2
			   AND runs.project_id = $3
			   AND runs.environment_id = $4
			   AND runs.id = $5
		)
			INSERT INTO run_waits (
				id, public_id, org_id, cell_id, project_id, environment_id, run_id, kind, state,
				live_wait_started_at, owner_runtime_instance_id, owner_runtime_epoch,
				owner_run_id, owner_run_lease_id, owner_worker_instance_id,
				owner_run_state_version
			)
			SELECT $1, $7, org_id, cell_id, project_id, environment_id, run_id, $6::run_wait_kind,
		       CASE WHEN runtime_instance_id IS NULL THEN 'cancelled'::run_wait_state ELSE 'live_waiting'::run_wait_state END,
		       CASE WHEN runtime_instance_id IS NULL THEN NULL ELSE now() END,
		       runtime_instance_id,
		       runtime_epoch,
		       CASE WHEN runtime_instance_id IS NULL THEN NULL ELSE run_id END,
		       run_lease_id,
		       worker_instance_id,
		       CASE WHEN runtime_instance_id IS NULL THEN NULL ELSE state_version END
		  FROM run_scope
	`, runWaitID, ids.orgID, ids.projectID, ids.environmentID, ids.runID, kind, testWaitPublicID(t)); err != nil {
		t.Fatal(err)
	}
	queries := db.New(pool)
	runWait, err := queries.GetRunWait(ctx, db.GetRunWaitParams{
		OrgID:         pgvalue.UUID(ids.orgID),
		ProjectID:     pgvalue.UUID(ids.projectID),
		EnvironmentID: pgvalue.UUID(ids.environmentID),
		ID:            pgvalue.UUID(runWaitID),
	})
	if err != nil {
		t.Fatal(err)
	}
	return runWait
}

func seedCheckpointingRunWait(t *testing.T, ctx context.Context, queries *db.Queries, pool *pgxpool.Pool, ids integrationIDs, runLeaseID uuid.UUID, workerID uuid.UUID, kind db.RunWaitKind) (db.RunWait, uuid.UUID) {
	t.Helper()
	row, err := queries.CreateHotRunWait(ctx, db.CreateHotRunWaitParams{
		PublicID:         testWaitPublicID(t),
		ID:               pgvalue.UUID(uuid.Must(uuid.NewV7())),
		Kind:             kind,
		OrgID:            pgvalue.UUID(ids.orgID),
		ProjectID:        pgvalue.UUID(ids.projectID),
		EnvironmentID:    pgvalue.UUID(ids.environmentID),
		RunID:            pgvalue.UUID(ids.runID),
		RunLeaseID:       pgvalue.UUID(runLeaseID),
		WorkerInstanceID: pgvalue.UUID(workerID),
		CheckpointDelay:  pgvalue.Interval(0),
	})
	if err != nil {
		t.Fatal(err)
	}
	runWait := runWaitFromCreateHotRunWaitRow(row)
	setRunWaitCurrentWorkspaceVersion(t, ctx, pool, ids, runWait)
	checkpointID := uuid.Must(uuid.NewV7())
	claimRuntimeCheckpointWaitForTest(t, ctx, pool, queries, ids, runWait, runLeaseID, workerID, checkpointID)
	return runWait, checkpointID
}

func claimRuntimeCheckpointWaitForTest(t *testing.T, ctx context.Context, pool *pgxpool.Pool, queries *db.Queries, ids integrationIDs, runWait db.RunWait, runLeaseID uuid.UUID, workerID uuid.UUID, checkpointID uuid.UUID) {
	t.Helper()
	if _, err := queries.ClaimRuntimeCheckpointWait(ctx, db.ClaimRuntimeCheckpointWaitParams{
		OrgID:               pgvalue.UUID(ids.orgID),
		ProjectID:           pgvalue.UUID(ids.projectID),
		EnvironmentID:       pgvalue.UUID(ids.environmentID),
		RunID:               pgvalue.UUID(ids.runID),
		RunWaitID:           runWait.ID,
		RunLeaseID:          pgvalue.UUID(runLeaseID),
		WorkerInstanceID:    pgvalue.UUID(workerID),
		RuntimeCheckpointID: pgvalue.UUID(checkpointID),
	}); err != nil {
		t.Fatal(err)
	}
	var checkpointState db.RuntimeCheckpointState
	if err := pool.QueryRow(ctx, `
		SELECT state
		  FROM runtime_checkpoints
		 WHERE org_id = $1
		   AND run_id = $2
		   AND id = $3
	`, ids.orgID, ids.runID, checkpointID).Scan(&checkpointState); err != nil {
		t.Fatal(err)
	}
	if checkpointState != db.RuntimeCheckpointStateCreating {
		t.Fatalf("checkpoint state after claim = %s, want creating", checkpointState)
	}
}

func resolveCheckpointedRunWaitForTest(t *testing.T, ctx context.Context, pool *pgxpool.Pool, ids integrationIDs, runWait db.RunWait) {
	t.Helper()
	tag, err := pool.Exec(ctx, `
		UPDATE run_waits
		   SET state = 'resolved_checkpointed',
		       resolved_at = now(),
		       updated_at = now()
		 WHERE org_id = $1
		   AND id = $2
		   AND state = 'checkpointed_waiting'
	`, ids.orgID, pgvalue.MustUUIDValue(runWait.ID))
	if err != nil {
		t.Fatal(err)
	}
	if tag.RowsAffected() != 1 {
		t.Fatalf("resolved checkpointed run wait rows = %d, want 1", tag.RowsAffected())
	}
}

func readyRuntimeCheckpointParamsForRun(t *testing.T, ctx context.Context, pool *pgxpool.Pool, ids integrationIDs, runWait db.RunWait, runLeaseID uuid.UUID, workerID uuid.UUID, checkpointID uuid.UUID) db.CreateReadyRuntimeCheckpointForRunWaitParams {
	t.Helper()
	params := readyRuntimeCheckpointParams(ids, runWait, runLeaseID, workerID, checkpointID)
	params.WorkerCommandID = seedAcceptedRuntimeCheckpointWorkerCommand(t, ctx, pool, ids, runWait, runLeaseID, workerID)
	if err := pool.QueryRow(ctx, `
		SELECT runtime_id,
		       runtime_arch,
		       runtime_abi,
		       kernel_digest,
		       initramfs_digest,
		       rootfs_digest,
		       cni_profile
		  FROM run_runtime_requirements
		 WHERE org_id = $1
		   AND run_id = $2
	`, ids.orgID, ids.runID).Scan(
		&params.RuntimeID,
		&params.RuntimeArch,
		&params.RuntimeABI,
		&params.KernelDigest,
		&params.InitramfsDigest,
		&params.RootfsDigest,
		&params.CniProfile,
	); err != nil {
		t.Fatal(err)
	}
	return params
}

func seedAcceptedRuntimeCheckpointWorkerCommand(t *testing.T, ctx context.Context, pool *pgxpool.Pool, ids integrationIDs, runWait db.RunWait, runLeaseID uuid.UUID, workerID uuid.UUID) int64 {
	t.Helper()
	var workerCommandID int64
	if err := pool.QueryRow(ctx, `
			INSERT INTO worker_commands (
				org_id, cell_id, project_id, environment_id, run_id, run_wait_id,
				run_lease_id, worker_instance_id, runtime_instance_id,
				runtime_epoch, run_state_version, kind, payload, accepted_at
			)
			SELECT run_waits.org_id,
			       run_waits.cell_id,
			       run_waits.project_id,
		       run_waits.environment_id,
		       run_waits.run_id,
		       run_waits.id,
		       run_waits.owner_run_lease_id,
		       run_waits.owner_worker_instance_id,
		       run_waits.owner_runtime_instance_id,
		       run_waits.owner_runtime_epoch,
		       run_waits.owner_run_state_version,
		       'runtime_checkpoint_wait',
		       '{}'::jsonb,
		       now()
		  FROM run_waits
		 WHERE run_waits.org_id = $1
		   AND run_waits.project_id = $2
		   AND run_waits.environment_id = $3
		   AND run_waits.id = $4
		   AND run_waits.run_id = $5
		   AND run_waits.owner_run_lease_id = $6
		   AND run_waits.owner_worker_instance_id = $7
		   AND run_waits.state = 'checkpointing'
		RETURNING id
	`, ids.orgID, ids.projectID, ids.environmentID, runWait.ID, ids.runID, runLeaseID, workerID).Scan(&workerCommandID); err != nil {
		t.Fatal(err)
	}
	return workerCommandID
}

func seedRestoreReadyWorkspaceMount(t *testing.T, ctx context.Context, pool *pgxpool.Pool, ids integrationIDs, workerID uuid.UUID) {
	t.Helper()
	workspaceMountID := uuid.Must(uuid.NewV7())
	runtimeInstanceID := uuid.Must(uuid.NewV7())
	var runtimeID string
	if err := pool.QueryRow(ctx, `
		SELECT runtime_id
		  FROM run_runtime_requirements
		 WHERE org_id = $1
		   AND run_id = $2
	`, ids.orgID, ids.runID).Scan(&runtimeID); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `
		INSERT INTO workspace_mounts (
			id, org_id, cell_id, project_id, environment_id, workspace_id, deployment_sandbox_id, sandbox_fingerprint,
			image_artifact_id, image_artifact_format, rootfs_digest, image_digest, image_format, workspace_artifact_id,
			workspace_artifact_encoding, workspace_artifact_entry_count, workspace_artifact_digest,
			workspace_artifact_size_bytes, workspace_artifact_media_type, workspace_mount_path,
			runtime_abi, guestd_abi, adapter_abi, state, mounted_at, last_heartbeat_at
		)
		SELECT $1, workspaces.org_id, workspaces.cell_id, workspaces.project_id, workspaces.environment_id, workspaces.id,
		       deployment_sandboxes.id, workspaces.sandbox_fingerprint,
		       image_artifact.id, deployment_sandboxes.image_artifact_format, deployment_sandboxes.rootfs_digest,
		       deployment_sandboxes.image_digest, deployment_sandboxes.image_format,
		       workspace_artifact.id, workspace_versions.artifact_encoding, workspace_versions.artifact_entry_count,
		       workspace_artifact.digest, workspace_artifact.size_bytes, workspace_artifact.media_type,
		       deployment_sandboxes.workspace_mount_path, deployment_sandboxes.runtime_abi,
		       deployment_sandboxes.guestd_abi, deployment_sandboxes.adapter_abi, 'mounted', now(), now()
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
	`, workspaceMountID, ids.orgID, ids.workspaceID); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `
		INSERT INTO runtime_instances (
			id, org_id, cell_id, project_id, environment_id, worker_instance_id,
			runtime_release_id, deployment_sandbox_id, runtime_key_hash, runtime_key,
			sandbox_fingerprint, rootfs_digest, image_digest, image_format,
			sandbox_image_artifact_id, sandbox_image_artifact_digest,
			sandbox_image_artifact_format, workspace_mount_path, runtime_abi,
			guestd_abi, adapter_abi, network_policy, reserved_cpu_millis,
			reserved_memory_mib, reserved_disk_mib, reserved_execution_slots,
			workspace_mount_id, state, instance_token, expires_at,
			last_heartbeat_at, bound_at
		)
		SELECT $1, workspace_mounts.org_id, workspace_mounts.cell_id, workspace_mounts.project_id,
		       workspace_mounts.environment_id, $2, $3,
		       workspace_mounts.deployment_sandbox_id, $4, '{}'::jsonb,
		       workspace_mounts.sandbox_fingerprint, workspace_mounts.rootfs_digest,
		       workspace_mounts.image_digest, workspace_mounts.image_format,
		       workspace_mounts.image_artifact_id, image_artifact.digest,
		       workspace_mounts.image_artifact_format, workspace_mounts.workspace_mount_path,
		       workspace_mounts.runtime_abi, workspace_mounts.guestd_abi,
		       workspace_mounts.adapter_abi, '{}'::jsonb,
		       1000, 1024, 4096, 1,
		       workspace_mounts.id, 'binding',
		       'restore-runtime-instance-token-' || $5, now() + interval '1 hour', now(), now()
		  FROM workspace_mounts
		  JOIN artifacts AS image_artifact
		    ON image_artifact.org_id = workspace_mounts.org_id
		   AND image_artifact.project_id = workspace_mounts.project_id
		   AND image_artifact.environment_id = workspace_mounts.environment_id
		   AND image_artifact.id = workspace_mounts.image_artifact_id
		 WHERE workspace_mounts.org_id = $6
		   AND workspace_mounts.id = $7
	`, runtimeInstanceID, workerID, runtimeID, "restore-runtime-key-"+shortUUID(runtimeInstanceID), shortUUID(runtimeInstanceID), ids.orgID, workspaceMountID); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `
		UPDATE workspace_mounts
		   SET runtime_instance_id = $1,
		       updated_at = now()
		 WHERE org_id = $2
		   AND id = $3
	`, runtimeInstanceID, ids.orgID, workspaceMountID); err != nil {
		t.Fatal(err)
	}
}

func readyRuntimeCheckpointParams(ids integrationIDs, runWait db.RunWait, runLeaseID uuid.UUID, workerID uuid.UUID, checkpointID uuid.UUID) db.CreateReadyRuntimeCheckpointForRunWaitParams {
	return db.CreateReadyRuntimeCheckpointForRunWaitParams{
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
	}
}

func runWaitFromCreateHotRunWaitRow(row db.CreateHotRunWaitRow) db.RunWait {
	return db.RunWait{
		ID:                         row.ID,
		OrgID:                      row.OrgID,
		ProjectID:                  row.ProjectID,
		EnvironmentID:              row.EnvironmentID,
		RunID:                      row.RunID,
		Kind:                       row.Kind,
		CorrelationID:              row.CorrelationID,
		State:                      row.State,
		TimeoutAt:                  row.TimeoutAt,
		RuntimeCheckpointDueAt:     row.RuntimeCheckpointDueAt,
		RuntimeCheckpointStartedAt: row.RuntimeCheckpointStartedAt,
		LiveWaitStartedAt:          row.LiveWaitStartedAt,
		OwnerRuntimeInstanceID:     row.OwnerRuntimeInstanceID,
		OwnerRuntimeEpoch:          row.OwnerRuntimeEpoch,
		OwnerRunID:                 row.OwnerRunID,
		OwnerRunLeaseID:            row.OwnerRunLeaseID,
		OwnerWorkerInstanceID:      row.OwnerWorkerInstanceID,
		OwnerRunStateVersion:       row.OwnerRunStateVersion,
		RuntimeCheckpointID:        row.RuntimeCheckpointID,
		WorkspaceVersionID:         row.WorkspaceVersionID,
		ActiveElapsedMsAtPark:      row.ActiveElapsedMsAtPark,
		ParkedAt:                   row.ParkedAt,
		ResolvedAt:                 row.ResolvedAt,
		ResumedAt:                  row.ResumedAt,
		CancelledAt:                row.CancelledAt,
		CreatedAt:                  row.CreatedAt,
		UpdatedAt:                  row.UpdatedAt,
	}
}

func runtimeStateForRun(t *testing.T, ctx context.Context, pool *pgxpool.Pool, ids integrationIDs) (db.RuntimeInstanceState, uuid.UUID) {
	t.Helper()
	var state db.RuntimeInstanceState
	var currentRunWaitID pgtype.UUID
	if err := pool.QueryRow(ctx, `
		SELECT runtime_instances.state,
		       runtime_instances.owner_run_wait_id
		  FROM runs
		  JOIN workspace_mounts
		    ON workspace_mounts.org_id = runs.org_id
		   AND workspace_mounts.id = runs.workspace_mount_id
		  JOIN runtime_instances
		    ON runtime_instances.org_id = workspace_mounts.org_id
		   AND runtime_instances.id = workspace_mounts.runtime_instance_id
		 WHERE runs.org_id = $1
		   AND runs.id = $2
	`, ids.orgID, ids.runID).Scan(&state, &currentRunWaitID); err != nil {
		t.Fatal(err)
	}
	return state, optionalUUIDValue(currentRunWaitID)
}

func runtimeStateForWorkspaceMount(t *testing.T, ctx context.Context, pool *pgxpool.Pool, ids integrationIDs) (db.RuntimeInstanceState, uuid.UUID) {
	t.Helper()
	var state db.RuntimeInstanceState
	var currentRunWaitID pgtype.UUID
	if err := pool.QueryRow(ctx, `
		SELECT runtime_instances.state,
		       runtime_instances.owner_run_wait_id
		  FROM workspace_mounts
		  JOIN runtime_instances
		    ON runtime_instances.org_id = workspace_mounts.org_id
		   AND runtime_instances.id = workspace_mounts.runtime_instance_id
		 WHERE workspace_mounts.org_id = $1
		   AND workspace_mounts.workspace_id = $2
		 ORDER BY workspace_mounts.updated_at DESC
		 LIMIT 1
	`, ids.orgID, ids.workspaceID).Scan(&state, &currentRunWaitID); err != nil {
		t.Fatal(err)
	}
	return state, optionalUUIDValue(currentRunWaitID)
}

func optionalUUIDValue(value pgtype.UUID) uuid.UUID {
	if !value.Valid {
		return uuid.Nil
	}
	id, err := pgvalue.UUIDValue(value)
	if err != nil {
		return uuid.Nil
	}
	return id
}

func ensureRunningWorkspaceMount(t *testing.T, ctx context.Context, pool interface {
	Exec(context.Context, string, ...any) (pgconn.CommandTag, error)
	QueryRow(context.Context, string, ...any) pgx.Row
}, ids integrationIDs) {
	t.Helper()
	workspaceMountID := uuid.Must(uuid.NewV7())
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
			  FROM workspace_mounts
			 WHERE org_id = $2
			   AND workspace_id = $3
			   AND state = 'mounted'
			 LIMIT 1
		),
		inserted AS (
			INSERT INTO workspace_mounts (
				id, org_id, cell_id, project_id, environment_id, workspace_id, deployment_sandbox_id, sandbox_fingerprint,
				image_artifact_id, image_artifact_format, rootfs_digest, image_digest, image_format,
				workspace_artifact_id, workspace_artifact_encoding, workspace_artifact_entry_count,
				workspace_artifact_digest, workspace_artifact_size_bytes, workspace_artifact_media_type,
				workspace_mount_path, runtime_abi, guestd_abi, adapter_abi, state
			)
			SELECT $1, workspaces.org_id, workspaces.cell_id, workspaces.project_id, workspaces.environment_id, workspaces.id,
			       deployment_sandboxes.id, workspaces.sandbox_fingerprint,
			       image_artifact.id, deployment_sandboxes.image_artifact_format, deployment_sandboxes.rootfs_digest,
		       deployment_sandboxes.image_digest, deployment_sandboxes.image_format,
		       workspace_artifact.id, workspace_versions.artifact_encoding, workspace_versions.artifact_entry_count,
			       workspace_artifact.digest, workspace_artifact.size_bytes, workspace_artifact.media_type,
			       deployment_sandboxes.workspace_mount_path, deployment_sandboxes.runtime_abi,
			       deployment_sandboxes.guestd_abi, deployment_sandboxes.adapter_abi, 'mounted'
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
	`, workspaceMountID, ids.orgID, ids.workspaceID).Scan(&id); err != nil {
		t.Fatal(err)
	}
}

func seedActiveWorkspaceLeaseForRun(t *testing.T, ctx context.Context, pool interface {
	Exec(context.Context, string, ...any) (pgconn.CommandTag, error)
	QueryRow(context.Context, string, ...any) pgx.Row
}, ids integrationIDs) {
	t.Helper()
	ensureRunningWorkspaceMount(t, ctx, pool, ids)
	leaseID := uuid.Must(uuid.NewV7())
	var workspaceMountID uuid.UUID
	if err := pool.QueryRow(ctx, `
		SELECT id
		  FROM workspace_mounts
		 WHERE org_id = $1
		   AND workspace_id = $2
		   AND state = 'mounted'
		 LIMIT 1
	`, ids.orgID, ids.workspaceID).Scan(&workspaceMountID); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `
		INSERT INTO workspace_leases (
			id, org_id, cell_id, project_id, environment_id, workspace_id, workspace_mount_id,
			lease_kind, state, owner_run_id, base_version_id, acquired_version_id,
			acquired_fencing_generation, fencing_token, expires_at
		)
		SELECT $1, org_id, cell_id, project_id, environment_id, id, $2,
		       'write', 'active', $3, current_version_id, current_version_id,
		       1, 'test-fencing-token', now() + interval '1 hour'
		  FROM workspaces
		 WHERE org_id = $4
		   AND id = $5
	`, leaseID, workspaceMountID, ids.runID, ids.orgID, ids.workspaceID); err != nil {
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
			id, public_id, org_id, cell_id, project_id, environment_id, workspace_id, kind, state,
			artifact_id, artifact_encoding, artifact_entry_count, content_digest, size_bytes, promoted_at
		)
		SELECT $1, $8, $2, $3, $4, $5, $6, 'system', 'ready',
		       artifacts.id, 'tar', 0, artifacts.digest, artifacts.size_bytes, now()
		  FROM artifacts
		 WHERE artifacts.org_id = $2
		   AND artifacts.cell_id = $3
		   AND artifacts.project_id = $4
		   AND artifacts.environment_id = $5
		   AND artifacts.id = $7
	`, nextVersionID, ids.orgID, dbtest.DefaultCellID, ids.projectID, ids.environmentID, ids.workspaceID, nextArtifactID, testWorkspaceVersionPublicID(t)); err != nil {
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
	ensureRunningWorkspaceMount(t, ctx, pool, ids)
	var workspaceMountID uuid.UUID
	if err := pool.QueryRow(ctx, `
			SELECT id
			  FROM workspace_mounts
			 WHERE org_id = $1
			   AND workspace_id = $2
			   AND state = 'mounted'
			 LIMIT 1
	`, ids.orgID, ids.workspaceID).Scan(&workspaceMountID); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `
		INSERT INTO workspace_leases (
			id, org_id, cell_id, project_id, environment_id, workspace_id, workspace_mount_id,
			lease_kind, state, owner_run_id, base_version_id, acquired_version_id,
			acquired_fencing_generation, fencing_token, expires_at
		)
		SELECT $1, org_id, cell_id, project_id, environment_id, id, $2,
		       'write', 'released', $3, current_version_id, current_version_id,
		       1, 'test-fencing-token', now() + interval '1 hour'
		  FROM workspaces
		 WHERE org_id = $4
		   AND id = $5
	`, leaseID, workspaceMountID, ids.runID, ids.orgID, ids.workspaceID); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `
		INSERT INTO runtime_checkpoints (
			id, org_id, cell_id, project_id, environment_id, workspace_id, run_id,
			source_workspace_lease_id, workspace_mount_id, base_workspace_version_id,
			state, runtime_backend, runtime_id, runtime_arch, runtime_abi, kernel_digest,
			initramfs_digest, rootfs_digest, runtime_config_digest, cni_profile, manifest, ready_at
		)
		SELECT $1, workspaces.org_id, workspaces.cell_id, workspaces.project_id, workspaces.environment_id,
		       workspaces.id, $2, $3, $4, workspaces.current_version_id,
		       'ready', 'test', 'test-runtime', 'arm64', 'test-abi', 'sha256:kernel',
		       'sha256:initramfs', 'sha256:rootfs', 'sha256:config', 'test-cni', '{}', now()
		  FROM workspaces
		 WHERE workspaces.org_id = $5
		   AND workspaces.id = $6
	`, checkpointID, ids.runID, leaseID, workspaceMountID, ids.orgID, ids.workspaceID); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `
		UPDATE run_waits
		   SET state = 'checkpointed_waiting',
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

func markRunWaitLiveWaiting(t *testing.T, ctx context.Context, pool *pgxpool.Pool, ids integrationIDs, runWait db.RunWait) {
	t.Helper()
	var currentRunLeaseID uuid.UUID
	err := pool.QueryRow(ctx, `
		SELECT current_run_lease_id
		  FROM runs
		 WHERE org_id = $1
		   AND id = $2
		   AND current_run_lease_id IS NOT NULL
	`, ids.orgID, ids.runID).Scan(&currentRunLeaseID)
	if errors.Is(err, pgx.ErrNoRows) {
		_, _, _ = seedRunningSessionLease(t, ctx, pool, ids)
	} else if err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `
		WITH scope AS (
			SELECT runs.id AS run_id,
			       runs.state_version,
			       run_leases.id AS run_lease_id,
			       run_leases.worker_instance_id,
			       runtime_instances.id AS runtime_instance_id,
			       runtime_instances.runtime_epoch
			  FROM runs
			  JOIN run_leases
			    ON run_leases.org_id = runs.org_id
			   AND run_leases.run_id = runs.id
			   AND run_leases.id = runs.current_run_lease_id
			  JOIN workspace_mounts
			    ON workspace_mounts.org_id = runs.org_id
			   AND workspace_mounts.id = runs.workspace_mount_id
			  JOIN runtime_instances
			    ON runtime_instances.org_id = workspace_mounts.org_id
			   AND runtime_instances.id = workspace_mounts.runtime_instance_id
			 WHERE runs.org_id = $1
			   AND runs.id = $2
		)
		UPDATE run_waits
		   SET state = 'live_waiting',
		       live_wait_started_at = COALESCE(run_waits.live_wait_started_at, now()),
		       owner_runtime_instance_id = scope.runtime_instance_id,
		       owner_runtime_epoch = scope.runtime_epoch,
		       owner_run_id = scope.run_id,
		       owner_run_lease_id = scope.run_lease_id,
		       owner_worker_instance_id = scope.worker_instance_id,
		       owner_run_state_version = scope.state_version,
		       runtime_checkpoint_id = NULL,
		       workspace_version_id = NULL,
		       active_elapsed_ms_at_park = NULL,
		       resolved_at = NULL,
		       updated_at = now()
		  FROM scope
		 WHERE run_waits.org_id = $1
		   AND run_waits.run_id = scope.run_id
		   AND run_waits.id = $3
	`, ids.orgID, ids.runID, pgvalue.MustUUIDValue(runWait.ID)); err != nil {
		t.Fatal(err)
	}
}
