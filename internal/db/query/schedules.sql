-- name: CreateSchedule :one
INSERT INTO schedules (
    id,
    public_id,
    org_id,
    project_id,
    environment_id,
    source,
    key,
    task_declared_id,
    declarative_deployment_definition_id,
    declarative_deployment_id,
    workspace_ref_id,
    workspace_ref_key,
    workspace_id,
    cron_pattern,
    timezone,
    queue_name,
    concurrency_key,
    queue_concurrency_limit,
    priority,
    queued_ttl_ms,
    max_active_duration_ms,
    retry_policy,
    run_metadata,
    run_tags,
    state,
    effective_from,
    next_fire_at,
    metadata,
    tags
)
VALUES (
    sqlc.arg(id),
    sqlc.arg(public_id),
    sqlc.arg(org_id),
    sqlc.arg(project_id),
    sqlc.arg(environment_id),
    sqlc.arg(source),
    sqlc.arg(key),
    sqlc.arg(task_declared_id),
    sqlc.narg(declarative_deployment_definition_id),
    sqlc.narg(declarative_deployment_id),
    sqlc.narg(workspace_ref_id),
    sqlc.narg(workspace_ref_key),
    sqlc.narg(workspace_id),
    sqlc.arg(cron_pattern),
    sqlc.arg(timezone),
    sqlc.arg(queue_name),
    sqlc.narg(concurrency_key),
    sqlc.narg(queue_concurrency_limit),
    sqlc.arg(priority),
    sqlc.narg(queued_ttl_ms),
    sqlc.arg(max_active_duration_ms),
    sqlc.arg(retry_policy),
    coalesce(sqlc.narg(run_metadata)::jsonb, '{}'::jsonb),
    coalesce(sqlc.narg(run_tags)::text[], '{}'::text[]),
    sqlc.arg(state),
    sqlc.arg(effective_from),
    sqlc.narg(next_fire_at),
    coalesce(sqlc.narg(metadata)::jsonb, '{}'::jsonb),
    coalesce(sqlc.narg(tags)::text[], '{}'::text[])
)
RETURNING *;

-- name: GetSchedule :one
SELECT *
  FROM schedules
 WHERE environment_id = sqlc.arg(environment_id)
   AND id = sqlc.arg(id);

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
SELECT *
  FROM schedules
 WHERE environment_id = sqlc.arg(environment_id)
   AND id = sqlc.arg(id)
   AND state = 'active'
   AND generation = sqlc.arg(expected_generation)
   AND next_fire_at = sqlc.arg(expected_scheduled_at)
   AND claimed_by = sqlc.arg(claimed_by)
   AND claim_expires_at > now()
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
       last_error = NULL,
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
       last_error = sqlc.arg(last_error),
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
