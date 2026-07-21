package dispatch

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/google/uuid"
	"github.com/helmrdotdev/helmr/internal/db"
	"github.com/helmrdotdev/helmr/internal/pgvalue"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgtype"
)

type placeRunParams struct {
	Lease                 db.LeaseRunLeaseParams
	WorkspaceMountID      pgtype.UUID
	ObservationFreshAfter pgtype.Timestamptz
}

type ReadyRunCandidate struct {
	OrgID                   pgtype.UUID
	RunID                   pgtype.UUID
	ExpectedRunStateVersion int64
}

type ReadyRunPlacement struct {
	Lease             db.LeaseRunLeaseRow
	LeaseCreated      bool
	WorkspaceMountID  pgtype.UUID
	WorkerInstanceID  pgtype.UUID
	WorkerEpoch       int64
	RuntimeInstanceID pgtype.UUID
}

func isUniqueViolation(err error) bool {
	var pgErr *pgconn.PgError
	return errors.As(err, &pgErr) && pgErr.Code == "23505"
}

// PlaceReadyRun turns a reconstructable ready-index candidate into durable
// authority. Candidate discovery is intentionally outside the authority
// transaction; PlaceRun relocks and rechecks every durable fence.
func (d *Authority) PlaceReadyRun(ctx context.Context, candidate ReadyRunCandidate, observationFreshAfter pgtype.Timestamptz) (ReadyRunPlacement, error) {
	candidateStillQueued, err := d.readyRunCandidateExists(ctx, candidate)
	if err != nil {
		return ReadyRunPlacement{}, err
	}
	if !candidateStillQueued {
		return ReadyRunPlacement{}, ErrCandidateChanged
	}
	mount, err := d.ensureReadyRunWorkspace(ctx, candidate, observationFreshAfter)
	if err != nil {
		return ReadyRunPlacement{}, err
	}
	placement := ReadyRunPlacement{
		WorkspaceMountID: mount.id, WorkerInstanceID: mount.workerID,
		WorkerEpoch: mount.workerEpoch, RuntimeInstanceID: mount.runtimeID,
	}
	if mount.state != db.WorkspaceMountStateMounted {
		return placement, nil
	}
	var groupID, protocolVersion string
	var workerID, runtimeID, slotID pgtype.UUID
	var workerEpoch, slotGeneration, leaseSequence int64
	err = d.pool.QueryRow(ctx, `
SELECT runtime_instances.worker_group_id, runtime_instances.worker_instance_id,
       runtime_instances.worker_epoch, runtime_instances.id,
       worker_network_slots.id, worker_network_slots.generation,
       worker_instances.protocol_version,
       COALESCE((SELECT max(lease_sequence) FROM run_leases WHERE run_id = runs.id), 0) + 1
  FROM runs
  JOIN workspaces ON workspaces.org_id = runs.org_id
                 AND workspaces.project_id = runs.project_id
                 AND workspaces.environment_id = runs.environment_id
                 AND workspaces.id = runs.workspace_id
  JOIN runtime_instances
    ON runtime_instances.org_id = runs.org_id
   AND runtime_instances.project_id = runs.project_id
   AND runtime_instances.environment_id = runs.environment_id
   AND runtime_instances.region_id = workspaces.region_id
   AND runtime_instances.runtime_identity_id = runs.runtime_identity_id
   AND runtime_instances.deployment_sandbox_id = workspaces.deployment_sandbox_id
   AND runtime_instances.id = $5
   AND runtime_instances.observed_state = 'ready'
   AND runtime_instances.workspace_id = runs.workspace_id
   AND runtime_instances.workspace_version_id = workspaces.current_version_id
   AND runtime_instances.reserved_cpu_millis >= runs.requested_milli_cpu
   AND runtime_instances.reserved_memory_bytes >= runs.requested_memory_mib * 1024 * 1024
   AND runtime_instances.reserved_workload_disk_bytes >= runs.requested_disk_mib * 1024 * 1024
   AND runtime_instances.reserved_execution_slots >= runs.requested_execution_slots
  JOIN worker_network_slots
    ON worker_network_slots.runtime_instance_id = runtime_instances.id
   AND worker_network_slots.worker_instance_id = runtime_instances.worker_instance_id
   AND worker_network_slots.worker_epoch = runtime_instances.worker_epoch
   AND worker_network_slots.state = 'bound'
  JOIN workspace_mounts
    ON workspace_mounts.org_id = runs.org_id
   AND workspace_mounts.project_id = runs.project_id
   AND workspace_mounts.environment_id = runs.environment_id
   AND workspace_mounts.workspace_id = runs.workspace_id
   AND workspace_mounts.id = $6
   AND workspace_mounts.runtime_instance_id = runtime_instances.id
   AND workspace_mounts.state = 'mounted'
  JOIN worker_instances
    ON worker_instances.id = runtime_instances.worker_instance_id
   AND worker_instances.worker_group_id = runtime_instances.worker_group_id
   AND worker_instances.current_epoch = runtime_instances.worker_epoch
   AND worker_instances.state = 'active'
   AND worker_instances.supports_run
  JOIN worker_groups
    ON worker_groups.id = runtime_instances.worker_group_id
   AND worker_groups.region_id = runtime_instances.region_id
   AND worker_groups.state = 'active'
   AND worker_groups.allows_run
   AND worker_groups.protocol_version = worker_instances.protocol_version
  JOIN worker_observations
    ON worker_observations.worker_instance_id = worker_instances.id
   AND worker_observations.worker_epoch = worker_instances.current_epoch
   AND worker_observations.observed_at >= $3
   AND worker_observations.run_paused_reason IS NULL
 WHERE runs.org_id = $1 AND runs.id = $2
   AND runs.state_version = $4
   AND runs.status = 'queued' AND runs.current_run_lease_id IS NULL
   AND runs.queue_timestamp <= now()
   AND (runs.queued_expires_at IS NULL OR runs.queued_expires_at > now())
   AND NOT EXISTS (
       SELECT 1 FROM run_leases
        WHERE run_leases.runtime_instance_id = runtime_instances.id
          AND run_leases.state IN ('assigned','starting','running','checkpointing'))
   AND NOT EXISTS (
       SELECT 1 FROM workspace_leases
        WHERE workspace_leases.workspace_id = runs.workspace_id
          AND workspace_leases.state IN ('active','releasing'))
 ORDER BY worker_network_slots.slot_name, worker_network_slots.id
 LIMIT 1`, candidate.OrgID, candidate.RunID, observationFreshAfter,
		candidate.ExpectedRunStateVersion, mount.runtimeID, mount.id).Scan(&groupID, &workerID, &workerEpoch,
		&runtimeID, &slotID, &slotGeneration, &protocolVersion, &leaseSequence)
	if err != nil {
		if err == pgx.ErrNoRows {
			candidateStillQueued, revalidateErr := d.readyRunCandidateExists(ctx, candidate)
			if revalidateErr != nil {
				return ReadyRunPlacement{}, revalidateErr
			}
			if !candidateStillQueued {
				return ReadyRunPlacement{}, ErrCandidateChanged
			}
			return ReadyRunPlacement{}, ErrCapacityUnavailable
		}
		return ReadyRunPlacement{}, fmt.Errorf("discover mounted run placement: %w", err)
	}

	now := time.Now().UTC()
	snapshot, _ := json.Marshal(map[string]any{"source": "dispatcher", "run_state_version": candidate.ExpectedRunStateVersion})
	row, err := d.placeRun(ctx, placeRunParams{
		ObservationFreshAfter: observationFreshAfter,
		WorkspaceMountID:      mount.id,
		Lease: db.LeaseRunLeaseParams{
			OrgID: candidate.OrgID, RunID: candidate.RunID,
			ExpectedRunStateVersion: candidate.ExpectedRunStateVersion,
			RunLeaseID:              pgvalue.UUID(uuid.Must(uuid.NewV7())), LeaseSequence: leaseSequence,
			WorkerGroupID: groupID, WorkerInstanceID: workerID, WorkerEpoch: workerEpoch,
			RuntimeInstanceID: runtimeID, NetworkSlotID: slotID,
			NetworkSlotGeneration: slotGeneration, WorkerProtocolVersion: protocolVersion,
			RequestedScratchBytes: 0, ResourceSnapshot: snapshot,
			StartDeadlineAt: pgvalue.Timestamptz(now.Add(time.Minute)),
			ExpiresAt:       pgvalue.Timestamptz(now.Add(5 * time.Minute)),
		},
	})
	if err != nil && (errors.Is(err, pgx.ErrNoRows) || errors.Is(err, ErrCandidateChanged)) {
		candidateStillQueued, revalidateErr := d.readyRunCandidateExists(ctx, candidate)
		if revalidateErr != nil {
			return ReadyRunPlacement{}, revalidateErr
		}
		if !candidateStillQueued {
			return ReadyRunPlacement{}, ErrCandidateChanged
		}
	}
	if err != nil {
		return ReadyRunPlacement{}, err
	}
	placement.Lease = row
	placement.LeaseCreated = true
	return placement, nil
}

type runWorkspaceMount struct {
	id, workerID, runtimeID pgtype.UUID
	workerEpoch             int64
	state                   db.WorkspaceMountState
}

func (d *Authority) ensureReadyRunWorkspace(ctx context.Context, candidate ReadyRunCandidate, observationFreshAfter pgtype.Timestamptz) (runWorkspaceMount, error) {
	tx, err := d.begin(ctx)
	if err != nil {
		return runWorkspaceMount{}, fmt.Errorf("begin run workspace preparation: %w", err)
	}
	defer rollback(ctx, tx)

	var workspaceID, workspaceVersionID pgtype.UUID
	if err := tx.QueryRow(ctx, `SELECT workspace_id FROM runs WHERE org_id = $1 AND id = $2`,
		candidate.OrgID, candidate.RunID).Scan(&workspaceID); err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return runWorkspaceMount{}, ErrCandidateChanged
		}
		return runWorkspaceMount{}, fmt.Errorf("discover run workspace preparation source: %w", err)
	}
	if _, err := tx.Exec(ctx, `SELECT pg_advisory_xact_lock(hashtextextended(concat_ws(':', 'workspace-mount', $1::uuid::text, $2::uuid::text), 0))`, candidate.OrgID, workspaceID); err != nil {
		return runWorkspaceMount{}, fmt.Errorf("lock run workspace mount key: %w", err)
	}
	if err := tx.QueryRow(ctx, `
SELECT runs.workspace_id, workspaces.current_version_id
  FROM runs
  JOIN workspaces ON workspaces.org_id = runs.org_id
                 AND workspaces.project_id = runs.project_id
                 AND workspaces.environment_id = runs.environment_id
                 AND workspaces.id = runs.workspace_id
 WHERE runs.org_id = $1 AND runs.id = $2 AND runs.state_version = $3
   AND runs.status = 'queued' AND runs.current_run_lease_id IS NULL
   AND runs.queue_timestamp <= now()
   AND (runs.queued_expires_at IS NULL OR runs.queued_expires_at > now())
 FOR UPDATE OF runs, workspaces`, candidate.OrgID, candidate.RunID,
		candidate.ExpectedRunStateVersion).Scan(&workspaceID, &workspaceVersionID); err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return runWorkspaceMount{}, ErrCandidateChanged
		}
		return runWorkspaceMount{}, fmt.Errorf("lock run workspace preparation source: %w", err)
	}
	var mount runWorkspaceMount
	err = tx.QueryRow(ctx, `
SELECT workspace_mounts.id, workspace_mounts.worker_instance_id,
       workspace_mounts.worker_epoch, workspace_mounts.runtime_instance_id,
       workspace_mounts.state
  FROM workspace_mounts
  JOIN runtime_instances ON runtime_instances.id = workspace_mounts.runtime_instance_id
                        AND runtime_instances.worker_instance_id = workspace_mounts.worker_instance_id
                        AND runtime_instances.worker_epoch = workspace_mounts.worker_epoch
 WHERE workspace_mounts.org_id = $1 AND workspace_mounts.workspace_id = $2
   AND workspace_mounts.state IN ('mounting','mounted','unmounting')
 FOR UPDATE OF workspace_mounts`, candidate.OrgID, workspaceID).Scan(
		&mount.id, &mount.workerID, &mount.workerEpoch, &mount.runtimeID, &mount.state)
	if err == nil {
		if err := tx.Commit(ctx); err != nil {
			return runWorkspaceMount{}, fmt.Errorf("commit existing run workspace preparation: %w", err)
		}
		return mount, nil
	}
	if !errors.Is(err, pgx.ErrNoRows) {
		return runWorkspaceMount{}, fmt.Errorf("discover active run workspace mount: %w", err)
	}

	var groupID string
	var runtimeID, workerID pgtype.UUID
	var workerEpoch int64
	err = tx.QueryRow(ctx, `
SELECT runtime_instances.worker_group_id, runtime_instances.worker_instance_id,
       runtime_instances.worker_epoch, runtime_instances.id
  FROM runs
  JOIN workspaces ON workspaces.org_id = runs.org_id
                 AND workspaces.project_id = runs.project_id
                 AND workspaces.environment_id = runs.environment_id
                 AND workspaces.id = runs.workspace_id
  JOIN runtime_instances ON runtime_instances.org_id = runs.org_id
                        AND runtime_instances.project_id = runs.project_id
                        AND runtime_instances.environment_id = runs.environment_id
                        AND runtime_instances.region_id = workspaces.region_id
                        AND runtime_instances.runtime_identity_id = runs.runtime_identity_id
                        AND runtime_instances.deployment_sandbox_id = workspaces.deployment_sandbox_id
                        AND runtime_instances.observed_state = 'ready'
                        AND runtime_instances.workspace_id IS NULL
                        AND runtime_instances.reserved_workspace_id IS NULL
                        AND runtime_instances.reserved_cpu_millis >= runs.requested_milli_cpu
                        AND runtime_instances.reserved_memory_bytes >= runs.requested_memory_mib * 1024 * 1024
                        AND runtime_instances.reserved_workload_disk_bytes >= runs.requested_disk_mib * 1024 * 1024
                        AND runtime_instances.reserved_execution_slots >= runs.requested_execution_slots
  JOIN worker_network_slots ON worker_network_slots.runtime_instance_id = runtime_instances.id
                    AND worker_network_slots.worker_instance_id = runtime_instances.worker_instance_id
                    AND worker_network_slots.worker_epoch = runtime_instances.worker_epoch
                    AND worker_network_slots.state = 'bound'
  JOIN worker_instances ON worker_instances.id = runtime_instances.worker_instance_id
                       AND worker_instances.worker_group_id = runtime_instances.worker_group_id
                       AND worker_instances.current_epoch = runtime_instances.worker_epoch
                       AND worker_instances.state = 'active' AND worker_instances.supports_run
  JOIN worker_groups ON worker_groups.id = runtime_instances.worker_group_id
                    AND worker_groups.region_id = runtime_instances.region_id
                    AND worker_groups.state = 'active' AND worker_groups.allows_run
                    AND worker_groups.protocol_version = worker_instances.protocol_version
  JOIN worker_observations ON worker_observations.worker_instance_id = worker_instances.id
                          AND worker_observations.worker_epoch = worker_instances.current_epoch
                          AND worker_observations.observed_at >= $4
                          AND worker_observations.run_paused_reason IS NULL
 WHERE runs.org_id = $1 AND runs.id = $2 AND runs.state_version = $3
   AND runs.status = 'queued' AND runs.current_run_lease_id IS NULL
 ORDER BY runtime_instances.ready_at, runtime_instances.id
 LIMIT 1 FOR UPDATE OF runtime_instances`, candidate.OrgID, candidate.RunID,
		candidate.ExpectedRunStateVersion, observationFreshAfter).Scan(
		&groupID, &workerID, &workerEpoch, &runtimeID)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return runWorkspaceMount{}, ErrCapacityUnavailable
		}
		return runWorkspaceMount{}, fmt.Errorf("discover run workspace runtime: %w", err)
	}
	result, err := tx.Exec(ctx, `
UPDATE runtime_instances
   SET workspace_id = $2, workspace_version_id = $3, updated_at = now()
 WHERE id = $1 AND worker_group_id = $4 AND worker_instance_id = $5
   AND worker_epoch = $6 AND observed_state = 'ready'
   AND workspace_id IS NULL AND reserved_workspace_id IS NULL`, runtimeID,
		workspaceID, workspaceVersionID, groupID, workerID, workerEpoch)
	if err != nil {
		return runWorkspaceMount{}, fmt.Errorf("bind run workspace runtime: %w", err)
	}
	if result.RowsAffected() != 1 {
		return runWorkspaceMount{}, ErrCapacityUnavailable
	}
	request, _ := json.Marshal(map[string]string{"source": "dispatcher"})
	ensured, err := db.New(tx).EnsureWorkspaceMountRequested(ctx, db.EnsureWorkspaceMountRequestedParams{
		ID: pgvalue.UUID(uuid.Must(uuid.NewV7())), Priority: 0, Request: request,
		OrgID: candidate.OrgID, WorkspaceID: workspaceID,
	})
	if err != nil {
		return runWorkspaceMount{}, fmt.Errorf("create run workspace mount: %w", err)
	}
	if err := tx.Commit(ctx); err != nil {
		return runWorkspaceMount{}, fmt.Errorf("commit run workspace preparation: %w", err)
	}
	return runWorkspaceMount{id: ensured.ID, workerID: ensured.WorkerInstanceID,
		workerEpoch: ensured.WorkerEpoch, runtimeID: ensured.RuntimeInstanceID,
		state: ensured.State}, nil
}

func (d *Authority) readyRunCandidateExists(ctx context.Context, candidate ReadyRunCandidate) (bool, error) {
	var exists bool
	if err := d.pool.QueryRow(ctx, `
SELECT EXISTS (
    SELECT 1 FROM runs
     WHERE org_id = $1 AND id = $2 AND state_version = $3
       AND status = 'queued' AND current_run_lease_id IS NULL
       AND queue_timestamp <= now()
       AND (queued_expires_at IS NULL OR queued_expires_at > now())
)`, candidate.OrgID, candidate.RunID, candidate.ExpectedRunStateVersion).Scan(&exists); err != nil {
		return false, fmt.Errorf("revalidate ready run candidate: %w", err)
	}
	return exists, nil
}

// PlaceRun grants direct run authority after taking the normative advisory,
// worker, source, runtime, and network-slot locks in that order.
func (d *Authority) placeRun(ctx context.Context, params placeRunParams) (db.LeaseRunLeaseRow, error) {
	tx, err := d.begin(ctx)
	if err != nil {
		return db.LeaseRunLeaseRow{}, fmt.Errorf("begin run placement: %w", err)
	}
	defer rollback(ctx, tx)

	var projectID, environmentID, workspaceID pgtype.UUID
	var regionID, queueClass, queueName string
	var concurrencyKey pgtype.Text
	err = tx.QueryRow(ctx, `
SELECT runs.project_id, runs.environment_id, runs.workspace_id, workspaces.region_id,
       runs.queue_class, runs.queue_name, runs.concurrency_key
  FROM runs
  JOIN workspaces ON workspaces.org_id = runs.org_id
                 AND workspaces.project_id = runs.project_id
                 AND workspaces.environment_id = runs.environment_id
                 AND workspaces.id = runs.workspace_id
 WHERE runs.org_id = $1 AND runs.id = $2`, params.Lease.OrgID, params.Lease.RunID).Scan(
		&projectID, &environmentID, &workspaceID, &regionID, &queueClass, &queueName, &concurrencyKey)
	if err != nil {
		return db.LeaseRunLeaseRow{}, fmt.Errorf("discover run placement scope: %w", err)
	}

	q := db.New(tx)
	if err := q.LockRunLeaseConcurrencyScope(ctx, db.LockRunLeaseConcurrencyScopeParams{
		OrgID: params.Lease.OrgID, ProjectID: projectID, EnvironmentID: environmentID,
		QueueClass: queueClass, QueueName: queueName, ConcurrencyKey: concurrencyKey,
	}); err != nil {
		return db.LeaseRunLeaseRow{}, fmt.Errorf("lock run concurrency scope: %w", err)
	}
	if _, err := tx.Exec(ctx, `SELECT pg_advisory_xact_lock(hashtextextended(concat_ws(':', 'workspace-writer', $1::uuid::text, $2::uuid::text), 0))`, params.Lease.OrgID, workspaceID); err != nil {
		return db.LeaseRunLeaseRow{}, fmt.Errorf("lock run workspace writer key: %w", err)
	}
	if err := lockSource(ctx, tx, "run", params.Lease.RunID); err != nil {
		return db.LeaseRunLeaseRow{}, err
	}
	if err := lockWorkerFence(ctx, tx, workerFence{
		GroupID: params.Lease.WorkerGroupID, RegionID: regionID,
		WorkerInstanceID: params.Lease.WorkerInstanceID, WorkerEpoch: params.Lease.WorkerEpoch,
		WorkerProtocolVersion: params.Lease.WorkerProtocolVersion,
		ObservationFreshAfter: params.ObservationFreshAfter, Role: "run",
	}); err != nil {
		return db.LeaseRunLeaseRow{}, err
	}

	// Lock the logical source before physical children. The generated placement
	// query repeats the source predicate and therefore closes discovery races.
	var runLocked bool
	if err := tx.QueryRow(ctx, `SELECT true FROM runs WHERE org_id = $1 AND id = $2 FOR UPDATE`, params.Lease.OrgID, params.Lease.RunID).Scan(&runLocked); err != nil {
		return db.LeaseRunLeaseRow{}, fmt.Errorf("lock run: %w", err)
	}
	var runtimeLocked bool
	if err := tx.QueryRow(ctx, `
SELECT true
  FROM runtime_instances
  JOIN runs ON runs.org_id = runtime_instances.org_id
           AND runs.project_id = runtime_instances.project_id
           AND runs.environment_id = runtime_instances.environment_id
           AND runs.id = $6
  JOIN workspaces ON workspaces.org_id = runs.org_id
                 AND workspaces.project_id = runs.project_id
                 AND workspaces.environment_id = runs.environment_id
                 AND workspaces.id = runs.workspace_id
 WHERE runtime_instances.id = $1 AND runtime_instances.worker_group_id = $2
   AND runtime_instances.worker_instance_id = $3
   AND runtime_instances.worker_epoch = $4 AND runtime_instances.region_id = $5
   AND runtime_instances.runtime_identity_id = runs.runtime_identity_id
   AND runtime_instances.deployment_sandbox_id = workspaces.deployment_sandbox_id
	AND runtime_instances.workspace_id = runs.workspace_id
	AND runtime_instances.workspace_version_id = workspaces.current_version_id
   AND runtime_instances.reserved_cpu_millis >= runs.requested_milli_cpu
   AND runtime_instances.reserved_memory_bytes >= runs.requested_memory_mib * 1024 * 1024
   AND runtime_instances.reserved_workload_disk_bytes >= runs.requested_disk_mib * 1024 * 1024
   AND runtime_instances.reserved_execution_slots >= runs.requested_execution_slots
   AND runtime_instances.observed_state IN ('allocated','preparing','ready')
 FOR UPDATE OF runtime_instances`, params.Lease.RuntimeInstanceID, params.Lease.WorkerGroupID,
		params.Lease.WorkerInstanceID, params.Lease.WorkerEpoch, regionID,
		params.Lease.RunID).Scan(&runtimeLocked); err != nil {
		return db.LeaseRunLeaseRow{}, fmt.Errorf("lock run runtime: %w", err)
	}
	var slotOK bool
	err = tx.QueryRow(ctx, `
SELECT runtime_instance_id = $4
  FROM worker_network_slots
 WHERE id = $1 AND worker_instance_id = $2 AND worker_epoch = $3
   AND generation = $5 AND state = 'bound'
 FOR UPDATE`, params.Lease.NetworkSlotID, params.Lease.WorkerInstanceID,
		params.Lease.WorkerEpoch, params.Lease.RuntimeInstanceID,
		params.Lease.NetworkSlotGeneration).Scan(&slotOK)
	if err != nil || !slotOK {
		if err == nil {
			err = fmt.Errorf("network slot owner mismatch")
		}
		return db.LeaseRunLeaseRow{}, fmt.Errorf("lock run network slot: %w", err)
	}
	var mountLocked bool
	if err := tx.QueryRow(ctx, `
SELECT true
  FROM workspace_mounts
 WHERE org_id = $1 AND workspace_id = (SELECT workspace_id FROM runs WHERE org_id = $1 AND id = $2)
   AND id = $3 AND worker_group_id = $4 AND worker_instance_id = $5
   AND worker_epoch = $6 AND runtime_instance_id = $7 AND state = 'mounted'
 FOR UPDATE`, params.Lease.OrgID, params.Lease.RunID, params.WorkspaceMountID,
		params.Lease.WorkerGroupID, params.Lease.WorkerInstanceID,
		params.Lease.WorkerEpoch, params.Lease.RuntimeInstanceID).Scan(&mountLocked); err != nil {
		return db.LeaseRunLeaseRow{}, fmt.Errorf("lock run workspace mount: %w", err)
	}
	var hasConsumerCapacity bool
	err = tx.QueryRow(ctx, `
SELECT count(run_leases.id) < worker_instances.max_run_consumers
  FROM worker_instances
  LEFT JOIN run_leases
    ON run_leases.worker_instance_id = worker_instances.id
   AND run_leases.worker_epoch = worker_instances.current_epoch
   AND run_leases.state IN ('assigned','starting','running','checkpointing')
 WHERE worker_instances.id = $1 AND worker_instances.current_epoch = $2
 GROUP BY worker_instances.max_run_consumers`, params.Lease.WorkerInstanceID,
		params.Lease.WorkerEpoch).Scan(&hasConsumerCapacity)
	if err != nil {
		return db.LeaseRunLeaseRow{}, fmt.Errorf("check run consumer capacity: %w", err)
	}
	if !hasConsumerCapacity {
		return db.LeaseRunLeaseRow{}, ErrCapacityUnavailable
	}
	if _, err := q.AcquireWorkspaceWriteLease(ctx, db.AcquireWorkspaceWriteLeaseParams{
		ID: pgvalue.UUID(uuid.Must(uuid.NewV7())), OwnerRunID: params.Lease.RunID,
		OwnerProcessID: pgtype.UUID{}, AcquiredVersionID: pgtype.UUID{},
		FencingToken: uuid.Must(uuid.NewV7()).String(), ExpiresAt: params.Lease.ExpiresAt,
		OrgID: params.Lease.OrgID, WorkspaceID: workspaceID,
		WorkspaceMountID: params.WorkspaceMountID,
	}); err != nil {
		if isUniqueViolation(err) {
			return db.LeaseRunLeaseRow{}, ErrCapacityUnavailable
		}
		return db.LeaseRunLeaseRow{}, fmt.Errorf("acquire run workspace writer: %w", err)
	}

	row, err := q.LeaseRunLease(ctx, params.Lease)
	if err != nil {
		return db.LeaseRunLeaseRow{}, fmt.Errorf("insert run lease: %w", err)
	}
	if err := tx.Commit(ctx); err != nil {
		return db.LeaseRunLeaseRow{}, fmt.Errorf("commit run placement: %w", err)
	}
	return row, nil
}
