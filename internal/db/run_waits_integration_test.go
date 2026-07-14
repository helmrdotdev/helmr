package db_test

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/helmrdotdev/helmr/internal/db"
	"github.com/helmrdotdev/helmr/internal/pgvalue"
	"github.com/helmrdotdev/helmr/internal/publicid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
)

func TestCheckpointedWaitResolutionQueuesAndBindsResumeLeaseExactlyOnce(t *testing.T) {
	ctx := context.Background()
	pool := newIntegrationDB(t, ctx)
	ids := seedIntegration(t, ctx, pool)
	fixture := seedWorkspaceRuntimeFixture(t, ctx, ids, true, pool)
	queries := db.New(pool)

	var groupID string
	var workspaceVersionID uuid.UUID
	if err := pool.QueryRow(ctx, `SELECT worker_group_id FROM worker_instances WHERE id = $1`, fixture.workerID).Scan(&groupID); err != nil {
		t.Fatal(err)
	}
	if err := pool.QueryRow(ctx, `SELECT current_version_id FROM workspaces WHERE id = $1`, ids.workspaceID).Scan(&workspaceVersionID); err != nil {
		t.Fatal(err)
	}
	workspaceLease, err := queries.AcquireWorkspaceWriteLease(ctx, db.AcquireWorkspaceWriteLeaseParams{
		ID:                pgvalue.UUID(uuid.Must(uuid.NewV7())),
		OwnerRunID:        pgvalue.UUID(ids.runID),
		AcquiredVersionID: pgvalue.UUID(workspaceVersionID),
		FencingToken:      "checkpointed-wait-resume",
		ExpiresAt:         pgvalue.Timestamptz(time.Now().Add(10 * time.Minute)),
		OrgID:             pgvalue.UUID(ids.orgID),
		WorkspaceID:       pgvalue.UUID(ids.workspaceID),
		WorkspaceMountID:  pgvalue.UUID(fixture.mountID),
	})
	if err != nil {
		t.Fatal(err)
	}

	sourceLeaseID := uuid.Must(uuid.NewV7())
	mustExec(t, ctx, pool, `
		INSERT INTO run_leases (
			id, org_id, project_id, environment_id, run_id, workspace_id, region_id,
			lease_sequence, task_attempt_number, worker_group_id, worker_instance_id,
			worker_epoch, runtime_instance_id, network_slot_id, network_slot_generation,
			queue_name, queue_class, runtime_identity_id, requested_cpu_millis,
			requested_memory_bytes, requested_workload_disk_bytes, requested_scratch_bytes,
			requested_execution_slots, state, assigned_at, start_deadline_at, claimed_at,
			started_at, expires_at, checkpointed_at, terminal_at, terminal_reason_code
		) VALUES (
			$1, $2, $3, $4, $5, $6, 'us-east-1', 1, 1, $7, $8, 1, $9, $10, 1,
			'default', 'run', 'test-runtime', 1000, 1073741824, 4294967296, 0, 1,
			'checkpointed', now() - interval '3 minutes', now() + interval '2 minutes',
			now() - interval '2 minutes', now() - interval '1 minute',
			now() + interval '5 minutes', now(), now(), 'checkpoint_committed'
		)
	`, sourceLeaseID, ids.orgID, ids.projectID, ids.environmentID, ids.runID,
		ids.workspaceID, groupID, fixture.workerID, fixture.runtimeID, fixture.slotID)
	mustExec(t, ctx, pool, `
		UPDATE runs
		   SET state_version = 5, queued_expires_at = now() - interval '1 minute'
		 WHERE org_id = $1 AND id = $2
	`, ids.orgID, ids.runID)

	waitID := uuid.Must(uuid.NewV7())
	runWaitID := uuid.Must(uuid.NewV7())
	checkpointID := uuid.Must(uuid.NewV7())
	mustExec(t, ctx, pool, `
		INSERT INTO waits (
			id, public_id, org_id, project_id, environment_id, kind, state,
			completed_after, completed_at, result
		) VALUES ($1, $2, $3, $4, $5, 'timer', 'completed', now() - interval '1 second', now(), 'null')
	`, waitID, testPublicID(t, publicid.Wait), ids.orgID, ids.projectID, ids.environmentID)
	mustExec(t, ctx, pool, `
		INSERT INTO run_waits (
			id, org_id, project_id, environment_id, run_id, wait_id, state,
			expected_run_state_version, current_run_lease_id, hot_wait_started_at
		) VALUES ($1, $2, $3, $4, $5, $6, 'hot_waiting', 5, $7, now())
	`, runWaitID, ids.orgID, ids.projectID, ids.environmentID, ids.runID, waitID, sourceLeaseID)
	mustExec(t, ctx, pool, `
		INSERT INTO run_checkpoints (
			id, org_id, project_id, environment_id, workspace_id, run_id, run_wait_id,
			source_run_lease_id, source_runtime_instance_id, source_worker_instance_id,
			source_worker_epoch, source_workspace_lease_id, workspace_mount_id,
			base_workspace_version_id, state, runtime_backend, runtime_identity_id,
			runtime_arch, runtime_abi, kernel_digest, initramfs_digest, rootfs_digest,
			runtime_config_digest, cni_profile, manifest, creation_expires_at, ready_at
		) VALUES (
			$1, $2, $3, $4, $5, $6, $7, $8, $9, $10, 1, $11, $12, $13,
			'ready', 'firecracker', 'test-runtime', 'arm64', 'test', 'sha256:kernel',
			'sha256:initramfs', 'sha256:rootfs', 'sha256:runtime-config', 'default',
			'{"ready":true}', now() + interval '5 minutes', now()
		)
	`, checkpointID, ids.orgID, ids.projectID, ids.environmentID, ids.workspaceID,
		ids.runID, runWaitID, sourceLeaseID, fixture.runtimeID, fixture.workerID,
		pgvalue.MustUUIDValue(workspaceLease.ID), fixture.mountID, workspaceVersionID)
	mustExec(t, ctx, pool, `
		UPDATE run_waits
		   SET state = 'checkpointed_waiting', current_run_lease_id = NULL,
		       prior_run_lease_id = $1, run_checkpoint_id = $2,
		       reserved_workspace_id = $3, reserved_workspace_version_id = $4,
		       active_elapsed_ms_at_park = 1000
		 WHERE id = $5
	`, sourceLeaseID, checkpointID, ids.workspaceID, workspaceVersionID, runWaitID)

	resolved, err := queries.RequeueResolvedRunWaits(ctx, db.RequeueResolvedRunWaitsParams{
		OrgID: pgvalue.UUID(ids.orgID), LimitCount: 10,
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(resolved) != 1 || resolved[0].State != db.RunWaitStateCheckpointedWaiting || resolved[0].ResumeRequestVersion != 1 || resolved[0].ExpectedRunStateVersion != 6 {
		t.Fatalf("resolved waits = %+v, want one checkpointed wait with resume version 1 and run version 6", resolved)
	}
	var queuedExpiresAt pgtype.Timestamptz
	if err := pool.QueryRow(ctx, `SELECT queued_expires_at FROM runs WHERE id = $1`, ids.runID).Scan(&queuedExpiresAt); err != nil {
		t.Fatal(err)
	}
	if queuedExpiresAt.Valid {
		t.Fatalf("queued expiration remained %s, want NULL after checkpoint resume", queuedExpiresAt.Time)
	}
	replayed, err := queries.RequeueResolvedRunWaits(ctx, db.RequeueResolvedRunWaitsParams{
		OrgID: pgvalue.UUID(ids.orgID), LimitCount: 10,
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(replayed) != 0 {
		t.Fatalf("replayed resolution returned %d rows, want 0", len(replayed))
	}

	resumeLeaseID := uuid.Must(uuid.NewV7())
	lease, err := queries.LeaseRunLease(ctx, db.LeaseRunLeaseParams{
		OrgID:                   pgvalue.UUID(ids.orgID),
		RunID:                   pgvalue.UUID(ids.runID),
		ExpectedRunStateVersion: 6,
		RunLeaseID:              pgvalue.UUID(resumeLeaseID),
		LeaseSequence:           2,
		WorkerGroupID:           groupID,
		WorkerInstanceID:        pgvalue.UUID(fixture.workerID),
		WorkerEpoch:             1,
		RuntimeInstanceID:       pgvalue.UUID(fixture.runtimeID),
		NetworkSlotID:           pgvalue.UUID(fixture.slotID),
		NetworkSlotGeneration:   1,
		WorkerProtocolVersion:   "helmr.worker.v0",
		RequestedScratchBytes:   0,
		ResourceSnapshot:        []byte(`{}`),
		StartDeadlineAt:         pgvalue.Timestamptz(time.Now().Add(2 * time.Minute)),
		ExpiresAt:               pgvalue.Timestamptz(time.Now().Add(5 * time.Minute)),
	})
	if err != nil {
		t.Fatal(err)
	}
	bound, err := queries.GetRunWaitByID(ctx, db.GetRunWaitByIDParams{
		OrgID: pgvalue.UUID(ids.orgID), RunWaitID: pgvalue.UUID(runWaitID),
	})
	if err != nil {
		t.Fatal(err)
	}
	if bound.State != db.RunWaitStateResuming || bound.CurrentRunLeaseID != lease.ID || bound.ExpectedRunStateVersion != 7 {
		t.Fatalf("bound wait = state:%s lease:%s version:%d, want resuming/%s/7", bound.State, pgvalue.UUIDString(bound.CurrentRunLeaseID), bound.ExpectedRunStateVersion, pgvalue.UUIDString(lease.ID))
	}
	mustExec(t, ctx, pool, `UPDATE run_waits SET resume_requested_at = now() - interval '10 minutes' WHERE id = $1`, runWaitID)
	stale, err := queries.RequeueStaleResumingRunWaits(ctx, db.RequeueStaleResumingRunWaitsParams{
		OrgID:      pgvalue.UUID(ids.orgID),
		StaleAfter: pgtype.Interval{Microseconds: (5 * time.Minute).Microseconds(), Valid: true},
		LimitCount: 10,
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(stale) != 0 {
		t.Fatalf("freshly bound resume was requeued from old request time: %+v", stale)
	}
	staleTx, err := pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := staleTx.Exec(ctx, `UPDATE run_waits SET resuming_at = now() - interval '10 minutes' WHERE id = $1`, runWaitID); err != nil {
		staleTx.Rollback(ctx) //nolint:errcheck
		t.Fatal(err)
	}
	staleRows, err := db.New(staleTx).RequeueStaleResumingRunWaits(ctx, db.RequeueStaleResumingRunWaitsParams{
		OrgID:      pgvalue.UUID(ids.orgID),
		StaleAfter: pgtype.Interval{Microseconds: (5 * time.Minute).Microseconds(), Valid: true},
		LimitCount: 10,
	})
	if err != nil {
		staleTx.Rollback(ctx) //nolint:errcheck
		t.Fatal(err)
	}
	if len(staleRows) != 1 || staleRows[0].State != db.RunWaitStateCheckpointedWaiting || staleRows[0].ResumeRequestVersion != 1 {
		staleTx.Rollback(ctx) //nolint:errcheck
		t.Fatalf("stale resume recovery = %+v, want one checkpointed wait preserving resume version 1", staleRows)
	}
	if err := staleTx.Rollback(ctx); err != nil {
		t.Fatal(err)
	}

	if _, err := queries.StartRunLease(ctx, db.StartRunLeaseParams{
		ExpiresAt:               pgvalue.Timestamptz(time.Now().Add(5 * time.Minute)),
		OrgID:                   pgvalue.UUID(ids.orgID),
		RunID:                   pgvalue.UUID(ids.runID),
		RunLeaseID:              lease.ID,
		LeaseSequence:           lease.LeaseSequence,
		WorkerInstanceID:        lease.WorkerInstanceID,
		WorkerEpoch:             lease.WorkerEpoch,
		RuntimeInstanceID:       lease.RuntimeInstanceID,
		NetworkSlotID:           lease.NetworkSlotID,
		NetworkSlotGeneration:   lease.NetworkSlotGeneration,
		ExpectedRunStateVersion: 7,
	}); err != nil {
		t.Fatal(err)
	}
	started, err := queries.GetRunWaitByID(ctx, db.GetRunWaitByIDParams{
		OrgID: pgvalue.UUID(ids.orgID), RunWaitID: pgvalue.UUID(runWaitID),
	})
	if err != nil {
		t.Fatal(err)
	}
	if started.ExpectedRunStateVersion != 8 {
		t.Fatalf("started wait run version = %d, want 8", started.ExpectedRunStateVersion)
	}
	ackLossTx, err := pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := ackLossTx.Exec(ctx, `
		UPDATE runs
		   SET current_run_lease_id = NULL, status = 'queued', execution_status = 'queued',
		       state_version = state_version + 1, queue_timestamp = now()
		 WHERE org_id = $1 AND id = $2 AND current_run_lease_id = $3
	`, ids.orgID, ids.runID, resumeLeaseID); err != nil {
		ackLossTx.Rollback(ctx) //nolint:errcheck
		t.Fatal(err)
	}
	if _, err := ackLossTx.Exec(ctx, `
		UPDATE run_leases
		   SET state = 'lost', terminal_at = now(), terminal_reason_code = 'lease_expired', updated_at = now()
		 WHERE org_id = $1 AND run_id = $2 AND id = $3
	`, ids.orgID, ids.runID, resumeLeaseID); err != nil {
		ackLossTx.Rollback(ctx) //nolint:errcheck
		t.Fatal(err)
	}
	_, err = db.New(ackLossTx).MarkRunResumeWaitResumed(ctx, db.MarkRunResumeWaitResumedParams{
		ResumeRequestVersion: 1,
		OrgID:                pgvalue.UUID(ids.orgID),
		RunID:                pgvalue.UUID(ids.runID),
		RunWaitID:            pgvalue.UUID(runWaitID),
		RunLeaseID:           lease.ID,
		RunCheckpointID:      pgvalue.UUID(checkpointID),
	})
	if !errors.Is(err, pgx.ErrNoRows) {
		ackLossTx.Rollback(ctx) //nolint:errcheck
		t.Fatalf("resume acknowledgement after lease loss error = %v, want pgx.ErrNoRows", err)
	}
	var lostRaceState db.RunWaitState
	var lostRaceAckVersion int64
	if err := ackLossTx.QueryRow(ctx, `SELECT state, resume_ack_version FROM run_waits WHERE id = $1`, runWaitID).Scan(&lostRaceState, &lostRaceAckVersion); err != nil {
		ackLossTx.Rollback(ctx) //nolint:errcheck
		t.Fatal(err)
	}
	if lostRaceState != db.RunWaitStateResuming || lostRaceAckVersion != 0 {
		ackLossTx.Rollback(ctx) //nolint:errcheck
		t.Fatalf("lease-loss ack race mutated wait to %s/%d, want resuming/0", lostRaceState, lostRaceAckVersion)
	}
	if err := ackLossTx.Rollback(ctx); err != nil {
		t.Fatal(err)
	}

	acknowledged, err := queries.MarkRunResumeWaitResumed(ctx, db.MarkRunResumeWaitResumedParams{
		ResumeRequestVersion: 1,
		OrgID:                pgvalue.UUID(ids.orgID),
		RunID:                pgvalue.UUID(ids.runID),
		RunWaitID:            pgvalue.UUID(runWaitID),
		RunLeaseID:           lease.ID,
		RunCheckpointID:      pgvalue.UUID(checkpointID),
	})
	if err != nil {
		t.Fatal(err)
	}
	if acknowledged.State != db.RunWaitStateReleased || acknowledged.ResumeAckVersion != 1 {
		t.Fatalf("acknowledged wait = %s/%d, want released/1", acknowledged.State, acknowledged.ResumeAckVersion)
	}

	var status db.RunStatus
	var executionStatus db.RunExecutionStatus
	var stateVersion int64
	if err := pool.QueryRow(ctx, `SELECT status, execution_status, state_version FROM runs WHERE id = $1`, ids.runID).Scan(&status, &executionStatus, &stateVersion); err != nil {
		t.Fatal(err)
	}
	if status != db.RunStatusRunning || executionStatus != db.RunExecutionStatusExecuting || stateVersion != 9 {
		t.Fatalf("run = %s/%s version %d, want running/executing version 9", status, executionStatus, stateVersion)
	}
}

func TestRunWaitResolutionSerializesBeforeCheckpointClaim(t *testing.T) {
	ctx := context.Background()
	pool := newIntegrationDB(t, ctx)
	ids := seedIntegration(t, ctx, pool)
	fixture := seedWorkspaceRuntimeFixture(t, ctx, ids, true, pool)
	queries := db.New(pool)

	var groupID string
	if err := pool.QueryRow(ctx, `SELECT worker_group_id FROM worker_instances WHERE id = $1`, fixture.workerID).Scan(&groupID); err != nil {
		t.Fatal(err)
	}
	runLeaseID := uuid.Must(uuid.NewV7())
	mustExec(t, ctx, pool, `
		INSERT INTO run_leases (
			id, org_id, project_id, environment_id, run_id, workspace_id, region_id,
			lease_sequence, task_attempt_number, worker_group_id, worker_instance_id,
			worker_epoch, runtime_instance_id, network_slot_id, network_slot_generation,
			queue_name, queue_class, runtime_identity_id, requested_cpu_millis,
			requested_memory_bytes, requested_workload_disk_bytes, requested_scratch_bytes,
			requested_execution_slots, state, assigned_at, start_deadline_at, claimed_at,
			started_at, renewed_at, expires_at
		) VALUES (
			$1, $2, $3, $4, $5, $6, 'us-east-1', 1, 1, $7, $8, 1, $9, $10, 1,
			'default', 'run', 'test-runtime', 1000, 1073741824, 4294967296, 0, 1,
			'running', now() - interval '3 minutes', now() + interval '2 minutes',
			now() - interval '2 minutes', now() - interval '1 minute', now(),
			now() + interval '5 minutes'
		)
	`, runLeaseID, ids.orgID, ids.projectID, ids.environmentID, ids.runID,
		ids.workspaceID, groupID, fixture.workerID, fixture.runtimeID, fixture.slotID)
	mustExec(t, ctx, pool, `
		UPDATE runs
		   SET status = 'waiting', execution_status = 'waiting',
		       current_run_lease_id = $1, state_version = 5
		 WHERE org_id = $2 AND id = $3
	`, runLeaseID, ids.orgID, ids.runID)

	waitID := uuid.Must(uuid.NewV7())
	runWaitID := uuid.Must(uuid.NewV7())
	mustExec(t, ctx, pool, `
		INSERT INTO waits (
			id, public_id, org_id, project_id, environment_id, kind,
			completed_after
		) VALUES ($1, $2, $3, $4, $5, 'timer', now() - interval '1 second')
	`, waitID, testPublicID(t, publicid.Wait), ids.orgID, ids.projectID, ids.environmentID)
	mustExec(t, ctx, pool, `
		INSERT INTO run_waits (
			id, org_id, project_id, environment_id, run_id, wait_id, state,
			expected_run_state_version, current_run_lease_id,
			run_checkpoint_due_at, hot_wait_started_at
		) VALUES ($1, $2, $3, $4, $5, $6, 'hot_waiting', 5, $7, now() - interval '1 second', now())
	`, runWaitID, ids.orgID, ids.projectID, ids.environmentID, ids.runID, waitID, runLeaseID)

	resolutionTx, err := pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer resolutionTx.Rollback(ctx) //nolint:errcheck
	if _, err := resolutionTx.Exec(ctx, `
		UPDATE waits
		   SET state = 'completed', result = 'null', completed_at = now(), updated_at = now()
		 WHERE org_id = $1 AND id = $2
	`, ids.orgID, waitID); err != nil {
		t.Fatal(err)
	}

	claimResult := make(chan error, 1)
	go func() {
		_, claimErr := queries.ClaimRunCheckpointWait(ctx, db.ClaimRunCheckpointWaitParams{
			OrgID:                   pgvalue.UUID(ids.orgID),
			RunID:                   pgvalue.UUID(ids.runID),
			RunWaitID:               pgvalue.UUID(runWaitID),
			RunLeaseID:              pgvalue.UUID(runLeaseID),
			ExpectedRunStateVersion: 5,
		})
		claimResult <- claimErr
	}()

	select {
	case claimErr := <-claimResult:
		t.Fatalf("checkpoint claim completed before wait resolution committed: %v", claimErr)
	case <-time.After(100 * time.Millisecond):
	}
	if err := resolutionTx.Commit(ctx); err != nil {
		t.Fatal(err)
	}
	select {
	case claimErr := <-claimResult:
		if !errors.Is(claimErr, pgx.ErrNoRows) {
			t.Fatalf("checkpoint claim error = %v, want pgx.ErrNoRows after resolution", claimErr)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("checkpoint claim remained blocked after wait resolution committed")
	}
}
