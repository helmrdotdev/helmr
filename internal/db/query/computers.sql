-- name: CreateComputerFromCurrentDeployment :one
WITH selected_definition AS (
    SELECT deployment_definitions.environment_id,
           deployment_definitions.id AS deployment_definition_id,
           deployment_definitions.computer_spec_id,
           deployment_definitions.deployment_id AS creation_deployment_id,
           deployment_definitions.declared_id AS sandbox_declared_id,
           projects.default_region_id
      FROM deployment_definitions
      JOIN environments
        ON environments.id = deployment_definitions.environment_id
       AND environments.current_deployment_id = deployment_definitions.deployment_id
      JOIN deployments
        ON deployments.environment_id = deployment_definitions.environment_id
       AND deployments.id = deployment_definitions.deployment_id
      JOIN projects
        ON projects.id = environments.project_id
       AND projects.id = sqlc.arg(project_id)
       AND environments.org_id = sqlc.arg(org_id)
     WHERE deployment_definitions.environment_id = sqlc.arg(environment_id)
       AND deployment_definitions.id = sqlc.arg(deployment_definition_id)
       AND deployment_definitions.kind = 'sandbox'
       AND deployment_definitions.declared_id = sqlc.arg(sandbox_declared_id)
), created_computer AS (
    INSERT INTO computers (
        id,
        environment_id,
        region_id,
        sandbox_declared_id,
        head_disk_version_id,
        key
    , computer_spec_id, creation_deployment_id)
    SELECT sqlc.arg(id),
           selected_definition.environment_id,
           selected_definition.default_region_id,
           selected_definition.sandbox_declared_id,
           sqlc.arg(initial_version_id),
           sqlc.narg(key), selected_definition.computer_spec_id, selected_definition.creation_deployment_id

      FROM selected_definition
    RETURNING computers.id, computers.environment_id, computers.region_id, computers.sandbox_declared_id, computers.computer_spec_id, computers.creation_deployment_id, computers.key, computers.revision, computers.writer_generation, computers.head_disk_version_id, computers.status, computers.desired_state, computers.dirty_state, computers.last_activity_at, computers.created_at, computers.updated_at, computers.deleted_at
), created_version AS (
    INSERT INTO computer_disk_versions (
        id,
        environment_id,
        computer_id,
        status,
        root_pack_digest,
        logical_bytes,
        writer_generation,
        published_at
    )
    SELECT sqlc.arg(initial_version_id),
           created_computer.environment_id,
           created_computer.id,
           'initializing',
           NULL,
           0,
           0,
           NULL
      FROM created_computer
    RETURNING computer_id
)
SELECT created_computer.*
  FROM created_computer
  JOIN created_version ON created_version.computer_id = created_computer.id;

-- name: ResolveCurrentComputerDefinitionForCreate :one
SELECT deployment_definitions.*
  FROM deployment_definitions
  JOIN deployments
    ON deployments.environment_id = deployment_definitions.environment_id
   AND deployments.id = deployment_definitions.deployment_id
  JOIN environments
    ON environments.id = deployment_definitions.environment_id
 WHERE deployment_definitions.environment_id = sqlc.arg(environment_id)
   AND deployment_definitions.kind = 'sandbox'
   AND deployment_definitions.declared_id = sqlc.arg(sandbox_declared_id)
   AND environments.current_deployment_id = deployment_definitions.deployment_id
 LIMIT 1;

-- name: ResolveRunPinnedComputerDefinitionForCreate :one
SELECT deployment_definitions.*
  FROM runs
  JOIN deployment_definitions
    ON deployment_definitions.environment_id = runs.environment_id
   AND deployment_definitions.deployment_id = runs.deployment_id
   AND deployment_definitions.kind = 'sandbox'
   AND deployment_definitions.declared_id = sqlc.arg(sandbox_declared_id)
 WHERE runs.environment_id = sqlc.arg(environment_id)
   AND runs.id = sqlc.arg(run_id)
   AND runs.status IN ('queued', 'running', 'waiting', 'retry_delayed')
 LIMIT 1;

-- name: GetComputer :one
SELECT computers.id,
       computers.environment_id,
       computers.region_id,
       computers.sandbox_declared_id,
       computers.computer_spec_id,
       computers.creation_deployment_id,
       computers.key,
       computers.revision,
       computers.writer_generation,
       computers.head_disk_version_id,
       computers.status,
       computers.preparation_failure,
       CASE
         WHEN computers.status='recovery_required' OR computers.recovery_failure IS NOT NULL OR computers.preparation_failure IS NOT NULL OR computers.dirty_state IN ('dirty_state_lost') THEN 'unavailable'
         WHEN residency_instance.observed_state='lost' THEN 'unavailable'
         WHEN residency_instance.admission_state='restoring' THEN 'restoring'
         WHEN residency_instance.desired_state='closed' OR residency_instance.admission_state IN ('draining','checkpointing','resuming_capture','closed') THEN 'parking'
         WHEN residency_instance.observed_state='ready' AND residency_instance.admission_state='open' THEN 'running'
         WHEN residency_instance.id IS NOT NULL THEN 'starting'
         WHEN EXISTS(SELECT 1 FROM computer_checkpoints cp WHERE cp.computer_id=computers.id AND cp.status='ready' AND cp.resume_committed_at IS NULL) THEN 'parked'
         ELSE 'cold'
       END::text AS residency,
       COALESCE(computers.preparation_failure,computers.recovery_failure,
         CASE WHEN computers.status='recovery_required' OR computers.dirty_state IN ('dirty_state_lost') OR residency_instance.observed_state='lost'
         THEN '{"code":"computer_recovery_required","message":"Computer execution state is unavailable"}'::jsonb END) AS residency_error,

       computers.desired_state,
       computers.dirty_state,
       computers.last_activity_at,
       computers.created_at,
       computers.updated_at,
       computers.deleted_at
  FROM computers
  LEFT JOIN computer_instances residency_instance ON residency_instance.computer_id=computers.id AND residency_instance.reclaimed_at IS NULL
  JOIN environments ON environments.id = computers.environment_id
 WHERE environments.org_id = sqlc.arg(org_id)
   AND environments.project_id = sqlc.arg(project_id)
   AND computers.environment_id = sqlc.arg(environment_id)
   AND computers.id = sqlc.arg(id);

-- name: GetComputerListItemByKey :one
SELECT computers.id,
       computers.key,
       computers.sandbox_declared_id::text AS sandbox_id,
       computers.creation_deployment_id AS deployment_id,
       computers.status,
       computers.preparation_failure,
       CASE
         WHEN computers.status='recovery_required' OR computers.recovery_failure IS NOT NULL OR computers.preparation_failure IS NOT NULL OR computers.dirty_state IN ('dirty_state_lost') THEN 'unavailable'
         WHEN residency_instance.observed_state='lost' THEN 'unavailable'
         WHEN residency_instance.admission_state='restoring' THEN 'restoring'
         WHEN residency_instance.desired_state='closed' OR residency_instance.admission_state IN ('draining','checkpointing','resuming_capture','closed') THEN 'parking'
         WHEN residency_instance.observed_state='ready' AND residency_instance.admission_state='open' THEN 'running'
         WHEN residency_instance.id IS NOT NULL THEN 'starting'
         WHEN EXISTS(SELECT 1 FROM computer_checkpoints cp WHERE cp.computer_id=computers.id AND cp.status='ready' AND cp.resume_committed_at IS NULL) THEN 'parked'
         ELSE 'cold'
       END::text AS residency,
       COALESCE(computers.preparation_failure,computers.recovery_failure,
         CASE WHEN computers.status='recovery_required' OR computers.dirty_state IN ('dirty_state_lost') OR residency_instance.observed_state='lost'
         THEN '{"code":"computer_recovery_required","message":"Computer execution state is unavailable"}'::jsonb END) AS residency_error,

       computers.last_activity_at,
       computers.created_at,
       computers.updated_at
  FROM computers
  LEFT JOIN computer_instances residency_instance ON residency_instance.computer_id=computers.id AND residency_instance.reclaimed_at IS NULL
  JOIN environments ON environments.id = computers.environment_id
 WHERE environments.org_id = sqlc.arg(org_id)
   AND environments.project_id = sqlc.arg(project_id)
   AND computers.environment_id = sqlc.arg(environment_id)
   AND computers.key = sqlc.arg(key)
   AND computers.deleted_at IS NULL;

-- name: ListComputerListItems :many
SELECT computers.id,
       computers.key,
       computers.sandbox_declared_id::text AS sandbox_id,
       computers.creation_deployment_id AS deployment_id,
       computers.status,
       computers.preparation_failure,
       CASE
         WHEN computers.status='recovery_required' OR computers.recovery_failure IS NOT NULL OR computers.preparation_failure IS NOT NULL OR computers.dirty_state IN ('dirty_state_lost') THEN 'unavailable'
         WHEN residency_instance.observed_state='lost' THEN 'unavailable'
         WHEN residency_instance.admission_state='restoring' THEN 'restoring'
         WHEN residency_instance.desired_state='closed' OR residency_instance.admission_state IN ('draining','checkpointing','resuming_capture','closed') THEN 'parking'
         WHEN residency_instance.observed_state='ready' AND residency_instance.admission_state='open' THEN 'running'
         WHEN residency_instance.id IS NOT NULL THEN 'starting'
         WHEN EXISTS(SELECT 1 FROM computer_checkpoints cp WHERE cp.computer_id=computers.id AND cp.status='ready' AND cp.resume_committed_at IS NULL) THEN 'parked'
         ELSE 'cold'
       END::text AS residency,
       COALESCE(computers.preparation_failure,computers.recovery_failure,
         CASE WHEN computers.status='recovery_required' OR computers.dirty_state IN ('dirty_state_lost') OR residency_instance.observed_state='lost'
         THEN '{"code":"computer_recovery_required","message":"Computer execution state is unavailable"}'::jsonb END) AS residency_error,

       computers.last_activity_at,
       computers.created_at,
       computers.updated_at
  FROM computers
  LEFT JOIN computer_instances residency_instance ON residency_instance.computer_id=computers.id AND residency_instance.reclaimed_at IS NULL
  JOIN environments ON environments.id = computers.environment_id
 WHERE environments.org_id = sqlc.arg(org_id)
   AND environments.project_id = sqlc.arg(project_id)
   AND computers.environment_id = sqlc.arg(environment_id)
   AND computers.deleted_at IS NULL
   AND (
       NOT sqlc.arg(has_after)::boolean
       OR (computers.created_at, computers.id) < (sqlc.arg(after_created_at)::timestamptz, sqlc.arg(after_id)::uuid)
   )
 ORDER BY computers.created_at DESC, computers.id DESC
 LIMIT sqlc.arg(row_limit);

-- name: CreateComputerSecret :one
INSERT INTO computer_secrets (
    computer_id,
    environment_id,
    placement_kind,
    placement_target,
    secret_id, mode, allowed_origins, placeholder
) VALUES (
    sqlc.arg(computer_id),
    sqlc.arg(environment_id),
    sqlc.arg(placement_kind),
    sqlc.arg(placement_target),
    sqlc.arg(secret_id), sqlc.arg(mode), COALESCE(sqlc.arg(allowed_origins)::text[], '{}'::text[]), sqlc.arg(placeholder)
)
RETURNING *;

-- Admission and logical membership changes serialize on the Computer. Physical
-- admission additionally locks its current instance and validates its program.
-- name: LockComputerAdmissionAuthority :one
SELECT computers.*,environments.org_id,environments.project_id
FROM computers JOIN environments ON environments.id=computers.environment_id
JOIN computer_disk_versions head ON head.computer_id=computers.id
 AND head.id=computers.head_disk_version_id AND head.status IN ('initializing','committed')
WHERE computers.environment_id=sqlc.arg(environment_id) AND computers.id=sqlc.arg(id)
FOR UPDATE OF computers;

-- name: TouchComputerForAdmission :one
UPDATE computers SET desired_state='active',revision=revision+1,
 last_activity_at=clock_timestamp(),updated_at=clock_timestamp()
WHERE computers.environment_id=sqlc.arg(environment_id) AND computers.id=sqlc.arg(id)
 AND revision=sqlc.arg(expected_revision) AND status='active' AND deleted_at IS NULL
 AND dirty_state NOT IN ('dirty_state_lost')
 AND preparation_failure IS NULL AND recovery_failure IS NULL
RETURNING *;

-- name: LockComputerForDelete :one
SELECT computers.*,
 EXISTS(SELECT 1 FROM computer_instances i WHERE i.computer_id=computers.id
         AND i.reclaimed_at IS NULL) AS has_instance,
 (EXISTS(SELECT 1 FROM sessions s WHERE s.computer_id=computers.id AND s.status IN ('open','closing'))
 OR EXISTS(SELECT 1 FROM runs r WHERE r.computer_id=computers.id
            AND r.status IN ('queued','running','waiting','retry_delayed','cancel_requested'))
 OR EXISTS(SELECT 1 FROM run_leases l WHERE l.computer_id=computers.id AND l.process_reconciled_at IS NULL)
 OR EXISTS(SELECT 1 FROM computer_commands c WHERE c.computer_id=computers.id
            AND (c.terminal_at IS NULL OR (c.computer_instance_id IS NOT NULL AND c.process_reconciled_at IS NULL)))) AS has_members
FROM computers JOIN environments e ON e.id=computers.environment_id
WHERE e.org_id=sqlc.arg(org_id) AND e.project_id=sqlc.arg(project_id)
 AND computers.environment_id=sqlc.arg(environment_id) AND computers.id=sqlc.arg(id)

FOR UPDATE OF computers;

-- Physical close and reclaim follow this desired state. Logical members must be
-- settled first; an unreclaimed idle instance does not prevent requesting delete.
-- name: MarkComputerDeleting :one
UPDATE computers SET status='deleting',desired_state='deleted',
 dirty_state=CASE WHEN dirty_state='dirty_state_lost' THEN 'clean' ELSE dirty_state END,
 revision=revision+1,updated_at=clock_timestamp()
WHERE computers.environment_id=sqlc.arg(environment_id) AND computers.id=sqlc.arg(id)
 AND computers.revision=sqlc.arg(expected_revision) AND computers.status IN ('active','recovery_required')
 AND NOT EXISTS(SELECT 1 FROM sessions s WHERE s.computer_id=computers.id AND s.status IN ('open','closing'))
 AND NOT EXISTS(SELECT 1 FROM runs r WHERE r.computer_id=computers.id
                 AND r.status IN ('queued','running','waiting','retry_delayed','cancel_requested'))
 AND NOT EXISTS(SELECT 1 FROM run_leases l WHERE l.computer_id=computers.id AND l.process_reconciled_at IS NULL)
 AND NOT EXISTS(SELECT 1 FROM computer_commands c WHERE c.computer_id=computers.id
                 AND (c.terminal_at IS NULL OR (c.computer_instance_id IS NOT NULL AND c.process_reconciled_at IS NULL)))
RETURNING computers.*;

-- name: FinalizeDeletingComputers :many
WITH eligible AS (
 SELECT computers.id FROM computers WHERE status='deleting' AND desired_state='deleted'
 AND NOT EXISTS(SELECT 1 FROM sessions s WHERE s.computer_id=computers.id AND s.status IN ('open','closing'))
 AND NOT EXISTS(SELECT 1 FROM runs r WHERE r.computer_id=computers.id
                 AND r.status IN ('queued','running','waiting','retry_delayed','cancel_requested'))
 AND NOT EXISTS(SELECT 1 FROM run_leases l WHERE l.computer_id=computers.id AND l.process_reconciled_at IS NULL)
 AND NOT EXISTS(SELECT 1 FROM computer_commands c WHERE c.computer_id=computers.id
                 AND (c.terminal_at IS NULL OR (c.computer_instance_id IS NOT NULL AND c.process_reconciled_at IS NULL)))
 AND NOT EXISTS(SELECT 1 FROM computer_instances i WHERE i.computer_id=computers.id AND i.reclaimed_at IS NULL)
 ORDER BY computers.updated_at,computers.id LIMIT sqlc.arg(row_limit) FOR UPDATE OF computers SKIP LOCKED
)
UPDATE computers SET key=NULL,head_disk_version_id=NULL,
 dirty_state='clean',status='deleted',revision=revision+1,deleted_at=clock_timestamp(),updated_at=clock_timestamp()
FROM eligible WHERE computers.id=eligible.id RETURNING computers.id;

-- name: LockActorInputComputer :one
SELECT c.* FROM computers c JOIN sessions s ON s.environment_id=c.environment_id AND s.computer_id=c.id
WHERE c.environment_id=sqlc.arg(environment_id) AND c.id=sqlc.arg(id)
 AND s.id=sqlc.arg(session_id) AND s.status='open'
FOR UPDATE OF c;

-- name: LockChildComputerPair :many
SELECT * FROM computers WHERE environment_id=sqlc.arg(environment_id)
 AND id=ANY(sqlc.arg(computer_ids)::uuid[]) ORDER BY id FOR UPDATE;

-- The Computer and its live instance are locked before this read. A deployment
-- must declare the same immutable spec; open or unreconciled program members pin
-- their deployment independently of whether their process is currently resident.
-- name: GetComputerProgramAdmission :one
SELECT EXISTS(SELECT 1 FROM deployment_definitions d
              WHERE d.environment_id=sqlc.arg(environment_id) AND d.deployment_id=sqlc.arg(deployment_id)
              AND d.kind='sandbox' AND d.computer_spec_id=sqlc.arg(computer_spec_id)) AS spec_compatible,
 NOT EXISTS(SELECT 1 FROM sessions s JOIN deployment_definitions d ON d.id=s.deployment_definition_id
            AND d.environment_id=s.environment_id
            WHERE s.environment_id=sqlc.arg(environment_id) AND s.computer_id=sqlc.arg(computer_id)
            AND s.status IN ('open','closing') AND d.deployment_id<>sqlc.arg(deployment_id))
 AND NOT EXISTS(SELECT 1 FROM runs r WHERE r.environment_id=sqlc.arg(environment_id)
                AND r.computer_id=sqlc.arg(computer_id) AND r.deployment_id<>sqlc.arg(deployment_id)
                AND (r.status IN ('queued','running','waiting','retry_delayed','cancel_requested')
                     OR EXISTS(SELECT 1 FROM run_leases l WHERE l.run_id=r.id AND l.process_reconciled_at IS NULL)))
 AS program_compatible;

-- name: LockComputer :one
SELECT * FROM computers WHERE environment_id=sqlc.arg(environment_id)
 AND id=sqlc.arg(id) FOR UPDATE;
