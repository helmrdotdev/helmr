-- name: CreateSchedule :one
WITH schedule AS (
    INSERT INTO task_schedules (
        id,
        org_id,
        project_id,
        environment_id,
        task_id,
        dedup_key,
        cron_expression,
        timezone,
        payload,
        secret_bindings,
        workspace,
        run_options,
        active
    ) VALUES (
        sqlc.arg(schedule_id),
        sqlc.arg(org_id),
        sqlc.arg(project_id),
        sqlc.arg(environment_id),
        sqlc.arg(task_id),
        sqlc.arg(dedup_key),
        sqlc.arg(cron_expression),
        sqlc.arg(timezone),
        sqlc.arg(payload)::jsonb,
        sqlc.arg(secret_bindings)::jsonb,
        sqlc.arg(workspace)::jsonb,
        sqlc.arg(run_options)::jsonb,
        sqlc.arg(active)
    )
    RETURNING *
),
instance AS (
    INSERT INTO task_schedule_instances (
        id,
        schedule_id,
        org_id,
        project_id,
        environment_id,
        active,
        next_scheduled_at,
        next_due_at
    )
    SELECT sqlc.arg(instance_id),
           schedule.id,
           schedule.org_id,
           schedule.project_id,
           sqlc.arg(environment_id),
           sqlc.arg(active),
           sqlc.narg(next_scheduled_at),
           sqlc.narg(next_due_at)
      FROM schedule
    RETURNING *
)
SELECT schedule.id AS schedule_id,
       instance.id AS instance_id,
       schedule.org_id,
       schedule.project_id,
       instance.environment_id,
       schedule.task_id,
       schedule.dedup_key,
       schedule.cron_expression,
       schedule.timezone,
       schedule.payload,
       schedule.secret_bindings,
       schedule.workspace,
       schedule.run_options,
       schedule.active AS schedule_active,
       instance.active AS instance_active,
       instance.generation,
       instance.next_scheduled_at,
       instance.next_due_at,
       instance.last_scheduled_at,
       schedule.created_at,
       schedule.updated_at
  FROM schedule
  JOIN instance ON true;

-- name: ListScheduleSummaries :many
SELECT task_schedules.id AS schedule_id,
       task_schedule_instances.id AS instance_id,
       task_schedules.org_id,
       task_schedules.project_id,
       task_schedule_instances.environment_id,
       task_schedules.task_id,
       task_schedules.dedup_key,
       task_schedules.cron_expression,
       task_schedules.timezone,
       task_schedules.payload,
       task_schedules.secret_bindings,
       task_schedules.workspace,
       task_schedules.run_options,
       task_schedules.active AS schedule_active,
       task_schedule_instances.active AS instance_active,
       task_schedule_instances.generation,
       task_schedule_instances.next_scheduled_at,
       task_schedule_instances.next_due_at,
       task_schedule_instances.last_scheduled_at,
       task_schedules.created_at,
       task_schedules.updated_at
  FROM task_schedules
  JOIN task_schedule_instances ON task_schedule_instances.schedule_id = task_schedules.id
 WHERE task_schedules.org_id = sqlc.arg(org_id)
   AND task_schedules.project_id = sqlc.arg(project_id)
   AND task_schedule_instances.environment_id = sqlc.arg(environment_id)
 ORDER BY task_schedules.created_at DESC, task_schedules.id DESC
 LIMIT sqlc.arg(row_limit);

-- name: GetScheduleSummary :one
SELECT task_schedules.id AS schedule_id,
       task_schedule_instances.id AS instance_id,
       task_schedules.org_id,
       task_schedules.project_id,
       task_schedule_instances.environment_id,
       task_schedules.task_id,
       task_schedules.dedup_key,
       task_schedules.cron_expression,
       task_schedules.timezone,
       task_schedules.payload,
       task_schedules.secret_bindings,
       task_schedules.workspace,
       task_schedules.run_options,
       task_schedules.active AS schedule_active,
       task_schedule_instances.active AS instance_active,
       task_schedule_instances.generation,
       task_schedule_instances.next_scheduled_at,
       task_schedule_instances.next_due_at,
       task_schedule_instances.last_scheduled_at,
       task_schedules.created_at,
       task_schedules.updated_at
  FROM task_schedules
  JOIN task_schedule_instances ON task_schedule_instances.schedule_id = task_schedules.id
 WHERE task_schedules.org_id = sqlc.arg(org_id)
   AND task_schedules.project_id = sqlc.arg(project_id)
   AND task_schedule_instances.environment_id = sqlc.arg(environment_id)
   AND task_schedules.id = sqlc.arg(schedule_id);

-- name: UpdateScheduleState :one
WITH updated_schedule AS (
    UPDATE task_schedules
       SET active = sqlc.arg(active),
           updated_at = now()
     WHERE task_schedules.org_id = sqlc.arg(org_id)
       AND task_schedules.project_id = sqlc.arg(project_id)
       AND task_schedules.id = sqlc.arg(schedule_id)
    RETURNING *
),
updated_instance AS (
    UPDATE task_schedule_instances
       SET active = sqlc.arg(active),
           generation = generation + 1,
           next_scheduled_at = sqlc.narg(next_scheduled_at),
           next_due_at = sqlc.narg(next_due_at),
           updated_at = now()
      FROM updated_schedule
     WHERE task_schedule_instances.schedule_id = updated_schedule.id
       AND task_schedule_instances.environment_id = sqlc.arg(environment_id)
    RETURNING task_schedule_instances.*
)
SELECT updated_schedule.id AS schedule_id,
       updated_instance.id AS instance_id,
       updated_schedule.org_id,
       updated_schedule.project_id,
       updated_instance.environment_id,
       updated_schedule.task_id,
       updated_schedule.dedup_key,
       updated_schedule.cron_expression,
       updated_schedule.timezone,
       updated_schedule.payload,
       updated_schedule.secret_bindings,
       updated_schedule.workspace,
       updated_schedule.run_options,
       updated_schedule.active AS schedule_active,
       updated_instance.active AS instance_active,
       updated_instance.generation,
       updated_instance.next_scheduled_at,
       updated_instance.next_due_at,
       updated_instance.last_scheduled_at,
       updated_schedule.created_at,
       updated_schedule.updated_at
  FROM updated_schedule
  JOIN updated_instance ON true;

-- name: DeleteSchedule :execrows
DELETE FROM task_schedules
 WHERE org_id = sqlc.arg(org_id)
   AND project_id = sqlc.arg(project_id)
   AND id = sqlc.arg(schedule_id);

-- name: ClaimDueScheduleInstances :many
SELECT task_schedules.id AS schedule_id,
       task_schedule_instances.id AS instance_id,
       task_schedules.org_id,
       task_schedules.project_id,
       task_schedule_instances.environment_id,
       task_schedules.task_id,
       task_schedules.cron_expression,
       task_schedules.timezone,
       task_schedules.payload,
       task_schedules.secret_bindings,
       task_schedules.workspace,
       task_schedules.run_options,
       task_schedule_instances.generation,
       task_schedule_instances.next_scheduled_at,
       task_schedule_instances.next_due_at,
       task_schedule_instances.last_scheduled_at
  FROM task_schedule_instances
  JOIN task_schedules ON task_schedules.id = task_schedule_instances.schedule_id
 WHERE task_schedules.active
   AND task_schedule_instances.active
   AND task_schedule_instances.next_due_at IS NOT NULL
   AND task_schedule_instances.next_due_at <= now()
 ORDER BY task_schedule_instances.next_due_at, task_schedule_instances.id
 LIMIT sqlc.arg(row_limit)
 FOR UPDATE OF task_schedule_instances SKIP LOCKED;

-- name: InsertScheduleFire :execrows
INSERT INTO task_schedule_fires (
    schedule_instance_id,
    scheduled_at,
    schedule_id,
    org_id,
    project_id,
    environment_id,
    generation,
    task_id,
    payload,
    secret_bindings,
    workspace,
    run_options
) VALUES (
    sqlc.arg(schedule_instance_id),
    sqlc.arg(scheduled_at),
    sqlc.arg(schedule_id),
    sqlc.arg(org_id),
    sqlc.arg(project_id),
    sqlc.arg(environment_id),
    sqlc.arg(generation),
    sqlc.arg(task_id),
    sqlc.arg(payload)::jsonb,
    sqlc.arg(secret_bindings)::jsonb,
    sqlc.arg(workspace)::jsonb,
    sqlc.arg(run_options)::jsonb
)
ON CONFLICT (schedule_instance_id, scheduled_at) DO NOTHING;

-- name: AdvanceScheduleInstance :exec
UPDATE task_schedule_instances
   SET next_scheduled_at = sqlc.narg(next_scheduled_at),
       next_due_at = sqlc.narg(next_due_at),
       last_scheduled_at = sqlc.arg(last_scheduled_at),
       updated_at = now()
 WHERE id = sqlc.arg(instance_id)
   AND generation = sqlc.arg(generation);

-- name: ClaimDueScheduleFires :many
WITH candidate AS (
    SELECT task_schedule_fires.schedule_instance_id,
           task_schedule_fires.scheduled_at
      FROM task_schedule_fires
      JOIN task_schedules ON task_schedules.id = task_schedule_fires.schedule_id
      JOIN task_schedule_instances
        ON task_schedule_instances.id = task_schedule_fires.schedule_instance_id
       AND task_schedule_instances.generation = task_schedule_fires.generation
     WHERE task_schedules.active
       AND task_schedule_instances.active
       AND (
           (task_schedule_fires.status IN ('pending', 'failed') AND task_schedule_fires.next_attempt_at <= now())
           OR (task_schedule_fires.status = 'leased' AND task_schedule_fires.lease_expires_at <= now())
       )
     ORDER BY task_schedule_fires.next_attempt_at, task_schedule_fires.scheduled_at
     LIMIT sqlc.arg(row_limit)
     FOR UPDATE OF task_schedule_fires SKIP LOCKED
),
claimed AS (
    UPDATE task_schedule_fires
       SET status = 'leased',
           lease_id = sqlc.arg(lease_id),
           lease_expires_at = sqlc.arg(lease_expires_at),
           attempt_count = attempt_count + 1,
           updated_at = now()
      FROM candidate
     WHERE task_schedule_fires.schedule_instance_id = candidate.schedule_instance_id
       AND task_schedule_fires.scheduled_at = candidate.scheduled_at
    RETURNING task_schedule_fires.*
)
SELECT schedule_instance_id,
       scheduled_at,
       schedule_id,
       org_id,
       project_id,
       environment_id,
       generation,
       task_id,
       payload,
       secret_bindings,
       workspace,
       run_options,
       run_id,
       status,
       lease_id,
       lease_expires_at,
       attempt_count,
       next_attempt_at,
       error_message,
       completed_at,
       created_at,
       updated_at
  FROM claimed;

-- name: MarkScheduleFireCreated :exec
UPDATE task_schedule_fires
   SET run_id = sqlc.arg(run_id),
       status = 'created',
       lease_id = NULL,
       lease_expires_at = NULL,
       error_message = '',
       completed_at = now(),
       updated_at = now()
 WHERE schedule_instance_id = sqlc.arg(schedule_instance_id)
   AND scheduled_at = sqlc.arg(scheduled_at)
   AND lease_id = sqlc.arg(lease_id)
   AND status = 'leased';

-- name: MarkScheduleFireFailed :exec
UPDATE task_schedule_fires
   SET status = 'failed',
       lease_id = NULL,
       lease_expires_at = NULL,
       error_message = sqlc.arg(error_message),
       next_attempt_at = sqlc.arg(next_attempt_at),
       updated_at = now()
 WHERE schedule_instance_id = sqlc.arg(schedule_instance_id)
   AND scheduled_at = sqlc.arg(scheduled_at)
   AND lease_id = sqlc.arg(lease_id)
   AND status = 'leased';

-- name: ScheduleFireLeaseIsCurrent :one
SELECT EXISTS (
    SELECT 1
      FROM task_schedule_fires
      JOIN task_schedule_instances
        ON task_schedule_instances.id = task_schedule_fires.schedule_instance_id
       AND task_schedule_instances.generation = task_schedule_fires.generation
     WHERE task_schedule_fires.schedule_instance_id = sqlc.arg(schedule_instance_id)
       AND task_schedule_fires.scheduled_at = sqlc.arg(scheduled_at)
       AND task_schedule_fires.lease_id = sqlc.arg(lease_id)
       AND task_schedule_fires.status = 'leased'
       AND task_schedule_instances.active
) AS current;

-- name: MarkScheduleFireSuperseded :exec
UPDATE task_schedule_fires
   SET status = 'superseded',
       lease_id = NULL,
       lease_expires_at = NULL,
       error_message = 'schedule generation changed',
       completed_at = now(),
       updated_at = now()
 WHERE schedule_instance_id = sqlc.arg(schedule_instance_id)
   AND scheduled_at = sqlc.arg(scheduled_at)
   AND lease_id = sqlc.arg(lease_id)
   AND status = 'leased';

-- name: SupersedeScheduleInstanceFires :exec
UPDATE task_schedule_fires
   SET status = 'superseded',
       lease_id = NULL,
       lease_expires_at = NULL,
       error_message = 'schedule generation changed',
       completed_at = now(),
       updated_at = now()
 WHERE schedule_instance_id = sqlc.arg(schedule_instance_id)
   AND generation < sqlc.arg(generation)
   AND status IN ('pending', 'failed');
