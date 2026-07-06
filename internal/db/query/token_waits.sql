-- name: CreateTokenWait :one
INSERT INTO token_waits (
    id,
    org_id,
    worker_group_id,
    project_id,
    environment_id,
    run_wait_id,
    token_id,
    matched_completion_at
)
SELECT sqlc.arg(id),
       run_waits.org_id,
       run_waits.worker_group_id,
       run_waits.project_id,
       run_waits.environment_id,
       run_waits.id,
       tokens.id,
       CASE WHEN tokens.state IN ('completed', 'expired', 'cancelled')
            THEN COALESCE(tokens.completed_at, tokens.expired_at, tokens.cancelled_at, now())
            ELSE NULL::timestamptz
       END
  FROM run_waits
  JOIN tokens ON tokens.org_id = run_waits.org_id
             AND tokens.project_id = run_waits.project_id
             AND tokens.environment_id = run_waits.environment_id
             AND tokens.id = sqlc.arg(token_id)
 WHERE run_waits.org_id = sqlc.arg(org_id)
   AND run_waits.worker_group_id = sqlc.arg(worker_group_id)
   AND run_waits.project_id = sqlc.arg(project_id)
   AND run_waits.environment_id = sqlc.arg(environment_id)
   AND run_waits.id = sqlc.arg(run_wait_id)
   AND run_waits.kind = 'token'
RETURNING *;

-- name: ResolveImmediateTokenWait :one
WITH target_wait AS (
    SELECT token_waits.*, tokens.state AS token_state
      FROM token_waits
      JOIN run_waits ON run_waits.org_id = token_waits.org_id
                    AND run_waits.worker_group_id = token_waits.worker_group_id
                    AND run_waits.id = token_waits.run_wait_id
      JOIN tokens ON tokens.org_id = token_waits.org_id
                 AND tokens.project_id = token_waits.project_id
                 AND tokens.environment_id = token_waits.environment_id
                 AND tokens.id = token_waits.token_id
     WHERE token_waits.org_id = sqlc.arg(org_id)
       AND token_waits.worker_group_id = sqlc.arg(worker_group_id)
       AND token_waits.id = sqlc.arg(id)
       AND token_waits.matched_completion_at IS NOT NULL
       AND run_waits.state IN ('live_waiting', 'checkpointed_waiting')
     FOR UPDATE OF run_waits
),
resolved_wait AS (
    UPDATE run_waits
       SET state = CASE
             WHEN run_waits.state = 'live_waiting' AND target_wait.token_state IN ('completed', 'cancelled', 'expired') THEN 'resolved_live'::run_wait_state
             WHEN run_waits.state = 'checkpointed_waiting' AND target_wait.token_state IN ('completed', 'cancelled', 'expired') THEN 'resolved_checkpointed'::run_wait_state
             ELSE run_waits.state
           END,
           resolved_at = CASE WHEN target_wait.token_state IN ('completed', 'expired', 'cancelled') THEN now() ELSE run_waits.resolved_at END,
           updated_at = now()
     FROM target_wait
     WHERE run_waits.org_id = target_wait.org_id
       AND run_waits.worker_group_id = target_wait.worker_group_id
       AND run_waits.id = target_wait.run_wait_id
       AND run_waits.state IN ('live_waiting', 'checkpointed_waiting')
    RETURNING run_waits.id
)
SELECT target_wait.*
  FROM target_wait
  JOIN resolved_wait ON true;

-- name: GetTokenWaitForRunWait :one
SELECT *
 FROM token_waits
 WHERE org_id = sqlc.arg(org_id)
   AND worker_group_id = sqlc.arg(worker_group_id)
   AND project_id = sqlc.arg(project_id)
   AND environment_id = sqlc.arg(environment_id)
   AND run_wait_id = sqlc.arg(run_wait_id);
