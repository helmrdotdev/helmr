-- name: CreateWorkspaceStreamWakeup :one
INSERT INTO workspace_stream_wakeups (
    org_id,
    project_id,
    environment_id,
    workspace_id,
    resource_kind,
    resource_id,
    stream,
    cursor_offset,
    notification_kind
) VALUES (
    sqlc.arg(org_id),
    sqlc.arg(project_id),
    sqlc.arg(environment_id),
    sqlc.arg(workspace_id),
    sqlc.arg(resource_kind),
    sqlc.arg(resource_id),
    sqlc.arg(stream),
    sqlc.arg(cursor_offset),
    sqlc.arg(notification_kind)
)
RETURNING *;

-- name: ClaimWorkspaceStreamWakeups :many
WITH claimable AS (
    SELECT id
      FROM workspace_stream_wakeups
     WHERE attempts < sqlc.arg(max_attempts)::int
       AND (locked_until IS NULL OR locked_until < now())
     ORDER BY id ASC
     LIMIT sqlc.arg(row_limit)
     FOR UPDATE SKIP LOCKED
),
updated AS (
    UPDATE workspace_stream_wakeups
       SET locked_until = now() + sqlc.arg(lease_duration)::interval,
           attempts = workspace_stream_wakeups.attempts + 1
      FROM claimable
     WHERE workspace_stream_wakeups.id = claimable.id
    RETURNING workspace_stream_wakeups.*
)
SELECT *
  FROM updated
 ORDER BY id ASC;

-- name: DeleteWorkspaceStreamWakeup :exec
DELETE FROM workspace_stream_wakeups
 WHERE id = sqlc.arg(id);

-- name: MarkWorkspaceStreamWakeupFailed :exec
UPDATE workspace_stream_wakeups
   SET locked_until = CASE
           WHEN attempts >= sqlc.arg(max_attempts)::int THEN NULL
           ELSE now() + sqlc.arg(retry_after)::interval
       END,
       last_error = sqlc.arg(last_error)
 WHERE id = sqlc.arg(id);
