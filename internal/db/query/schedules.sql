-- name: ReconcileSchedule :exec
INSERT INTO schedules (
    id,
    environment_id,
    task_declared_id,
    deployment_definition_id,
    deployment_id,
    workspace_ref_id,
    workspace_ref_key,
    workspace_id,
    cron_pattern,
    timezone,
    cron_semantics_version,
    state,
    effective_from,
    next_fire_at
)
VALUES (
    sqlc.arg(id),
    sqlc.arg(environment_id),
    sqlc.arg(task_declared_id),
    sqlc.arg(deployment_definition_id),
    sqlc.arg(deployment_id),
    sqlc.narg(workspace_ref_id),
    sqlc.narg(workspace_ref_key),
    sqlc.narg(workspace_id),
    sqlc.arg(cron_pattern),
    sqlc.arg(timezone),
    sqlc.arg(cron_semantics_version),
    sqlc.arg(state),
    sqlc.arg(effective_from),
    sqlc.narg(next_fire_at)
)
ON CONFLICT (environment_id, task_declared_id)
DO UPDATE
   SET deployment_definition_id = excluded.deployment_definition_id,
       deployment_id = excluded.deployment_id,
       workspace_ref_id = excluded.workspace_ref_id,
       workspace_ref_key = excluded.workspace_ref_key,
       workspace_id = excluded.workspace_id,
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
       last_error_code = NULL,
       last_error_message = NULL,
       updated_at = now()
 WHERE schedules.deployment_definition_id IS DISTINCT FROM excluded.deployment_definition_id
    OR schedules.deployment_id IS DISTINCT FROM excluded.deployment_id
    OR schedules.workspace_ref_id IS DISTINCT FROM excluded.workspace_ref_id
    OR schedules.workspace_ref_key IS DISTINCT FROM excluded.workspace_ref_key
    OR schedules.workspace_id IS DISTINCT FROM excluded.workspace_id
    OR schedules.cron_pattern IS DISTINCT FROM excluded.cron_pattern
    OR schedules.timezone IS DISTINCT FROM excluded.timezone
    OR schedules.cron_semantics_version IS DISTINCT FROM excluded.cron_semantics_version;

-- name: ArchiveOmittedSchedules :exec
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
       last_error_code = NULL,
       last_error_message = NULL,
       updated_at = now()
 WHERE environment_id = sqlc.arg(environment_id)
   AND state <> 'archived'
   AND NOT (task_declared_id = ANY(sqlc.arg(task_declared_ids)::text[]));

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
       sqlc.narg(after_task_declared_id)::text IS NULL
       OR (schedules.task_declared_id, schedules.id) >
          (sqlc.narg(after_task_declared_id)::text, sqlc.arg(after_id)::uuid)
   )
 ORDER BY schedules.task_declared_id, schedules.id
 LIMIT sqlc.arg(limit_count)::integer;

-- name: ListPendingScheduleBindings :many
SELECT schedules.*,
       workspaces.id AS resolved_workspace_id
  FROM schedules
  JOIN workspaces
    ON workspaces.environment_id = schedules.environment_id
   AND workspaces.key = schedules.workspace_ref_key
   AND workspaces.state = 'active'
   AND workspaces.deleted_at IS NULL
 WHERE schedules.state = 'pending_workspace'
 ORDER BY schedules.environment_id, schedules.workspace_ref_key, schedules.id
 LIMIT sqlc.arg(limit_count)::integer;

-- name: ActivatePendingSchedule :one
UPDATE schedules
   SET workspace_id = sqlc.arg(workspace_id),
       generation = generation + 1,
       state = 'active',
       state_version = state_version + 1,
       effective_from = sqlc.arg(effective_from),
       next_fire_at = sqlc.arg(next_fire_at),
       updated_at = now()
 WHERE schedules.environment_id = sqlc.arg(environment_id)
   AND schedules.id = sqlc.arg(id)
   AND schedules.state = 'pending_workspace'
   AND schedules.generation = sqlc.arg(expected_generation)
   AND EXISTS (
       SELECT 1
         FROM workspaces
        WHERE workspaces.environment_id = schedules.environment_id
          AND workspaces.id = sqlc.arg(workspace_id)
          AND workspaces.key = schedules.workspace_ref_key
          AND workspaces.state = 'active'
          AND workspaces.deleted_at IS NULL
   )
RETURNING *;

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
       last_error_code = NULL,
       last_error_message = NULL,
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
       last_error_code = sqlc.arg(last_error_code),
       last_error_message = sqlc.arg(last_error_message),
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
