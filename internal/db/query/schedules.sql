-- name: CreateSchedule :one
WITH schedule AS (
    INSERT INTO task_schedules (
        id,
        org_id,
        project_id,
        environment_id,
        schedule_type,
        task_id,
        dedup_key,
        external_id,
        cron,
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
        sqlc.arg(schedule_type)::task_schedule_type,
        sqlc.arg(task_id),
        sqlc.arg(dedup_key),
        sqlc.narg(external_id),
        sqlc.arg(cron),
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
        next_scheduled_at
    )
    SELECT sqlc.arg(instance_id),
           schedule.id,
           schedule.org_id,
           schedule.project_id,
           sqlc.arg(environment_id),
           sqlc.arg(active),
           sqlc.narg(next_scheduled_at)
      FROM schedule
    RETURNING *
)
SELECT schedule.id AS schedule_id,
       instance.id AS instance_id,
       schedule.org_id,
       schedule.project_id,
       instance.environment_id,
       schedule.schedule_type,
       schedule.task_id,
       schedule.dedup_key,
       schedule.external_id,
       schedule.cron,
       schedule.timezone,
       schedule.payload,
       schedule.secret_bindings,
       schedule.workspace,
       schedule.run_options,
       schedule.active AS schedule_active,
       instance.active AS instance_active,
       instance.generation,
       instance.next_scheduled_at,
       instance.last_scheduled_at,
       instance.retry_after,
       instance.trigger_attempt_count,
       instance.trigger_error_message,
       schedule.deleted_at,
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
       task_schedules.schedule_type,
       task_schedules.task_id,
       task_schedules.dedup_key,
       task_schedules.external_id,
       task_schedules.cron,
       task_schedules.timezone,
       task_schedules.payload,
       task_schedules.secret_bindings,
       task_schedules.workspace,
       task_schedules.run_options,
       task_schedules.active AS schedule_active,
       task_schedule_instances.active AS instance_active,
       task_schedule_instances.generation,
       task_schedule_instances.next_scheduled_at,
       task_schedule_instances.last_scheduled_at,
       task_schedule_instances.retry_after,
       task_schedule_instances.trigger_attempt_count,
       task_schedule_instances.trigger_error_message,
       task_schedules.deleted_at,
       task_schedules.created_at,
       task_schedules.updated_at
  FROM task_schedules
  JOIN task_schedule_instances ON task_schedule_instances.schedule_id = task_schedules.id
 WHERE task_schedules.org_id = sqlc.arg(org_id)
   AND task_schedules.project_id = sqlc.arg(project_id)
   AND task_schedules.environment_id = sqlc.arg(environment_id)
   AND task_schedule_instances.environment_id = sqlc.arg(environment_id)
   AND task_schedules.deleted_at IS NULL
 ORDER BY task_schedules.created_at DESC, task_schedules.id DESC
 LIMIT sqlc.arg(row_limit);

-- name: GetScheduleSummary :one
SELECT task_schedules.id AS schedule_id,
       task_schedule_instances.id AS instance_id,
       task_schedules.org_id,
       task_schedules.project_id,
       task_schedule_instances.environment_id,
       task_schedules.schedule_type,
       task_schedules.task_id,
       task_schedules.dedup_key,
       task_schedules.external_id,
       task_schedules.cron,
       task_schedules.timezone,
       task_schedules.payload,
       task_schedules.secret_bindings,
       task_schedules.workspace,
       task_schedules.run_options,
       task_schedules.active AS schedule_active,
       task_schedule_instances.active AS instance_active,
       task_schedule_instances.generation,
       task_schedule_instances.next_scheduled_at,
       task_schedule_instances.last_scheduled_at,
       task_schedule_instances.retry_after,
       task_schedule_instances.trigger_attempt_count,
       task_schedule_instances.trigger_error_message,
       task_schedules.deleted_at,
       task_schedules.created_at,
       task_schedules.updated_at
  FROM task_schedules
  JOIN task_schedule_instances ON task_schedule_instances.schedule_id = task_schedules.id
 WHERE task_schedules.org_id = sqlc.arg(org_id)
   AND task_schedules.project_id = sqlc.arg(project_id)
   AND task_schedules.environment_id = sqlc.arg(environment_id)
   AND task_schedule_instances.environment_id = sqlc.arg(environment_id)
   AND task_schedules.id = sqlc.arg(schedule_id)
   AND task_schedules.deleted_at IS NULL;

-- name: ListDeclarativeScheduleSummariesForEnvironment :many
SELECT task_schedules.id AS schedule_id,
       task_schedule_instances.id AS instance_id,
       task_schedules.org_id,
       task_schedules.project_id,
       task_schedule_instances.environment_id,
       task_schedules.schedule_type,
       task_schedules.task_id,
       task_schedules.dedup_key,
       task_schedules.external_id,
       task_schedules.cron,
       task_schedules.timezone,
       task_schedules.payload,
       task_schedules.secret_bindings,
       task_schedules.workspace,
       task_schedules.run_options,
       task_schedules.active AS schedule_active,
       task_schedule_instances.active AS instance_active,
       task_schedule_instances.generation,
       task_schedule_instances.next_scheduled_at,
       task_schedule_instances.last_scheduled_at,
       task_schedule_instances.retry_after,
       task_schedule_instances.trigger_attempt_count,
       task_schedule_instances.trigger_error_message,
       task_schedules.deleted_at,
       task_schedules.created_at,
       task_schedules.updated_at
  FROM task_schedules
  JOIN task_schedule_instances ON task_schedule_instances.schedule_id = task_schedules.id
 WHERE task_schedules.org_id = sqlc.arg(org_id)
   AND task_schedules.project_id = sqlc.arg(project_id)
   AND task_schedules.environment_id = sqlc.arg(environment_id)
   AND task_schedule_instances.environment_id = sqlc.arg(environment_id)
   AND task_schedules.schedule_type = 'declarative'
   AND task_schedules.deleted_at IS NULL
 ORDER BY task_schedules.task_id ASC, task_schedules.dedup_key ASC;

-- name: UpdateScheduleState :one
WITH updated_schedule AS (
    UPDATE task_schedules
       SET active = sqlc.arg(active),
           updated_at = now()
     WHERE task_schedules.org_id = sqlc.arg(org_id)
       AND task_schedules.project_id = sqlc.arg(project_id)
       AND task_schedules.environment_id = sqlc.arg(environment_id)
       AND task_schedules.deleted_at IS NULL
       AND task_schedules.id = sqlc.arg(schedule_id)
    RETURNING *
),
updated_instances AS (
    UPDATE task_schedule_instances
       SET active = sqlc.arg(active),
           generation = generation + 1,
           next_scheduled_at = sqlc.narg(next_scheduled_at),
           retry_after = NULL,
           trigger_attempt_count = 0,
           trigger_error_message = '',
           updated_at = now()
      FROM updated_schedule
     WHERE task_schedule_instances.schedule_id = updated_schedule.id
       AND task_schedule_instances.environment_id = sqlc.arg(environment_id)
    RETURNING task_schedule_instances.*
),
updated_instance AS (
    SELECT *
      FROM updated_instances
     WHERE environment_id = sqlc.arg(environment_id)
)
SELECT updated_schedule.id AS schedule_id,
       updated_instance.id AS instance_id,
       updated_schedule.org_id,
       updated_schedule.project_id,
       updated_instance.environment_id,
       updated_schedule.schedule_type,
       updated_schedule.task_id,
       updated_schedule.dedup_key,
       updated_schedule.external_id,
       updated_schedule.cron,
       updated_schedule.timezone,
       updated_schedule.payload,
       updated_schedule.secret_bindings,
       updated_schedule.workspace,
       updated_schedule.run_options,
       updated_schedule.active AS schedule_active,
       updated_instance.active AS instance_active,
       updated_instance.generation,
       updated_instance.next_scheduled_at,
       updated_instance.last_scheduled_at,
       updated_instance.retry_after,
       updated_instance.trigger_attempt_count,
       updated_instance.trigger_error_message,
       updated_schedule.deleted_at,
       updated_schedule.created_at,
       updated_schedule.updated_at
  FROM updated_schedule
  JOIN updated_instance ON true;

-- name: UpdateSchedule :one
WITH updated_schedule AS (
    UPDATE task_schedules
       SET task_id = sqlc.arg(task_id),
           dedup_key = sqlc.arg(dedup_key),
           external_id = sqlc.narg(external_id),
           cron = sqlc.arg(cron),
           timezone = sqlc.arg(timezone),
           payload = sqlc.arg(payload)::jsonb,
           secret_bindings = sqlc.arg(secret_bindings)::jsonb,
           workspace = sqlc.arg(workspace)::jsonb,
           run_options = sqlc.arg(run_options)::jsonb,
           active = sqlc.arg(active),
           updated_at = now()
     WHERE task_schedules.org_id = sqlc.arg(org_id)
       AND task_schedules.project_id = sqlc.arg(project_id)
       AND task_schedules.environment_id = sqlc.arg(environment_id)
       AND task_schedules.deleted_at IS NULL
       AND task_schedules.id = sqlc.arg(schedule_id)
    RETURNING *
),
updated_instances AS (
    UPDATE task_schedule_instances
       SET active = sqlc.arg(active),
           generation = generation + 1,
           next_scheduled_at = sqlc.narg(next_scheduled_at),
           retry_after = NULL,
           trigger_attempt_count = 0,
           trigger_error_message = '',
           updated_at = now()
      FROM updated_schedule
     WHERE task_schedule_instances.schedule_id = updated_schedule.id
       AND task_schedule_instances.environment_id = sqlc.arg(environment_id)
    RETURNING task_schedule_instances.*
),
updated_instance AS (
    SELECT *
      FROM updated_instances
     WHERE environment_id = sqlc.arg(environment_id)
)
SELECT updated_schedule.id AS schedule_id,
       updated_instance.id AS instance_id,
       updated_schedule.org_id,
       updated_schedule.project_id,
       updated_instance.environment_id,
       updated_schedule.schedule_type,
       updated_schedule.task_id,
       updated_schedule.dedup_key,
       updated_schedule.external_id,
       updated_schedule.cron,
       updated_schedule.timezone,
       updated_schedule.payload,
       updated_schedule.secret_bindings,
       updated_schedule.workspace,
       updated_schedule.run_options,
       updated_schedule.active AS schedule_active,
       updated_instance.active AS instance_active,
       updated_instance.generation,
       updated_instance.next_scheduled_at,
       updated_instance.last_scheduled_at,
       updated_instance.retry_after,
       updated_instance.trigger_attempt_count,
       updated_instance.trigger_error_message,
       updated_schedule.deleted_at,
       updated_schedule.created_at,
       updated_schedule.updated_at
  FROM updated_schedule
  JOIN updated_instance ON true;

-- name: DeleteSchedule :one
WITH deleted_schedule AS (
    UPDATE task_schedules
       SET active = false,
           deleted_at = now(),
           updated_at = now()
     WHERE task_schedules.org_id = sqlc.arg(org_id)
       AND task_schedules.project_id = sqlc.arg(project_id)
       AND task_schedules.environment_id = sqlc.arg(environment_id)
       AND task_schedules.deleted_at IS NULL
       AND task_schedules.id = sqlc.arg(schedule_id)
    RETURNING id
),
deleted_instances AS (
    UPDATE task_schedule_instances
       SET active = false,
           generation = generation + 1,
           next_scheduled_at = NULL,
           retry_after = NULL,
           updated_at = now()
      FROM deleted_schedule
     WHERE task_schedule_instances.schedule_id = deleted_schedule.id
       AND task_schedule_instances.environment_id = sqlc.arg(environment_id)
    RETURNING task_schedule_instances.*
)
SELECT count(*)::bigint FROM deleted_schedule;

-- name: ListScheduleIndexEntries :many
WITH index_entries AS (
    SELECT task_schedules.id AS schedule_id,
           task_schedule_instances.id AS instance_id,
           task_schedules.org_id,
           task_schedules.project_id,
           task_schedule_instances.environment_id,
           task_schedule_instances.generation,
           task_schedule_instances.next_scheduled_at,
           task_schedule_instances.retry_after,
           coalesce(task_schedule_instances.retry_after, task_schedule_instances.next_scheduled_at) AS available_at
      FROM task_schedule_instances
      JOIN task_schedules ON task_schedules.id = task_schedule_instances.schedule_id
     WHERE task_schedules.active
       AND task_schedules.deleted_at IS NULL
       AND task_schedule_instances.active
       AND task_schedule_instances.next_scheduled_at IS NOT NULL
       AND coalesce(task_schedule_instances.retry_after, task_schedule_instances.next_scheduled_at) <= sqlc.arg(available_before)
)
SELECT schedule_id,
       instance_id,
       org_id,
       project_id,
       environment_id,
       generation,
       next_scheduled_at,
       retry_after,
       available_at
 FROM index_entries
 WHERE (
       sqlc.narg(after_available_at)::timestamptz IS NULL
       OR available_at > sqlc.narg(after_available_at)::timestamptz
       OR (
           available_at = sqlc.narg(after_available_at)::timestamptz
           AND instance_id > sqlc.narg(after_instance_id)::uuid
       )
   )
 ORDER BY available_at, instance_id
 LIMIT sqlc.arg(row_limit);

-- name: GetScheduleRetryAfter :one
SELECT task_schedule_instances.retry_after
  FROM task_schedule_instances
  JOIN task_schedules ON task_schedules.id = task_schedule_instances.schedule_id
 WHERE task_schedule_instances.id = sqlc.arg(instance_id)
   AND task_schedule_instances.generation = sqlc.arg(generation)
   AND task_schedule_instances.next_scheduled_at = sqlc.arg(scheduled_at)
   AND task_schedule_instances.active
   AND task_schedule_instances.retry_after > now()
   AND task_schedules.active
   AND task_schedules.deleted_at IS NULL;

-- name: GetScheduleTriggerCandidate :one
SELECT task_schedules.id AS schedule_id,
       task_schedule_instances.id AS instance_id,
       task_schedules.org_id,
       task_schedules.project_id,
       task_schedule_instances.environment_id,
       task_schedules.task_id,
       task_schedules.cron,
       task_schedules.timezone,
       task_schedules.payload,
       task_schedules.secret_bindings,
       task_schedules.workspace,
       task_schedules.run_options,
       task_schedule_instances.generation,
       task_schedule_instances.next_scheduled_at,
       task_schedule_instances.last_scheduled_at,
       task_schedule_instances.retry_after,
       task_schedule_instances.trigger_attempt_count,
       task_schedule_instances.trigger_error_message
  FROM task_schedule_instances
  JOIN task_schedules ON task_schedules.id = task_schedule_instances.schedule_id
 WHERE task_schedule_instances.id = sqlc.arg(instance_id)
   AND task_schedule_instances.generation = sqlc.arg(generation)
   AND task_schedule_instances.next_scheduled_at = sqlc.arg(scheduled_at)
   AND task_schedule_instances.active
   AND (
       task_schedule_instances.retry_after IS NULL
       OR task_schedule_instances.retry_after <= now()
   )
   AND task_schedules.active
   AND task_schedules.deleted_at IS NULL;

-- name: ScheduleInstanceTriggerIsCurrent :one
SELECT EXISTS (
    SELECT 1
      FROM task_schedule_instances
      JOIN task_schedules ON task_schedules.id = task_schedule_instances.schedule_id
     WHERE task_schedule_instances.id = sqlc.arg(instance_id)
       AND task_schedule_instances.generation = sqlc.arg(generation)
       AND task_schedule_instances.next_scheduled_at = sqlc.arg(scheduled_at)
       AND task_schedule_instances.schedule_id = sqlc.arg(schedule_id)
       AND task_schedule_instances.org_id = sqlc.arg(org_id)
       AND task_schedule_instances.project_id = sqlc.arg(project_id)
       AND task_schedule_instances.environment_id = sqlc.arg(environment_id)
       AND task_schedule_instances.active
       AND (
           task_schedule_instances.retry_after IS NULL
           OR task_schedule_instances.retry_after <= now()
       )
       AND task_schedules.active
       AND task_schedules.deleted_at IS NULL
) AS current;

-- name: AdvanceScheduleInstance :one
UPDATE task_schedule_instances
   SET next_scheduled_at = sqlc.narg(next_scheduled_at),
       last_scheduled_at = sqlc.arg(last_scheduled_at),
       retry_after = NULL,
       trigger_attempt_count = 0,
       trigger_error_message = '',
       updated_at = now()
 WHERE id = sqlc.arg(instance_id)
   AND generation = sqlc.arg(generation)
   AND next_scheduled_at = sqlc.arg(last_scheduled_at)
   AND active
 RETURNING id AS instance_id,
           generation,
           next_scheduled_at;

-- name: SkipScheduleInstanceTrigger :one
UPDATE task_schedule_instances
	   SET next_scheduled_at = sqlc.arg(next_scheduled_at),
	       last_scheduled_at = sqlc.arg(last_scheduled_at),
	       retry_after = NULL,
	       trigger_attempt_count = 0,
	       trigger_error_message = '',
	       updated_at = now()
 WHERE id = sqlc.arg(instance_id)
   AND generation = sqlc.arg(generation)
   AND next_scheduled_at = sqlc.arg(last_scheduled_at)
   AND active
 RETURNING id AS instance_id,
           generation,
           next_scheduled_at;

-- name: MarkScheduleInstanceTriggerFailed :execrows
UPDATE task_schedule_instances
   SET trigger_attempt_count = trigger_attempt_count + 1,
       trigger_error_message = sqlc.arg(error_message),
       retry_after = sqlc.arg(retry_after),
       updated_at = now()
 WHERE id = sqlc.arg(instance_id)
   AND generation = sqlc.arg(generation)
   AND next_scheduled_at = sqlc.arg(scheduled_at)
   AND active;
