-- name: GetWaitpointForResponseTokenCreation :one
SELECT waitpoints.id,
       waitpoints.kind
  FROM run_waits
  JOIN run_wait_dependencies ON run_wait_dependencies.org_id = run_waits.org_id
                            AND run_wait_dependencies.run_wait_id = run_waits.id
  JOIN waitpoints ON waitpoints.org_id = run_wait_dependencies.org_id
                 AND waitpoints.id = run_wait_dependencies.waitpoint_id
  JOIN runs ON runs.org_id = run_waits.org_id
           AND runs.id = run_waits.run_id
 WHERE run_waits.org_id = sqlc.arg(org_id)
   AND run_waits.run_id = sqlc.arg(run_id)
   AND waitpoints.id = sqlc.arg(waitpoint_id)
   AND waitpoints.status = 'pending'
   AND run_waits.status = 'waiting'
   AND runs.status = 'waiting'
   AND runs.current_execution_id IS NULL;

-- name: CreateWaitpointResponseToken :one
WITH target_waitpoint AS (
    SELECT waitpoints.*,
           run_waits.id AS run_wait_id,
           run_waits.run_id
      FROM run_waits
      JOIN run_wait_dependencies ON run_wait_dependencies.org_id = run_waits.org_id
                                AND run_wait_dependencies.run_wait_id = run_waits.id
      JOIN waitpoints ON waitpoints.org_id = run_wait_dependencies.org_id
                     AND waitpoints.id = run_wait_dependencies.waitpoint_id
      JOIN runs ON runs.org_id = run_waits.org_id
               AND runs.id = run_waits.run_id
     WHERE run_waits.org_id = sqlc.arg(org_id)
       AND run_waits.run_id = sqlc.arg(run_id)
       AND waitpoints.id = sqlc.arg(waitpoint_id)
       AND waitpoints.status = 'pending'
       AND run_waits.status = 'waiting'
       AND runs.status = 'waiting'
       AND runs.current_execution_id IS NULL
)
INSERT INTO waitpoint_response_tokens (
    id,
    org_id,
    run_id,
    run_wait_id,
    waitpoint_id,
    token_hash,
    expires_at,
    external_subject,
    metadata
)
SELECT
    sqlc.arg(id),
    target_waitpoint.org_id,
    target_waitpoint.run_id,
    target_waitpoint.run_wait_id,
    target_waitpoint.id,
    sqlc.arg(token_hash),
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
                 AND waitpoints.id = waitpoint_response_tokens.waitpoint_id
  JOIN run_waits ON run_waits.org_id = waitpoint_response_tokens.org_id
                AND run_waits.id = waitpoint_response_tokens.run_wait_id
  JOIN runs ON runs.org_id = waitpoint_response_tokens.org_id
           AND runs.id = waitpoint_response_tokens.run_id
 WHERE waitpoint_response_tokens.id = sqlc.arg(id)
   AND waitpoint_response_tokens.token_hash = sqlc.arg(token_hash)
   AND waitpoint_response_tokens.status = 'pending'
   AND (waitpoint_response_tokens.expires_at IS NULL OR waitpoint_response_tokens.expires_at > now())
   AND waitpoints.status = 'pending'
   AND run_waits.status = 'waiting'
   AND runs.status = 'waiting'
   AND runs.current_execution_id IS NULL;

-- name: CompleteWaitpointResponseToken :one
WITH current_token AS (
    SELECT waitpoint_response_tokens.*
      FROM waitpoint_response_tokens
      JOIN waitpoints ON waitpoints.org_id = waitpoint_response_tokens.org_id
                     AND waitpoints.id = waitpoint_response_tokens.waitpoint_id
      JOIN run_waits ON run_waits.org_id = waitpoint_response_tokens.org_id
                    AND run_waits.id = waitpoint_response_tokens.run_wait_id
      JOIN runs ON runs.org_id = waitpoint_response_tokens.org_id
               AND runs.id = waitpoint_response_tokens.run_id
     WHERE waitpoint_response_tokens.id = sqlc.arg(id)
       AND waitpoint_response_tokens.token_hash = sqlc.arg(token_hash)
       AND waitpoint_response_tokens.status = 'pending'
       AND (waitpoint_response_tokens.expires_at IS NULL OR waitpoint_response_tokens.expires_at > now())
       AND waitpoints.kind = sqlc.arg(kind)
       AND waitpoints.status = 'pending'
       AND run_waits.status = 'waiting'
       AND runs.status = 'waiting'
       AND runs.current_execution_id IS NULL
     FOR UPDATE OF waitpoint_response_tokens, run_waits, waitpoints, runs
),
suspended_queue_entry AS (
    SELECT run_queue_items.org_id,
           run_queue_items.run_id
      FROM run_queue_items
      JOIN current_token ON current_token.org_id = run_queue_items.org_id
                        AND current_token.run_id = run_queue_items.run_id
     WHERE run_queue_items.status = 'suspended'
     FOR UPDATE OF run_queue_items
),
completed_token AS (
    UPDATE waitpoint_response_tokens
       SET status = 'completed',
           completed_at = now(),
           completed_by_principal = sqlc.arg(completed_by_principal),
           completed_via = sqlc.arg(completed_via),
           external_subject = COALESCE(sqlc.narg(external_subject), waitpoint_response_tokens.external_subject),
           metadata = waitpoint_response_tokens.metadata || sqlc.arg(metadata)::jsonb
      FROM current_token
      JOIN suspended_queue_entry ON suspended_queue_entry.org_id = current_token.org_id
                                AND suspended_queue_entry.run_id = current_token.run_id
     WHERE waitpoint_response_tokens.id = current_token.id
       AND waitpoint_response_tokens.token_hash = current_token.token_hash
       AND waitpoint_response_tokens.status = 'pending'
    RETURNING waitpoint_response_tokens.*
),
recorded_response AS (
    INSERT INTO waitpoint_responses (
        id,
        org_id,
        run_id,
        run_wait_id,
        waitpoint_id,
        response_key,
        action,
        resolution_kind,
        resolution,
        event_payload,
        completed_by_principal,
        completed_via,
        external_subject,
        metadata
    )
    SELECT
        sqlc.arg(response_id),
        completed_token.org_id,
        completed_token.run_id,
        completed_token.run_wait_id,
        completed_token.waitpoint_id,
        sqlc.arg(response_key),
        sqlc.arg(action),
        sqlc.arg(resolution_kind),
        sqlc.arg(resolution),
        sqlc.arg(event_payload)::jsonb,
        sqlc.arg(completed_by_principal),
        sqlc.arg(completed_via),
        COALESCE(sqlc.narg(external_subject), completed_token.external_subject),
        sqlc.arg(metadata)::jsonb
      FROM completed_token
    ON CONFLICT (org_id, run_id, run_wait_id, waitpoint_id, response_key) DO UPDATE
       SET action = EXCLUDED.action,
           resolution_kind = EXCLUDED.resolution_kind,
           resolution = EXCLUDED.resolution,
           event_payload = EXCLUDED.event_payload,
           completed_by_principal = EXCLUDED.completed_by_principal,
           completed_via = EXCLUDED.completed_via,
           external_subject = EXCLUDED.external_subject,
           metadata = waitpoint_responses.metadata || EXCLUDED.metadata
    RETURNING id
)
SELECT completed_token.*
  FROM completed_token
  JOIN recorded_response ON true;

-- name: RevokeWaitpointResponseToken :execrows
UPDATE waitpoint_response_tokens
   SET status = 'revoked'
 WHERE org_id = sqlc.arg(org_id)
   AND id = sqlc.arg(id)
   AND status = 'pending';
