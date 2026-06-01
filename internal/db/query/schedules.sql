-- name: CreateSchedule :one
WITH schedule AS (
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
        payload,
        secret_bindings,
        workspace,
        run_options,
        active
    ) VALUES (
        sqlc.arg(schedule_id),
        sqlc.arg(org_id),
        sqlc.arg(project_id),
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
    ON CONFLICT (org_id, project_id, dedup_key) WHERE deleted_at IS NULL
    DO UPDATE SET updated_at = task_schedules.updated_at
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
    ON CONFLICT (schedule_id, environment_id)
    DO UPDATE SET updated_at = task_schedule_instances.updated_at
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
       instance.next_due_at,
       instance.last_scheduled_at,
       instance.materialize_attempt_count,
       instance.materialize_error_message,
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
       task_schedule_instances.next_due_at,
       task_schedule_instances.last_scheduled_at,
       task_schedule_instances.materialize_attempt_count,
       task_schedule_instances.materialize_error_message,
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
       task_schedule_instances.next_due_at,
       task_schedule_instances.last_scheduled_at,
       task_schedule_instances.materialize_attempt_count,
       task_schedule_instances.materialize_error_message,
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
       task_schedule_instances.next_due_at,
       task_schedule_instances.last_scheduled_at,
       task_schedule_instances.materialize_attempt_count,
       task_schedule_instances.materialize_error_message,
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
       SET active = sqlc.arg(active),
           updated_at = now()
     WHERE task_schedules.org_id = sqlc.arg(org_id)
       AND task_schedules.project_id = sqlc.arg(project_id)
       AND task_schedules.deleted_at IS NULL
       AND EXISTS (
           SELECT 1
             FROM task_schedule_instances
            WHERE task_schedule_instances.schedule_id = task_schedules.id
              AND task_schedule_instances.environment_id = sqlc.arg(environment_id)
       )
       AND task_schedules.id = sqlc.arg(schedule_id)
    RETURNING *
),
updated_instances AS (
    UPDATE task_schedule_instances
       SET active = sqlc.arg(active),
           generation = generation + 1,
           next_scheduled_at = sqlc.narg(next_scheduled_at),
           next_due_at = CASE
               WHEN sqlc.arg(active)::boolean AND sqlc.narg(next_scheduled_at)::timestamptz IS NOT NULL
                   THEN sqlc.narg(next_scheduled_at)::timestamptz
                        + make_interval(secs => mod(abs(hashtextextended(task_schedule_instances.id::text, 0)), GREATEST(sqlc.arg(jitter_seconds)::bigint, 1))::int)
               ELSE NULL
           END,
           materialize_lease_id = NULL,
           materialize_lease_expires_at = NULL,
           materialize_attempt_count = 0,
           materialize_error_message = '',
           updated_at = now()
      FROM updated_schedule
     WHERE task_schedule_instances.schedule_id = updated_schedule.id
    RETURNING task_schedule_instances.*
),
superseded_fires AS (
    UPDATE task_schedule_fires
       SET status = 'superseded',
           lease_id = NULL,
           lease_expires_at = NULL,
           error_message = 'schedule generation changed',
           completed_at = now(),
           retention_expires_at = coalesce(retention_expires_at, now() + interval '90 days'),
           updated_at = now()
      FROM updated_instances
     WHERE task_schedule_fires.schedule_instance_id = updated_instances.id
       AND task_schedule_fires.generation < updated_instances.generation
       AND task_schedule_fires.status IN ('pending', 'failed', 'leased')
    RETURNING 1
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
       updated_instance.next_due_at,
       updated_instance.last_scheduled_at,
       updated_instance.materialize_attempt_count,
       updated_instance.materialize_error_message,
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
       AND task_schedules.deleted_at IS NULL
       AND EXISTS (
           SELECT 1
             FROM task_schedule_instances
            WHERE task_schedule_instances.schedule_id = task_schedules.id
              AND task_schedule_instances.environment_id = sqlc.arg(environment_id)
       )
       AND task_schedules.id = sqlc.arg(schedule_id)
    RETURNING *
),
updated_instances AS (
    UPDATE task_schedule_instances
       SET active = sqlc.arg(active),
           generation = generation + 1,
           next_scheduled_at = sqlc.narg(next_scheduled_at),
           next_due_at = CASE
               WHEN sqlc.arg(active)::boolean AND sqlc.narg(next_scheduled_at)::timestamptz IS NOT NULL
                   THEN sqlc.narg(next_scheduled_at)::timestamptz
                        + make_interval(secs => mod(abs(hashtextextended(task_schedule_instances.id::text, 0)), GREATEST(sqlc.arg(jitter_seconds)::bigint, 1))::int)
               ELSE NULL
           END,
           materialize_lease_id = NULL,
           materialize_lease_expires_at = NULL,
           materialize_attempt_count = 0,
           materialize_error_message = '',
           updated_at = now()
      FROM updated_schedule
     WHERE task_schedule_instances.schedule_id = updated_schedule.id
    RETURNING task_schedule_instances.*
),
superseded_fires AS (
    UPDATE task_schedule_fires
       SET status = 'superseded',
           lease_id = NULL,
           lease_expires_at = NULL,
           error_message = 'schedule generation changed',
           completed_at = now(),
           retention_expires_at = coalesce(retention_expires_at, now() + interval '90 days'),
           updated_at = now()
      FROM updated_instances
     WHERE task_schedule_fires.schedule_instance_id = updated_instances.id
       AND task_schedule_fires.generation < updated_instances.generation
       AND task_schedule_fires.status IN ('pending', 'failed', 'leased')
    RETURNING 1
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
       updated_instance.next_due_at,
       updated_instance.last_scheduled_at,
       updated_instance.materialize_attempt_count,
       updated_instance.materialize_error_message,
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
       AND EXISTS (
           SELECT 1
             FROM task_schedule_instances
            WHERE task_schedule_instances.schedule_id = task_schedules.id
              AND task_schedule_instances.environment_id = sqlc.arg(environment_id)
       )
       AND task_schedules.id = sqlc.arg(schedule_id)
    RETURNING id
),
deleted_instances AS (
    UPDATE task_schedule_instances
       SET active = false,
           generation = generation + 1,
           next_scheduled_at = NULL,
           next_due_at = NULL,
           materialize_lease_id = NULL,
           materialize_lease_expires_at = NULL,
           updated_at = now()
      FROM deleted_schedule
     WHERE task_schedule_instances.schedule_id = deleted_schedule.id
    RETURNING task_schedule_instances.*
),
superseded_fires AS (
    UPDATE task_schedule_fires
       SET status = 'superseded',
           lease_id = NULL,
           lease_expires_at = NULL,
           error_message = 'schedule deleted',
           completed_at = now(),
           retention_expires_at = coalesce(retention_expires_at, now() + interval '90 days'),
           updated_at = now()
      FROM deleted_instances
     WHERE task_schedule_fires.schedule_instance_id = deleted_instances.id
       AND task_schedule_fires.generation < deleted_instances.generation
       AND task_schedule_fires.status IN ('pending', 'failed', 'leased')
    RETURNING 1
)
SELECT count(*)::bigint FROM deleted_schedule;

-- name: ClaimDueScheduleInstances :many
WITH candidate AS (
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
           task_schedule_instances.next_due_at,
           task_schedule_instances.last_scheduled_at,
           task_schedule_instances.materialize_attempt_count,
           task_schedule_instances.materialize_error_message
      FROM task_schedule_instances
      JOIN task_schedules ON task_schedules.id = task_schedule_instances.schedule_id
     WHERE task_schedules.active
       AND task_schedules.deleted_at IS NULL
       AND task_schedule_instances.active
       AND task_schedule_instances.next_due_at IS NOT NULL
       AND task_schedule_instances.next_due_at <= now()
       AND (
           task_schedule_instances.materialize_lease_expires_at IS NULL
           OR task_schedule_instances.materialize_lease_expires_at <= now()
       )
     ORDER BY task_schedule_instances.next_due_at, task_schedule_instances.id
     LIMIT sqlc.arg(row_limit)
     FOR UPDATE OF task_schedule_instances SKIP LOCKED
),
claimed AS (
    UPDATE task_schedule_instances
       SET materialize_lease_id = sqlc.arg(lease_id),
           materialize_lease_expires_at = sqlc.arg(lease_expires_at),
           updated_at = now()
      FROM candidate
     WHERE task_schedule_instances.id = candidate.instance_id
    RETURNING candidate.*,
              task_schedule_instances.materialize_lease_id,
              task_schedule_instances.materialize_lease_expires_at
)
SELECT schedule_id,
       instance_id,
       org_id,
       project_id,
       environment_id,
       task_id,
       cron,
       timezone,
       payload,
       secret_bindings,
       workspace,
       run_options,
       generation,
       next_scheduled_at,
       next_due_at,
       last_scheduled_at,
       materialize_lease_id,
       materialize_lease_expires_at,
       materialize_attempt_count,
       materialize_error_message
  FROM claimed;

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
) SELECT
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
  FROM task_schedule_instances
  JOIN task_schedules ON task_schedules.id = task_schedule_instances.schedule_id
 WHERE task_schedule_instances.id = sqlc.arg(schedule_instance_id)
   AND task_schedule_instances.org_id = sqlc.arg(org_id)
   AND task_schedule_instances.project_id = sqlc.arg(project_id)
   AND task_schedule_instances.environment_id = sqlc.arg(environment_id)
   AND task_schedule_instances.schedule_id = sqlc.arg(schedule_id)
   AND task_schedule_instances.generation = sqlc.arg(generation)
   AND task_schedule_instances.materialize_lease_id = sqlc.arg(materialize_lease_id)
   AND task_schedule_instances.materialize_lease_expires_at > now()
   AND task_schedule_instances.active
   AND task_schedules.active
   AND task_schedules.deleted_at IS NULL
ON CONFLICT (schedule_instance_id, scheduled_at) DO NOTHING;

-- name: AdvanceScheduleInstance :execrows
UPDATE task_schedule_instances
   SET next_scheduled_at = sqlc.narg(next_scheduled_at),
       next_due_at = sqlc.narg(next_due_at),
       last_scheduled_at = sqlc.arg(last_scheduled_at),
       materialize_lease_id = NULL,
       materialize_lease_expires_at = NULL,
       materialize_attempt_count = 0,
       materialize_error_message = '',
       updated_at = now()
 WHERE id = sqlc.arg(instance_id)
   AND generation = sqlc.arg(generation)
   AND materialize_lease_id = sqlc.arg(materialize_lease_id)
   AND materialize_lease_expires_at > now();

-- name: MarkScheduleInstanceMaterializationFailed :execrows
UPDATE task_schedule_instances
   SET materialize_attempt_count = materialize_attempt_count + 1,
       materialize_error_message = sqlc.arg(error_message),
       materialize_lease_id = NULL,
       materialize_lease_expires_at = NULL,
       next_due_at = CASE
           WHEN materialize_attempt_count + 1 >= sqlc.arg(max_attempts) THEN NULL
           ELSE sqlc.arg(next_due_at)
       END,
       active = CASE
           WHEN materialize_attempt_count + 1 >= sqlc.arg(max_attempts) THEN false
           ELSE active
       END,
       updated_at = now()
 WHERE id = sqlc.arg(instance_id)
   AND generation = sqlc.arg(generation)
   AND materialize_lease_id = sqlc.arg(materialize_lease_id)
   AND materialize_lease_expires_at > now()
   AND next_scheduled_at = sqlc.arg(next_scheduled_at)
   AND active;

-- name: MarkExhaustedScheduleFires :execrows
WITH candidate AS (
    SELECT task_schedule_fires.schedule_instance_id,
           task_schedule_fires.scheduled_at
      FROM task_schedule_fires
     WHERE (
           (task_schedule_fires.status = 'leased' AND task_schedule_fires.lease_expires_at <= now())
           OR (task_schedule_fires.status = 'failed' AND task_schedule_fires.next_attempt_at <= now())
       )
       AND task_schedule_fires.attempt_count >= sqlc.arg(max_attempts)
     ORDER BY coalesce(task_schedule_fires.lease_expires_at, task_schedule_fires.next_attempt_at),
              task_schedule_fires.scheduled_at
     LIMIT sqlc.arg(row_limit)
     FOR UPDATE OF task_schedule_fires SKIP LOCKED
)
UPDATE task_schedule_fires
   SET status = 'exhausted',
       lease_id = NULL,
       lease_expires_at = NULL,
       error_message = CASE
           WHEN error_message LIKE 'schedule fire attempts exhausted%' THEN error_message
           WHEN error_message = '' THEN 'schedule fire attempts exhausted'
           ELSE 'schedule fire attempts exhausted: ' || error_message
       END,
       completed_at = coalesce(completed_at, now()),
       retention_expires_at = coalesce(retention_expires_at, now() + interval '90 days'),
       updated_at = now()
  FROM candidate
 WHERE task_schedule_fires.schedule_instance_id = candidate.schedule_instance_id
   AND task_schedule_fires.scheduled_at = candidate.scheduled_at;

-- name: ClaimDueScheduleFires :many
WITH ready_candidate AS (
    SELECT task_schedule_fires.schedule_instance_id,
           task_schedule_fires.scheduled_at
      FROM task_schedule_fires
      JOIN task_schedules ON task_schedules.id = task_schedule_fires.schedule_id
      JOIN task_schedule_instances
        ON task_schedule_instances.id = task_schedule_fires.schedule_instance_id
       AND task_schedule_instances.generation = task_schedule_fires.generation
     WHERE task_schedules.active
       AND task_schedules.deleted_at IS NULL
       AND task_schedule_instances.active
       AND task_schedule_fires.status IN ('pending', 'failed')
       AND task_schedule_fires.next_attempt_at <= now()
       AND task_schedule_fires.attempt_count < sqlc.arg(max_attempts)
     ORDER BY task_schedule_fires.next_attempt_at, task_schedule_fires.scheduled_at
     LIMIT sqlc.arg(row_limit)
     FOR UPDATE OF task_schedule_fires SKIP LOCKED
),
expired_lease_candidate AS (
    SELECT task_schedule_fires.schedule_instance_id,
           task_schedule_fires.scheduled_at
      FROM task_schedule_fires
      JOIN task_schedules ON task_schedules.id = task_schedule_fires.schedule_id
      JOIN task_schedule_instances
        ON task_schedule_instances.id = task_schedule_fires.schedule_instance_id
       AND task_schedule_instances.generation = task_schedule_fires.generation
     WHERE task_schedules.active
       AND task_schedules.deleted_at IS NULL
       AND task_schedule_instances.active
       AND task_schedule_fires.status = 'leased'
       AND task_schedule_fires.lease_expires_at <= now()
       AND task_schedule_fires.attempt_count < sqlc.arg(max_attempts)
     ORDER BY task_schedule_fires.lease_expires_at, task_schedule_fires.scheduled_at
     LIMIT sqlc.arg(row_limit)
     FOR UPDATE OF task_schedule_fires SKIP LOCKED
),
candidate AS (
    SELECT schedule_instance_id, scheduled_at
      FROM (
          SELECT schedule_instance_id, scheduled_at FROM ready_candidate
          UNION ALL
          SELECT schedule_instance_id, scheduled_at FROM expired_lease_candidate
      ) candidates
     LIMIT sqlc.arg(row_limit)
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
       retention_expires_at,
       created_at,
       updated_at
  FROM claimed;

-- name: DeleteExpiredScheduleFires :execrows
WITH candidate AS (
    SELECT task_schedule_fires.schedule_instance_id,
           task_schedule_fires.scheduled_at
      FROM task_schedule_fires
     WHERE task_schedule_fires.retention_expires_at IS NOT NULL
       AND task_schedule_fires.retention_expires_at <= now()
     ORDER BY task_schedule_fires.retention_expires_at,
              task_schedule_fires.schedule_instance_id,
              task_schedule_fires.scheduled_at
     LIMIT sqlc.arg(row_limit)
     FOR UPDATE OF task_schedule_fires SKIP LOCKED
)
DELETE FROM task_schedule_fires
 USING candidate
 WHERE task_schedule_fires.schedule_instance_id = candidate.schedule_instance_id
   AND task_schedule_fires.scheduled_at = candidate.scheduled_at;

-- name: MarkScheduleFireCreated :execrows
UPDATE task_schedule_fires
   SET run_id = sqlc.arg(run_id),
       status = 'created',
       lease_id = NULL,
       lease_expires_at = NULL,
       error_message = '',
       completed_at = now(),
       retention_expires_at = coalesce(retention_expires_at, now() + interval '90 days'),
       updated_at = now()
 WHERE schedule_instance_id = sqlc.arg(schedule_instance_id)
   AND scheduled_at = sqlc.arg(scheduled_at)
   AND lease_id = sqlc.arg(lease_id)
   AND lease_expires_at > now()
   AND status = 'leased';

-- name: MarkScheduleFireFailed :execrows
UPDATE task_schedule_fires
   SET status = CASE
           WHEN attempt_count >= sqlc.arg(max_attempts) THEN 'exhausted'::task_schedule_fire_status
           ELSE 'failed'::task_schedule_fire_status
       END,
       lease_id = NULL,
       lease_expires_at = NULL,
       error_message = CASE
           WHEN attempt_count >= sqlc.arg(max_attempts) THEN 'schedule fire attempts exhausted: ' || sqlc.arg(error_message)::text
           ELSE sqlc.arg(error_message)::text
       END,
       next_attempt_at = sqlc.arg(next_attempt_at),
       completed_at = CASE
           WHEN attempt_count >= sqlc.arg(max_attempts) THEN now()
           ELSE completed_at
       END,
       retention_expires_at = CASE
           WHEN attempt_count >= sqlc.arg(max_attempts) THEN coalesce(retention_expires_at, now() + interval '90 days')
           ELSE retention_expires_at
       END,
       updated_at = now()
 WHERE schedule_instance_id = sqlc.arg(schedule_instance_id)
   AND scheduled_at = sqlc.arg(scheduled_at)
   AND lease_id = sqlc.arg(lease_id)
   AND lease_expires_at > now()
   AND status = 'leased';

-- name: ScheduleFireLeaseIsCurrent :one
SELECT EXISTS (
    SELECT 1
      FROM task_schedule_fires
      JOIN task_schedule_instances
        ON task_schedule_instances.id = task_schedule_fires.schedule_instance_id
       AND task_schedule_instances.generation = task_schedule_fires.generation
      JOIN task_schedules ON task_schedules.id = task_schedule_fires.schedule_id
     WHERE task_schedule_fires.schedule_instance_id = sqlc.arg(schedule_instance_id)
       AND task_schedule_fires.scheduled_at = sqlc.arg(scheduled_at)
       AND task_schedule_fires.lease_id = sqlc.arg(lease_id)
       AND task_schedule_fires.lease_expires_at > now()
       AND task_schedule_fires.status = 'leased'
       AND task_schedule_instances.active
       AND task_schedules.active
       AND task_schedules.deleted_at IS NULL
) AS current;

-- name: MarkScheduleFireSuperseded :execrows
UPDATE task_schedule_fires
   SET status = 'superseded',
       lease_id = NULL,
       lease_expires_at = NULL,
       error_message = 'schedule generation changed',
       completed_at = now(),
       retention_expires_at = coalesce(retention_expires_at, now() + interval '90 days'),
       updated_at = now()
 WHERE schedule_instance_id = sqlc.arg(schedule_instance_id)
   AND scheduled_at = sqlc.arg(scheduled_at)
   AND lease_id = sqlc.arg(lease_id)
   AND lease_expires_at > now()
   AND status = 'leased';

-- name: SupersedeScheduleInstanceFires :execrows
UPDATE task_schedule_fires
   SET status = 'superseded',
       lease_id = NULL,
       lease_expires_at = NULL,
       error_message = 'schedule generation changed',
       completed_at = now(),
       retention_expires_at = coalesce(retention_expires_at, now() + interval '90 days'),
       updated_at = now()
 WHERE schedule_instance_id = sqlc.arg(schedule_instance_id)
   AND generation < sqlc.arg(generation)
   AND status IN ('pending', 'failed', 'leased');
