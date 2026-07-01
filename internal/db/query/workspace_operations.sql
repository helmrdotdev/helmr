-- name: RequestWorkspaceOperation :one
WITH active_mount AS MATERIALIZED (
    SELECT *
      FROM workspace_mounts
     WHERE workspace_mounts.org_id = sqlc.arg(org_id)
       AND workspace_mounts.project_id = sqlc.arg(project_id)
       AND workspace_mounts.environment_id = sqlc.arg(environment_id)
       AND workspace_mounts.workspace_id = sqlc.arg(workspace_id)
       AND workspace_mounts.id = sqlc.arg(workspace_mount_id)
       AND workspace_mounts.state = 'mounted'
),
active_write_lease AS MATERIALIZED (
    SELECT workspace_leases.id,
           workspace_leases.acquired_fencing_generation
      FROM active_mount
      JOIN workspace_leases
        ON workspace_leases.org_id = active_mount.org_id
       AND workspace_leases.project_id = active_mount.project_id
       AND workspace_leases.environment_id = active_mount.environment_id
       AND workspace_leases.workspace_id = active_mount.workspace_id
       AND workspace_leases.workspace_mount_id = active_mount.id
     WHERE sqlc.narg(write_lease_id)::uuid IS NOT NULL
       AND workspace_leases.id = sqlc.narg(write_lease_id)::uuid
       AND workspace_leases.lease_kind = 'write'
       AND workspace_leases.state = 'active'
       AND workspace_leases.fencing_token = coalesce(sqlc.arg(fencing_token)::text, '')
       AND workspace_leases.expires_at > now()
)
INSERT INTO workspace_operations (
    id,
    org_id,
    project_id,
    environment_id,
    workspace_id,
    workspace_mount_id,
    operation_kind,
    resource_kind,
    resource_id,
    request_fingerprint,
    operation_expires_at,
    priority,
    instance_lease_id,
    write_lease_id,
    fencing_token,
    fencing_generation,
    request
)
SELECT sqlc.arg(id),
       active_mount.org_id,
       active_mount.project_id,
       active_mount.environment_id,
       active_mount.workspace_id,
       active_mount.id,
       sqlc.arg(operation_kind)::workspace_operation_kind,
       sqlc.arg(resource_kind)::workspace_resource_kind,
       sqlc.arg(resource_id),
       sqlc.arg(request_fingerprint),
       sqlc.arg(operation_expires_at),
       sqlc.arg(priority),
       sqlc.narg(instance_lease_id),
       sqlc.narg(write_lease_id),
       coalesce(sqlc.arg(fencing_token)::text, ''),
       coalesce(active_write_lease.acquired_fencing_generation, active_mount.fencing_generation),
       coalesce(sqlc.arg(request)::jsonb, '{}'::jsonb)
  FROM active_mount
  LEFT JOIN active_write_lease ON true
 WHERE (
       (
           sqlc.narg(write_lease_id)::uuid IS NULL
           AND coalesce(sqlc.arg(fencing_token)::text, '') = ''
       )
       OR active_write_lease.id IS NOT NULL
   )
   AND (
       sqlc.arg(operation_kind)::workspace_operation_kind NOT IN ('start_exec', 'create_pty', 'resize_pty', 'close_pty')
       OR active_write_lease.id IS NOT NULL
   )
RETURNING *;

-- name: GetWorkspaceOperation :one
SELECT *
  FROM workspace_operations
 WHERE org_id = sqlc.arg(org_id)
   AND project_id = sqlc.arg(project_id)
   AND environment_id = sqlc.arg(environment_id)
   AND workspace_id = sqlc.arg(workspace_id)
   AND id = sqlc.arg(id);

-- name: GetActiveWorkspaceOperationByResource :one
SELECT *
  FROM workspace_operations
 WHERE org_id = sqlc.arg(org_id)
   AND project_id = sqlc.arg(project_id)
   AND environment_id = sqlc.arg(environment_id)
   AND workspace_id = sqlc.arg(workspace_id)
   AND workspace_mount_id = sqlc.arg(workspace_mount_id)
   AND operation_kind = sqlc.arg(operation_kind)::workspace_operation_kind
   AND resource_kind = sqlc.arg(resource_kind)::workspace_resource_kind
   AND resource_id = sqlc.arg(resource_id)
   AND state IN ('queued', 'claimed', 'running')
 ORDER BY requested_at ASC
 LIMIT 1;

-- name: ClaimWorkspaceOperation :one
WITH exhausted AS (
    UPDATE workspace_operations
       SET state = 'lost',
           error = jsonb_build_object('code', 'workspace_operation_claims_exhausted'),
           completed_at = now(),
           updated_at = now()
     WHERE workspace_operations.org_id = sqlc.arg(org_id)
       AND workspace_operations.workspace_mount_id = sqlc.arg(workspace_mount_id)
       AND workspace_operations.state = 'claimed'
       AND workspace_operations.claim_expires_at <= now()
       AND workspace_operations.claim_attempt >= sqlc.arg(max_claim_attempts)
    RETURNING workspace_operations.*
),
expired AS (
    UPDATE workspace_operations
       SET state = 'expired',
           error = jsonb_build_object('code', 'workspace_operation_expired'),
           completed_at = now(),
           updated_at = now()
     WHERE workspace_operations.org_id = sqlc.arg(org_id)
       AND workspace_operations.workspace_mount_id = sqlc.arg(workspace_mount_id)
       AND workspace_operations.state IN ('queued', 'claimed', 'running')
       AND workspace_operations.operation_expires_at <= now()
    RETURNING workspace_operations.*
),
terminal_start_exec_operations AS (
    SELECT * FROM exhausted
     WHERE operation_kind = 'start_exec'
       AND resource_kind = 'workspace_exec'::workspace_resource_kind
       AND resource_id IS NOT NULL
    UNION ALL
    SELECT * FROM expired
     WHERE operation_kind = 'start_exec'
       AND resource_kind = 'workspace_exec'::workspace_resource_kind
       AND resource_id IS NOT NULL
),
terminal_create_pty_operations AS (
    SELECT * FROM exhausted
     WHERE operation_kind = 'create_pty'
       AND resource_kind = 'workspace_pty'::workspace_resource_kind
       AND resource_id IS NOT NULL
    UNION ALL
    SELECT * FROM expired
     WHERE operation_kind = 'create_pty'
       AND resource_kind = 'workspace_pty'::workspace_resource_kind
       AND resource_id IS NOT NULL
),
terminal_pty_control_operations AS (
    SELECT * FROM exhausted
     WHERE operation_kind IN ('resize_pty', 'close_pty')
       AND resource_kind = 'workspace_pty'::workspace_resource_kind
       AND resource_id IS NOT NULL
    UNION ALL
    SELECT * FROM expired
     WHERE operation_kind IN ('resize_pty', 'close_pty')
       AND resource_kind = 'workspace_pty'::workspace_resource_kind
       AND resource_id IS NOT NULL
),
failed_start_execs AS (
    UPDATE workspace_execs
       SET state = 'failed',
           error = terminal_start_exec_operations.error,
           exited_at = coalesce(workspace_execs.exited_at, now()),
           updated_at = now()
      FROM terminal_start_exec_operations
     WHERE workspace_execs.org_id = terminal_start_exec_operations.org_id
       AND workspace_execs.project_id = terminal_start_exec_operations.project_id
       AND workspace_execs.environment_id = terminal_start_exec_operations.environment_id
       AND workspace_execs.workspace_id = terminal_start_exec_operations.workspace_id
       AND workspace_execs.workspace_mount_id = terminal_start_exec_operations.workspace_mount_id
       AND workspace_execs.id = terminal_start_exec_operations.resource_id
       AND workspace_execs.state IN ('queued', 'materializing')
    RETURNING workspace_execs.*
),
failed_create_ptys AS (
    UPDATE workspace_pty_sessions
       SET state = 'failed',
           error = terminal_create_pty_operations.error,
           closed_at = coalesce(workspace_pty_sessions.closed_at, now()),
           updated_at = now()
      FROM terminal_create_pty_operations
     WHERE workspace_pty_sessions.org_id = terminal_create_pty_operations.org_id
       AND workspace_pty_sessions.project_id = terminal_create_pty_operations.project_id
       AND workspace_pty_sessions.environment_id = terminal_create_pty_operations.environment_id
       AND workspace_pty_sessions.workspace_id = terminal_create_pty_operations.workspace_id
       AND workspace_pty_sessions.workspace_mount_id = terminal_create_pty_operations.workspace_mount_id
       AND workspace_pty_sessions.id = terminal_create_pty_operations.resource_id
       AND workspace_pty_sessions.state = 'creating'
    RETURNING workspace_pty_sessions.*
),
rolled_back_pty_controls AS (
    UPDATE workspace_pty_sessions
       SET state = CASE
               WHEN terminal_pty_control_operations.operation_kind = 'close_pty'
                    AND workspace_pty_sessions.resize_cols IS NOT NULL
                    AND workspace_pty_sessions.resize_rows IS NOT NULL
                   THEN 'resizing'::workspace_pty_state
               ELSE 'open'::workspace_pty_state
           END,
           resize_cols = CASE
               WHEN terminal_pty_control_operations.operation_kind = 'close_pty'
                   THEN workspace_pty_sessions.resize_cols
               ELSE NULL
           END,
           resize_rows = CASE
               WHEN terminal_pty_control_operations.operation_kind = 'close_pty'
                   THEN workspace_pty_sessions.resize_rows
               ELSE NULL
           END,
           updated_at = now()
      FROM terminal_pty_control_operations
     WHERE workspace_pty_sessions.org_id = terminal_pty_control_operations.org_id
       AND workspace_pty_sessions.project_id = terminal_pty_control_operations.project_id
       AND workspace_pty_sessions.environment_id = terminal_pty_control_operations.environment_id
       AND workspace_pty_sessions.workspace_id = terminal_pty_control_operations.workspace_id
       AND workspace_pty_sessions.workspace_mount_id = terminal_pty_control_operations.workspace_mount_id
       AND workspace_pty_sessions.id = terminal_pty_control_operations.resource_id
       AND (
           (
               terminal_pty_control_operations.operation_kind = 'resize_pty'
               AND workspace_pty_sessions.state = 'resizing'
               AND workspace_pty_sessions.resize_cols::text = terminal_pty_control_operations.request->>'cols'
               AND workspace_pty_sessions.resize_rows::text = terminal_pty_control_operations.request->>'rows'
           )
           OR (
               terminal_pty_control_operations.operation_kind = 'close_pty'
               AND workspace_pty_sessions.state = 'closing'
           )
       )
    RETURNING workspace_pty_sessions.*
),
released_terminal_operation_write_leases AS (
    UPDATE workspace_leases
       SET state = 'released',
           released_at = coalesce(workspace_leases.released_at, now()),
           updated_at = now()
      FROM (
          SELECT write_lease_id,
                 org_id,
                 project_id,
                 environment_id,
                 workspace_id,
                 workspace_mount_id
            FROM failed_start_execs
           WHERE write_lease_id IS NOT NULL
          UNION ALL
          SELECT write_lease_id,
                 org_id,
                 project_id,
                 environment_id,
                 workspace_id,
                 workspace_mount_id
            FROM failed_create_ptys
           WHERE write_lease_id IS NOT NULL
      ) AS terminal_operations
     WHERE workspace_leases.org_id = terminal_operations.org_id
       AND workspace_leases.project_id = terminal_operations.project_id
       AND workspace_leases.environment_id = terminal_operations.environment_id
       AND workspace_leases.workspace_id = terminal_operations.workspace_id
       AND workspace_leases.workspace_mount_id = terminal_operations.workspace_mount_id
       AND workspace_leases.id = terminal_operations.write_lease_id
       AND workspace_leases.lease_kind = 'write'
       AND workspace_leases.state IN ('active', 'releasing')
    RETURNING workspace_leases.id
),
terminal_operation_stream_wakeups AS (
    INSERT INTO workspace_stream_wakeups (org_id, project_id, environment_id, workspace_id, resource_kind, resource_id, stream, cursor_offset, notification_kind)
    SELECT failed_start_execs.org_id,
           failed_start_execs.project_id,
           failed_start_execs.environment_id,
           failed_start_execs.workspace_id,
           'workspace_exec'::workspace_resource_kind,
           failed_start_execs.id,
           stream_names.stream,
           stream_names.cursor_offset,
           'terminal'::workspace_stream_notification_kind
      FROM failed_start_execs
      CROSS JOIN LATERAL (VALUES ('stdout', failed_start_execs.stdout_cursor), ('stderr', failed_start_execs.stderr_cursor)) AS stream_names(stream, cursor_offset)
    UNION ALL
    SELECT failed_create_ptys.org_id,
           failed_create_ptys.project_id,
           failed_create_ptys.environment_id,
           failed_create_ptys.workspace_id,
           'workspace_pty'::workspace_resource_kind,
           failed_create_ptys.id,
           'output',
           failed_create_ptys.output_cursor,
           'terminal'::workspace_stream_notification_kind
      FROM failed_create_ptys
    RETURNING id
),
candidate AS (
    SELECT workspace_operations.id
      FROM workspace_operations
      JOIN workspace_mounts
        ON workspace_mounts.org_id = workspace_operations.org_id
       AND workspace_mounts.project_id = workspace_operations.project_id
       AND workspace_mounts.environment_id = workspace_operations.environment_id
       AND workspace_mounts.workspace_id = workspace_operations.workspace_id
       AND workspace_mounts.id = workspace_operations.workspace_mount_id
      JOIN runtime_instances
        ON runtime_instances.org_id = workspace_mounts.org_id
       AND runtime_instances.id = workspace_mounts.runtime_instance_id
     WHERE workspace_operations.org_id = sqlc.arg(org_id)
       AND workspace_operations.workspace_mount_id = sqlc.arg(workspace_mount_id)
       AND (
           workspace_operations.state = 'queued'
           OR (
               workspace_operations.state = 'claimed'
               AND workspace_operations.claim_expires_at <= now()
           )
       )
       AND workspace_operations.claim_attempt < sqlc.arg(max_claim_attempts)
       AND workspace_operations.operation_expires_at > now()
       AND (SELECT count(*) FROM released_terminal_operation_write_leases) >= 0
       AND (SELECT count(*) FROM terminal_operation_stream_wakeups) >= 0
       AND runtime_instances.worker_instance_id = sqlc.arg(worker_instance_id)
       AND runtime_instances.instance_token = sqlc.arg(runtime_instance_token)
       AND runtime_instances.state IN ('running', 'waiting_hot')
       AND workspace_mounts.state = 'mounted'
       AND (
           workspace_operations.write_lease_id IS NULL
           OR EXISTS (
               SELECT 1
                 FROM workspace_leases
                WHERE workspace_leases.org_id = workspace_operations.org_id
                  AND workspace_leases.project_id = workspace_operations.project_id
                  AND workspace_leases.environment_id = workspace_operations.environment_id
                  AND workspace_leases.workspace_id = workspace_operations.workspace_id
                  AND workspace_leases.workspace_mount_id = workspace_operations.workspace_mount_id
                  AND workspace_leases.id = workspace_operations.write_lease_id
                  AND workspace_leases.lease_kind = 'write'
                  AND workspace_leases.state = 'active'
                  AND workspace_leases.fencing_token = workspace_operations.fencing_token
                  AND workspace_leases.acquired_fencing_generation = workspace_operations.fencing_generation
                  AND workspace_leases.expires_at > now()
           )
       )
     ORDER BY workspace_operations.priority DESC,
              workspace_operations.requested_at ASC
     LIMIT 1
     FOR UPDATE SKIP LOCKED
)
UPDATE workspace_operations
   SET state = 'claimed',
       claimed_by_worker_instance_id = sqlc.arg(worker_instance_id),
       claim_token = sqlc.arg(claim_token),
       claim_attempt = workspace_operations.claim_attempt + 1,
       claim_expires_at = sqlc.arg(claim_expires_at),
       claimed_at = now(),
       updated_at = now()
 FROM candidate
 WHERE workspace_operations.id = candidate.id
   AND (
       workspace_operations.state = 'queued'
       OR (
           workspace_operations.state = 'claimed'
           AND workspace_operations.claim_expires_at <= now()
       )
   )
RETURNING workspace_operations.*;

-- name: StartWorkspaceOperation :one
WITH started AS (
    UPDATE workspace_operations
       SET state = 'running',
           updated_at = now()
     WHERE workspace_operations.org_id = sqlc.arg(org_id)
       AND workspace_operations.id = sqlc.arg(id)
       AND workspace_operations.claimed_by_worker_instance_id = sqlc.arg(worker_instance_id)
       AND workspace_operations.claim_token = sqlc.arg(claim_token)
	       AND workspace_operations.state = 'claimed'
	       AND workspace_operations.claim_expires_at > now()
	       AND workspace_operations.operation_expires_at > now()
	       AND (
	           workspace_operations.write_lease_id IS NULL
	           OR EXISTS (
	               SELECT 1
	                 FROM workspace_leases
	                WHERE workspace_leases.org_id = workspace_operations.org_id
	                  AND workspace_leases.project_id = workspace_operations.project_id
	                  AND workspace_leases.environment_id = workspace_operations.environment_id
	                  AND workspace_leases.workspace_id = workspace_operations.workspace_id
	                  AND workspace_leases.workspace_mount_id = workspace_operations.workspace_mount_id
	                  AND workspace_leases.id = workspace_operations.write_lease_id
	                  AND workspace_leases.lease_kind = 'write'
	                  AND workspace_leases.state = 'active'
	                  AND workspace_leases.fencing_token = workspace_operations.fencing_token
	                  AND workspace_leases.acquired_fencing_generation = workspace_operations.fencing_generation
	                  AND workspace_leases.expires_at > now()
	           )
	       )
    RETURNING *
)
SELECT * FROM started
UNION ALL
SELECT *
  FROM workspace_operations
 WHERE NOT EXISTS (SELECT 1 FROM started)
   AND workspace_operations.org_id = sqlc.arg(org_id)
       AND workspace_operations.id = sqlc.arg(id)
       AND workspace_operations.claimed_by_worker_instance_id = sqlc.arg(worker_instance_id)
	       AND workspace_operations.claim_token = sqlc.arg(claim_token)
	       AND workspace_operations.state = 'running'
	       AND workspace_operations.operation_expires_at > now()
	       AND (
	           workspace_operations.write_lease_id IS NULL
	           OR EXISTS (
	               SELECT 1
	                 FROM workspace_leases
	                WHERE workspace_leases.org_id = workspace_operations.org_id
	                  AND workspace_leases.project_id = workspace_operations.project_id
	                  AND workspace_leases.environment_id = workspace_operations.environment_id
	                  AND workspace_leases.workspace_id = workspace_operations.workspace_id
	                  AND workspace_leases.workspace_mount_id = workspace_operations.workspace_mount_id
	                  AND workspace_leases.id = workspace_operations.write_lease_id
	                  AND workspace_leases.lease_kind = 'write'
	                  AND workspace_leases.state = 'active'
	                  AND workspace_leases.fencing_token = workspace_operations.fencing_token
	                  AND workspace_leases.acquired_fencing_generation = workspace_operations.fencing_generation
	                  AND workspace_leases.expires_at > now()
	           )
	       )
LIMIT 1;

-- name: CompleteWorkspaceOperation :one
WITH completed AS (
    UPDATE workspace_operations
       SET state = 'completed',
           result = coalesce(sqlc.arg(result)::jsonb, '{}'::jsonb),
           completed_at = now(),
           updated_at = now()
     WHERE workspace_operations.org_id = sqlc.arg(org_id)
       AND workspace_operations.id = sqlc.arg(id)
       AND workspace_operations.claimed_by_worker_instance_id = sqlc.arg(worker_instance_id)
       AND workspace_operations.claim_token = sqlc.arg(claim_token)
	       AND workspace_operations.state = 'running'
	       AND workspace_operations.operation_expires_at > now()
	       AND (
	           workspace_operations.write_lease_id IS NULL
	           OR EXISTS (
	               SELECT 1
	                 FROM workspace_leases
	                WHERE workspace_leases.org_id = workspace_operations.org_id
	                  AND workspace_leases.project_id = workspace_operations.project_id
	                  AND workspace_leases.environment_id = workspace_operations.environment_id
	                  AND workspace_leases.workspace_id = workspace_operations.workspace_id
	                  AND workspace_leases.workspace_mount_id = workspace_operations.workspace_mount_id
	                  AND workspace_leases.id = workspace_operations.write_lease_id
	                  AND workspace_leases.lease_kind = 'write'
	                  AND workspace_leases.state = 'active'
	                  AND workspace_leases.fencing_token = workspace_operations.fencing_token
	                  AND workspace_leases.acquired_fencing_generation = workspace_operations.fencing_generation
	                  AND workspace_leases.expires_at > now()
	           )
	       )
    RETURNING *
)
SELECT * FROM completed
UNION ALL
SELECT *
  FROM workspace_operations
 WHERE NOT EXISTS (SELECT 1 FROM completed)
   AND workspace_operations.org_id = sqlc.arg(org_id)
       AND workspace_operations.id = sqlc.arg(id)
       AND workspace_operations.claimed_by_worker_instance_id = sqlc.arg(worker_instance_id)
       AND workspace_operations.claim_token = sqlc.arg(claim_token)
       AND workspace_operations.state = 'completed'
   AND workspace_operations.result = coalesce(sqlc.arg(result)::jsonb, '{}'::jsonb)
LIMIT 1;

-- name: FailWorkspaceOperation :one
WITH failed AS (
    UPDATE workspace_operations
       SET state = 'failed',
           error = coalesce(sqlc.arg(error)::jsonb, '{}'::jsonb),
           completed_at = now(),
           updated_at = now()
     WHERE workspace_operations.org_id = sqlc.arg(org_id)
       AND workspace_operations.id = sqlc.arg(id)
       AND workspace_operations.claimed_by_worker_instance_id = sqlc.arg(worker_instance_id)
       AND workspace_operations.claim_token = sqlc.arg(claim_token)
       AND workspace_operations.state IN ('claimed', 'running')
	       AND (
	           workspace_operations.state = 'running'
	           OR workspace_operations.claim_expires_at > now()
	       )
	       AND workspace_operations.operation_expires_at > now()
	       AND (
	           workspace_operations.write_lease_id IS NULL
	           OR EXISTS (
	               SELECT 1
	                 FROM workspace_leases
	                WHERE workspace_leases.org_id = workspace_operations.org_id
	                  AND workspace_leases.project_id = workspace_operations.project_id
	                  AND workspace_leases.environment_id = workspace_operations.environment_id
	                  AND workspace_leases.workspace_id = workspace_operations.workspace_id
	                  AND workspace_leases.workspace_mount_id = workspace_operations.workspace_mount_id
	                  AND workspace_leases.id = workspace_operations.write_lease_id
	                  AND workspace_leases.lease_kind = 'write'
	                  AND workspace_leases.state = 'active'
	                  AND workspace_leases.fencing_token = workspace_operations.fencing_token
	                  AND workspace_leases.acquired_fencing_generation = workspace_operations.fencing_generation
	                  AND workspace_leases.expires_at > now()
	           )
	       )
    RETURNING *
)
SELECT * FROM failed
UNION ALL
SELECT *
  FROM workspace_operations
 WHERE NOT EXISTS (SELECT 1 FROM failed)
   AND workspace_operations.org_id = sqlc.arg(org_id)
   AND workspace_operations.id = sqlc.arg(id)
   AND workspace_operations.claimed_by_worker_instance_id = sqlc.arg(worker_instance_id)
   AND workspace_operations.claim_token = sqlc.arg(claim_token)
   AND workspace_operations.state = 'failed'
   AND workspace_operations.error = coalesce(sqlc.arg(error)::jsonb, '{}'::jsonb)
LIMIT 1;
