-- name: CreateWaitpointResponseToken :one
WITH target_waitpoint AS (
    SELECT waitpoints.*
      FROM waitpoints
      JOIN runs ON runs.org_id = waitpoints.org_id
               AND runs.id = waitpoints.run_id
     WHERE waitpoints.org_id = sqlc.arg(org_id)
       AND waitpoints.run_id = sqlc.arg(run_id)
       AND waitpoints.id = sqlc.arg(waitpoint_id)
       AND waitpoints.status = 'pending'
       AND runs.status = 'waiting'
       AND runs.current_execution_id IS NULL
)
INSERT INTO waitpoint_response_tokens (
    id,
    org_id,
    run_id,
    waitpoint_id,
    token_hash,
    allowed_actions,
    expires_at,
    external_subject,
    metadata
)
SELECT
    sqlc.arg(id),
    target_waitpoint.org_id,
    target_waitpoint.run_id,
    target_waitpoint.id,
    sqlc.arg(token_hash),
    sqlc.arg(allowed_actions)::text[],
    sqlc.arg(expires_at),
    sqlc.narg(external_subject),
    sqlc.arg(metadata)
  FROM target_waitpoint
RETURNING *;

-- name: GetActiveWaitpointResponseToken :one
SELECT
    waitpoint_response_tokens.*,
    waitpoints.kind AS waitpoint_kind,
    waitpoints.display_text AS waitpoint_display_text
  FROM waitpoint_response_tokens
  JOIN waitpoints ON waitpoints.org_id = waitpoint_response_tokens.org_id
                 AND waitpoints.run_id = waitpoint_response_tokens.run_id
                 AND waitpoints.id = waitpoint_response_tokens.waitpoint_id
  JOIN runs ON runs.org_id = waitpoint_response_tokens.org_id
           AND runs.id = waitpoint_response_tokens.run_id
 WHERE waitpoint_response_tokens.id = sqlc.arg(id)
   AND waitpoint_response_tokens.token_hash = sqlc.arg(token_hash)
   AND waitpoint_response_tokens.status = 'pending'
   AND (waitpoint_response_tokens.expires_at IS NULL OR waitpoint_response_tokens.expires_at > now())
   AND waitpoints.status = 'pending'
   AND runs.status = 'waiting'
   AND runs.current_execution_id IS NULL;

-- name: CompleteWaitpointResponseToken :one
WITH current_token AS (
    SELECT waitpoint_response_tokens.*
      FROM waitpoint_response_tokens
      JOIN waitpoints ON waitpoints.org_id = waitpoint_response_tokens.org_id
                     AND waitpoints.run_id = waitpoint_response_tokens.run_id
                     AND waitpoints.id = waitpoint_response_tokens.waitpoint_id
      JOIN runs ON runs.org_id = waitpoint_response_tokens.org_id
               AND runs.id = waitpoint_response_tokens.run_id
     WHERE waitpoint_response_tokens.id = sqlc.arg(id)
       AND waitpoint_response_tokens.token_hash = sqlc.arg(token_hash)
       AND waitpoint_response_tokens.status = 'pending'
       AND (waitpoint_response_tokens.expires_at IS NULL OR waitpoint_response_tokens.expires_at > now())
       AND (
           sqlc.arg(action)::text = ANY(waitpoint_response_tokens.allowed_actions)
           OR (
               waitpoints.kind = 'message'
               AND sqlc.arg(action)::text IN ('message', 'reply')
               AND waitpoint_response_tokens.allowed_actions && ARRAY['message', 'reply']::text[]
           )
       )
       AND waitpoints.kind = sqlc.arg(kind)
       AND waitpoints.status = 'pending'
       AND runs.status = 'waiting'
       AND runs.current_execution_id IS NULL
     FOR UPDATE OF waitpoint_response_tokens, waitpoints, runs
),
completed_queue_entry AS (
    SELECT run_queue_entries.org_id,
           run_queue_entries.run_id
      FROM run_queue_entries
      JOIN current_token ON current_token.org_id = run_queue_entries.org_id
                        AND current_token.run_id = run_queue_entries.run_id
     WHERE run_queue_entries.status = 'completed'
     FOR UPDATE OF run_queue_entries
),
resolved AS (
    UPDATE waitpoints
       SET status = 'resolved',
           resolution_kind = sqlc.arg(resolution_kind),
           resolution = sqlc.arg(resolution),
           resolved_at = now()
      FROM current_token
      JOIN completed_queue_entry ON completed_queue_entry.org_id = current_token.org_id
                                AND completed_queue_entry.run_id = current_token.run_id
     WHERE waitpoints.org_id = current_token.org_id
       AND waitpoints.run_id = current_token.run_id
       AND waitpoints.id = current_token.waitpoint_id
       AND waitpoints.status = 'pending'
    RETURNING waitpoints.*
),
updated_run AS (
    UPDATE runs
       SET status = 'queued',
           updated_at = now()
      FROM resolved
     WHERE runs.org_id = resolved.org_id
       AND runs.id = resolved.run_id
       AND runs.status = 'waiting'
       AND runs.current_execution_id IS NULL
    RETURNING runs.id
),
continuation_queue_entry AS (
    UPDATE run_queue_entries
       SET status = 'queued',
           queue_message_id = '',
           leased_by_worker_host_id = NULL,
           lease_expires_at = NULL,
           queue_version = queue_version + 1,
           last_error = '',
           enqueued_at = now(),
           updated_at = now(),
           finished_at = NULL
      FROM updated_run
      JOIN completed_queue_entry ON completed_queue_entry.run_id = updated_run.id
     WHERE run_queue_entries.org_id = completed_queue_entry.org_id
       AND run_queue_entries.run_id = updated_run.id
       AND run_queue_entries.status = 'completed'
    RETURNING run_queue_entries.run_id
),
event AS (
    INSERT INTO run_events (org_id, run_id, kind, payload)
    SELECT resolved.org_id, resolved.run_id, 'waitpoint.resolved', sqlc.arg(payload)
      FROM resolved
    RETURNING id
),
completed_token AS (
    UPDATE waitpoint_response_tokens
       SET status = 'completed',
           completed_at = now(),
           completed_by_principal = sqlc.arg(completed_by_principal),
           completed_via = sqlc.arg(completed_via),
           external_subject = COALESCE(sqlc.narg(external_subject), waitpoint_response_tokens.external_subject),
           metadata = waitpoint_response_tokens.metadata || sqlc.arg(metadata)::jsonb
      FROM resolved
      JOIN current_token ON current_token.id = sqlc.arg(id)
     WHERE waitpoint_response_tokens.id = current_token.id
       AND waitpoint_response_tokens.token_hash = current_token.token_hash
       AND waitpoint_response_tokens.status = 'pending'
    RETURNING waitpoint_response_tokens.*
)
SELECT completed_token.*
  FROM completed_token
  JOIN updated_run ON true
  JOIN continuation_queue_entry ON true
  JOIN event ON true;

-- name: RevokeWaitpointResponseToken :execrows
UPDATE waitpoint_response_tokens
   SET status = 'revoked'
 WHERE org_id = sqlc.arg(org_id)
   AND id = sqlc.arg(id)
   AND status = 'pending';
