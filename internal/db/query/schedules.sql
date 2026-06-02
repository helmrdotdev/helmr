-- name: CreateSchedule :one
WITH existing_schedule AS (
    SELECT task_schedules.id,
           task_schedules.cron,
           task_schedules.timezone
      FROM task_schedules
     WHERE task_schedules.org_id = sqlc.arg(org_id)
       AND task_schedules.project_id = sqlc.arg(project_id)
       AND task_schedules.deleted_at IS NULL
       AND task_schedules.user_dedup_key = sqlc.narg(user_dedup_key)
     FOR UPDATE
),
schedule AS (
    INSERT INTO task_schedules (
        id,
        org_id,
        project_id,
        schedule_type,
        task_id,
        dedup_key,
        user_dedup_key,
        external_id,
        cron,
        timezone,
        active
    ) VALUES (
        sqlc.arg(schedule_id),
        sqlc.arg(org_id),
        sqlc.arg(project_id),
        sqlc.arg(schedule_type)::task_schedule_type,
        sqlc.arg(task_id),
        sqlc.arg(dedup_key),
        sqlc.narg(user_dedup_key),
        sqlc.narg(external_id),
        sqlc.arg(cron),
        sqlc.arg(timezone),
        true
    )
    ON CONFLICT (org_id, project_id, user_dedup_key)
    WHERE deleted_at IS NULL AND user_dedup_key IS NOT NULL
    DO UPDATE SET task_id = EXCLUDED.task_id,
                  external_id = EXCLUDED.external_id,
                  cron = EXCLUDED.cron,
                  timezone = EXCLUDED.timezone,
                  active = true,
                  updated_at = now()
    WHERE task_schedules.schedule_type = 'imperative'
    RETURNING id, org_id, project_id, schedule_type, task_id, dedup_key, user_dedup_key, external_id, cron, timezone, active, deleted_at, created_at, updated_at,
              EXISTS (
                  SELECT 1
                    FROM existing_schedule
                   WHERE existing_schedule.cron IS DISTINCT FROM sqlc.arg(cron)
                      OR existing_schedule.timezone IS DISTINCT FROM sqlc.arg(timezone)
              ) AS timing_changed
),
instance AS (
    INSERT INTO task_schedule_instances (
        id,
        schedule_id,
        org_id,
        project_id,
        environment_id,
        secret_bindings,
        workspace,
        run_options,
        active,
        next_scheduled_at
    )
    SELECT sqlc.arg(instance_id),
           schedule.id,
           schedule.org_id,
           schedule.project_id,
           sqlc.arg(environment_id),
           sqlc.arg(secret_bindings)::jsonb,
           sqlc.arg(workspace)::jsonb,
           sqlc.arg(run_options)::jsonb,
           sqlc.arg(active),
           CASE WHEN sqlc.arg(active) THEN sqlc.arg(next_scheduled_at)::timestamptz ELSE NULL END
      FROM schedule
    ON CONFLICT (schedule_id, environment_id) DO UPDATE
       SET secret_bindings = EXCLUDED.secret_bindings,
           workspace = EXCLUDED.workspace,
           run_options = EXCLUDED.run_options,
           active = EXCLUDED.active,
           generation = task_schedule_instances.generation + 1,
           next_scheduled_at = EXCLUDED.next_scheduled_at,
           retry_after = NULL,
           trigger_attempt_count = 0,
           trigger_error_message = '',
           updated_at = now()
    RETURNING id, schedule_id, org_id, project_id, environment_id, secret_bindings, workspace, run_options, active, generation, next_scheduled_at, last_scheduled_at, retry_after, trigger_attempt_count, trigger_error_message, created_at, updated_at
),
refreshed_instances AS (
    UPDATE task_schedule_instances
       SET generation = task_schedule_instances.generation + 1,
           next_scheduled_at = CASE WHEN task_schedule_instances.active THEN sqlc.arg(next_scheduled_at)::timestamptz ELSE NULL END,
           retry_after = NULL,
           trigger_attempt_count = 0,
           trigger_error_message = '',
           updated_at = now()
      FROM schedule
     WHERE task_schedule_instances.schedule_id = schedule.id
       AND task_schedule_instances.environment_id <> sqlc.arg(environment_id)
       AND schedule.timing_changed
    RETURNING task_schedule_instances.id
)
SELECT schedule.id AS schedule_id,
       instance.id AS instance_id,
       schedule.org_id,
       schedule.project_id,
       instance.environment_id,
       schedule.schedule_type,
       schedule.task_id,
       schedule.dedup_key,
       schedule.user_dedup_key,
       schedule.external_id,
       schedule.cron,
       schedule.timezone,
       instance.secret_bindings,
       instance.workspace,
       instance.run_options,
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
  JOIN instance ON true
  JOIN (SELECT count(*) FROM refreshed_instances) refreshed ON true;

-- name: CreateDeclarativeSchedule :one
WITH existing_schedule AS (
    SELECT task_schedules.id,
           task_schedules.cron,
           task_schedules.timezone
      FROM task_schedules
     WHERE task_schedules.org_id = sqlc.arg(org_id)
       AND task_schedules.project_id = sqlc.arg(project_id)
       AND task_schedules.deleted_at IS NULL
       AND task_schedules.dedup_key = sqlc.arg(dedup_key)
     FOR UPDATE
),
schedule AS (
    INSERT INTO task_schedules (
        id,
        org_id,
        project_id,
        schedule_type,
        task_id,
        dedup_key,
        external_id,
        cron,
        timezone,
        active
    ) VALUES (
        sqlc.arg(schedule_id),
        sqlc.arg(org_id),
        sqlc.arg(project_id),
        'declarative',
        sqlc.arg(task_id),
        sqlc.arg(dedup_key),
        sqlc.narg(external_id),
        sqlc.arg(cron),
        sqlc.arg(timezone),
        true
    )
    ON CONFLICT (org_id, project_id, dedup_key)
    WHERE deleted_at IS NULL
    DO UPDATE SET task_id = EXCLUDED.task_id,
                  external_id = EXCLUDED.external_id,
                  cron = EXCLUDED.cron,
                  timezone = EXCLUDED.timezone,
                  active = true,
                  updated_at = now()
    WHERE task_schedules.schedule_type = 'declarative'
    RETURNING id, org_id, project_id, schedule_type, task_id, dedup_key, user_dedup_key, external_id, cron, timezone, active, deleted_at, created_at, updated_at,
              EXISTS (
                  SELECT 1
                    FROM existing_schedule
                   WHERE existing_schedule.cron IS DISTINCT FROM sqlc.arg(cron)
                      OR existing_schedule.timezone IS DISTINCT FROM sqlc.arg(timezone)
              ) AS timing_changed
),
instance AS (
    INSERT INTO task_schedule_instances (
        id,
        schedule_id,
        org_id,
        project_id,
        environment_id,
        secret_bindings,
        workspace,
        run_options,
        active,
        next_scheduled_at
    )
    SELECT sqlc.arg(instance_id),
           schedule.id,
           schedule.org_id,
           schedule.project_id,
           sqlc.arg(environment_id),
           sqlc.arg(secret_bindings)::jsonb,
           sqlc.arg(workspace)::jsonb,
           sqlc.arg(run_options)::jsonb,
           sqlc.arg(active),
           CASE WHEN sqlc.arg(active) THEN sqlc.arg(next_scheduled_at)::timestamptz ELSE NULL END
      FROM schedule
    ON CONFLICT (schedule_id, environment_id) DO UPDATE
       SET secret_bindings = EXCLUDED.secret_bindings,
           workspace = EXCLUDED.workspace,
           run_options = EXCLUDED.run_options,
           active = EXCLUDED.active,
           generation = task_schedule_instances.generation + 1,
           next_scheduled_at = EXCLUDED.next_scheduled_at,
           retry_after = NULL,
           trigger_attempt_count = 0,
           trigger_error_message = '',
           updated_at = now()
    RETURNING id, schedule_id, org_id, project_id, environment_id, secret_bindings, workspace, run_options, active, generation, next_scheduled_at, last_scheduled_at, retry_after, trigger_attempt_count, trigger_error_message, created_at, updated_at
),
refreshed_instances AS (
    UPDATE task_schedule_instances
       SET generation = task_schedule_instances.generation + 1,
           next_scheduled_at = CASE WHEN task_schedule_instances.active THEN sqlc.arg(next_scheduled_at)::timestamptz ELSE NULL END,
           retry_after = NULL,
           trigger_attempt_count = 0,
           trigger_error_message = '',
           updated_at = now()
      FROM schedule
     WHERE task_schedule_instances.schedule_id = schedule.id
       AND task_schedule_instances.environment_id <> sqlc.arg(environment_id)
       AND schedule.timing_changed
    RETURNING task_schedule_instances.id
)
SELECT schedule.id AS schedule_id,
       instance.id AS instance_id,
       schedule.org_id,
       schedule.project_id,
       instance.environment_id,
       schedule.schedule_type,
       schedule.task_id,
       schedule.dedup_key,
       schedule.user_dedup_key,
       schedule.external_id,
       schedule.cron,
       schedule.timezone,
       instance.secret_bindings,
       instance.workspace,
       instance.run_options,
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
  JOIN instance ON true
  JOIN (SELECT count(*) FROM refreshed_instances) refreshed ON true;

-- name: ListScheduleSummaries :many
SELECT task_schedules.id AS schedule_id,
       task_schedule_instances.id AS instance_id,
       task_schedules.org_id,
       task_schedules.project_id,
       task_schedule_instances.environment_id,
       task_schedules.schedule_type,
       task_schedules.task_id,
       task_schedules.dedup_key,
       task_schedules.user_dedup_key,
       task_schedules.external_id,
       task_schedules.cron,
       task_schedules.timezone,
       task_schedule_instances.secret_bindings,
       task_schedule_instances.workspace,
       task_schedule_instances.run_options,
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
       task_schedules.user_dedup_key,
       task_schedules.external_id,
       task_schedules.cron,
       task_schedules.timezone,
       task_schedule_instances.secret_bindings,
       task_schedule_instances.workspace,
       task_schedule_instances.run_options,
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
       task_schedules.user_dedup_key,
       task_schedules.external_id,
       task_schedules.cron,
       task_schedules.timezone,
       task_schedule_instances.secret_bindings,
       task_schedule_instances.workspace,
       task_schedule_instances.run_options,
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
   AND task_schedule_instances.environment_id = sqlc.arg(environment_id)
   AND task_schedules.schedule_type = 'declarative'
   AND task_schedules.deleted_at IS NULL
 ORDER BY task_schedules.task_id ASC, task_schedules.dedup_key ASC;

-- name: UpdateScheduleState :one
WITH updated_schedule AS (
    UPDATE task_schedules
       SET active = true,
           updated_at = now()
     WHERE task_schedules.org_id = sqlc.arg(org_id)
       AND task_schedules.project_id = sqlc.arg(project_id)
       AND task_schedules.deleted_at IS NULL
       AND task_schedules.id = sqlc.arg(schedule_id)
    RETURNING *
),
updated_instances AS (
    UPDATE task_schedule_instances
       SET active = sqlc.arg(active),
           generation = task_schedule_instances.generation + 1,
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
       updated_schedule.user_dedup_key,
       updated_schedule.external_id,
       updated_schedule.cron,
       updated_schedule.timezone,
       updated_instance.secret_bindings,
       updated_instance.workspace,
       updated_instance.run_options,
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
WITH existing_schedule AS (
    SELECT task_schedules.id,
           task_schedules.cron,
           task_schedules.timezone
      FROM task_schedules
     WHERE task_schedules.org_id = sqlc.arg(org_id)
       AND task_schedules.project_id = sqlc.arg(project_id)
       AND task_schedules.deleted_at IS NULL
       AND task_schedules.id = sqlc.arg(schedule_id)
     FOR UPDATE
),
updated_schedule AS (
    UPDATE task_schedules
       SET task_id = sqlc.arg(task_id),
           external_id = sqlc.narg(external_id),
           cron = sqlc.arg(cron),
           timezone = sqlc.arg(timezone),
           active = true,
           updated_at = now()
      FROM existing_schedule
     WHERE task_schedules.org_id = sqlc.arg(org_id)
       AND task_schedules.project_id = sqlc.arg(project_id)
       AND task_schedules.deleted_at IS NULL
       AND task_schedules.id = sqlc.arg(schedule_id)
       AND task_schedules.id = existing_schedule.id
    RETURNING task_schedules.*,
              (
                  existing_schedule.cron IS DISTINCT FROM sqlc.arg(cron)
                  OR existing_schedule.timezone IS DISTINCT FROM sqlc.arg(timezone)
              ) AS timing_changed
),
updated_instances AS (
    UPDATE task_schedule_instances
       SET secret_bindings = CASE
               WHEN task_schedule_instances.environment_id = sqlc.arg(environment_id) THEN sqlc.arg(secret_bindings)::jsonb
               ELSE task_schedule_instances.secret_bindings
           END,
           workspace = CASE
               WHEN task_schedule_instances.environment_id = sqlc.arg(environment_id) THEN sqlc.arg(workspace)::jsonb
               ELSE task_schedule_instances.workspace
           END,
           run_options = CASE
               WHEN task_schedule_instances.environment_id = sqlc.arg(environment_id) THEN sqlc.arg(run_options)::jsonb
               ELSE task_schedule_instances.run_options
           END,
           active = CASE
               WHEN task_schedule_instances.environment_id = sqlc.arg(environment_id) THEN sqlc.arg(active)
               ELSE task_schedule_instances.active
           END,
           generation = CASE
               WHEN task_schedule_instances.environment_id = sqlc.arg(environment_id)
                    OR updated_schedule.timing_changed THEN task_schedule_instances.generation + 1
               ELSE task_schedule_instances.generation
           END,
           next_scheduled_at = CASE
               WHEN task_schedule_instances.environment_id = sqlc.arg(environment_id) THEN
                   CASE WHEN sqlc.arg(active) THEN sqlc.arg(next_scheduled_at)::timestamptz ELSE NULL END
               WHEN updated_schedule.timing_changed THEN
                   CASE WHEN task_schedule_instances.active THEN sqlc.arg(next_scheduled_at)::timestamptz ELSE NULL END
               ELSE task_schedule_instances.next_scheduled_at
           END,
           retry_after = CASE
               WHEN task_schedule_instances.environment_id = sqlc.arg(environment_id)
                    OR updated_schedule.timing_changed THEN NULL
               ELSE task_schedule_instances.retry_after
           END,
           trigger_attempt_count = CASE
               WHEN task_schedule_instances.environment_id = sqlc.arg(environment_id)
                    OR updated_schedule.timing_changed THEN 0
               ELSE task_schedule_instances.trigger_attempt_count
           END,
           trigger_error_message = CASE
               WHEN task_schedule_instances.environment_id = sqlc.arg(environment_id)
                    OR updated_schedule.timing_changed THEN ''
               ELSE task_schedule_instances.trigger_error_message
           END,
           updated_at = CASE
               WHEN task_schedule_instances.environment_id = sqlc.arg(environment_id)
                    OR updated_schedule.timing_changed THEN now()
               ELSE task_schedule_instances.updated_at
           END
      FROM updated_schedule
     WHERE task_schedule_instances.schedule_id = updated_schedule.id
       AND (
           task_schedule_instances.environment_id = sqlc.arg(environment_id)
           OR updated_schedule.timing_changed
       )
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
       updated_schedule.user_dedup_key,
       updated_schedule.external_id,
       updated_schedule.cron,
       updated_schedule.timezone,
       updated_instance.secret_bindings,
       updated_instance.workspace,
       updated_instance.run_options,
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
       AND task_schedules.deleted_at IS NULL
       AND task_schedules.id = sqlc.arg(schedule_id)
       AND NOT EXISTS (
           SELECT 1
             FROM task_schedule_instances remaining
            WHERE remaining.schedule_id = task_schedules.id
              AND remaining.environment_id <> sqlc.arg(environment_id)
       )
    RETURNING id
),
deleted_instance AS (
    DELETE FROM task_schedule_instances
     WHERE task_schedule_instances.schedule_id = sqlc.arg(schedule_id)
       AND task_schedule_instances.org_id = sqlc.arg(org_id)
       AND task_schedule_instances.project_id = sqlc.arg(project_id)
       AND task_schedule_instances.environment_id = sqlc.arg(environment_id)
    RETURNING id
)
SELECT count(*)::bigint FROM deleted_instance;

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
       task_schedules.external_id,
       task_schedules.cron,
       task_schedules.timezone,
       task_schedule_instances.secret_bindings,
       task_schedule_instances.workspace,
       task_schedule_instances.run_options,
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
