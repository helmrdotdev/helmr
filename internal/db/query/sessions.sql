-- name: GetTaskForStart :one
SELECT *
  FROM tasks
 WHERE org_id = sqlc.arg(org_id)
   AND project_id = sqlc.arg(project_id)
   AND environment_id = sqlc.arg(environment_id)
   AND task_id = sqlc.arg(task_id);

-- name: CreateSession :one
INSERT INTO sessions (
    id,
    public_id,
    org_id,
    worker_group_id,
    project_id,
    environment_id,
    task_id,
    initial_deployment_id,
    active_deployment_id,
    workspace_id,
    external_id,
    start_fingerprint,
    start_idempotency_key,
    start_idempotency_expires_at,
    metadata,
    tags,
    expires_at
)
SELECT
    sqlc.arg(id),
    sqlc.arg(public_id),
    workspaces.org_id,
    workspaces.worker_group_id,
    workspaces.project_id,
    workspaces.environment_id,
    sqlc.arg(task_id),
    sqlc.arg(initial_deployment_id),
    sqlc.arg(active_deployment_id),
    workspaces.id,
    sqlc.arg(external_id),
    sqlc.arg(start_fingerprint),
    coalesce(sqlc.arg(start_idempotency_key)::text, ''),
    sqlc.narg(start_idempotency_expires_at),
    coalesce(sqlc.arg(metadata)::jsonb, '{}'::jsonb),
    coalesce(sqlc.arg(tags)::text[], '{}'::text[]),
    sqlc.narg(expires_at)
  FROM workspaces
 WHERE workspaces.org_id = sqlc.arg(org_id)
   AND workspaces.project_id = sqlc.arg(project_id)
   AND workspaces.environment_id = sqlc.arg(environment_id)
   AND workspaces.id = sqlc.arg(workspace_id)
   AND workspaces.state = 'active'
   AND workspaces.archived_at IS NULL
   AND workspaces.deleted_at IS NULL
RETURNING *;

-- name: CreateWorkspace :one
INSERT INTO workspaces (
    id,
    public_id,
    org_id,
    worker_group_id,
    project_id,
    environment_id,
    deployment_sandbox_id,
    sandbox_id,
    sandbox_fingerprint,
    external_id,
    metadata,
    tags,
    retention_policy
) VALUES (
    sqlc.arg(id),
    sqlc.arg(public_id),
    sqlc.arg(org_id),
    sqlc.arg(worker_group_id),
    sqlc.arg(project_id),
    sqlc.arg(environment_id),
    sqlc.arg(deployment_sandbox_id),
    sqlc.arg(sandbox_id),
    sqlc.arg(sandbox_fingerprint),
    coalesce(sqlc.arg(external_id)::text, ''),
    coalesce(sqlc.arg(metadata)::jsonb, '{}'::jsonb),
    coalesce(sqlc.arg(tags)::text[], '{}'::text[]),
    coalesce(sqlc.arg(retention_policy)::jsonb, '{}'::jsonb)
)
RETURNING *;

-- name: GetWorkspaceForSessionStart :one
SELECT workspaces.id,
       workspaces.org_id,
       workspaces.worker_group_id,
       workspaces.project_id,
       workspaces.environment_id,
       workspaces.deployment_sandbox_id,
       deployment_sandboxes.deployment_id,
       workspaces.sandbox_id,
       workspaces.sandbox_fingerprint,
       workspaces.state,
       workspaces.archived_at,
       workspaces.deleted_at,
       deployment_sandboxes.workspace_mount_path,
       deployment_sandboxes.resource_floor AS deployment_sandbox_resource_floor,
       deployment_sandboxes.disk_floor_mib AS deployment_sandbox_disk_floor_mib,
       deployment_sandboxes.network_policy AS deployment_sandbox_network_policy,
       deployment_sandboxes.rootfs_digest AS deployment_sandbox_rootfs_digest,
       deployment_sandboxes.runtime_abi AS deployment_sandbox_runtime_abi,
       deployment_sandboxes.guestd_abi AS deployment_sandbox_guestd_abi,
       deployment_sandboxes.adapter_abi AS deployment_sandbox_adapter_abi,
       deployment_sandboxes.filesystem_format AS deployment_sandbox_filesystem_format,
       deployment_sandboxes.contract_version AS deployment_sandbox_contract_version,
       deployment_sandboxes.fingerprint AS deployment_sandbox_fingerprint
  FROM workspaces
  JOIN deployment_sandboxes
    ON deployment_sandboxes.org_id = workspaces.org_id
   AND deployment_sandboxes.project_id = workspaces.project_id
   AND deployment_sandboxes.environment_id = workspaces.environment_id
   AND deployment_sandboxes.id = workspaces.deployment_sandbox_id
  JOIN worker_groups ON worker_groups.id = workspaces.worker_group_id
                    AND worker_groups.state IN ('active', 'draining')
 WHERE workspaces.org_id = sqlc.arg(org_id)
   AND workspaces.project_id = sqlc.arg(project_id)
   AND workspaces.environment_id = sqlc.arg(environment_id)
   AND workspaces.id = sqlc.arg(workspace_id)
   AND workspaces.deleted_at IS NULL
 LIMIT 1;

-- name: GetWorkspaceSourceForSessionStart :one
SELECT workspaces.id,
       workspaces.org_id,
       workspaces.worker_group_id,
       workspaces.project_id,
       workspaces.environment_id,
       workspaces.deployment_sandbox_id,
       deployment_sandboxes.deployment_id
  FROM workspaces
  JOIN deployment_sandboxes
    ON deployment_sandboxes.org_id = workspaces.org_id
   AND deployment_sandboxes.project_id = workspaces.project_id
   AND deployment_sandboxes.environment_id = workspaces.environment_id
   AND deployment_sandboxes.id = workspaces.deployment_sandbox_id
 WHERE workspaces.org_id = sqlc.arg(org_id)
   AND workspaces.project_id = sqlc.arg(project_id)
   AND workspaces.environment_id = sqlc.arg(environment_id)
   AND workspaces.id = sqlc.arg(workspace_id)
   AND workspaces.deleted_at IS NULL
 LIMIT 1;

-- name: SetSessionCurrentRun :one
UPDATE sessions
   SET current_run_id = sqlc.arg(run_id),
       current_run_version = current_run_version + 1,
       updated_at = now()
WHERE sessions.org_id = sqlc.arg(org_id)
   AND sessions.project_id = sqlc.arg(project_id)
   AND sessions.environment_id = sqlc.arg(environment_id)
   AND sessions.id = sqlc.arg(session_id)
   AND sessions.status = 'open'
   AND (
       current_run_id IS NULL
       OR NOT EXISTS (
           SELECT 1
             FROM runs
            WHERE runs.org_id = sessions.org_id
              AND runs.project_id = sessions.project_id
              AND runs.environment_id = sessions.environment_id
              AND runs.id = sessions.current_run_id
              AND runs.status NOT IN ('succeeded', 'failed', 'cancelled', 'expired')
       )
   )
RETURNING *;

-- name: CreateSessionRun :one
INSERT INTO session_runs (
    id,
    public_id,
    org_id,
    worker_group_id,
    project_id,
    environment_id,
    session_id,
    run_id,
    deployment_id,
    previous_run_id,
    turn_index,
    reason
)
SELECT sqlc.arg(id),
       sqlc.arg(public_id),
       sessions.org_id,
       sessions.worker_group_id,
       sessions.project_id,
       sessions.environment_id,
       sessions.id,
       runs.id,
       sqlc.arg(deployment_id),
       sqlc.narg(previous_run_id),
       sqlc.arg(turn_index),
       sqlc.arg(reason)
  FROM sessions
  JOIN runs
    ON runs.org_id = sessions.org_id
   AND runs.worker_group_id = sessions.worker_group_id
   AND runs.project_id = sessions.project_id
   AND runs.environment_id = sessions.environment_id
   AND runs.session_id = sessions.id
   AND runs.id = sqlc.arg(run_id)
 WHERE sessions.org_id = sqlc.arg(org_id)
   AND sessions.project_id = sqlc.arg(project_id)
   AND sessions.environment_id = sqlc.arg(environment_id)
   AND sessions.id = sqlc.arg(session_id)
RETURNING *;

-- name: GetSessionByStartIdempotency :one
SELECT sessions.id AS session_id,
       sessions.org_id AS session_org_id,
       sessions.worker_group_id AS session_worker_group_id,
       sessions.project_id AS session_project_id,
       sessions.environment_id AS session_environment_id,
       sessions.task_id AS session_task_id,
       sessions.initial_deployment_id AS session_initial_deployment_id,
       sessions.active_deployment_id AS session_active_deployment_id,
       sessions.external_id AS session_external_id,
       sessions.start_fingerprint AS session_start_fingerprint,
       sessions.start_idempotency_key AS session_start_idempotency_key,
       sessions.start_idempotency_expires_at AS session_start_idempotency_expires_at,
       sessions.status AS session_status,
       sessions.current_run_id AS session_current_run_id,
       sessions.current_run_version AS session_current_run_version,
       sessions.workspace_id AS session_workspace_id,
       sessions.metadata AS session_metadata,
       sessions.tags AS session_tags,
       sessions.result AS session_result,
       sessions.terminal_reason AS session_terminal_reason,
       sessions.expires_at AS session_expires_at,
       sessions.cancelled_at AS session_cancelled_at,
       sessions.created_at AS session_created_at,
       sessions.updated_at AS session_updated_at,
       runs.id AS run_id,
       runs.org_id AS run_org_id,
       runs.worker_group_id AS run_worker_group_id,
       runs.project_id AS run_project_id,
       runs.environment_id AS run_environment_id,
       runs.deployment_id AS run_deployment_id,
       runs.deployment_task_id AS run_deployment_task_id,
       runs.deployment_version AS run_deployment_version,
       runs.api_version AS run_api_version,
       runs.sdk_version AS run_sdk_version,
       runs.cli_version AS run_cli_version,
       runs.task_id AS run_task_id,
       runs.current_attempt_number AS run_attempt_number,
       runs.status AS run_status,
       runs.execution_status AS run_execution_status,
       runs.terminal_outcome AS run_terminal_outcome,
       runs.payload AS run_payload,
       runs.output AS run_output,
       runs.metadata AS run_metadata,
       runs.tags AS run_tags,
       runs.error_message AS run_error_message,
       runs.exit_code AS run_exit_code,
       runs.created_at AS run_created_at,
       runs.updated_at AS run_updated_at
  FROM sessions
  JOIN session_runs ON session_runs.org_id = sessions.org_id
                   AND session_runs.project_id = sessions.project_id
                   AND session_runs.environment_id = sessions.environment_id
                   AND session_runs.session_id = sessions.id
                   AND session_runs.turn_index = 0
  JOIN runs ON runs.org_id = session_runs.org_id
           AND runs.project_id = session_runs.project_id
           AND runs.environment_id = session_runs.environment_id
           AND runs.id = session_runs.run_id
 WHERE sessions.org_id = sqlc.arg(org_id)
   AND sessions.project_id = sqlc.arg(project_id)
   AND sessions.environment_id = sqlc.arg(environment_id)
   AND sessions.task_id = sqlc.arg(task_id)
   AND sessions.start_idempotency_key = sqlc.arg(idempotency_key)
   AND sessions.start_idempotency_expires_at > now();

-- name: ClearExpiredSessionStartIdempotency :exec
UPDATE sessions
   SET start_idempotency_key = '',
       start_idempotency_expires_at = NULL,
       updated_at = now()
 WHERE org_id = sqlc.arg(org_id)
   AND project_id = sqlc.arg(project_id)
   AND environment_id = sqlc.arg(environment_id)
   AND task_id = sqlc.arg(task_id)
   AND start_idempotency_key = sqlc.arg(idempotency_key)
   AND start_idempotency_expires_at <= now();

-- name: SetSessionStartIdempotency :one
UPDATE sessions
   SET start_idempotency_key = sqlc.arg(idempotency_key),
       start_idempotency_expires_at = sqlc.arg(expires_at),
       updated_at = now()
 WHERE org_id = sqlc.arg(org_id)
   AND project_id = sqlc.arg(project_id)
   AND environment_id = sqlc.arg(environment_id)
   AND id = sqlc.arg(session_id)
   AND task_id = sqlc.arg(task_id)
   AND start_fingerprint = sqlc.arg(start_fingerprint)
   AND (
       start_idempotency_key = ''
       OR start_idempotency_key = sqlc.arg(idempotency_key)
       OR start_idempotency_expires_at <= now()
   )
RETURNING *;

-- name: GetSession :one
SELECT *
  FROM sessions
 WHERE org_id = sqlc.arg(org_id)
   AND project_id = sqlc.arg(project_id)
   AND environment_id = sqlc.arg(environment_id)
   AND id = sqlc.arg(id);

-- name: GetSessionActivity :one
SELECT sessions.id,
       CASE
         WHEN sessions.status <> 'open' THEN 'idle'
         WHEN EXISTS (
             SELECT 1
               FROM session_continuation_requests
              WHERE session_continuation_requests.org_id = sessions.org_id
                AND session_continuation_requests.project_id = sessions.project_id
                AND session_continuation_requests.environment_id = sessions.environment_id
                AND session_continuation_requests.session_id = sessions.id
                AND session_continuation_requests.status IN ('accepted', 'claimed')
         ) THEN 'queued'
         WHEN sessions.current_run_id IS NULL THEN 'idle'
         WHEN runs.id IS NULL THEN 'idle'
         WHEN runs.status IN ('succeeded', 'failed', 'cancelled', 'expired') THEN 'idle'
         WHEN active_wait.state IN ('hot_waiting', 'checkpointing', 'checkpointed_waiting', 'resuming') THEN 'waiting'
         WHEN runs.status = 'waiting' OR runs.execution_status = 'waiting' THEN 'waiting'
         WHEN runs.status = 'queued' OR runs.execution_status IN ('created', 'queued', 'leased') THEN 'queued'
         ELSE 'running'
       END::text AS activity,
       (
         sessions.status = 'open'
         AND (sessions.expires_at IS NULL OR sessions.expires_at > now())
         AND (
             sessions.current_run_id IS NULL
             OR runs.id IS NULL
             OR runs.status IN ('succeeded', 'failed', 'cancelled', 'expired')
         )
         AND NOT EXISTS (
             SELECT 1
               FROM session_continuation_requests
              WHERE session_continuation_requests.org_id = sessions.org_id
                AND session_continuation_requests.project_id = sessions.project_id
                AND session_continuation_requests.environment_id = sessions.environment_id
                AND session_continuation_requests.session_id = sessions.id
                AND session_continuation_requests.status IN ('accepted', 'claimed')
         )
       )::bool AS can_close
  FROM sessions
  LEFT JOIN runs
    ON runs.org_id = sessions.org_id
   AND runs.worker_group_id = sessions.worker_group_id
   AND runs.project_id = sessions.project_id
   AND runs.environment_id = sessions.environment_id
   AND runs.id = sessions.current_run_id
  LEFT JOIN LATERAL (
       SELECT run_waits.state
         FROM run_waits
        WHERE run_waits.org_id = sessions.org_id
          AND run_waits.worker_group_id = sessions.worker_group_id
          AND run_waits.project_id = sessions.project_id
          AND run_waits.environment_id = sessions.environment_id
          AND run_waits.run_id = sessions.current_run_id
          AND run_waits.state IN ('hot_waiting', 'checkpointing', 'checkpointed_waiting', 'resuming')
        ORDER BY run_waits.created_at DESC, run_waits.id DESC
        LIMIT 1
 ) active_wait ON true
 WHERE sessions.org_id = sqlc.arg(org_id)
   AND sessions.project_id = sqlc.arg(project_id)
   AND sessions.environment_id = sqlc.arg(environment_id)
   AND sessions.id = sqlc.arg(id);

-- name: ListSessionActivities :many
SELECT sessions.id,
       CASE
         WHEN sessions.status <> 'open' THEN 'idle'
         WHEN EXISTS (
             SELECT 1
              FROM session_continuation_requests
             WHERE session_continuation_requests.org_id = sessions.org_id
               AND session_continuation_requests.worker_group_id = sessions.worker_group_id
               AND session_continuation_requests.project_id = sessions.project_id
               AND session_continuation_requests.environment_id = sessions.environment_id
               AND session_continuation_requests.session_id = sessions.id
                AND session_continuation_requests.status IN ('accepted', 'claimed')
         ) THEN 'queued'
         WHEN sessions.current_run_id IS NULL THEN 'idle'
         WHEN runs.id IS NULL THEN 'idle'
         WHEN runs.status IN ('succeeded', 'failed', 'cancelled', 'expired') THEN 'idle'
         WHEN active_wait.state IN ('hot_waiting', 'checkpointing', 'checkpointed_waiting', 'resuming') THEN 'waiting'
         WHEN runs.status = 'waiting' OR runs.execution_status = 'waiting' THEN 'waiting'
         WHEN runs.status = 'queued' OR runs.execution_status IN ('created', 'queued', 'leased') THEN 'queued'
         ELSE 'running'
       END::text AS activity,
       (
         sessions.status = 'open'
         AND (sessions.expires_at IS NULL OR sessions.expires_at > now())
         AND (
             sessions.current_run_id IS NULL
             OR runs.id IS NULL
             OR runs.status IN ('succeeded', 'failed', 'cancelled', 'expired')
         )
         AND NOT EXISTS (
             SELECT 1
              FROM session_continuation_requests
             WHERE session_continuation_requests.org_id = sessions.org_id
               AND session_continuation_requests.worker_group_id = sessions.worker_group_id
               AND session_continuation_requests.project_id = sessions.project_id
               AND session_continuation_requests.environment_id = sessions.environment_id
               AND session_continuation_requests.session_id = sessions.id
                AND session_continuation_requests.status IN ('accepted', 'claimed')
         )
       )::bool AS can_close
  FROM sessions
  JOIN unnest(sqlc.arg(session_ids)::uuid[]) AS target(id)
    ON target.id = sessions.id
  LEFT JOIN runs
    ON runs.org_id = sessions.org_id
   AND runs.worker_group_id = sessions.worker_group_id
   AND runs.project_id = sessions.project_id
   AND runs.environment_id = sessions.environment_id
   AND runs.id = sessions.current_run_id
  LEFT JOIN LATERAL (
       SELECT run_waits.state
         FROM run_waits
        WHERE run_waits.org_id = sessions.org_id
          AND run_waits.worker_group_id = sessions.worker_group_id
          AND run_waits.project_id = sessions.project_id
          AND run_waits.environment_id = sessions.environment_id
          AND run_waits.run_id = sessions.current_run_id
          AND run_waits.state IN ('hot_waiting', 'checkpointing', 'checkpointed_waiting', 'resuming')
        ORDER BY run_waits.created_at DESC, run_waits.id DESC
        LIMIT 1
 ) active_wait ON true
 WHERE sessions.org_id = sqlc.arg(org_id)
   AND sessions.project_id = sqlc.arg(project_id)
   AND sessions.environment_id = sqlc.arg(environment_id)
;

-- name: LockSession :one
SELECT *
  FROM sessions
 WHERE org_id = sqlc.arg(org_id)
   AND project_id = sqlc.arg(project_id)
   AND environment_id = sqlc.arg(environment_id)
   AND id = sqlc.arg(id)
 FOR UPDATE;

-- name: GetSessionByExternalIDInWorkerGroup :one
SELECT *
  FROM sessions
 WHERE org_id = sqlc.arg(org_id)
   AND project_id = sqlc.arg(project_id)
   AND environment_id = sqlc.arg(environment_id)
   AND external_id = sqlc.arg(external_id)
   AND external_id <> '';

-- name: GetSessionInWorkerGroup :one
SELECT *
  FROM sessions
 WHERE org_id = sqlc.arg(org_id)
   AND worker_group_id = sqlc.arg(worker_group_id)
   AND project_id = sqlc.arg(project_id)
   AND environment_id = sqlc.arg(environment_id)
   AND id = sqlc.arg(id);

-- name: GetSessionByExternalID :one
SELECT *
  FROM sessions
 WHERE org_id = sqlc.arg(org_id)
   AND project_id = sqlc.arg(project_id)
   AND environment_id = sqlc.arg(environment_id)
   AND external_id = sqlc.arg(external_id)
   AND external_id <> '';

-- name: ListSessions :many
SELECT sessions.*
 FROM sessions
 WHERE sessions.org_id = sqlc.arg(org_id)
   AND sessions.project_id = sqlc.arg(project_id)
   AND sessions.environment_id = sqlc.arg(environment_id)
   AND (
       sqlc.arg(status_filter)::text = ''
       OR sessions.status::text = sqlc.arg(status_filter)::text
   )
   AND (
       sqlc.arg(task_id_filter)::text = ''
       OR sessions.task_id = sqlc.arg(task_id_filter)
   )
   AND (
       sqlc.arg(external_id_filter)::text = ''
       OR sessions.external_id = sqlc.arg(external_id_filter)
   )
 ORDER BY sessions.updated_at DESC, sessions.id DESC
 LIMIT sqlc.arg(row_limit);

-- name: PatchSession :one
UPDATE sessions
   SET metadata = coalesce(sqlc.arg(metadata)::jsonb, sessions.metadata),
       tags = coalesce(sqlc.arg(tags)::text[], sessions.tags),
       expires_at = CASE
           WHEN sqlc.narg(expires_at)::timestamptz IS NULL THEN sessions.expires_at
           ELSE sqlc.narg(expires_at)::timestamptz
       END,
       updated_at = now()
 WHERE sessions.org_id = sqlc.arg(org_id)
   AND sessions.project_id = sqlc.arg(project_id)
   AND sessions.environment_id = sqlc.arg(environment_id)
   AND sessions.id = sqlc.arg(id)
   AND sessions.status = 'open'
   AND (
       sqlc.narg(expires_at)::timestamptz IS NULL
       OR (
           sessions.expires_at IS NOT NULL
           AND sessions.expires_at > now()
           AND sqlc.narg(expires_at)::timestamptz > sessions.expires_at
       )
   )
RETURNING *;

-- name: CloseSession :one
UPDATE sessions
   SET status = 'closed',
       closed_at = now(),
       closed_reason = sqlc.arg(reason)::text,
       terminal_reason = jsonb_build_object('reason', sqlc.arg(reason)::text, 'origin', 'api'),
       updated_at = now()
 WHERE sessions.org_id = sqlc.arg(org_id)
   AND sessions.project_id = sqlc.arg(project_id)
   AND sessions.environment_id = sqlc.arg(environment_id)
   AND sessions.id = sqlc.arg(id)
   AND sessions.status = 'open'
   AND (sessions.expires_at IS NULL OR sessions.expires_at > now())
   AND NOT EXISTS (
       SELECT 1
         FROM runs
        WHERE runs.org_id = sessions.org_id
          AND runs.project_id = sessions.project_id
          AND runs.environment_id = sessions.environment_id
          AND runs.id = sessions.current_run_id
          AND runs.status NOT IN ('succeeded', 'failed', 'cancelled', 'expired')
   )
   AND NOT EXISTS (
       SELECT 1
         FROM session_continuation_requests
        WHERE session_continuation_requests.org_id = sessions.org_id
          AND session_continuation_requests.project_id = sessions.project_id
          AND session_continuation_requests.environment_id = sessions.environment_id
          AND session_continuation_requests.session_id = sessions.id
          AND session_continuation_requests.status IN ('accepted', 'claimed')
   )
RETURNING *;

-- name: ExpireSessionIfDue :one
UPDATE sessions
   SET status = 'expired',
       expired_at = now(),
       terminal_reason = jsonb_build_object('reason', 'session_expired', 'origin', 'api'),
       result = jsonb_build_object(
           'ok', false,
           'error', jsonb_build_object(
               'name', 'SessionExpired',
               'message', 'session expired',
               'details', jsonb_build_object('origin', 'api')
           )
       ),
       updated_at = now()
 WHERE sessions.org_id = sqlc.arg(org_id)
   AND sessions.project_id = sqlc.arg(project_id)
   AND sessions.environment_id = sqlc.arg(environment_id)
   AND sessions.id = sqlc.arg(id)
   AND sessions.status = 'open'
   AND sessions.expires_at IS NOT NULL
   AND sessions.expires_at <= now()
   AND NOT EXISTS (
       SELECT 1
         FROM runs
        WHERE runs.org_id = sessions.org_id
          AND runs.project_id = sessions.project_id
          AND runs.environment_id = sessions.environment_id
          AND runs.id = sessions.current_run_id
          AND runs.status NOT IN ('succeeded', 'failed', 'cancelled', 'expired')
   )
   AND NOT EXISTS (
       SELECT 1
         FROM session_continuation_requests
        WHERE session_continuation_requests.org_id = sessions.org_id
          AND session_continuation_requests.project_id = sessions.project_id
          AND session_continuation_requests.environment_id = sessions.environment_id
          AND session_continuation_requests.session_id = sessions.id
          AND session_continuation_requests.status IN ('accepted', 'claimed')
   )
RETURNING *;

-- name: ExpireDueSessions :many
UPDATE sessions
   SET status = 'expired',
       expired_at = now(),
       terminal_reason = jsonb_build_object('reason', 'session_expired', 'origin', 'sweeper'),
       result = jsonb_build_object(
           'ok', false,
           'error', jsonb_build_object(
               'name', 'SessionExpired',
               'message', 'session expired',
               'details', jsonb_build_object('origin', 'sweeper')
           )
       ),
       updated_at = now()
 WHERE sessions.org_id = sqlc.arg(org_id)
   AND sessions.worker_group_id = sqlc.arg(worker_group_id)
   AND sessions.status = 'open'
   AND sessions.expires_at IS NOT NULL
   AND sessions.expires_at <= now()
   AND NOT EXISTS (
       SELECT 1
         FROM runs
        WHERE runs.org_id = sessions.org_id
          AND runs.worker_group_id = sessions.worker_group_id
          AND runs.project_id = sessions.project_id
          AND runs.environment_id = sessions.environment_id
          AND runs.id = sessions.current_run_id
          AND runs.status NOT IN ('succeeded', 'failed', 'cancelled', 'expired')
   )
   AND NOT EXISTS (
       SELECT 1
         FROM session_continuation_requests
        WHERE session_continuation_requests.org_id = sessions.org_id
          AND session_continuation_requests.worker_group_id = sessions.worker_group_id
          AND session_continuation_requests.project_id = sessions.project_id
          AND session_continuation_requests.environment_id = sessions.environment_id
          AND session_continuation_requests.session_id = sessions.id
          AND session_continuation_requests.status IN ('accepted', 'claimed')
   )
RETURNING *;

-- name: GetSessionByOrgID :one
SELECT *
  FROM sessions
 WHERE org_id = sqlc.arg(org_id)
   AND id = sqlc.arg(id);

-- name: CancelSession :one
WITH target_session AS (
    SELECT *
     FROM sessions
     WHERE sessions.org_id = sqlc.arg(org_id)
       AND sessions.project_id = sqlc.arg(project_id)
       AND sessions.environment_id = sqlc.arg(environment_id)
       AND sessions.id = sqlc.arg(id)
     FOR UPDATE
),
cancelled_session AS (
    UPDATE sessions
       SET status = 'cancelled',
           cancelled_at = now(),
           result = jsonb_build_object(
               'ok', false,
               'error', jsonb_build_object(
                   'name', 'TaskCancelled',
                   'message', sqlc.arg(reason)::text,
                   'details', jsonb_build_object('origin', 'api')
               )
           ),
           terminal_reason = jsonb_build_object('reason', sqlc.arg(reason)::text, 'origin', 'api'),
           updated_at = now()
      FROM target_session
     WHERE sessions.org_id = target_session.org_id
       AND sessions.worker_group_id = target_session.worker_group_id
       AND sessions.id = target_session.id
       AND sessions.status = 'open'
    RETURNING sessions.*
),
ended_session_run AS (
    UPDATE session_runs
       SET ended_at = now()
     FROM target_session
     JOIN cancelled_session ON true
     WHERE target_session.current_run_id IS NOT NULL
       AND session_runs.org_id = target_session.org_id
       AND session_runs.project_id = target_session.project_id
       AND session_runs.environment_id = target_session.environment_id
       AND session_runs.session_id = target_session.id
       AND session_runs.run_id = target_session.current_run_id
    RETURNING session_runs.id
)
SELECT sessions.*
  FROM sessions
  JOIN cancelled_session ON cancelled_session.org_id = sessions.org_id
                        AND cancelled_session.id = sessions.id;

-- name: ListSessionRuns :many
SELECT session_runs.*,
       runs.status,
       runs.execution_status,
       runs.terminal_outcome,
       runs.created_at AS run_created_at,
       runs.updated_at AS run_updated_at
  FROM session_runs
  JOIN runs ON runs.org_id = session_runs.org_id
           AND runs.project_id = session_runs.project_id
           AND runs.environment_id = session_runs.environment_id
           AND runs.id = session_runs.run_id
 WHERE session_runs.org_id = sqlc.arg(org_id)
   AND session_runs.project_id = sqlc.arg(project_id)
   AND session_runs.environment_id = sqlc.arg(environment_id)
   AND session_runs.session_id = sqlc.arg(session_id)
 ORDER BY session_runs.turn_index ASC, session_runs.created_at ASC;

-- name: GetSessionRunByRunID :one
SELECT *
  FROM session_runs
 WHERE org_id = sqlc.arg(org_id)
   AND project_id = sqlc.arg(project_id)
   AND environment_id = sqlc.arg(environment_id)
   AND session_id = sqlc.arg(session_id)
   AND run_id = sqlc.arg(run_id);
