-- name: CreateSchedule :one
WITH schedule_lock AS MATERIALIZED (
    SELECT pg_advisory_xact_lock(hashtextextended(
        concat_ws(':',
            'task_schedule',
            sqlc.arg(org_id)::uuid::text,
            sqlc.arg(project_id)::uuid::text,
            'imperative',
            coalesce(sqlc.narg(user_dedup_key)::text, sqlc.arg(dedup_key)::text)
        ),
        0
    ))
),
existing_schedule AS MATERIALIZED (
    SELECT task_schedules.id,
           task_schedules.cron,
           task_schedules.timezone
      FROM task_schedules
      JOIN schedule_lock ON true
     WHERE task_schedules.org_id = sqlc.arg(org_id)
       AND task_schedules.project_id = sqlc.arg(project_id)
       AND task_schedules.schedule_type = 'imperative'
       AND sqlc.narg(user_dedup_key)::text IS NOT NULL
       AND task_schedules.user_dedup_key = sqlc.narg(user_dedup_key)::text
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
       AND task_schedules.id = existing_schedule.id
       AND task_schedules.schedule_type = 'imperative'
    RETURNING task_schedules.id, task_schedules.org_id, task_schedules.cell_id, task_schedules.project_id, task_schedules.schedule_type, task_schedules.task_id, task_schedules.dedup_key, task_schedules.user_dedup_key, task_schedules.external_id, task_schedules.cron, task_schedules.timezone, task_schedules.active, task_schedules.created_at, task_schedules.updated_at,
              (
                  existing_schedule.cron IS DISTINCT FROM sqlc.arg(cron)
                  OR existing_schedule.timezone IS DISTINCT FROM sqlc.arg(timezone)
              ) AS timing_changed
),
inserted_schedule AS (
    INSERT INTO task_schedules (
        id,
        org_id,
        cell_id,
        project_id,
        schedule_type,
        task_id,
        dedup_key,
        user_dedup_key,
        external_id,
        cron,
        timezone,
        active
    )
    SELECT sqlc.arg(schedule_id),
           sqlc.arg(org_id),
           sqlc.arg(cell_id),
           sqlc.arg(project_id),
           sqlc.arg(schedule_type)::task_schedule_type,
           sqlc.arg(task_id),
           sqlc.arg(dedup_key),
           sqlc.narg(user_dedup_key),
           sqlc.narg(external_id),
           sqlc.arg(cron),
           sqlc.arg(timezone),
           true
      FROM schedule_lock
     WHERE NOT EXISTS (SELECT 1 FROM updated_schedule)
    RETURNING id, org_id, cell_id, project_id, schedule_type, task_id, dedup_key, user_dedup_key, external_id, cron, timezone, active, created_at, updated_at,
              false AS timing_changed
),
schedule AS (
    SELECT *
      FROM updated_schedule
    UNION ALL
    SELECT *
      FROM inserted_schedule
),
instance_inputs AS (
    SELECT sqlc.arg(instance_id) AS id,
		           schedule.id AS schedule_id,
		           schedule.org_id,
		           schedule.cell_id,
		           schedule.project_id,
	           sqlc.arg(environment_id)::uuid AS environment_id,
	           schedule.task_id,
	           sqlc.arg(run_options)::jsonb AS run_options,
           sqlc.arg(active) AS active,
           CASE WHEN sqlc.arg(active) THEN sqlc.arg(next_fire_at)::timestamptz ELSE NULL END AS next_fire_at
      FROM schedule
    UNION ALL
    SELECT uuidv7() AS id,
		           task_schedule_instances.schedule_id,
		           task_schedule_instances.org_id,
		           task_schedule_instances.cell_id,
		           task_schedule_instances.project_id,
	           task_schedule_instances.environment_id,
	           schedule.task_id,
	           task_schedule_instances.run_options,
	           task_schedule_instances.active,
           CASE WHEN task_schedule_instances.active THEN sqlc.arg(next_fire_at)::timestamptz ELSE NULL END AS next_fire_at
      FROM task_schedule_instances
      JOIN schedule ON schedule.id = task_schedule_instances.schedule_id
     WHERE task_schedule_instances.environment_id <> sqlc.arg(environment_id)
       AND schedule.timing_changed
),
instances AS (
    INSERT INTO task_schedule_instances (
        id,
		        schedule_id,
		        org_id,
		        cell_id,
		        project_id,
	        environment_id,
	        task_id,
	        run_options,
	        active,
        next_fire_at
    )
    SELECT id,
		           schedule_id,
		           org_id,
		           cell_id,
		           project_id,
	           environment_id,
	           task_id,
	           run_options,
	           active,
           next_fire_at
      FROM instance_inputs
    ON CONFLICT (schedule_id, environment_id) DO UPDATE
	       SET run_options = EXCLUDED.run_options,
	           task_id = EXCLUDED.task_id,
	           active = EXCLUDED.active,
           generation = task_schedule_instances.generation + 1,
           next_fire_at = EXCLUDED.next_fire_at,
           retry_after = NULL,
           trigger_attempt_count = 0,
           trigger_error_kind = '',
           trigger_error_message = '',
           last_trigger_run_id = NULL,
           updated_at = now()
    RETURNING id, schedule_id, org_id, project_id, environment_id, run_options, active, generation, next_fire_at, last_fire_at, retry_after, trigger_attempt_count, trigger_error_kind, trigger_error_message, last_trigger_run_id, created_at, updated_at
),
instance AS (
    SELECT *
      FROM instances
     WHERE environment_id = sqlc.arg(environment_id)
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
       instance.run_options,
       schedule.active AS schedule_active,
       instance.active AS instance_active,
       instance.generation,
       instance.next_fire_at,
       instance.last_fire_at,
       instance.retry_after,
       instance.trigger_attempt_count,
       instance.trigger_error_kind,
       instance.trigger_error_message,
       instance.last_trigger_run_id,
       schedule.created_at,
       schedule.updated_at
  FROM schedule
  JOIN instance ON true;

-- name: CreateDeclarativeSchedule :one
WITH schedule_lock AS MATERIALIZED (
    SELECT pg_advisory_xact_lock(hashtextextended(
        concat_ws(':',
            'task_schedule',
            sqlc.arg(org_id)::uuid::text,
            sqlc.arg(project_id)::uuid::text,
            'declarative',
            sqlc.arg(dedup_key)::text
        ),
        0
    ))
),
existing_schedule AS MATERIALIZED (
    SELECT task_schedules.id,
           task_schedules.cron,
           task_schedules.timezone
      FROM task_schedules
      JOIN schedule_lock ON true
     WHERE task_schedules.org_id = sqlc.arg(org_id)
       AND task_schedules.project_id = sqlc.arg(project_id)
       AND task_schedules.schedule_type = 'declarative'
       AND task_schedules.dedup_key = sqlc.arg(dedup_key)
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
       AND task_schedules.id = existing_schedule.id
       AND task_schedules.schedule_type = 'declarative'
    RETURNING task_schedules.id, task_schedules.org_id, task_schedules.cell_id, task_schedules.project_id, task_schedules.schedule_type, task_schedules.task_id, task_schedules.dedup_key, task_schedules.user_dedup_key, task_schedules.external_id, task_schedules.cron, task_schedules.timezone, task_schedules.active, task_schedules.created_at, task_schedules.updated_at,
              (
                  existing_schedule.cron IS DISTINCT FROM sqlc.arg(cron)
                  OR existing_schedule.timezone IS DISTINCT FROM sqlc.arg(timezone)
              ) AS timing_changed
),
inserted_schedule AS (
    INSERT INTO task_schedules (
        id,
        org_id,
        cell_id,
        project_id,
        schedule_type,
        task_id,
        dedup_key,
        external_id,
        cron,
        timezone,
        active
    )
    SELECT sqlc.arg(schedule_id),
           sqlc.arg(org_id),
           sqlc.arg(cell_id),
           sqlc.arg(project_id),
           'declarative',
           sqlc.arg(task_id),
           sqlc.arg(dedup_key),
           sqlc.narg(external_id),
           sqlc.arg(cron),
           sqlc.arg(timezone),
           true
      FROM schedule_lock
     WHERE NOT EXISTS (SELECT 1 FROM updated_schedule)
    RETURNING id, org_id, cell_id, project_id, schedule_type, task_id, dedup_key, user_dedup_key, external_id, cron, timezone, active, created_at, updated_at,
              false AS timing_changed
),
schedule AS (
    SELECT *
      FROM updated_schedule
    UNION ALL
    SELECT *
      FROM inserted_schedule
),
instance_inputs AS (
    SELECT sqlc.arg(instance_id) AS id,
		           schedule.id AS schedule_id,
		           schedule.org_id,
		           schedule.cell_id,
		           schedule.project_id,
	           sqlc.arg(environment_id)::uuid AS environment_id,
	           schedule.task_id,
	           sqlc.arg(run_options)::jsonb AS run_options,
           sqlc.arg(active) AS active,
           CASE WHEN sqlc.arg(active) THEN sqlc.arg(next_fire_at)::timestamptz ELSE NULL END AS next_fire_at
      FROM schedule
    UNION ALL
    SELECT uuidv7() AS id,
		           task_schedule_instances.schedule_id,
		           task_schedule_instances.org_id,
		           task_schedule_instances.cell_id,
		           task_schedule_instances.project_id,
	           task_schedule_instances.environment_id,
	           schedule.task_id,
	           task_schedule_instances.run_options,
           task_schedule_instances.active,
           CASE WHEN task_schedule_instances.active THEN sqlc.arg(next_fire_at)::timestamptz ELSE NULL END AS next_fire_at
      FROM task_schedule_instances
      JOIN schedule ON schedule.id = task_schedule_instances.schedule_id
     WHERE task_schedule_instances.environment_id <> sqlc.arg(environment_id)
       AND schedule.timing_changed
),
instances AS (
    INSERT INTO task_schedule_instances (
        id,
		        schedule_id,
		        org_id,
		        cell_id,
		        project_id,
	        environment_id,
	        task_id,
	        run_options,
	        active,
        next_fire_at
    )
    SELECT id,
		           schedule_id,
		           org_id,
		           cell_id,
		           project_id,
	           environment_id,
	           task_id,
	           run_options,
           active,
           next_fire_at
      FROM instance_inputs
    ON CONFLICT (schedule_id, environment_id) DO UPDATE
	       SET run_options = EXCLUDED.run_options,
	           task_id = EXCLUDED.task_id,
	           active = EXCLUDED.active,
           generation = task_schedule_instances.generation + 1,
           next_fire_at = EXCLUDED.next_fire_at,
           retry_after = NULL,
           trigger_attempt_count = 0,
           trigger_error_kind = '',
           trigger_error_message = '',
           last_trigger_run_id = NULL,
           updated_at = now()
    RETURNING id, schedule_id, org_id, project_id, environment_id, run_options, active, generation, next_fire_at, last_fire_at, retry_after, trigger_attempt_count, trigger_error_kind, trigger_error_message, last_trigger_run_id, created_at, updated_at
),
instance AS (
    SELECT *
      FROM instances
     WHERE environment_id = sqlc.arg(environment_id)
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
       instance.run_options,
       schedule.active AS schedule_active,
       instance.active AS instance_active,
       instance.generation,
       instance.next_fire_at,
       instance.last_fire_at,
       instance.retry_after,
       instance.trigger_attempt_count,
       instance.trigger_error_kind,
       instance.trigger_error_message,
       instance.last_trigger_run_id,
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
       task_schedules.user_dedup_key,
       task_schedules.external_id,
       task_schedules.cron,
       task_schedules.timezone,
       task_schedule_instances.run_options,
       task_schedules.active AS schedule_active,
       task_schedule_instances.active AS instance_active,
       task_schedule_instances.generation,
       task_schedule_instances.next_fire_at,
       task_schedule_instances.last_fire_at,
       task_schedule_instances.retry_after,
       task_schedule_instances.trigger_attempt_count,
       task_schedule_instances.trigger_error_kind,
       task_schedule_instances.trigger_error_message,
       task_schedule_instances.last_trigger_run_id,
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
       task_schedules.schedule_type,
       task_schedules.task_id,
       task_schedules.dedup_key,
       task_schedules.user_dedup_key,
       task_schedules.external_id,
       task_schedules.cron,
       task_schedules.timezone,
       task_schedule_instances.run_options,
       task_schedules.active AS schedule_active,
       task_schedule_instances.active AS instance_active,
       task_schedule_instances.generation,
       task_schedule_instances.next_fire_at,
       task_schedule_instances.last_fire_at,
       task_schedule_instances.retry_after,
       task_schedule_instances.trigger_attempt_count,
       task_schedule_instances.trigger_error_kind,
       task_schedule_instances.trigger_error_message,
       task_schedule_instances.last_trigger_run_id,
       task_schedules.created_at,
       task_schedules.updated_at
 FROM task_schedules
  JOIN task_schedule_instances ON task_schedule_instances.schedule_id = task_schedules.id
 WHERE task_schedules.org_id = sqlc.arg(org_id)
   AND task_schedules.project_id = sqlc.arg(project_id)
   AND task_schedule_instances.environment_id = sqlc.arg(environment_id)
   AND task_schedules.id = sqlc.arg(schedule_id);

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
       task_schedule_instances.run_options,
       task_schedules.active AS schedule_active,
       task_schedule_instances.active AS instance_active,
       task_schedule_instances.generation,
       task_schedule_instances.next_fire_at,
       task_schedule_instances.last_fire_at,
       task_schedule_instances.retry_after,
       task_schedule_instances.trigger_attempt_count,
       task_schedule_instances.trigger_error_kind,
       task_schedule_instances.trigger_error_message,
       task_schedule_instances.last_trigger_run_id,
       task_schedules.created_at,
       task_schedules.updated_at
 FROM task_schedules
  JOIN task_schedule_instances ON task_schedule_instances.schedule_id = task_schedules.id
 WHERE task_schedules.org_id = sqlc.arg(org_id)
   AND task_schedules.project_id = sqlc.arg(project_id)
   AND task_schedule_instances.environment_id = sqlc.arg(environment_id)
   AND task_schedules.schedule_type = 'declarative'
 ORDER BY task_schedules.task_id ASC, task_schedules.dedup_key ASC;

-- name: UpdateScheduleState :one
WITH updated_schedule AS (
    UPDATE task_schedules
       SET active = true,
           updated_at = now()
     WHERE task_schedules.org_id = sqlc.arg(org_id)
       AND task_schedules.project_id = sqlc.arg(project_id)
       AND task_schedules.id = sqlc.arg(schedule_id)
    RETURNING *
),
updated_instances AS (
    UPDATE task_schedule_instances
       SET active = sqlc.arg(active),
           generation = task_schedule_instances.generation + 1,
           next_fire_at = sqlc.narg(next_fire_at),
           retry_after = NULL,
           trigger_attempt_count = 0,
           trigger_error_kind = '',
           trigger_error_message = '',
           last_trigger_run_id = NULL,
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
       updated_instance.run_options,
       updated_schedule.active AS schedule_active,
       updated_instance.active AS instance_active,
       updated_instance.generation,
       updated_instance.next_fire_at,
       updated_instance.last_fire_at,
       updated_instance.retry_after,
       updated_instance.trigger_attempt_count,
       updated_instance.trigger_error_kind,
       updated_instance.trigger_error_message,
       updated_instance.last_trigger_run_id,
       updated_schedule.created_at,
       updated_schedule.updated_at
  FROM updated_schedule
  JOIN updated_instance ON true;

-- name: UpdateSchedule :one
WITH existing_schedule AS MATERIALIZED (
    SELECT task_schedules.id,
           task_schedules.cron,
           task_schedules.timezone
      FROM task_schedules
     WHERE task_schedules.org_id = sqlc.arg(org_id)
       AND task_schedules.project_id = sqlc.arg(project_id)
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
       SET run_options = CASE
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
           next_fire_at = CASE
               WHEN task_schedule_instances.environment_id = sqlc.arg(environment_id) THEN
                   CASE WHEN sqlc.arg(active) THEN sqlc.arg(next_fire_at)::timestamptz ELSE NULL END
               WHEN updated_schedule.timing_changed THEN
                   CASE WHEN task_schedule_instances.active THEN sqlc.arg(next_fire_at)::timestamptz ELSE NULL END
               ELSE task_schedule_instances.next_fire_at
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
           trigger_error_kind = CASE
               WHEN task_schedule_instances.environment_id = sqlc.arg(environment_id)
                    OR updated_schedule.timing_changed THEN ''
               ELSE task_schedule_instances.trigger_error_kind
           END,
           last_trigger_run_id = CASE
               WHEN task_schedule_instances.environment_id = sqlc.arg(environment_id)
                    OR updated_schedule.timing_changed THEN NULL
               ELSE task_schedule_instances.last_trigger_run_id
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
       updated_instance.run_options,
       updated_schedule.active AS schedule_active,
       updated_instance.active AS instance_active,
       updated_instance.generation,
       updated_instance.next_fire_at,
       updated_instance.last_fire_at,
       updated_instance.retry_after,
       updated_instance.trigger_attempt_count,
       updated_instance.trigger_error_kind,
       updated_instance.trigger_error_message,
       updated_instance.last_trigger_run_id,
       updated_schedule.created_at,
       updated_schedule.updated_at
  FROM updated_schedule
  JOIN updated_instance ON true;

-- name: DeleteSchedule :one
WITH target_schedule AS MATERIALIZED (
    SELECT task_schedules.id
      FROM task_schedules
     WHERE task_schedules.org_id = sqlc.arg(org_id)
       AND task_schedules.project_id = sqlc.arg(project_id)
       AND task_schedules.id = sqlc.arg(schedule_id)
     FOR UPDATE
),
target_instance AS MATERIALIZED (
    SELECT task_schedule_instances.id
      FROM task_schedule_instances
      JOIN target_schedule ON target_schedule.id = task_schedule_instances.schedule_id
     WHERE task_schedule_instances.org_id = sqlc.arg(org_id)
       AND task_schedule_instances.project_id = sqlc.arg(project_id)
       AND task_schedule_instances.environment_id = sqlc.arg(environment_id)
     FOR UPDATE
),
deleted_schedule AS (
    DELETE FROM task_schedules
     WHERE task_schedules.org_id = sqlc.arg(org_id)
       AND task_schedules.project_id = sqlc.arg(project_id)
       AND task_schedules.id = sqlc.arg(schedule_id)
       AND EXISTS (SELECT 1 FROM target_instance)
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
       AND EXISTS (SELECT 1 FROM target_instance)
       AND NOT EXISTS (SELECT 1 FROM deleted_schedule)
    RETURNING id
)
SELECT count(*)::bigint FROM target_instance;

-- name: ListScheduleRepairEntries :many
WITH index_entries AS (
    SELECT task_schedules.id AS schedule_id,
           task_schedule_instances.id AS instance_id,
           task_schedules.org_id,
           task_schedules.project_id,
           task_schedule_instances.environment_id,
           task_schedule_instances.generation,
           task_schedule_instances.next_fire_at,
           task_schedule_instances.retry_after,
           coalesce(task_schedule_instances.retry_after, task_schedule_instances.next_fire_at) AS available_at
      FROM task_schedule_instances
      JOIN task_schedules ON task_schedules.id = task_schedule_instances.schedule_id
     WHERE task_schedules.active
       AND task_schedule_instances.active
       AND task_schedule_instances.next_fire_at IS NOT NULL
       AND coalesce(task_schedule_instances.retry_after, task_schedule_instances.next_fire_at) <= sqlc.arg(available_before)
)
SELECT schedule_id,
       instance_id,
       org_id,
       project_id,
       environment_id,
       generation,
       next_fire_at,
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

-- name: ListScheduleInstancesForRegistration :many
SELECT task_schedules.id AS schedule_id,
       task_schedule_instances.id AS instance_id,
       task_schedules.active AS schedule_active,
       task_schedule_instances.active AS instance_active,
       task_schedule_instances.generation,
       task_schedule_instances.next_fire_at,
       task_schedule_instances.retry_after
  FROM task_schedules
  JOIN task_schedule_instances ON task_schedule_instances.schedule_id = task_schedules.id
 WHERE task_schedules.org_id = sqlc.arg(org_id)
   AND task_schedules.project_id = sqlc.arg(project_id)
   AND task_schedules.id = sqlc.arg(schedule_id)
 ORDER BY task_schedule_instances.environment_id;

-- name: GetScheduleRetryAfter :one
SELECT task_schedule_instances.retry_after
  FROM task_schedule_instances
  JOIN task_schedules ON task_schedules.id = task_schedule_instances.schedule_id
 WHERE task_schedule_instances.id = sqlc.arg(instance_id)
   AND task_schedule_instances.generation = sqlc.arg(generation)
   AND task_schedule_instances.next_fire_at = sqlc.arg(scheduled_at)
   AND task_schedule_instances.active
   AND task_schedule_instances.retry_after > now()
   AND task_schedules.active;

-- name: GetScheduleTriggerCandidate :one
SELECT task_schedules.id AS schedule_id,
       task_schedule_instances.id AS instance_id,
       task_schedules.org_id,
       task_schedules.project_id,
       task_schedule_instances.environment_id,
       task_schedules.schedule_type,
       task_schedules.task_id,
       task_schedules.external_id,
       task_schedules.cron,
       task_schedules.timezone,
       task_schedule_instances.run_options,
       task_schedule_instances.generation,
       task_schedule_instances.next_fire_at,
       task_schedule_instances.last_fire_at,
       task_schedule_instances.retry_after,
       task_schedule_instances.trigger_attempt_count,
       task_schedule_instances.trigger_error_kind,
       task_schedule_instances.trigger_error_message,
       task_schedule_instances.last_trigger_run_id
  FROM task_schedule_instances
  JOIN task_schedules ON task_schedules.id = task_schedule_instances.schedule_id
 WHERE task_schedule_instances.id = sqlc.arg(instance_id)
   AND task_schedule_instances.generation = sqlc.arg(generation)
   AND task_schedule_instances.next_fire_at = sqlc.arg(scheduled_at)
   AND task_schedule_instances.active
   AND (
       task_schedule_instances.retry_after IS NULL
       OR task_schedule_instances.retry_after <= now()
   )
   AND task_schedules.active;

-- name: ScheduleInstanceTriggerIsCurrent :one
SELECT EXISTS (
    SELECT 1
      FROM task_schedule_instances
      JOIN task_schedules ON task_schedules.id = task_schedule_instances.schedule_id
     WHERE task_schedule_instances.id = sqlc.arg(instance_id)
       AND task_schedule_instances.generation = sqlc.arg(generation)
       AND task_schedule_instances.next_fire_at = sqlc.arg(scheduled_at)
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
) AS current;

-- name: AdvanceScheduleInstance :one
UPDATE task_schedule_instances
   SET next_fire_at = sqlc.narg(next_fire_at),
       last_fire_at = sqlc.arg(last_fire_at),
       retry_after = NULL,
       trigger_attempt_count = 0,
       trigger_error_kind = '',
       trigger_error_message = '',
       last_trigger_run_id = sqlc.arg(last_trigger_run_id),
       updated_at = now()
 WHERE id = sqlc.arg(instance_id)
   AND generation = sqlc.arg(generation)
   AND next_fire_at = sqlc.arg(last_fire_at)
   AND active
 RETURNING id AS instance_id,
           generation,
           next_fire_at;

-- name: SkipScheduleInstanceTrigger :one
UPDATE task_schedule_instances
	   SET next_fire_at = sqlc.arg(next_fire_at),
	       retry_after = NULL,
	       trigger_attempt_count = 0,
	       trigger_error_kind = '',
	       trigger_error_message = '',
	       last_trigger_run_id = NULL,
	       updated_at = now()
 WHERE id = sqlc.arg(instance_id)
   AND generation = sqlc.arg(generation)
   AND next_fire_at = sqlc.arg(last_fire_at)
   AND active
 RETURNING id AS instance_id,
           generation,
           next_fire_at;

-- name: StopScheduleInstanceTrigger :execrows
UPDATE task_schedule_instances
   SET next_fire_at = NULL,
       retry_after = NULL,
       updated_at = now()
 WHERE id = sqlc.arg(instance_id)
   AND generation = sqlc.arg(generation)
   AND next_fire_at = sqlc.arg(scheduled_at)
   AND active;

-- name: DeferScheduleInstanceTrigger :execrows
UPDATE task_schedule_instances
   SET retry_after = sqlc.arg(retry_after),
       updated_at = now()
 WHERE id = sqlc.arg(instance_id)
   AND generation = sqlc.arg(generation)
   AND next_fire_at = sqlc.arg(scheduled_at)
   AND active;

-- name: MarkScheduleInstanceTriggerFailed :execrows
UPDATE task_schedule_instances
   SET trigger_attempt_count = trigger_attempt_count + 1,
       trigger_error_kind = sqlc.arg(error_kind),
       trigger_error_message = sqlc.arg(error_message),
       retry_after = sqlc.arg(retry_after),
       updated_at = now()
 WHERE id = sqlc.arg(instance_id)
   AND generation = sqlc.arg(generation)
   AND next_fire_at = sqlc.arg(scheduled_at)
   AND active;
