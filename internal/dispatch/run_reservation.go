package dispatch

import (
	"context"
	"errors"
	"fmt"
	"time"
	"uuid"

	"github.com/helmrdotdev/helmr/internal/capacity"
	"github.com/helmrdotdev/helmr/internal/db"
	"github.com/helmrdotdev/helmr/internal/pgvalue"
	"github.com/helmrdotdev/helmr/internal/run"
	"github.com/helmrdotdev/helmr/internal/workerapi"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgtype"
)

type runRuntime struct {
	id                      pgtype.UUID
	groupID                 string
	workerID                pgtype.UUID
	workerEpoch             int64
	runtimeIdentityID       string
	runtimeSubstrateID      pgtype.UUID
	deploymentDefinition    pgtype.UUID
	programDeployment       pgtype.UUID
	restoreCheckpoint       pgtype.UUID
	reservedRunID           pgtype.UUID
	reservedAttempt         pgtype.Int4
	reservedProcessID       pgtype.UUID
	reservedVersionID       pgtype.UUID
	reservationExpiresAt    pgtype.Timestamptz
	reservationActive       bool
	desiredState            db.RuntimeDesiredState
	desiredVersion          int64
	observedState           db.RuntimeObservedState
	observedDesiredVersion  int64
	cpuMillis               int64
	memoryBytes             int64
	guestEphemeralDiskBytes int64
	executionSlots          int32
}

func (d *Authority) prepareRunWorkspace(
	ctx context.Context,
	candidate ReadyRunCandidate,
) (runWorkspaceMount, error) {
	tx, err := d.begin(ctx)
	if err != nil {
		return runWorkspaceMount{}, fmt.Errorf("begin run preparation: %w", err)
	}
	defer rollback(ctx, tx)

	environmentID, queueName, concurrencyKey, err := lockRunQueueScope(ctx, tx, candidate)
	if err != nil {
		return runWorkspaceMount{}, classifyRunCandidateError(err)
	}
	if err := lockRunSecrets(ctx, tx, candidate); err != nil {
		return runWorkspaceMount{}, classifyRunCandidateError(err)
	}
	authority, err := lockRunPlacementAuthority(ctx, tx, candidate)
	if err != nil {
		return runWorkspaceMount{}, classifyRunCandidateError(err)
	}
	if authority.environmentID != environmentID ||
		authority.queueName != queueName ||
		authority.concurrencyKey != concurrencyKey {
		return runWorkspaceMount{}, ErrCandidateChanged
	}
	runtime, err := discoverRunRuntime(ctx, tx, authority.workspaceID)
	if err == nil {
		mount, err := d.useRunRuntime(
			ctx,
			tx,
			authority,
			runtime,
		)
		if err != nil {
			return runWorkspaceMount{}, err
		}
		if err := tx.Commit(ctx); err != nil {
			return runWorkspaceMount{}, fmt.Errorf("commit run preparation: %w", err)
		}
		return mount, nil
	}
	if !errors.Is(err, pgx.ErrNoRows) {
		return runWorkspaceMount{}, fmt.Errorf("discover workspace runtime: %w", err)
	}
	if err := d.checkRunQueuePreparationConcurrency(ctx, tx, authority); err != nil {
		return runWorkspaceMount{}, err
	}
	worker, err := selectRunWorker(
		ctx,
		tx,
		authority,
	)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			pressured, pressureErr := requestRunCapacityPressure(ctx, tx, authority)
			if pressureErr != nil {
				return runWorkspaceMount{}, pressureErr
			}
			if pressured {
				if err := tx.Commit(ctx); err != nil {
					return runWorkspaceMount{}, fmt.Errorf("commit run capacity pressure: %w", err)
				}
			}
			return runWorkspaceMount{}, ErrCapacityUnavailable
		}
		return runWorkspaceMount{}, fmt.Errorf("select run worker: %w", err)
	}
	if err := lockWorkerFence(ctx, tx, workerFence{
		GroupID:          worker.groupID,
		RegionID:         authority.regionID,
		WorkerInstanceID: worker.workerID,
		WorkerEpoch:      worker.workerEpoch,
		RunArchitecture:  authority.architecture,
		RequirePrimary:   !authority.restoreCheckpointID.Valid,
	}); err != nil {
		return runWorkspaceMount{}, ErrCapacityUnavailable
	}
	if err := lockRunWorkerCapacity(ctx, tx, authority, worker); err != nil {
		return runWorkspaceMount{}, err
	}
	runtimeID := pgvalue.UUID(uuid.NewV7())
	var reservedAt time.Time
	if err := tx.QueryRow(ctx, `SELECT transaction_timestamp()`).Scan(&reservedAt); err != nil {
		return runWorkspaceMount{}, fmt.Errorf("sample run reservation time: %w", err)
	}
	row, err := db.New(tx).CreateRunRuntimeReservation(
		ctx,
		db.CreateRunRuntimeReservationParams{
			ID:                              runtimeID,
			OrgID:                           authority.orgID,
			WorkerGroupID:                   worker.groupID,
			ProjectID:                       authority.projectID,
			EnvironmentID:                   authority.environmentID,
			RegionID:                        authority.regionID,
			WorkerInstanceID:                worker.workerID,
			RuntimeIdentityID:               worker.runtimeIdentityID,
			DeploymentDefinitionID:          authority.workspaceDefinitionID,
			WorkerEpoch:                     pgtype.Int8{Int64: worker.workerEpoch, Valid: true},
			ReservedCPUMillis:               authority.resources.cpuMillis,
			ReservedMemoryBytes:             authority.resources.memoryBytes,
			ReservedGuestEphemeralDiskBytes: authority.resources.guestEphemeralDiskBytes,
			ReservedExecutionSlots:          authority.resources.executionSlots,
			WorkspaceID:                     authority.workspaceID,
			ProgramDeploymentID:             authority.deploymentID,
			RestoreCheckpointID:             authority.restoreCheckpointID,
			RequiredCPUConfigDigest: pgtype.Text{
				String: authority.restoreCPUConfigDigest,
				Valid:  authority.restoreCheckpointID.Valid,
			},
			RunID: authority.runID,
			AttemptNumber: pgtype.Int4{
				Int32: authority.attemptNumber,
				Valid: true,
			},
			BaseWorkspaceVersionID: authority.baseVersionID,
			ReservationExpiresAt: pgtype.Timestamptz{
				Time:  reservedAt.Add(run.ReservationTTL),
				Valid: true,
			},
		},
	)
	if err != nil {
		if isConstraintConflict(err) {
			return runWorkspaceMount{}, ErrCapacityUnavailable
		}
		return runWorkspaceMount{}, fmt.Errorf("create run runtime reservation: %w", err)
	}
	if err := tx.Commit(ctx); err != nil {
		return runWorkspaceMount{}, fmt.Errorf("commit run runtime reservation: %w", err)
	}
	return runWorkspaceMount{
		workerID:  row.WorkerInstanceID,
		epoch:     row.WorkerEpoch,
		runtimeID: row.ID,
	}, nil
}

func (d *Authority) useRunRuntime(
	ctx context.Context,
	tx pgx.Tx,
	authority runPlacementAuthority,
	runtime runRuntime,
) (runWorkspaceMount, error) {
	if err := lockWorkerFence(ctx, tx, workerFence{
		GroupID:          runtime.groupID,
		RegionID:         authority.regionID,
		WorkerInstanceID: runtime.workerID,
		WorkerEpoch:      runtime.workerEpoch,
		RunArchitecture:  authority.architecture,
	}); err != nil {
		return runWorkspaceMount{}, ErrCapacityUnavailable
	}
	locked, err := lockRunRuntime(ctx, tx, runtime)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return runWorkspaceMount{}, ErrCapacityUnavailable
		}
		return runWorkspaceMount{}, fmt.Errorf("lock workspace runtime: %w", err)
	}
	if err := validateRunRuntime(authority, locked); err != nil {
		return runWorkspaceMount{}, ErrCapacityUnavailable
	}
	mount, err := getActiveRunMount(ctx, tx, authority, locked)
	if err == nil {
		if authority.restoreMountGeneration.Valid &&
			mount.fencingGeneration != authority.restoreMountGeneration.Int64 {
			return runWorkspaceMount{}, ErrCapacityUnavailable
		}
		return mount, nil
	}
	if !errors.Is(err, pgx.ErrNoRows) {
		return runWorkspaceMount{}, fmt.Errorf("read active workspace mount: %w", err)
	}
	if !runRuntimeReady(locked) ||
		!locked.reservedRunID.Valid ||
		locked.reservedRunID != authority.runID {
		return runWorkspaceMount{
			workerID:  locked.workerID,
			epoch:     locked.workerEpoch,
			runtimeID: locked.id,
		}, nil
	}
	requested, err := db.New(tx).EnsureRunWorkspaceMountRequested(
		ctx,
		db.EnsureRunWorkspaceMountRequestedParams{
			ID:                 pgvalue.UUID(uuid.NewV7()),
			Request:            []byte(`{"kind":"run"}`),
			OrgID:              authority.orgID,
			WorkspaceID:        authority.workspaceID,
			RuntimeInstanceID:  locked.id,
			RunID:              authority.runID,
			AttemptNumber:      locked.reservedAttempt,
			WorkspaceVersionID: authority.baseVersionID,
			FencingGeneration:  requestedRunMountGeneration(authority),
		},
	)
	if err != nil {
		return runWorkspaceMount{}, fmt.Errorf("request workspace mount: %w", err)
	}
	return runWorkspaceMount{
		id:                requested.ID,
		workerID:          requested.WorkerInstanceID,
		epoch:             requested.WorkerEpoch,
		runtimeID:         requested.RuntimeInstanceID,
		state:             requested.State,
		fencingGeneration: requested.FencingGeneration,
	}, nil
}

func requestedRunMountGeneration(authority runPlacementAuthority) int64 {
	if authority.restoreMountGeneration.Valid {
		return authority.restoreMountGeneration.Int64
	}
	return 1
}

func discoverRunRuntime(
	ctx context.Context,
	tx pgx.Tx,
	workspaceID pgtype.UUID,
) (runRuntime, error) {
	return scanRunRuntime(tx.QueryRow(ctx, runRuntimeSQL(false), workspaceID))
}

func lockRunRuntime(
	ctx context.Context,
	tx pgx.Tx,
	runtime runRuntime,
) (runRuntime, error) {
	return scanRunRuntime(tx.QueryRow(
		ctx,
		runRuntimeSQL(true),
		runtime.id,
		runtime.workerID,
		runtime.workerEpoch,
	))
}

func runRuntimeSQL(lock bool) string {
	if !lock {
		return `
SELECT runtime_instances.id,
       runtime_instances.worker_group_id,
       runtime_instances.worker_instance_id,
       runtime_instances.worker_epoch,
       runtime_instances.runtime_identity_id,
       runtime_instances.runtime_substrate_id,
       runtime_instances.deployment_definition_id,
       runtime_instances.program_deployment_id,
       runtime_instances.restore_checkpoint_id,
       runtime_instances.reserved_run_id,
       runtime_instances.reserved_attempt_number,
       runtime_instances.reserved_process_id,
       runtime_instances.reserved_workspace_version_id,
       runtime_instances.reservation_expires_at,
       coalesce(
           runtime_instances.reservation_expires_at > transaction_timestamp(),
           false
       ),
       runtime_instances.desired_state,
       runtime_instances.desired_version,
       runtime_instances.observed_state,
       runtime_instances.observed_desired_version,
       runtime_instances.reserved_cpu_millis,
       runtime_instances.reserved_memory_bytes,
       runtime_instances.reserved_guest_ephemeral_disk_bytes,
       runtime_instances.reserved_execution_slots
  FROM runtime_instances
  JOIN worker_instances
    ON worker_instances.id = runtime_instances.worker_instance_id
   AND worker_instances.worker_group_id = runtime_instances.worker_group_id
 WHERE runtime_instances.workspace_id = $1
   AND runtime_instances.reclaimed_at IS NULL`
	}
	return `
SELECT runtime_instances.id,
       runtime_instances.worker_group_id,
       runtime_instances.worker_instance_id,
       runtime_instances.worker_epoch,
       runtime_instances.runtime_identity_id,
       runtime_instances.runtime_substrate_id,
       runtime_instances.deployment_definition_id,
       runtime_instances.program_deployment_id,
       runtime_instances.restore_checkpoint_id,
       runtime_instances.reserved_run_id,
       runtime_instances.reserved_attempt_number,
       runtime_instances.reserved_process_id,
       runtime_instances.reserved_workspace_version_id,
       runtime_instances.reservation_expires_at,
       coalesce(
           runtime_instances.reservation_expires_at > transaction_timestamp(),
           false
       ),
       runtime_instances.desired_state,
       runtime_instances.desired_version,
       runtime_instances.observed_state,
       runtime_instances.observed_desired_version,
       runtime_instances.reserved_cpu_millis,
       runtime_instances.reserved_memory_bytes,
       runtime_instances.reserved_guest_ephemeral_disk_bytes,
       runtime_instances.reserved_execution_slots
  FROM runtime_instances
  JOIN worker_instances
    ON worker_instances.id = runtime_instances.worker_instance_id
   AND worker_instances.worker_group_id = runtime_instances.worker_group_id
 WHERE runtime_instances.id = $1
   AND runtime_instances.worker_instance_id = $2
   AND runtime_instances.worker_epoch = $3
   AND runtime_instances.reclaimed_at IS NULL
 FOR UPDATE OF runtime_instances`
}

type rowScanner interface {
	Scan(...any) error
}

func scanRunRuntime(row rowScanner) (runRuntime, error) {
	var runtime runRuntime
	err := row.Scan(
		&runtime.id,
		&runtime.groupID,
		&runtime.workerID,
		&runtime.workerEpoch,
		&runtime.runtimeIdentityID,
		&runtime.runtimeSubstrateID,
		&runtime.deploymentDefinition,
		&runtime.programDeployment,
		&runtime.restoreCheckpoint,
		&runtime.reservedRunID,
		&runtime.reservedAttempt,
		&runtime.reservedProcessID,
		&runtime.reservedVersionID,
		&runtime.reservationExpiresAt,
		&runtime.reservationActive,
		&runtime.desiredState,
		&runtime.desiredVersion,
		&runtime.observedState,
		&runtime.observedDesiredVersion,
		&runtime.cpuMillis,
		&runtime.memoryBytes,
		&runtime.guestEphemeralDiskBytes,
		&runtime.executionSlots,
	)
	return runtime, err
}

func runRuntimeReady(runtime runRuntime) bool {
	return runtime.desiredState == db.RuntimeDesiredStateReady &&
		runtime.observedState == db.RuntimeObservedStateReady &&
		runtime.observedDesiredVersion == runtime.desiredVersion
}

func validateRunRuntime(
	authority runPlacementAuthority,
	runtime runRuntime,
) error {
	if runtime.deploymentDefinition != authority.workspaceDefinitionID ||
		!runtime.programDeployment.Valid ||
		runtime.programDeployment != authority.deploymentID ||
		runtime.cpuMillis != authority.resources.cpuMillis ||
		runtime.memoryBytes != authority.resources.memoryBytes ||
		runtime.guestEphemeralDiskBytes != authority.resources.guestEphemeralDiskBytes ||
		runtime.executionSlots != authority.resources.executionSlots {
		return errors.New("workspace runtime does not match run authority")
	}
	if authority.restoreCheckpointID.Valid &&
		(runtime.restoreCheckpoint != authority.restoreCheckpointID ||
			runtime.runtimeIdentityID != authority.restoreRuntimeIdentityID ||
			runtime.runtimeSubstrateID != authority.restoreSubstrateID) {
		return errors.New("workspace runtime does not match checkpoint source")
	}
	if runtime.reservedProcessID.Valid {
		return errors.New("workspace runtime is reserved by a process")
	}
	if runtime.reservedRunID.Valid {
		if runtime.reservedRunID != authority.runID ||
			!runtime.reservedAttempt.Valid ||
			runtime.reservedAttempt.Int32 != authority.attemptNumber ||
			runtime.reservedVersionID != authority.baseVersionID ||
			!runtime.reservationExpiresAt.Valid ||
			!runtime.reservationActive {
			return errors.New("workspace runtime reservation does not match run")
		}
	}
	return nil
}

func getActiveRunMount(
	ctx context.Context,
	tx pgx.Tx,
	authority runPlacementAuthority,
	runtime runRuntime,
) (runWorkspaceMount, error) {
	var mount runWorkspaceMount
	err := tx.QueryRow(ctx, `
SELECT id, worker_instance_id, worker_epoch, runtime_instance_id, state,
       fencing_generation
  FROM workspace_mounts
 WHERE org_id = $1
   AND project_id = $2
   AND environment_id = $3
   AND region_id = $4
   AND workspace_id = $5
   AND materialized_version_id = $6
   AND worker_group_id = $7
   AND worker_instance_id = $8
   AND worker_epoch = $9
   AND runtime_instance_id = $10
   AND state IN ('mounting', 'mounted', 'unmounting')
 FOR UPDATE`,
		authority.orgID,
		authority.projectID,
		authority.environmentID,
		authority.regionID,
		authority.workspaceID,
		authority.baseVersionID,
		runtime.groupID,
		runtime.workerID,
		runtime.workerEpoch,
		runtime.id,
	).Scan(
		&mount.id,
		&mount.workerID,
		&mount.epoch,
		&mount.runtimeID,
		&mount.state,
		&mount.fencingGeneration,
	)
	return mount, err
}

type runWorker struct {
	groupID           string
	workerID          pgtype.UUID
	workerEpoch       int64
	runtimeIdentityID string
}

func selectRunWorker(
	ctx context.Context,
	tx pgx.Tx,
	authority runPlacementAuthority,
) (runWorker, error) {
	rows, err := db.New(tx).ListWorkerCapacityBins(ctx, db.ListWorkerCapacityBinsParams{
		RegionID: authority.regionID, ObservationFreshnessSeconds: workerapi.WorkerObservationFreshnessSeconds,
	})
	if err != nil {
		return runWorker{}, err
	}
	selected, ok := capacity.SelectRunWorker(rows, runCapacityRequirements(authority))
	if !ok || !selected.WorkerEpoch.Valid || !selected.RuntimeIdentityID.Valid {
		return runWorker{}, pgx.ErrNoRows
	}
	return runWorker{
		groupID: selected.WorkerGroupID, workerID: selected.WorkerInstanceID,
		workerEpoch: selected.WorkerEpoch.Int64, runtimeIdentityID: selected.RuntimeIdentityID.String,
	}, nil
}

func requestRunCapacityPressure(
	ctx context.Context,
	tx pgx.Tx,
	authority runPlacementAuthority,
) (bool, error) {
	rows, err := db.New(tx).ListWorkerCapacityBins(ctx, db.ListWorkerCapacityBinsParams{
		RegionID: authority.regionID, ObservationFreshnessSeconds: workerapi.WorkerObservationFreshnessSeconds,
	})
	if err != nil {
		return false, fmt.Errorf("list run capacity pressure candidates: %w", err)
	}
	if _, available := capacity.SelectRunWorker(rows, runCapacityRequirements(authority)); available {
		return false, nil
	}
	for _, selected := range capacity.RunWorkerCapacityPressureCandidates(rows, runCapacityRequirements(authority)) {
		if !selected.WorkerEpoch.Valid || !selected.RuntimeIdentityID.Valid {
			continue
		}
		if err := lockWorkerFence(ctx, tx, workerFence{
			GroupID: selected.WorkerGroupID, RegionID: authority.regionID,
			WorkerInstanceID: selected.WorkerInstanceID, WorkerEpoch: selected.WorkerEpoch.Int64,
			RunArchitecture: authority.architecture, RequirePrimary: !authority.restoreCheckpointID.Valid,
		}); err != nil {
			if errors.Is(err, pgx.ErrNoRows) {
				continue
			}
			return false, fmt.Errorf("lock run capacity pressure worker: %w", err)
		}
		if err := checkLockedWorkerRuntimeAdmission(
			ctx, tx, selected.WorkerInstanceID, selected.WorkerEpoch.Int64,
		); err != nil {
			if errors.Is(err, pgx.ErrNoRows) {
				continue
			}
			return false, fmt.Errorf("check run capacity pressure Runtime admission: %w", err)
		}
		worker := runWorker{
			groupID: selected.WorkerGroupID, workerID: selected.WorkerInstanceID,
			workerEpoch: selected.WorkerEpoch.Int64, runtimeIdentityID: selected.RuntimeIdentityID.String,
		}
		if err := lockRunWorkerCapacity(ctx, tx, authority, worker); err == nil {
			return false, nil
		} else if !errors.Is(err, ErrCapacityUnavailable) {
			return false, fmt.Errorf("recheck run capacity pressure worker: %w", err)
		}
		mounts, err := db.New(tx).RequestCapacityPressureIdleWorkspaceMountStopsForWorker(
			ctx,
			db.RequestCapacityPressureIdleWorkspaceMountStopsForWorkerParams{
				WorkerInstanceID: selected.WorkerInstanceID,
				WorkerEpoch:      selected.WorkerEpoch.Int64,
				LimitCount:       1,
			},
		)
		if err != nil {
			return false, fmt.Errorf("request run capacity pressure Workspace stop: %w", err)
		}
		if len(mounts) > 0 {
			return true, nil
		}
	}
	return false, nil
}

func runCapacityRequirements(authority runPlacementAuthority) capacity.RunRequirements {
	return capacity.RunRequirements{
		Resources: capacity.ResourceVector{
			CPUMillis: authority.resources.cpuMillis, MemoryBytes: authority.resources.memoryBytes,
			GuestEphemeralDiskBytes: authority.resources.guestEphemeralDiskBytes, VMSlots: 1,
		},
		Architecture: authority.architecture, WorkerGroupID: authority.restoreWorkerGroupID,
		RuntimeIdentityID: authority.restoreRuntimeIdentityID, VCPUCount: authority.restoreVMVCPUCount,
		CPUConfigDigest: authority.restoreCPUConfigDigest,
		SubstrateFormat: authority.restoreSubstrateFormat, SubstrateContract: authority.restoreSubstrateContract,
	}
}

func lockRunWorkerCapacity(
	ctx context.Context,
	tx pgx.Tx,
	authority runPlacementAuthority,
	worker runWorker,
) error {
	var available bool
	err := tx.QueryRow(ctx, `
SELECT worker_instances.per_vm_cpu_millis >= $4
       AND worker_instances.per_vm_memory_bytes >= $5
       AND worker_instances.per_vm_guest_ephemeral_disk_bytes >= $6
       AND worker_instances.max_vm_slots > (
           SELECT count(*)
             FROM runtime_instances
            WHERE runtime_instances.worker_instance_id = worker_instances.id
              AND runtime_instances.worker_epoch = worker_instances.current_epoch
              AND (
                  runtime_instances.observed_state IN ('allocated', 'ready')
                  OR (
                      runtime_instances.observed_state IN ('failed', 'lost')
                      AND runtime_instances.reclaimed_at IS NULL
                  )
              )
       )
       AND worker_instances.max_runtime_starts > (
           SELECT count(*)
             FROM runtime_instances
            WHERE runtime_instances.worker_instance_id = worker_instances.id
              AND runtime_instances.worker_epoch = worker_instances.current_epoch
              AND runtime_instances.observed_state = 'allocated'
       )
       AND worker_instances.epoch_cpu_millis - usage.cpu_millis >= $4
       AND worker_instances.epoch_memory_bytes - usage.memory_bytes >= $5
       AND worker_instances.epoch_guest_ephemeral_disk_bytes - usage.guest_ephemeral_disk_bytes >= $6
  FROM worker_instances
 CROSS JOIN LATERAL (
     SELECT
         coalesce((
             SELECT sum(reserved_cpu_millis)
               FROM runtime_instances
              WHERE worker_instance_id = worker_instances.id
                AND worker_epoch = worker_instances.current_epoch
                AND reclaimed_at IS NULL
	         ), 0) AS cpu_millis,
         coalesce((
             SELECT sum(reserved_memory_bytes)
               FROM runtime_instances
              WHERE worker_instance_id = worker_instances.id
                AND worker_epoch = worker_instances.current_epoch
                AND reclaimed_at IS NULL
	         ), 0) AS memory_bytes,
         coalesce((
             SELECT sum(reserved_guest_ephemeral_disk_bytes)
               FROM runtime_instances
              WHERE worker_instance_id = worker_instances.id
                AND worker_epoch = worker_instances.current_epoch
                AND reclaimed_at IS NULL
	         ), 0) AS guest_ephemeral_disk_bytes
 ) AS usage
 WHERE worker_instances.id = $1
   AND worker_instances.worker_group_id = $2
   AND worker_instances.current_epoch = $3
 FOR UPDATE OF worker_instances`,
		worker.workerID,
		worker.groupID,
		worker.workerEpoch,
		authority.resources.cpuMillis,
		authority.resources.memoryBytes,
		authority.resources.guestEphemeralDiskBytes,
	).Scan(&available)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return ErrCapacityUnavailable
		}
		return fmt.Errorf("lock run worker capacity: %w", err)
	}
	if !available {
		return ErrCapacityUnavailable
	}
	return nil
}

func (d *Authority) checkRunQueuePreparationConcurrency(
	ctx context.Context,
	tx pgx.Tx,
	authority runPlacementAuthority,
) error {
	var active int64
	var pinnedLimit pgtype.Int8
	err := tx.QueryRow(ctx, `
SELECT count(*),
       min(prepared_runs.queue_concurrency_limit)
  FROM runtime_instances
  JOIN runs AS prepared_runs
    ON prepared_runs.environment_id = runtime_instances.environment_id
   AND prepared_runs.id = runtime_instances.reserved_run_id
 WHERE runtime_instances.environment_id = $1
   AND prepared_runs.queue_name = $2
   AND prepared_runs.concurrency_key IS NOT DISTINCT FROM $3::text
   AND runtime_instances.reserved_run_id IS NOT NULL
   AND runtime_instances.reclaimed_at IS NULL`,
		authority.environmentID,
		authority.queueName,
		authority.concurrencyKey,
	).Scan(&active, &pinnedLimit)
	if err != nil {
		return fmt.Errorf("read run preparation budget: %w", err)
	}
	limit := authority.queueLimit
	if pinnedLimit.Valid && (!limit.Valid || pinnedLimit.Int64 < limit.Int64) {
		limit = pinnedLimit
	}
	if limit.Valid && active >= limit.Int64 {
		return ErrCapacityUnavailable
	}
	return nil
}

func classifyRunCandidateError(err error) error {
	if errors.Is(err, pgx.ErrNoRows) {
		return ErrCandidateChanged
	}
	return err
}

func isConstraintConflict(err error) bool {
	var pgErr *pgconn.PgError
	return errors.As(err, &pgErr) && pgErr.Code == "23505"
}
