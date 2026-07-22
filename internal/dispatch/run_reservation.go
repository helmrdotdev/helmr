package dispatch

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/google/uuid"
	"github.com/helmrdotdev/helmr/internal/db"
	"github.com/helmrdotdev/helmr/internal/jsoncanon"
	"github.com/helmrdotdev/helmr/internal/pgvalue"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgtype"
)

type runRuntime struct {
	id                    pgtype.UUID
	groupID               string
	workerID              pgtype.UUID
	workerEpoch           int64
	protocolVersion       string
	runtimeIdentityID     string
	runtimeSubstrateID    pgtype.UUID
	deploymentDefinition  pgtype.UUID
	programDeployment     pgtype.UUID
	restoreCheckpoint     pgtype.UUID
	reservedRunID         pgtype.UUID
	reservedAttempt       pgtype.Int4
	reservedVersionID     pgtype.UUID
	reservationExpiresAt  pgtype.Timestamptz
	reservationActive     bool
	observedState         db.RuntimeObservedState
	networkPolicy         []byte
	cpuMillis             int64
	memoryBytes           int64
	workloadDiskBytes     int64
	scratchBytes          int64
	executionSlots        int32
	networkSlotID         pgtype.UUID
	networkSlotGeneration int64
	networkSlotState      db.WorkerNetworkSlotState
}

func (d *Authority) prepareRunWorkspace(
	ctx context.Context,
	candidate ReadyRunCandidate,
	observationFreshAfter pgtype.Timestamptz,
) (runWorkspaceMount, error) {
	tx, err := d.begin(ctx)
	if err != nil {
		return runWorkspaceMount{}, fmt.Errorf("begin Run preparation: %w", err)
	}
	defer rollback(ctx, tx)

	if err := lockRunQueueScope(ctx, tx, candidate); err != nil {
		return runWorkspaceMount{}, classifyRunCandidateError(err)
	}
	if err := lockRunRestoreSecrets(ctx, tx, candidate); err != nil {
		return runWorkspaceMount{}, classifyRunCandidateError(err)
	}
	authority, err := lockRunPlacementAuthority(ctx, tx, candidate)
	if err != nil {
		return runWorkspaceMount{}, classifyRunCandidateError(err)
	}
	runtime, err := discoverRunRuntime(ctx, tx, authority.workspaceID)
	if err == nil {
		mount, err := d.useRunRuntime(
			ctx,
			tx,
			authority,
			runtime,
			observationFreshAfter,
		)
		if err != nil {
			return runWorkspaceMount{}, err
		}
		if err := tx.Commit(ctx); err != nil {
			return runWorkspaceMount{}, fmt.Errorf("commit Run preparation: %w", err)
		}
		return mount, nil
	}
	if !errors.Is(err, pgx.ErrNoRows) {
		return runWorkspaceMount{}, fmt.Errorf("discover Workspace runtime: %w", err)
	}
	if err := d.checkRunPreparationBudget(ctx, tx, authority); err != nil {
		return runWorkspaceMount{}, err
	}
	worker, err := selectRunWorker(
		ctx,
		tx,
		authority,
		observationFreshAfter,
	)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return runWorkspaceMount{}, ErrCapacityUnavailable
		}
		return runWorkspaceMount{}, fmt.Errorf("select Run worker: %w", err)
	}
	if err := lockWorkerFence(ctx, tx, workerFence{
		GroupID:               worker.groupID,
		RegionID:              authority.regionID,
		WorkerInstanceID:      worker.workerID,
		WorkerEpoch:           worker.workerEpoch,
		WorkerProtocolVersion: worker.protocolVersion,
		ObservationFreshAfter: observationFreshAfter,
		Role:                  "run",
		RunArchitecture:       authority.architecture,
	}); err != nil {
		return runWorkspaceMount{}, ErrCapacityUnavailable
	}
	if err := lockRunWorkerCapacity(ctx, tx, authority, worker); err != nil {
		return runWorkspaceMount{}, err
	}
	runtimeID := pgvalue.UUID(uuid.Must(uuid.NewV7()))
	var reservedAt time.Time
	if err := tx.QueryRow(ctx, `SELECT transaction_timestamp()`).Scan(&reservedAt); err != nil {
		return runWorkspaceMount{}, fmt.Errorf("sample Run reservation time: %w", err)
	}
	row, err := db.New(tx).CreateRunRuntimeReservation(
		ctx,
		db.CreateRunRuntimeReservationParams{
			ID:                        runtimeID,
			OrgID:                     authority.orgID,
			WorkerGroupID:             worker.groupID,
			ProjectID:                 authority.projectID,
			EnvironmentID:             authority.environmentID,
			RegionID:                  authority.regionID,
			WorkerInstanceID:          worker.workerID,
			RuntimeIdentityID:         worker.runtimeIdentityID,
			DeploymentDefinitionID:    authority.workspaceDefinitionID,
			WorkerEpoch:               worker.workerEpoch,
			NetworkPolicy:             authority.networkPolicy,
			ReservedCpuMillis:         authority.resources.cpuMillis,
			ReservedMemoryBytes:       authority.resources.memoryBytes,
			ReservedWorkloadDiskBytes: authority.resources.workloadDisk,
			ReservedScratchBytes:      authority.resources.scratchBytes,
			ReservedExecutionSlots:    authority.resources.executionSlots,
			WorkspaceID:               authority.workspaceID,
			ProgramDeploymentID:       authority.deploymentID,
			RestoreCheckpointID:       authority.restoreCheckpointID,
			RunID:                     authority.runID,
			AttemptNumber: pgtype.Int4{
				Int32: authority.attemptNumber,
				Valid: true,
			},
			BaseWorkspaceVersionID: authority.baseVersionID,
			ReservationExpiresAt: pgtype.Timestamptz{
				Time:  reservedAt.Add(d.runPolicy.ReservationTTL),
				Valid: true,
			},
			NetworkSlotID:         worker.networkSlotID,
			NetworkSlotGeneration: worker.networkSlotGeneration,
		},
	)
	if err != nil {
		if isConstraintConflict(err) {
			return runWorkspaceMount{}, ErrCapacityUnavailable
		}
		return runWorkspaceMount{}, fmt.Errorf("create Run runtime reservation: %w", err)
	}
	if err := tx.Commit(ctx); err != nil {
		return runWorkspaceMount{}, fmt.Errorf("commit Run runtime reservation: %w", err)
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
	observationFreshAfter pgtype.Timestamptz,
) (runWorkspaceMount, error) {
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
		return runWorkspaceMount{}, ErrCapacityUnavailable
	}
	locked, err := lockRunRuntime(ctx, tx, runtime)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return runWorkspaceMount{}, ErrCapacityUnavailable
		}
		return runWorkspaceMount{}, fmt.Errorf("lock Workspace runtime: %w", err)
	}
	if err := validateRunRuntime(authority, locked); err != nil {
		return runWorkspaceMount{}, ErrCapacityUnavailable
	}
	mount, err := getActiveRunMount(ctx, tx, authority, locked)
	if err == nil {
		return mount, nil
	}
	if !errors.Is(err, pgx.ErrNoRows) {
		return runWorkspaceMount{}, fmt.Errorf("read active Workspace Mount: %w", err)
	}
	if locked.observedState != db.RuntimeObservedStateReady ||
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
			ID:                 pgvalue.UUID(uuid.Must(uuid.NewV7())),
			Request:            []byte(`{"kind":"run"}`),
			OrgID:              authority.orgID,
			WorkspaceID:        authority.workspaceID,
			RuntimeInstanceID:  locked.id,
			RunID:              authority.runID,
			AttemptNumber:      locked.reservedAttempt,
			WorkspaceVersionID: authority.baseVersionID,
		},
	)
	if err != nil {
		return runWorkspaceMount{}, fmt.Errorf("request Workspace Mount: %w", err)
	}
	return runWorkspaceMount{
		id:        requested.ID,
		workerID:  requested.WorkerInstanceID,
		epoch:     requested.WorkerEpoch,
		runtimeID: requested.RuntimeInstanceID,
		state:     requested.State,
	}, nil
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
	var slotID pgtype.UUID
	err := tx.QueryRow(ctx, `
SELECT id
  FROM worker_network_slots
 WHERE id = $1
   AND worker_group_id = $2
   AND worker_instance_id = $3
   AND worker_epoch = $4
   AND generation = $5
   AND runtime_instance_id = $6
   AND state IN ('assigned', 'bound')
 FOR UPDATE`,
		runtime.networkSlotID,
		runtime.groupID,
		runtime.workerID,
		runtime.workerEpoch,
		runtime.networkSlotGeneration,
		runtime.id,
	).Scan(&slotID)
	if err != nil {
		return runRuntime{}, err
	}
	return scanRunRuntime(tx.QueryRow(
		ctx,
		runRuntimeSQL(true),
		runtime.id,
		runtime.workerID,
		runtime.workerEpoch,
		runtime.networkSlotID,
		runtime.networkSlotGeneration,
	))
}

func runRuntimeSQL(lock bool) string {
	if !lock {
		return `
SELECT runtime_instances.id,
       runtime_instances.worker_group_id,
       runtime_instances.worker_instance_id,
       runtime_instances.worker_epoch,
       worker_instances.protocol_version,
       runtime_instances.runtime_identity_id,
       runtime_instances.runtime_substrate_id,
       runtime_instances.deployment_definition_id,
       runtime_instances.program_deployment_id,
       runtime_instances.restore_checkpoint_id,
       runtime_instances.reserved_run_id,
       runtime_instances.reserved_attempt_number,
       runtime_instances.reserved_workspace_version_id,
       runtime_instances.reservation_expires_at,
       coalesce(
           runtime_instances.reservation_expires_at > transaction_timestamp(),
           false
       ),
       runtime_instances.observed_state,
       runtime_instances.network_policy,
       runtime_instances.reserved_cpu_millis,
       runtime_instances.reserved_memory_bytes,
       runtime_instances.reserved_workload_disk_bytes,
       runtime_instances.reserved_scratch_bytes,
       runtime_instances.reserved_execution_slots,
       worker_network_slots.id,
       worker_network_slots.generation,
       worker_network_slots.state
  FROM runtime_instances
  JOIN worker_instances
    ON worker_instances.id = runtime_instances.worker_instance_id
   AND worker_instances.worker_group_id = runtime_instances.worker_group_id
  JOIN worker_network_slots
    ON worker_network_slots.worker_group_id = runtime_instances.worker_group_id
   AND worker_network_slots.worker_instance_id = runtime_instances.worker_instance_id
   AND worker_network_slots.worker_epoch = runtime_instances.worker_epoch
   AND worker_network_slots.runtime_instance_id = runtime_instances.id
   AND worker_network_slots.state IN ('assigned', 'bound')
 WHERE runtime_instances.workspace_id = $1
   AND runtime_instances.reclaimed_at IS NULL`
	}
	return `
SELECT runtime_instances.id,
       runtime_instances.worker_group_id,
       runtime_instances.worker_instance_id,
       runtime_instances.worker_epoch,
       worker_instances.protocol_version,
       runtime_instances.runtime_identity_id,
       runtime_instances.runtime_substrate_id,
       runtime_instances.deployment_definition_id,
       runtime_instances.program_deployment_id,
       runtime_instances.restore_checkpoint_id,
       runtime_instances.reserved_run_id,
       runtime_instances.reserved_attempt_number,
       runtime_instances.reserved_workspace_version_id,
       runtime_instances.reservation_expires_at,
       coalesce(
           runtime_instances.reservation_expires_at > transaction_timestamp(),
           false
       ),
       runtime_instances.observed_state,
       runtime_instances.network_policy,
       runtime_instances.reserved_cpu_millis,
       runtime_instances.reserved_memory_bytes,
       runtime_instances.reserved_workload_disk_bytes,
       runtime_instances.reserved_scratch_bytes,
       runtime_instances.reserved_execution_slots,
       worker_network_slots.id,
       worker_network_slots.generation,
       worker_network_slots.state
  FROM runtime_instances
  JOIN worker_instances
    ON worker_instances.id = runtime_instances.worker_instance_id
   AND worker_instances.worker_group_id = runtime_instances.worker_group_id
  JOIN worker_network_slots
    ON worker_network_slots.worker_group_id = runtime_instances.worker_group_id
   AND worker_network_slots.worker_instance_id = runtime_instances.worker_instance_id
   AND worker_network_slots.worker_epoch = runtime_instances.worker_epoch
   AND worker_network_slots.runtime_instance_id = runtime_instances.id
 WHERE runtime_instances.id = $1
   AND runtime_instances.worker_instance_id = $2
   AND runtime_instances.worker_epoch = $3
   AND runtime_instances.reclaimed_at IS NULL
   AND worker_network_slots.id = $4
   AND worker_network_slots.generation = $5
   AND worker_network_slots.state IN ('assigned', 'bound')
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
		&runtime.protocolVersion,
		&runtime.runtimeIdentityID,
		&runtime.runtimeSubstrateID,
		&runtime.deploymentDefinition,
		&runtime.programDeployment,
		&runtime.restoreCheckpoint,
		&runtime.reservedRunID,
		&runtime.reservedAttempt,
		&runtime.reservedVersionID,
		&runtime.reservationExpiresAt,
		&runtime.reservationActive,
		&runtime.observedState,
		&runtime.networkPolicy,
		&runtime.cpuMillis,
		&runtime.memoryBytes,
		&runtime.workloadDiskBytes,
		&runtime.scratchBytes,
		&runtime.executionSlots,
		&runtime.networkSlotID,
		&runtime.networkSlotGeneration,
		&runtime.networkSlotState,
	)
	return runtime, err
}

func validateRunRuntime(
	authority runPlacementAuthority,
	runtime runRuntime,
) error {
	networkPolicy, err := jsoncanon.Transform(runtime.networkPolicy)
	if err != nil {
		return fmt.Errorf("canonicalize Workspace runtime network policy: %w", err)
	}
	if runtime.deploymentDefinition != authority.workspaceDefinitionID ||
		!runtime.programDeployment.Valid ||
		runtime.programDeployment != authority.deploymentID ||
		runtime.restoreCheckpoint != authority.restoreCheckpointID ||
		runtime.cpuMillis != authority.resources.cpuMillis ||
		runtime.memoryBytes != authority.resources.memoryBytes ||
		runtime.workloadDiskBytes != authority.resources.workloadDisk ||
		runtime.scratchBytes != authority.resources.scratchBytes ||
		runtime.executionSlots != authority.resources.executionSlots ||
		!bytes.Equal(networkPolicy, authority.networkPolicy) {
		return errors.New("Workspace runtime does not match Run authority")
	}
	if authority.restoreCheckpointID.Valid &&
		(runtime.runtimeIdentityID != authority.restoreRuntimeIdentityID ||
			runtime.runtimeSubstrateID != authority.restoreSubstrateID) {
		return errors.New("Workspace runtime does not match Checkpoint source")
	}
	if runtime.reservedRunID.Valid {
		if runtime.reservedRunID != authority.runID ||
			!runtime.reservedAttempt.Valid ||
			runtime.reservedAttempt.Int32 != authority.attemptNumber ||
			runtime.reservedVersionID != authority.baseVersionID ||
			!runtime.reservationExpiresAt.Valid ||
			!runtime.reservationActive {
			return errors.New("Workspace runtime reservation does not match Run")
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
	groupID               string
	workerID              pgtype.UUID
	workerEpoch           int64
	protocolVersion       string
	runtimeIdentityID     string
	networkSlotID         pgtype.UUID
	networkSlotGeneration int64
}

func selectRunWorker(
	ctx context.Context,
	tx pgx.Tx,
	authority runPlacementAuthority,
	observationFreshAfter pgtype.Timestamptz,
) (runWorker, error) {
	var worker runWorker
	err := tx.QueryRow(ctx, `
SELECT worker_groups.id,
       worker_instances.id,
       worker_instances.current_epoch,
       worker_instances.protocol_version,
       worker_instances.runtime_identity_id,
       worker_network_slots.id,
       worker_network_slots.generation
  FROM worker_groups
  JOIN worker_instances
    ON worker_instances.worker_group_id = worker_groups.id
   AND worker_instances.state = 'active'
   AND worker_instances.supports_run
   AND worker_instances.certified_at IS NOT NULL
   AND worker_instances.protocol_version = worker_groups.protocol_version
  JOIN runtime_identities
    ON runtime_identities.id = worker_instances.runtime_identity_id
   AND runtime_identities.runtime_arch = $2
   AND ($8::text = '' OR runtime_identities.id = $8)
  JOIN worker_observations
    ON worker_observations.worker_instance_id = worker_instances.id
   AND worker_observations.worker_epoch = worker_instances.current_epoch
   AND worker_observations.observed_at >= $3
   AND worker_observations.run_paused_reason IS NULL
  JOIN worker_network_slots
    ON worker_network_slots.worker_group_id = worker_groups.id
   AND worker_network_slots.worker_instance_id = worker_instances.id
   AND worker_network_slots.worker_epoch = worker_instances.current_epoch
   AND worker_network_slots.state = 'available'
   AND worker_network_slots.runtime_instance_id IS NULL
 CROSS JOIN LATERAL (
     SELECT
         coalesce((
             SELECT sum(reserved_cpu_millis)
               FROM runtime_instances
              WHERE worker_instance_id = worker_instances.id
                AND worker_epoch = worker_instances.current_epoch
                AND reclaimed_at IS NULL
         ), 0) + coalesce((
             SELECT sum(requested_cpu_millis)
               FROM deployment_build_leases
              WHERE worker_instance_id = worker_instances.id
                AND worker_epoch = worker_instances.current_epoch
                AND state IN ('assigned', 'starting', 'running')
         ), 0) AS cpu_millis,
         coalesce((
             SELECT sum(reserved_memory_bytes)
               FROM runtime_instances
              WHERE worker_instance_id = worker_instances.id
                AND worker_epoch = worker_instances.current_epoch
                AND reclaimed_at IS NULL
         ), 0) + coalesce((
             SELECT sum(requested_memory_bytes)
               FROM deployment_build_leases
              WHERE worker_instance_id = worker_instances.id
                AND worker_epoch = worker_instances.current_epoch
                AND state IN ('assigned', 'starting', 'running')
         ), 0) AS memory_bytes,
         coalesce((
             SELECT sum(reserved_workload_disk_bytes)
               FROM runtime_instances
              WHERE worker_instance_id = worker_instances.id
                AND worker_epoch = worker_instances.current_epoch
                AND reclaimed_at IS NULL
         ), 0) + coalesce((
             SELECT sum(requested_workload_disk_bytes)
               FROM deployment_build_leases
              WHERE worker_instance_id = worker_instances.id
                AND worker_epoch = worker_instances.current_epoch
                AND state IN ('assigned', 'starting', 'running')
         ), 0) AS workload_disk_bytes,
         coalesce((
             SELECT sum(reserved_scratch_bytes)
               FROM runtime_instances
              WHERE worker_instance_id = worker_instances.id
                AND worker_epoch = worker_instances.current_epoch
                AND reclaimed_at IS NULL
         ), 0) + coalesce((
             SELECT sum(requested_scratch_bytes)
               FROM deployment_build_leases
              WHERE worker_instance_id = worker_instances.id
                AND worker_epoch = worker_instances.current_epoch
                AND state IN ('assigned', 'starting', 'running')
         ), 0) AS scratch_bytes
 ) AS usage
 WHERE worker_groups.region_id = $1
   AND worker_groups.state = 'active'
   AND worker_groups.allows_run
   AND worker_instances.per_vm_cpu_millis >= $4
   AND worker_instances.per_vm_memory_bytes >= $5
   AND worker_instances.per_vm_workload_disk_bytes >= $6
   AND worker_instances.per_vm_scratch_bytes >= $7
   AND worker_instances.certified_cpu_millis - usage.cpu_millis >= $4
   AND worker_instances.certified_memory_bytes - usage.memory_bytes >= $5
   AND worker_instances.certified_workload_disk_bytes - usage.workload_disk_bytes >= $6
   AND worker_instances.certified_scratch_bytes - usage.scratch_bytes >= $7
   AND ($9::text = '' OR worker_instances.substrate_format = $9)
   AND ($10::text = '' OR worker_instances.substrate_builder_abi = $10)
   AND ($11::text = '' OR worker_instances.substrate_layout_abi = $11)
   AND worker_instances.max_vm_slots > (
       SELECT count(*)
         FROM runtime_instances
        WHERE runtime_instances.worker_instance_id = worker_instances.id
          AND runtime_instances.worker_epoch = worker_instances.current_epoch
          AND (
              runtime_instances.observed_state IN ('allocated', 'preparing', 'ready', 'closing')
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
          AND runtime_instances.observed_state IN ('allocated', 'preparing')
   )
 ORDER BY worker_instances.updated_at, worker_instances.id,
          worker_network_slots.slot_name, worker_network_slots.id
 LIMIT 1`,
		authority.regionID,
		authority.architecture,
		observationFreshAfter,
		authority.resources.cpuMillis,
		authority.resources.memoryBytes,
		authority.resources.workloadDisk,
		authority.resources.scratchBytes,
		authority.restoreRuntimeIdentityID,
		authority.restoreSubstrateFormat,
		authority.restoreSubstrateBuilder,
		authority.restoreSubstrateLayout,
	).Scan(
		&worker.groupID,
		&worker.workerID,
		&worker.workerEpoch,
		&worker.protocolVersion,
		&worker.runtimeIdentityID,
		&worker.networkSlotID,
		&worker.networkSlotGeneration,
	)
	return worker, err
}

func lockRunWorkerCapacity(
	ctx context.Context,
	tx pgx.Tx,
	authority runPlacementAuthority,
	worker runWorker,
) error {
	var available bool
	err := tx.QueryRow(ctx, `
SELECT worker_network_slots.state = 'available'
       AND worker_network_slots.runtime_instance_id IS NULL
       AND worker_instances.per_vm_cpu_millis >= $6
       AND worker_instances.per_vm_memory_bytes >= $7
       AND worker_instances.per_vm_workload_disk_bytes >= $8
       AND worker_instances.per_vm_scratch_bytes >= $9
       AND worker_instances.max_vm_slots > (
           SELECT count(*)
             FROM runtime_instances
            WHERE runtime_instances.worker_instance_id = worker_instances.id
              AND runtime_instances.worker_epoch = worker_instances.current_epoch
              AND (
                  runtime_instances.observed_state IN ('allocated', 'preparing', 'ready', 'closing')
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
              AND runtime_instances.observed_state IN ('allocated', 'preparing')
       )
       AND worker_instances.certified_cpu_millis - usage.cpu_millis >= $6
       AND worker_instances.certified_memory_bytes - usage.memory_bytes >= $7
       AND worker_instances.certified_workload_disk_bytes - usage.workload_disk_bytes >= $8
       AND worker_instances.certified_scratch_bytes - usage.scratch_bytes >= $9
  FROM worker_instances
  JOIN worker_network_slots
   ON worker_network_slots.worker_group_id = worker_instances.worker_group_id
   AND worker_network_slots.worker_instance_id = worker_instances.id
   AND worker_network_slots.worker_epoch = worker_instances.current_epoch
 CROSS JOIN LATERAL (
     SELECT
         coalesce((
             SELECT sum(reserved_cpu_millis)
               FROM runtime_instances
              WHERE worker_instance_id = worker_instances.id
                AND worker_epoch = worker_instances.current_epoch
                AND reclaimed_at IS NULL
         ), 0) + coalesce((
             SELECT sum(requested_cpu_millis)
               FROM deployment_build_leases
              WHERE worker_instance_id = worker_instances.id
                AND worker_epoch = worker_instances.current_epoch
                AND state IN ('assigned', 'starting', 'running')
         ), 0) AS cpu_millis,
         coalesce((
             SELECT sum(reserved_memory_bytes)
               FROM runtime_instances
              WHERE worker_instance_id = worker_instances.id
                AND worker_epoch = worker_instances.current_epoch
                AND reclaimed_at IS NULL
         ), 0) + coalesce((
             SELECT sum(requested_memory_bytes)
               FROM deployment_build_leases
              WHERE worker_instance_id = worker_instances.id
                AND worker_epoch = worker_instances.current_epoch
                AND state IN ('assigned', 'starting', 'running')
         ), 0) AS memory_bytes,
         coalesce((
             SELECT sum(reserved_workload_disk_bytes)
               FROM runtime_instances
              WHERE worker_instance_id = worker_instances.id
                AND worker_epoch = worker_instances.current_epoch
                AND reclaimed_at IS NULL
         ), 0) + coalesce((
             SELECT sum(requested_workload_disk_bytes)
               FROM deployment_build_leases
              WHERE worker_instance_id = worker_instances.id
                AND worker_epoch = worker_instances.current_epoch
                AND state IN ('assigned', 'starting', 'running')
         ), 0) AS workload_disk_bytes,
         coalesce((
             SELECT sum(reserved_scratch_bytes)
               FROM runtime_instances
              WHERE worker_instance_id = worker_instances.id
                AND worker_epoch = worker_instances.current_epoch
                AND reclaimed_at IS NULL
         ), 0) + coalesce((
             SELECT sum(requested_scratch_bytes)
               FROM deployment_build_leases
              WHERE worker_instance_id = worker_instances.id
                AND worker_epoch = worker_instances.current_epoch
                AND state IN ('assigned', 'starting', 'running')
         ), 0) AS scratch_bytes
 ) AS usage
 WHERE worker_instances.id = $1
   AND worker_instances.worker_group_id = $2
   AND worker_instances.current_epoch = $3
   AND worker_network_slots.id = $4
   AND worker_network_slots.generation = $5
 FOR UPDATE OF worker_network_slots`,
		worker.workerID,
		worker.groupID,
		worker.workerEpoch,
		worker.networkSlotID,
		worker.networkSlotGeneration,
		authority.resources.cpuMillis,
		authority.resources.memoryBytes,
		authority.resources.workloadDisk,
		authority.resources.scratchBytes,
	).Scan(&available)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return ErrCapacityUnavailable
		}
		return fmt.Errorf("lock Run worker capacity: %w", err)
	}
	if !available {
		return ErrCapacityUnavailable
	}
	return nil
}

func (d *Authority) checkRunPreparationBudget(
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
		return fmt.Errorf("read Run preparation budget: %w", err)
	}
	limit := d.runPolicy.PreparationLimit
	if authority.queueLimit.Valid && authority.queueLimit.Int64 < limit {
		limit = authority.queueLimit.Int64
	}
	if pinnedLimit.Valid && pinnedLimit.Int64 < limit {
		limit = pinnedLimit.Int64
	}
	if active >= limit {
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
