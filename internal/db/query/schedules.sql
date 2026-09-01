-- name: ReconcileSchedules :many
WITH input AS (
SELECT batch.id,
       batch.task_declared_id,
       batch.deployment_definition_id,
       batch.deployment_id,
       batch.cron_pattern,
       batch.timezone,
       batch.effective_from,
       batch.next_fire_at,
       batch.ordinality
  FROM ROWS FROM (
      unnest(sqlc.arg(ids)::uuid[]),
      unnest(sqlc.arg(task_declared_ids)::text[]),
      unnest(sqlc.arg(deployment_definition_ids)::uuid[]),
      unnest(sqlc.arg(deployment_ids)::uuid[]),
      unnest(sqlc.arg(cron_patterns)::text[]),
      unnest(sqlc.arg(timezones)::text[]),
      unnest(sqlc.arg(effective_froms)::timestamptz[]),
      unnest(sqlc.arg(next_fire_ats)::timestamptz[])
  ) WITH ORDINALITY AS batch(
      id,
      task_declared_id,
      deployment_definition_id,
      deployment_id,
      cron_pattern,
      timezone,
      effective_from,
      next_fire_at,
      ordinality
  )
 WHERE cardinality(sqlc.arg(ids)::uuid[]) BETWEEN 1 AND 10000
   AND cardinality(sqlc.arg(ids)::uuid[]) = cardinality(sqlc.arg(task_declared_ids)::text[])
   AND cardinality(sqlc.arg(ids)::uuid[]) = cardinality(sqlc.arg(deployment_definition_ids)::uuid[])
   AND cardinality(sqlc.arg(ids)::uuid[]) = cardinality(sqlc.arg(deployment_ids)::uuid[])
   AND cardinality(sqlc.arg(ids)::uuid[]) = cardinality(sqlc.arg(cron_patterns)::text[])
   AND cardinality(sqlc.arg(ids)::uuid[]) = cardinality(sqlc.arg(timezones)::text[])
   AND cardinality(sqlc.arg(ids)::uuid[]) = cardinality(sqlc.arg(effective_froms)::timestamptz[])
   AND cardinality(sqlc.arg(ids)::uuid[]) = cardinality(sqlc.arg(next_fire_ats)::timestamptz[])
), reconciled AS (
INSERT INTO schedules (
    id,
    environment_id,
    task_declared_id,
    deployment_definition_id,
    deployment_id,
    cron_pattern,
    timezone,
    cron_semantics_version,
    state,
    effective_from,
    next_fire_at
)
SELECT input.id,
       sqlc.arg(environment_id),
       input.task_declared_id,
       input.deployment_definition_id,
       input.deployment_id,
       input.cron_pattern,
       input.timezone,
       sqlc.arg(cron_semantics_version),
       'active',
       input.effective_from,
       input.next_fire_at
  FROM input
ON CONFLICT (environment_id, task_declared_id)
DO UPDATE
   SET deployment_definition_id = excluded.deployment_definition_id,
       deployment_id = excluded.deployment_id,
       cron_pattern = excluded.cron_pattern,
       timezone = excluded.timezone,
       cron_semantics_version = excluded.cron_semantics_version,
       generation = schedules.generation + 1,
       state = excluded.state,
       state_version = schedules.state_version + 1,
       effective_from = excluded.effective_from,
       next_fire_at = excluded.next_fire_at,
       last_fire_at = NULL,
       claimed_by = NULL,
       claim_expires_at = NULL,
       retry_step = NULL,
       retry_after = NULL,
       updated_at = now()
 WHERE schedules.deployment_definition_id IS DISTINCT FROM excluded.deployment_definition_id
    OR schedules.deployment_id IS DISTINCT FROM excluded.deployment_id
    OR schedules.cron_pattern IS DISTINCT FROM excluded.cron_pattern
    OR schedules.timezone IS DISTINCT FROM excluded.timezone
    OR schedules.cron_semantics_version IS DISTINCT FROM excluded.cron_semantics_version
RETURNING schedules.id, schedules.environment_id, schedules.task_declared_id
), existing AS (
SELECT schedules.id,
       schedules.environment_id,
       schedules.task_declared_id,
       input.ordinality
  FROM input
  JOIN schedules
    ON schedules.environment_id = sqlc.arg(environment_id)
   AND schedules.task_declared_id = input.task_declared_id
 WHERE NOT EXISTS (
       SELECT 1
         FROM reconciled
        WHERE reconciled.task_declared_id = input.task_declared_id
   )
 FOR UPDATE OF schedules
), result AS (
SELECT reconciled.id,
       reconciled.environment_id,
       reconciled.task_declared_id,
       input.ordinality
  FROM reconciled
  JOIN input USING (task_declared_id)
UNION ALL
SELECT existing.id,
       existing.environment_id,
       existing.task_declared_id,
       existing.ordinality
  FROM existing
)
SELECT id, environment_id, task_declared_id
  FROM result
 ORDER BY ordinality;

-- name: ArchiveOmittedSchedules :exec
WITH archived AS (
UPDATE schedules
   SET deployment_definition_id = NULL,
       deployment_id = NULL,
       generation = generation + 1,
       state = 'archived',
       state_version = state_version + 1,
       effective_from = sqlc.arg(effective_from),
       next_fire_at = NULL,
       claimed_by = NULL,
       claim_expires_at = NULL,
       retry_step = NULL,
       retry_after = NULL,
       updated_at = now()
 WHERE environment_id = sqlc.arg(environment_id)
   AND state <> 'archived'
   AND NOT (task_declared_id = ANY(sqlc.arg(task_declared_ids)::text[]))
RETURNING id
)
DELETE FROM schedule_secrets
 USING archived
 WHERE schedule_secrets.environment_id = sqlc.arg(environment_id)
   AND schedule_secrets.schedule_id = archived.id;

-- name: GetSchedule :one
SELECT *
  FROM schedules
 WHERE environment_id = sqlc.arg(environment_id)
   AND id = sqlc.arg(id);

-- name: GetScheduleByID :one
SELECT schedules.*
  FROM schedules
  JOIN environments
    ON environments.id = schedules.environment_id
 WHERE environments.org_id = sqlc.arg(org_id)
   AND environments.project_id = sqlc.arg(project_id)
   AND schedules.environment_id = sqlc.arg(environment_id)
   AND schedules.id = sqlc.arg(id);

-- name: ListSchedules :many
SELECT schedules.*
  FROM schedules
  JOIN environments
    ON environments.id = schedules.environment_id
 WHERE environments.org_id = sqlc.arg(org_id)
   AND environments.project_id = sqlc.arg(project_id)
   AND schedules.environment_id = sqlc.arg(environment_id)
   AND (
       sqlc.narg(task_declared_id)::text IS NULL
       OR schedules.task_declared_id = sqlc.narg(task_declared_id)::text
   )
   AND (
       sqlc.narg(after_task_declared_id)::text IS NULL
       OR (schedules.task_declared_id, schedules.id) >
          (sqlc.narg(after_task_declared_id)::text, sqlc.arg(after_id)::uuid)
   )
 ORDER BY schedules.task_declared_id, schedules.id
 LIMIT sqlc.arg(limit_count)::integer;

-- name: DeleteScheduleSecretsForSchedules :exec
DELETE FROM schedule_secrets
 WHERE environment_id = sqlc.arg(environment_id)
   AND schedule_id = ANY(sqlc.arg(schedule_ids)::uuid[]);

-- name: InsertScheduleSecrets :execrows
WITH valid AS (
SELECT true AS ok
 WHERE cardinality(sqlc.arg(schedule_ids)::uuid[]) BETWEEN 1 AND 10000
   AND cardinality(sqlc.arg(placement_schedule_ids)::uuid[]) <= 640000
   AND cardinality(sqlc.arg(placement_schedule_ids)::uuid[]) = cardinality(sqlc.arg(placement_kinds)::text[])
   AND cardinality(sqlc.arg(placement_schedule_ids)::uuid[]) = cardinality(sqlc.arg(placement_targets)::text[])
   AND cardinality(sqlc.arg(placement_schedule_ids)::uuid[]) = cardinality(sqlc.arg(secret_ids)::uuid[])
   AND NOT EXISTS (
       SELECT 1
         FROM unnest(sqlc.arg(placement_schedule_ids)::uuid[]) AS placement_schedule_id
        WHERE NOT (placement_schedule_id = ANY(sqlc.arg(schedule_ids)::uuid[]))
   )
), input AS (
SELECT batch.schedule_id,
       batch.placement_kind,
       batch.placement_target,
       batch.secret_id
  FROM ROWS FROM (
      unnest(sqlc.arg(placement_schedule_ids)::uuid[]),
      unnest(sqlc.arg(placement_kinds)::text[]),
      unnest(sqlc.arg(placement_targets)::text[]),
      unnest(sqlc.arg(secret_ids)::uuid[])
  ) AS batch(schedule_id, placement_kind, placement_target, secret_id)
  JOIN valid ON true
)
INSERT INTO schedule_secrets (
    schedule_id,
    environment_id,
    placement_kind,
    placement_target,
    secret_id
)
SELECT input.schedule_id,
       sqlc.arg(environment_id),
       input.placement_kind,
       input.placement_target,
       input.secret_id
  FROM input;

-- name: ListScheduleSecrets :many
SELECT *
  FROM schedule_secrets
 WHERE environment_id = sqlc.arg(environment_id)
   AND schedule_id = sqlc.arg(schedule_id)
 ORDER BY secret_id, placement_kind, placement_target;

-- name: CreateWorkspaceForScheduleFire :one
WITH selected_definition AS (
    SELECT schedules.environment_id,
           definition.id AS deployment_definition_id,
           definition.declared_id AS sandbox_declared_id,
           projects.default_region_id
      FROM schedules
      JOIN deployment_definitions AS definition
        ON definition.environment_id = schedules.environment_id
       AND definition.deployment_id = schedules.deployment_id
       AND definition.kind = 'sandbox'
       AND definition.declared_id = sqlc.arg(sandbox_declared_id)
      JOIN environments
        ON environments.id = schedules.environment_id
      JOIN projects
        ON projects.id = environments.project_id
     WHERE schedules.environment_id = sqlc.arg(environment_id)
       AND schedules.id = sqlc.arg(schedule_id)
       AND schedules.generation = sqlc.arg(expected_generation)
       AND schedules.state = 'active'
), created_workspace AS (
    INSERT INTO workspaces (
        id,
        environment_id,
        region_id,
        sandbox_declared_id,
        deployment_definition_id,
        head_version_id,
        key
    )
    SELECT sqlc.arg(id),
           selected_definition.environment_id,
           selected_definition.default_region_id,
           selected_definition.sandbox_declared_id,
           selected_definition.deployment_definition_id,
           sqlc.arg(initial_version_id),
           NULL
      FROM selected_definition
    RETURNING *
), created_version AS (
    INSERT INTO workspace_versions (
        id,
        environment_id,
        workspace_id,
        kind,
        state,
        content_digest,
        size_bytes,
        entry_count,
        ownership_generation,
        writer_generation,
        published_at
    )
    SELECT sqlc.arg(initial_version_id),
           created_workspace.environment_id,
           created_workspace.id,
           'system'::workspace_version_kind,
           'committed',
           'sha256:d2ce8eece19cb4f6db14e37f6d986da7eec7f654f3b91c5c706e9d74e7d2bc96',
           0,
           0,
           0,
           0,
           now()
      FROM created_workspace
    RETURNING workspace_id
)
SELECT created_workspace.*
  FROM created_workspace
  JOIN created_version ON created_version.workspace_id = created_workspace.id;

-- name: ClaimDueSchedules :many
WITH candidates AS (
    SELECT id
      FROM schedules
     WHERE state = 'active'
       AND next_fire_at IS NOT NULL
       AND next_fire_at <= now()
       AND (retry_after IS NULL OR retry_after <= now())
       AND (claimed_by IS NULL OR claim_expires_at <= now())
     ORDER BY coalesce(retry_after, next_fire_at), next_fire_at, id
     FOR UPDATE SKIP LOCKED
     LIMIT sqlc.arg(limit_count)::integer
)
UPDATE schedules
   SET claimed_by = sqlc.arg(claimed_by),
       claim_expires_at = sqlc.arg(claim_expires_at),
       updated_at = now()
  FROM candidates
 WHERE schedules.id = candidates.id
RETURNING schedules.*;

-- name: LockClaimedSchedule :one
SELECT sqlc.embed(schedules),
       environments.org_id,
       environments.project_id
  FROM schedules
  JOIN environments
    ON environments.id = schedules.environment_id
 WHERE schedules.environment_id = sqlc.arg(environment_id)
   AND schedules.id = sqlc.arg(id)
   AND schedules.state = 'active'
   AND schedules.generation = sqlc.arg(expected_generation)
   AND schedules.next_fire_at = sqlc.arg(expected_scheduled_at)
   AND schedules.claimed_by = sqlc.arg(claimed_by)
   AND schedules.claim_expires_at > now()
 FOR UPDATE;

-- name: GetScheduledRunReceipt :one
SELECT *
  FROM runs
 WHERE environment_id = sqlc.arg(environment_id)
   AND schedule_id = sqlc.arg(schedule_id)
   AND scheduled_at = sqlc.arg(scheduled_at)
   AND cause_kind = 'schedule';

-- name: AdvanceScheduleCursor :one
UPDATE schedules
   SET last_fire_at = sqlc.arg(expected_scheduled_at),
       next_fire_at = sqlc.arg(next_fire_at),
       claimed_by = NULL,
       claim_expires_at = NULL,
       retry_step = NULL,
       retry_after = NULL,
       state_version = state_version + 1,
       updated_at = now()
 WHERE environment_id = sqlc.arg(environment_id)
   AND id = sqlc.arg(id)
   AND state = 'active'
   AND generation = sqlc.arg(expected_generation)
   AND next_fire_at = sqlc.arg(expected_scheduled_at)
   AND claimed_by = sqlc.arg(claimed_by)
   AND claim_expires_at > now()
RETURNING *;

-- name: MarkScheduleAdmissionRetryable :one
UPDATE schedules
   SET retry_step = sqlc.arg(retry_step),
       retry_after = sqlc.arg(retry_after),
       claimed_by = NULL,
       claim_expires_at = NULL,
       updated_at = now()
 WHERE environment_id = sqlc.arg(environment_id)
   AND id = sqlc.arg(id)
   AND state = 'active'
   AND generation = sqlc.arg(expected_generation)
   AND next_fire_at = sqlc.arg(expected_scheduled_at)
   AND retry_step IS NOT DISTINCT FROM sqlc.narg(expected_retry_step)
   AND claimed_by = sqlc.arg(claimed_by)
   AND claim_expires_at > now()
RETURNING *;

-- name: MarkScheduleAdmissionErrored :one
UPDATE schedules
   SET state = 'errored',
       state_version = state_version + 1,
       retry_step = NULL,
       retry_after = NULL,
       last_failure = sqlc.arg(last_failure),
       claimed_by = NULL,
       claim_expires_at = NULL,
       updated_at = now()
 WHERE environment_id = sqlc.arg(environment_id)
   AND id = sqlc.arg(id)
   AND state = 'active'
   AND generation = sqlc.arg(expected_generation)
   AND next_fire_at = sqlc.arg(expected_scheduled_at)
   AND claimed_by = sqlc.arg(claimed_by)
   AND claim_expires_at > now()
RETURNING *;
