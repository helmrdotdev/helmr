package dispatch

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/google/uuid"
	"github.com/helmrdotdev/helmr/internal/db"
	"github.com/helmrdotdev/helmr/internal/pgvalue"
	"github.com/helmrdotdev/helmr/internal/workspace"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
)

func (d *Authority) grantFreshRun(
	ctx context.Context,
	candidate ReadyRunCandidate,
	expectedMount runWorkspaceMount,
	observationFreshAfter pgtype.Timestamptz,
) (db.RunLease, error) {
	tx, err := d.begin(ctx)
	if err != nil {
		return db.RunLease{}, fmt.Errorf("begin Run Lease grant: %w", err)
	}
	defer rollback(ctx, tx)

	if err := lockRunQueueScope(ctx, tx, candidate); err != nil {
		return db.RunLease{}, classifyRunCandidateError(err)
	}
	authority, err := lockFreshRunAuthority(ctx, tx, candidate)
	if err != nil {
		return db.RunLease{}, classifyRunCandidateError(err)
	}
	runtime, err := discoverRunRuntime(ctx, tx, authority.workspaceID)
	if err != nil {
		return db.RunLease{}, err
	}
	if runtime.id != expectedMount.runtimeID ||
		runtime.workerID != expectedMount.workerID ||
		runtime.workerEpoch != expectedMount.epoch {
		return db.RunLease{}, ErrCapacityUnavailable
	}
	if err := lockWorkerFence(ctx, tx, workerFence{
		GroupID:               runtime.groupID,
		RegionID:              authority.regionID,
		WorkerInstanceID:      runtime.workerID,
		WorkerEpoch:           runtime.workerEpoch,
		WorkerProtocolVersion: runtime.protocolVersion,
		ObservationFreshAfter: observationFreshAfter,
		Role:                  "run",
		RunArchitecture:       authority.architecture,
	}); err != nil {
		return db.RunLease{}, ErrCapacityUnavailable
	}
	runtime, err = lockRunRuntime(ctx, tx, runtime)
	if err != nil {
		return db.RunLease{}, err
	}
	if err := validateRunRuntime(authority, runtime); err != nil ||
		runtime.observedState != db.RuntimeObservedStateReady ||
		runtime.networkSlotState != db.WorkerNetworkSlotStateBound {
		return db.RunLease{}, ErrCapacityUnavailable
	}
	mount, err := getActiveRunMount(ctx, tx, authority, runtime)
	if err != nil {
		return db.RunLease{}, err
	}
	if mount.id != expectedMount.id ||
		mount.state != db.WorkspaceMountStateMounted {
		return db.RunLease{}, ErrCapacityUnavailable
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
		return db.RunLease{}, fmt.Errorf("allocate Run Lease sequence: %w", err)
	}
	runLeaseID := pgvalue.UUID(uuid.Must(uuid.NewV7()))
	workspaceLeaseUUID := uuid.Must(uuid.NewV7())
	workspaceLeaseID := pgvalue.UUID(workspaceLeaseUUID)
	workspaceUUID, err := pgvalue.UUIDValue(authority.workspaceID)
	if err != nil {
		return db.RunLease{}, fmt.Errorf("decode Workspace ID: %w", err)
	}
	writerGeneration := authority.writerGeneration + 1
	mountGeneration := mount.fencingGeneration + 1
	capability, err := d.fencingKeys.DeriveActive(workspace.FenceInput{
		LeaseID:                workspaceLeaseUUID,
		WorkspaceID:            workspaceUUID,
		OwnershipGeneration:    authority.ownershipGeneration,
		WriterGeneration:       writerGeneration,
		MountFencingGeneration: mountGeneration,
	})
	if err != nil {
		return db.RunLease{}, fmt.Errorf("derive Workspace Lease fence: %w", err)
	}
	q := db.New(tx)
	leaseExpiresAt := pgtype.Timestamptz{
		Time:  grantedAt.Add(d.runPolicy.LeaseTTL),
		Valid: true,
	}
	lease, err := q.InsertAssignedRunLease(
		ctx,
		db.InsertAssignedRunLeaseParams{
			ID:                         runLeaseID,
			OrgID:                      authority.orgID,
			ProjectID:                  authority.projectID,
			EnvironmentID:              authority.environmentID,
			RunID:                      authority.runID,
			WorkspaceID:                authority.workspaceID,
			RegionID:                   authority.regionID,
			LeaseSequence:              leaseSequence,
			AttemptNumber:              authority.attemptNumber,
			WorkerGroupID:              runtime.groupID,
			WorkerInstanceID:           runtime.workerID,
			WorkerEpoch:                runtime.workerEpoch,
			RuntimeInstanceID:          runtime.id,
			NetworkSlotID:              runtime.networkSlotID,
			NetworkSlotGeneration:      runtime.networkSlotGeneration,
			RuntimeIdentityID:          runtime.runtimeIdentityID,
			WorkerProtocolVersion:      runtime.protocolVersion,
			RequestedCpuMillis:         authority.resources.cpuMillis,
			RequestedMemoryBytes:       authority.resources.memoryBytes,
			RequestedWorkloadDiskBytes: authority.resources.workloadDisk,
			RequestedScratchBytes:      authority.resources.scratchBytes,
			RequestedExecutionSlots:    authority.resources.executionSlots,
			TraceID:                    authority.traceID,
			SpanID: pgtype.Text{
				String: authority.rootSpanID,
				Valid:  authority.rootSpanID != "",
			},
			StartDeadlineAt: pgtype.Timestamptz{
				Time:  grantedAt.Add(d.runPolicy.StartDeadline),
				Valid: true,
			},
			ExpiresAt: leaseExpiresAt,
		},
	)
	if err != nil {
		if isConstraintConflict(err) {
			return db.RunLease{}, ErrCapacityUnavailable
		}
		return db.RunLease{}, fmt.Errorf("insert Run Lease: %w", err)
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
		return db.RunLease{}, fmt.Errorf("advance Workspace writer: %w", err)
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
		return db.RunLease{}, fmt.Errorf("advance Workspace Mount fence: %w", err)
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
			FencingKeyFingerprint:  capability.KeyFingerprint.Bytes(),
			FencingTokenHash:       capability.Hash,
			ExpiresAt:              leaseExpiresAt,
		},
	); err != nil {
		if isConstraintConflict(err) {
			return db.RunLease{}, ErrCapacityUnavailable
		}
		return db.RunLease{}, fmt.Errorf("insert Workspace Lease: %w", err)
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
			return db.RunLease{}, fmt.Errorf("consume Run runtime reservation: %w", err)
		}
		if consumed != 1 {
			return db.RunLease{}, ErrCapacityUnavailable
		}
	}
	if _, err := q.SetRunCurrentLease(
		ctx,
		db.SetRunCurrentLeaseParams{
			RunLeaseID:           runLeaseID,
			ID:                   authority.runID,
			OrgID:                authority.orgID,
			ExpectedStateVersion: authority.stateVersion,
			AttemptNumber:        authority.attemptNumber,
		},
	); err != nil {
		return db.RunLease{}, fmt.Errorf("set current Run Lease: %w", err)
	}
	if err := tx.Commit(ctx); err != nil {
		return db.RunLease{}, fmt.Errorf("commit Run Lease grant: %w", err)
	}
	return lease, nil
}

func (d *Authority) checkRunLeaseConcurrency(
	ctx context.Context,
	tx pgx.Tx,
	authority freshRunAuthority,
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
		return fmt.Errorf("read Run Lease concurrency: %w", err)
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
       <= worker_instances.max_run_consumers
  FROM worker_instances
  LEFT JOIN run_leases
    ON run_leases.worker_instance_id = worker_instances.id
   AND run_leases.worker_epoch = worker_instances.current_epoch
   AND run_leases.state IN ('assigned', 'starting', 'running', 'checkpointing', 'finalizing')
 WHERE worker_instances.id = $1
   AND worker_instances.current_epoch = $2
 GROUP BY worker_instances.max_run_consumers`,
		runtime.workerID,
		runtime.workerEpoch,
		runtime.executionSlots,
	).Scan(&available)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return ErrCapacityUnavailable
		}
		return fmt.Errorf("check Run consumer capacity: %w", err)
	}
	if !available {
		return ErrCapacityUnavailable
	}
	return nil
}
