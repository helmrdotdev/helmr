-- name: GetTaskForStart :one
SELECT *
  FROM tasks
 WHERE org_id = sqlc.arg(org_id)
   AND project_id = sqlc.arg(project_id)
   AND environment_id = sqlc.arg(environment_id)
   AND task_id = sqlc.arg(task_id);

-- name: CreateTaskSession :one
INSERT INTO task_sessions (
    id,
    org_id,
    project_id,
    environment_id,
    task_id,
    initial_deployment_id,
    active_deployment_id,
    workspace_id,
    external_id,
    start_fingerprint,
    metadata,
    tags,
    expires_at
) VALUES (
    sqlc.arg(id),
    sqlc.arg(org_id),
    sqlc.arg(project_id),
    sqlc.arg(environment_id),
    sqlc.arg(task_id),
    sqlc.arg(initial_deployment_id),
    sqlc.arg(active_deployment_id),
    sqlc.arg(workspace_id),
    sqlc.arg(external_id),
    sqlc.arg(start_fingerprint),
    coalesce(sqlc.arg(metadata)::jsonb, '{}'::jsonb),
    coalesce(sqlc.arg(tags)::text[], '{}'::text[]),
    sqlc.narg(expires_at)
)
RETURNING *;

-- name: CreateWorkspace :one
INSERT INTO workspaces (
    id,
    org_id,
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
    sqlc.arg(org_id),
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

-- name: GetWorkspaceForTaskStart :one
SELECT workspaces.id,
       workspaces.org_id,
       workspaces.project_id,
       workspaces.environment_id,
       workspaces.deployment_sandbox_id,
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
 WHERE workspaces.org_id = sqlc.arg(org_id)
   AND workspaces.project_id = sqlc.arg(project_id)
   AND workspaces.environment_id = sqlc.arg(environment_id)
   AND workspaces.id = sqlc.arg(workspace_id)
   AND workspaces.deleted_at IS NULL
 LIMIT 1;

-- name: SetTaskSessionCurrentRun :one
UPDATE task_sessions
   SET current_run_id = sqlc.arg(run_id),
       current_run_version = current_run_version + 1,
       updated_at = now()
 WHERE org_id = sqlc.arg(org_id)
   AND project_id = sqlc.arg(project_id)
   AND environment_id = sqlc.arg(environment_id)
   AND id = sqlc.arg(task_session_id)
   AND status = 'open'
   AND current_run_id IS NULL
RETURNING *;

-- name: CreateTaskSessionRun :one
INSERT INTO task_session_runs (
    id,
    org_id,
    project_id,
    environment_id,
    task_session_id,
    run_id,
    deployment_id,
    previous_run_id,
    turn_index
) VALUES (
    sqlc.arg(id),
    sqlc.arg(org_id),
    sqlc.arg(project_id),
    sqlc.arg(environment_id),
    sqlc.arg(task_session_id),
    sqlc.arg(run_id),
    sqlc.arg(deployment_id),
    sqlc.narg(previous_run_id),
    sqlc.arg(turn_index)
)
RETURNING *;

-- name: GetTaskStartIdempotency :one
SELECT task_start_idempotencies.*,
       task_sessions.id AS session_id,
       task_sessions.org_id AS session_org_id,
       task_sessions.project_id AS session_project_id,
       task_sessions.environment_id AS session_environment_id,
       task_sessions.task_id AS session_task_id,
       task_sessions.initial_deployment_id AS session_initial_deployment_id,
       task_sessions.active_deployment_id AS session_active_deployment_id,
       task_sessions.external_id AS session_external_id,
       task_sessions.start_fingerprint AS session_start_fingerprint,
       task_sessions.status AS session_status,
       task_sessions.current_run_id AS session_current_run_id,
       task_sessions.current_run_version AS session_current_run_version,
       task_sessions.workspace_id AS session_workspace_id,
       task_sessions.metadata AS session_metadata,
       task_sessions.tags AS session_tags,
       task_sessions.result AS session_result,
       task_sessions.terminal_reason AS session_terminal_reason,
       task_sessions.expires_at AS session_expires_at,
       task_sessions.completed_at AS session_completed_at,
       task_sessions.failed_at AS session_failed_at,
       task_sessions.cancelled_at AS session_cancelled_at,
       task_sessions.created_at AS session_created_at,
       task_sessions.updated_at AS session_updated_at,
       runs.id AS run_id,
       runs.org_id AS run_org_id,
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
  FROM task_start_idempotencies
  JOIN task_sessions ON task_sessions.org_id = task_start_idempotencies.org_id
                    AND task_sessions.project_id = task_start_idempotencies.project_id
                    AND task_sessions.environment_id = task_start_idempotencies.environment_id
                    AND task_sessions.id = task_start_idempotencies.task_session_id
  JOIN runs ON runs.org_id = task_start_idempotencies.org_id
           AND runs.project_id = task_start_idempotencies.project_id
           AND runs.environment_id = task_start_idempotencies.environment_id
           AND runs.id = task_start_idempotencies.first_run_id
 WHERE task_start_idempotencies.org_id = sqlc.arg(org_id)
   AND task_start_idempotencies.project_id = sqlc.arg(project_id)
   AND task_start_idempotencies.environment_id = sqlc.arg(environment_id)
   AND task_start_idempotencies.task_id = sqlc.arg(task_id)
   AND task_start_idempotencies.idempotency_key = sqlc.arg(idempotency_key)
   AND task_start_idempotencies.expires_at > now();

-- name: DeleteExpiredTaskStartIdempotency :exec
DELETE FROM task_start_idempotencies
 WHERE org_id = sqlc.arg(org_id)
   AND project_id = sqlc.arg(project_id)
   AND environment_id = sqlc.arg(environment_id)
   AND task_id = sqlc.arg(task_id)
   AND idempotency_key = sqlc.arg(idempotency_key)
   AND expires_at <= now();

-- name: CreateTaskStartIdempotency :one
INSERT INTO task_start_idempotencies (
    id,
    org_id,
    project_id,
    environment_id,
    task_id,
    idempotency_key,
    request_fingerprint,
    task_session_id,
    first_run_id,
    expires_at
) VALUES (
    sqlc.arg(id),
    sqlc.arg(org_id),
    sqlc.arg(project_id),
    sqlc.arg(environment_id),
    sqlc.arg(task_id),
    sqlc.arg(idempotency_key),
    sqlc.arg(request_fingerprint),
    sqlc.arg(task_session_id),
    sqlc.arg(first_run_id),
    sqlc.arg(expires_at)
)
ON CONFLICT (org_id, project_id, environment_id, task_id, idempotency_key) DO NOTHING
RETURNING *;

-- name: TouchTaskStartIdempotency :exec
UPDATE task_start_idempotencies
   SET last_used_at = now()
 WHERE org_id = sqlc.arg(org_id)
   AND id = sqlc.arg(id);

-- name: GetTaskSession :one
SELECT *
  FROM task_sessions
 WHERE org_id = sqlc.arg(org_id)
   AND project_id = sqlc.arg(project_id)
   AND environment_id = sqlc.arg(environment_id)
   AND id = sqlc.arg(id);

-- name: LockTaskSession :one
SELECT *
  FROM task_sessions
 WHERE org_id = sqlc.arg(org_id)
   AND project_id = sqlc.arg(project_id)
   AND environment_id = sqlc.arg(environment_id)
   AND id = sqlc.arg(id)
 FOR UPDATE;

-- name: GetTaskSessionByExternalID :one
SELECT *
  FROM task_sessions
 WHERE org_id = sqlc.arg(org_id)
   AND project_id = sqlc.arg(project_id)
   AND environment_id = sqlc.arg(environment_id)
   AND task_id = sqlc.arg(task_id)
   AND external_id = sqlc.arg(external_id)
   AND external_id <> '';

-- name: ListTaskSessions :many
SELECT *
  FROM task_sessions
 WHERE org_id = sqlc.arg(org_id)
   AND project_id = sqlc.arg(project_id)
   AND environment_id = sqlc.arg(environment_id)
   AND (
       sqlc.arg(status_filter)::text = ''
       OR status::text = sqlc.arg(status_filter)::text
   )
   AND (
       sqlc.arg(task_id_filter)::text = ''
       OR task_id = sqlc.arg(task_id_filter)
   )
 ORDER BY updated_at DESC, id DESC
 LIMIT sqlc.arg(row_limit);

-- name: PatchTaskSession :one
UPDATE task_sessions
   SET metadata = coalesce(sqlc.arg(metadata)::jsonb, task_sessions.metadata),
       tags = coalesce(sqlc.arg(tags)::text[], task_sessions.tags),
       expires_at = CASE
           WHEN sqlc.narg(expires_at)::timestamptz IS NULL THEN task_sessions.expires_at
           ELSE sqlc.narg(expires_at)::timestamptz
       END,
       updated_at = now()
 WHERE org_id = sqlc.arg(org_id)
   AND project_id = sqlc.arg(project_id)
   AND environment_id = sqlc.arg(environment_id)
   AND id = sqlc.arg(id)
   AND status = 'open'
   AND (
       sqlc.narg(expires_at)::timestamptz IS NULL
       OR (
           task_sessions.expires_at IS NOT NULL
           AND task_sessions.expires_at > now()
           AND sqlc.narg(expires_at)::timestamptz > task_sessions.expires_at
       )
   )
RETURNING *;

-- name: CloseTaskSession :one
UPDATE task_sessions
   SET status = 'closed',
       closed_at = now(),
       closed_reason = sqlc.arg(reason),
       terminal_reason = jsonb_build_object('reason', sqlc.arg(reason), 'origin', 'api'),
       current_run_id = NULL,
       current_run_version = current_run_version + 1,
       updated_at = now()
 WHERE org_id = sqlc.arg(org_id)
   AND project_id = sqlc.arg(project_id)
   AND environment_id = sqlc.arg(environment_id)
   AND id = sqlc.arg(id)
   AND status = 'open'
   AND current_run_id IS NULL
RETURNING *;

-- name: GetTaskSessionByOrgID :one
SELECT *
  FROM task_sessions
 WHERE org_id = sqlc.arg(org_id)
   AND id = sqlc.arg(id);

-- name: CancelTaskSession :one
WITH target_session AS (
    SELECT *
      FROM task_sessions
     WHERE task_sessions.org_id = sqlc.arg(org_id)
       AND task_sessions.project_id = sqlc.arg(project_id)
       AND task_sessions.environment_id = sqlc.arg(environment_id)
       AND task_sessions.id = sqlc.arg(id)
     FOR UPDATE
),
cancelled_session AS (
    UPDATE task_sessions
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
           current_run_id = NULL,
           current_run_version = task_sessions.current_run_version + 1,
           updated_at = now()
      FROM target_session
     WHERE task_sessions.org_id = target_session.org_id
       AND task_sessions.id = target_session.id
       AND task_sessions.status = 'open'
    RETURNING task_sessions.*
),
ended_session_run AS (
    UPDATE task_session_runs
       SET ended_at = now()
      FROM target_session
      JOIN cancelled_session ON true
     WHERE target_session.current_run_id IS NOT NULL
       AND task_session_runs.org_id = target_session.org_id
       AND task_session_runs.project_id = target_session.project_id
       AND task_session_runs.environment_id = target_session.environment_id
       AND task_session_runs.task_session_id = target_session.id
       AND task_session_runs.run_id = target_session.current_run_id
    RETURNING task_session_runs.id
)
SELECT task_sessions.*
  FROM task_sessions
  JOIN cancelled_session ON cancelled_session.org_id = task_sessions.org_id
                        AND cancelled_session.id = task_sessions.id;

-- name: ListTaskSessionRuns :many
SELECT task_session_runs.*,
       runs.status,
       runs.execution_status,
       runs.terminal_outcome,
       runs.created_at AS run_created_at,
       runs.updated_at AS run_updated_at
  FROM task_session_runs
  JOIN runs ON runs.org_id = task_session_runs.org_id
           AND runs.project_id = task_session_runs.project_id
           AND runs.environment_id = task_session_runs.environment_id
           AND runs.id = task_session_runs.run_id
 WHERE task_session_runs.org_id = sqlc.arg(org_id)
   AND task_session_runs.project_id = sqlc.arg(project_id)
   AND task_session_runs.environment_id = sqlc.arg(environment_id)
   AND task_session_runs.task_session_id = sqlc.arg(task_session_id)
 ORDER BY task_session_runs.turn_index ASC, task_session_runs.created_at ASC;

-- name: GetTaskSessionStreamByName :one
SELECT *
  FROM streams
 WHERE org_id = sqlc.arg(org_id)
   AND project_id = sqlc.arg(project_id)
   AND environment_id = sqlc.arg(environment_id)
   AND session_id = sqlc.arg(task_session_id)
   AND name = sqlc.arg(name)
   AND direction = sqlc.arg(direction)::stream_direction;

-- name: ListTaskSessionStreams :many
SELECT *
  FROM streams
 WHERE org_id = sqlc.arg(org_id)
   AND project_id = sqlc.arg(project_id)
   AND environment_id = sqlc.arg(environment_id)
   AND session_id = sqlc.arg(task_session_id)
 ORDER BY name ASC, direction ASC;
