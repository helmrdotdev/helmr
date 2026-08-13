package dispatch

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/google/uuid"
	"github.com/helmrdotdev/helmr/internal/db"
	"github.com/helmrdotdev/helmr/internal/pgvalue"
	"github.com/helmrdotdev/helmr/internal/run"
	"github.com/helmrdotdev/helmr/internal/workspace"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
)

func (d *Authority) grantFreshRun(
	ctx context.Context,
	candidate ReadyRunCandidate,
	expectedMount runWorkspaceMount,
) (db.RunLease, error) {
	tx, err := d.begin(ctx)
	if err != nil {
		return db.RunLease{}, fmt.Errorf("begin run lease grant: %w", err)
	}
	defer rollback(ctx, tx)

	environmentID, queueName, concurrencyKey, err := lockRunQueueScope(ctx, tx, candidate)
	if err != nil {
		return db.RunLease{}, classifyRunCandidateError(err)
	}
	if err := lockRunSecrets(ctx, tx, candidate); err != nil {
		return db.RunLease{}, classifyRunCandidateError(err)
	}
	authority, err := lockRunPlacementAuthority(ctx, tx, candidate)
	if err != nil {
		return db.RunLease{}, classifyRunCandidateError(err)
	}
	if authority.environmentID != environmentID ||
		authority.queueName != queueName ||
		authority.concurrencyKey != concurrencyKey {
		return db.RunLease{}, ErrCandidateChanged
	}
	retainedRuntimeID, hasRetainedHandoff := authority.retainedHandoffRuntimeID()
	retainedHandoff := hasRetainedHandoff &&
		expectedMount.runtimeID == retainedRuntimeID
	if retainedHandoff && authority.handoffChildWaitID.Valid &&
		!authority.sameWorkspaceResume &&
		(expectedMount.runtimeID != authority.handoffRuntimeID ||
			expectedMount.id != authority.handoffWorkspaceMountID ||
			expectedMount.fencingGeneration != authority.handoffMountGeneration.Int64) {
		return db.RunLease{}, ErrCapacityUnavailable
	}
	if retainedHandoff && authority.sameWorkspaceResume &&
		(expectedMount.runtimeID != authority.resumeHandoffRuntimeID ||
			expectedMount.id != authority.resumeHandoffMountID) {
		return db.RunLease{}, ErrCapacityUnavailable
	}
	var runtime runRuntime
	if retainedHandoff {
		runtime, err = discoverHandoffRunRuntime(
			ctx,
			tx,
			authority.workspaceID,
			retainedRuntimeID,
		)
	} else {
		runtime, err = discoverRunRuntime(ctx, tx, authority.workspaceID)
	}
	if err != nil {
		return db.RunLease{}, err
	}
	if runtime.id != expectedMount.runtimeID ||
		runtime.workerID != expectedMount.workerID ||
		runtime.workerEpoch != expectedMount.epoch {
		return db.RunLease{}, ErrCapacityUnavailable
	}
	if err := lockWorkerFence(ctx, tx, workerFence{
		GroupID:          runtime.groupID,
		RegionID:         authority.regionID,
		WorkerInstanceID: runtime.workerID,
		WorkerEpoch:      runtime.workerEpoch,
		RunArchitecture:  authority.architecture,
	}); err != nil {
		return db.RunLease{}, ErrCapacityUnavailable
	}
	runtime, err = lockRunRuntime(ctx, tx, runtime)
	if err != nil {
		return db.RunLease{}, err
	}
	if err := validateRunRuntime(authority, runtime); err != nil ||
		!runRuntimeReady(runtime) {
		return db.RunLease{}, ErrCapacityUnavailable
	}
	mount, err := getActiveRunMount(ctx, tx, authority, runtime)
	if err != nil {
		return db.RunLease{}, err
	}
	if mount.id != expectedMount.id ||
		mount.state != db.WorkspaceMountStateMounted ||
		(authority.handoffChildWaitID.Valid && !authority.sameWorkspaceResume &&
			mount.fencingGeneration != authority.handoffMountGeneration.Int64) {
		return db.RunLease{}, ErrCapacityUnavailable
	}
	if authority.handoffChildWaitID.Valid && retainedHandoff {
		if err := lockSameWorkspaceHandoffChain(
			ctx,
			tx,
			authority,
			runtime,
			mount,
		); err != nil {
			return db.RunLease{}, err
		}
	}
	if err := d.checkRunLeaseConcurrency(ctx, tx, authority); err != nil {
		return db.RunLease{}, err
	}
	if err := checkRunConsumerCapacity(ctx, tx, runtime); err != nil {
		return db.RunLease{}, err
	}

	var grantedAt time.Time
	var leaseSequence int64
	err = tx.QueryRow(ctx, `
SELECT transaction_timestamp(),
       coalesce(max(lease_sequence), 0) + 1
  FROM run_leases
 WHERE run_id = $1`,
		authority.runID,
	).Scan(&grantedAt, &leaseSequence)
	if err != nil {
		return db.RunLease{}, fmt.Errorf("allocate run lease sequence: %w", err)
	}
	runLeaseID := pgvalue.UUID(uuid.Must(uuid.NewV7()))
	workspaceLeaseUUID := uuid.Must(uuid.NewV7())
	workspaceLeaseID := pgvalue.UUID(workspaceLeaseUUID)
	workspaceUUID, err := pgvalue.UUIDValue(authority.workspaceID)
	if err != nil {
		return db.RunLease{}, fmt.Errorf("decode workspace ID: %w", err)
	}
	writerGeneration := authority.writerGeneration + 1
	mountGeneration := mount.fencingGeneration + 1
	capability, err := d.fencingKey.Derive(workspace.FenceInput{
		LeaseID:                workspaceLeaseUUID,
		WorkspaceID:            workspaceUUID,
		OwnershipGeneration:    authority.ownershipGeneration,
		WriterGeneration:       writerGeneration,
		MountFencingGeneration: mountGeneration,
	})
	if err != nil {
		return db.RunLease{}, fmt.Errorf("derive workspace lease fence: %w", err)
	}
	q := db.New(tx)
	leaseExpiresAt := pgtype.Timestamptz{
		Time:  grantedAt.Add(run.LeaseTTL),
		Valid: true,
	}
	lease, err := q.InsertAssignedRunLease(
		ctx,
		db.InsertAssignedRunLeaseParams{
			ID:                               runLeaseID,
			OrgID:                            authority.orgID,
			ProjectID:                        authority.projectID,
			EnvironmentID:                    authority.environmentID,
			RunID:                            authority.runID,
			WorkspaceID:                      authority.workspaceID,
			RegionID:                         authority.regionID,
			LeaseSequence:                    leaseSequence,
			AttemptNumber:                    authority.attemptNumber,
			WorkerGroupID:                    runtime.groupID,
			WorkerInstanceID:                 runtime.workerID,
			WorkerEpoch:                      runtime.workerEpoch,
			RuntimeInstanceID:                runtime.id,
			RuntimeIdentityID:                runtime.runtimeIdentityID,
			RequestedCPUMillis:               authority.resources.cpuMillis,
			RequestedMemoryBytes:             authority.resources.memoryBytes,
			RequestedGuestEphemeralDiskBytes: authority.resources.guestEphemeralDiskBytes,
			RequestedExecutionSlots:          authority.resources.executionSlots,
			TraceID:                          authority.traceID,
			SpanID: pgtype.Text{
				String: authority.rootSpanID,
				Valid:  authority.rootSpanID != "",
			},
			StartDeadlineAt: pgtype.Timestamptz{
				Time:  grantedAt.Add(run.StartDeadline),
				Valid: true,
			},
			ExpiresAt: leaseExpiresAt,
		},
	)
	if err != nil {
		if isConstraintConflict(err) {
			return db.RunLease{}, ErrCapacityUnavailable
		}
		return db.RunLease{}, fmt.Errorf("insert run lease: %w", err)
	}
	if _, err := q.AdvanceRunWorkspaceWriter(
		ctx,
		db.AdvanceRunWorkspaceWriterParams{
			WriterGeneration:         writerGeneration,
			OrgID:                    authority.orgID,
			ProjectID:                authority.projectID,
			EnvironmentID:            authority.environmentID,
			WorkspaceID:              authority.workspaceID,
			OwnershipGeneration:      authority.ownershipGeneration,
			ExpectedWriterGeneration: authority.writerGeneration,
		},
	); err != nil {
		return db.RunLease{}, fmt.Errorf("advance workspace writer: %w", err)
	}
	if _, err := q.AdvanceRunWorkspaceMountFence(
		ctx,
		db.AdvanceRunWorkspaceMountFenceParams{
			FencingGeneration:         mountGeneration,
			ID:                        mount.id,
			OrgID:                     authority.orgID,
			ProjectID:                 authority.projectID,
			EnvironmentID:             authority.environmentID,
			RegionID:                  authority.regionID,
			WorkerGroupID:             runtime.groupID,
			WorkerInstanceID:          runtime.workerID,
			WorkerEpoch:               runtime.workerEpoch,
			RuntimeInstanceID:         runtime.id,
			WorkspaceID:               authority.workspaceID,
			BaseWorkspaceVersionID:    authority.baseVersionID,
			ExpectedFencingGeneration: mount.fencingGeneration,
		},
	); err != nil {
		return db.RunLease{}, fmt.Errorf("advance workspace mount fence: %w", err)
	}
	if _, err := q.InsertRunWorkspaceLease(
		ctx,
		db.InsertRunWorkspaceLeaseParams{
			ID:                     workspaceLeaseID,
			OrgID:                  authority.orgID,
			WorkerGroupID:          runtime.groupID,
			ProjectID:              authority.projectID,
			EnvironmentID:          authority.environmentID,
			RegionID:               authority.regionID,
			WorkerInstanceID:       runtime.workerID,
			WorkerEpoch:            runtime.workerEpoch,
			RuntimeInstanceID:      runtime.id,
			WorkspaceID:            authority.workspaceID,
			WorkspaceMountID:       mount.id,
			OwnerRunLeaseID:        runLeaseID,
			BaseVersionID:          authority.baseVersionID,
			OwnershipGeneration:    authority.ownershipGeneration,
			WriterGeneration:       writerGeneration,
			MountFencingGeneration: mountGeneration,
			FencingTokenHash:       capability.Hash,
			ExpiresAt:              leaseExpiresAt,
		},
	); err != nil {
		if isConstraintConflict(err) {
			return db.RunLease{}, ErrCapacityUnavailable
		}
		return db.RunLease{}, fmt.Errorf("insert workspace lease: %w", err)
	}
	if runtime.reservedRunID.Valid {
		consumed, err := q.ConsumeRunRuntimeReservation(
			ctx,
			db.ConsumeRunRuntimeReservationParams{
				ID:          runtime.id,
				WorkspaceID: authority.workspaceID,
				RunID:       authority.runID,
				AttemptNumber: pgtype.Int4{
					Int32: authority.attemptNumber,
					Valid: true,
				},
				BaseWorkspaceVersionID: authority.baseVersionID,
				RestoreCheckpointID:    runtime.restoreCheckpoint,
			},
		)
		if err != nil {
			return db.RunLease{}, fmt.Errorf("consume run runtime reservation: %w", err)
		}
		if consumed != 1 {
			return db.RunLease{}, ErrCapacityUnavailable
		}
	}
	grantedRun, err := q.SetRunCurrentLease(
		ctx,
		db.SetRunCurrentLeaseParams{
			RunLeaseID:           runLeaseID,
			ID:                   authority.runID,
			OrgID:                authority.orgID,
			ExpectedStateVersion: authority.stateVersion,
			AttemptNumber:        authority.attemptNumber,
		},
	)
	if err != nil {
		return db.RunLease{}, fmt.Errorf("set current run lease: %w", err)
	}
	if authority.handoffChildWaitID.Valid {
		var boundWaitID pgtype.UUID
		err = tx.QueryRow(ctx, `
UPDATE run_waits
   SET child_writer_generation = $1,
       updated_at = transaction_timestamp()
 WHERE id = $2
   AND child_run_id = $3
   AND workspace_id = $4
   AND child_parent_owned IS TRUE
   AND condition_state = 'pending'
   AND suspension_state = 'parked'
   AND current_run_lease_id IS NULL
   AND prior_run_lease_id IS NOT NULL
   AND handoff_runtime_instance_id = $5
   AND handoff_workspace_mount_id = $6
   AND handoff_mount_generation = $7
   AND ownership_generation = $8
   AND parent_writer_generation = $9
   AND child_writer_generation IS NULL
RETURNING id`,
			writerGeneration,
			authority.handoffChildWaitID,
			authority.runID,
			authority.workspaceID,
			authority.handoffRuntimeID,
			authority.handoffWorkspaceMountID,
			authority.handoffMountGeneration.Int64,
			authority.handoffOwnership.Int64,
			authority.handoffParentWriter.Int64,
		).Scan(&boundWaitID)
		if err != nil {
			return db.RunLease{}, fmt.Errorf(
				"bind same-workspace child run lease: %w",
				err,
			)
		}
	}
	if authority.resumeRunWaitID.Valid {
		var boundWaitID pgtype.UUID
		if authority.sameWorkspaceResume {
			err = tx.QueryRow(ctx, `
UPDATE run_waits
   SET suspension_state = 'resuming',
       current_run_lease_id = $1,
       expected_run_state_version = $2,
       resume_writer_generation = $3,
       updated_at = transaction_timestamp()
 WHERE id = $4
   AND run_id = $5
   AND attempt_number = $6
   AND workspace_id = $7
   AND suspension_state = 'resume_pending'
   AND current_run_lease_id IS NULL
   AND resume_request_version = $8
   AND resume_workspace_version_id = $9
   AND handoff_runtime_instance_id = $10
   AND handoff_workspace_mount_id = $11
   AND handoff_mount_generation = $12
   AND ownership_generation = $13
   AND parent_writer_generation = $14
   AND child_writer_generation = $15
   AND resume_writer_generation IS NULL
   AND (
       (condition_state = 'completed'
        AND handoff_resume_checkpoint_id = $16)
       OR
       (condition_state IN ('failed', 'cancelled')
        AND handoff_resume_checkpoint_id IS NULL
        AND suspend_checkpoint_id = $16)
   )
RETURNING id`,
				runLeaseID,
				grantedRun.StateVersion,
				writerGeneration,
				authority.resumeRunWaitID,
				authority.runID,
				authority.attemptNumber,
				authority.workspaceID,
				authority.resumeRequestVersion,
				authority.baseVersionID,
				authority.resumeHandoffRuntimeID,
				authority.resumeHandoffMountID,
				authority.resumeHandoffMountGen.Int64,
				authority.resumeHandoffOwnership.Int64,
				authority.resumeHandoffParentWriter.Int64,
				authority.resumeHandoffChildWriter.Int64,
				authority.restoreCheckpointID,
			).Scan(&boundWaitID)
		} else {
			err = tx.QueryRow(ctx, `
UPDATE run_waits
   SET suspension_state = 'resuming',
       current_run_lease_id = $1,
       expected_run_state_version = $2,
       updated_at = transaction_timestamp()
 WHERE id = $3
   AND run_id = $4
   AND attempt_number = $5
   AND workspace_id = $6
   AND suspension_state = 'resume_pending'
   AND current_run_lease_id IS NULL
   AND suspend_checkpoint_id = $7
   AND resume_request_version = $8
   AND handoff_runtime_instance_id IS NULL
   AND handoff_workspace_mount_id IS NULL
   AND handoff_resume_checkpoint_id IS NULL
RETURNING id`,
				runLeaseID,
				grantedRun.StateVersion,
				authority.resumeRunWaitID,
				authority.runID,
				authority.attemptNumber,
				authority.workspaceID,
				authority.restoreCheckpointID,
				authority.resumeRequestVersion,
			).Scan(&boundWaitID)
		}
		if err != nil {
			return db.RunLease{}, fmt.Errorf("bind resuming run wait: %w", err)
		}
	}
	if err := tx.Commit(ctx); err != nil {
		return db.RunLease{}, fmt.Errorf("commit run lease grant: %w", err)
	}
	return lease, nil
}

func (d *Authority) checkRunLeaseConcurrency(
	ctx context.Context,
	tx pgx.Tx,
	authority runPlacementAuthority,
) error {
	var active int64
	var activeLimit pgtype.Int8
	err := tx.QueryRow(ctx, `
SELECT count(*),
       min(active_runs.queue_concurrency_limit)
  FROM run_leases
  JOIN runs AS active_runs
    ON active_runs.id = run_leases.run_id
   AND active_runs.environment_id = run_leases.environment_id
 WHERE active_runs.environment_id = $1
   AND active_runs.queue_name = $2
   AND active_runs.concurrency_key IS NOT DISTINCT FROM $3::text
   AND run_leases.state IN ('assigned', 'starting', 'running', 'checkpointing', 'finalizing')`,
		authority.environmentID,
		authority.queueName,
		authority.concurrencyKey,
	).Scan(&active, &activeLimit)
	if err != nil {
		return fmt.Errorf("read run lease concurrency: %w", err)
	}
	limit := authority.queueLimit
	if activeLimit.Valid && (!limit.Valid || activeLimit.Int64 < limit.Int64) {
		limit = activeLimit
	}
	if limit.Valid && active >= limit.Int64 {
		return ErrCapacityUnavailable
	}
	return nil
}

func checkRunConsumerCapacity(
	ctx context.Context,
	tx pgx.Tx,
	runtime runRuntime,
) error {
	var available bool
	err := tx.QueryRow(ctx, `
SELECT coalesce(sum(run_leases.requested_execution_slots), 0) + $3
       <= worker_instances.max_vm_slots
  FROM worker_instances
  LEFT JOIN run_leases
    ON run_leases.worker_instance_id = worker_instances.id
   AND run_leases.worker_epoch = worker_instances.current_epoch
   AND run_leases.state IN ('assigned', 'starting', 'running', 'checkpointing', 'finalizing')
 WHERE worker_instances.id = $1
   AND worker_instances.current_epoch = $2
 GROUP BY worker_instances.max_vm_slots`,
		runtime.workerID,
		runtime.workerEpoch,
		runtime.executionSlots,
	).Scan(&available)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return ErrCapacityUnavailable
		}
		return fmt.Errorf("check run consumer capacity: %w", err)
	}
	if !available {
		return ErrCapacityUnavailable
	}
	return nil
}
