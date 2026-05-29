-- name: CreateWaitpointResponseToken :one
WITH target_waitpoint AS (
    SELECT waitpoints.*
      FROM waitpoints
      JOIN runs ON runs.org_id = waitpoints.org_id
               AND runs.id = waitpoints.run_id
     WHERE waitpoints.org_id = sqlc.arg(org_id)
       AND waitpoints.run_id = sqlc.arg(run_id)
       AND waitpoints.id = sqlc.arg(waitpoint_id)
       AND waitpoints.status = 'waiting'
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
   AND waitpoints.status = 'waiting'
   AND runs.status = 'waiting'
   AND runs.current_execution_id IS NULL;

-- name: CompleteWaitpointResponseToken :one
WITH current_token AS (
    SELECT waitpoint_response_tokens.*,
           CAST(GREATEST(
               COALESCE(NULLIF((waitpoints.policy_snapshot #>> '{config,resolution,count}')::int, 0), 1),
               1
           ) AS int) AS quorum_count
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
       AND waitpoints.status = 'waiting'
       AND runs.status = 'waiting'
       AND runs.current_execution_id IS NULL
     FOR UPDATE OF waitpoint_response_tokens, waitpoints, runs
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
prior_response AS (
    SELECT waitpoint_responses.id
      FROM waitpoint_responses
      JOIN current_token ON current_token.org_id = waitpoint_responses.org_id
                        AND current_token.run_id = waitpoint_responses.run_id
                        AND current_token.waitpoint_id = waitpoint_responses.waitpoint_id
     WHERE waitpoint_responses.response_key = sqlc.arg(response_key)
),
recorded_response AS (
    INSERT INTO waitpoint_responses (
        id,
        org_id,
        run_id,
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
    ON CONFLICT (org_id, run_id, waitpoint_id, response_key) DO UPDATE
       SET action = EXCLUDED.action,
           resolution_kind = EXCLUDED.resolution_kind,
           resolution = EXCLUDED.resolution,
           event_payload = EXCLUDED.event_payload,
           completed_by_principal = EXCLUDED.completed_by_principal,
           completed_via = EXCLUDED.completed_via,
           external_subject = EXCLUDED.external_subject,
           metadata = waitpoint_responses.metadata || EXCLUDED.metadata
    RETURNING id
),
eligible_resolution AS (
    SELECT current_token.org_id,
           current_token.run_id,
           current_token.waitpoint_id
      FROM current_token
      JOIN completed_token ON completed_token.id = current_token.id
      JOIN recorded_response ON true
     WHERE (
           SELECT count(*)::int
             FROM waitpoint_responses
            WHERE waitpoint_responses.org_id = current_token.org_id
              AND waitpoint_responses.run_id = current_token.run_id
              AND waitpoint_responses.waitpoint_id = current_token.waitpoint_id
       ) + CASE WHEN NOT EXISTS (SELECT 1 FROM prior_response) THEN 1 ELSE 0 END >= current_token.quorum_count
),
resolved AS (
    UPDATE waitpoints
       SET status = 'resuming',
           resolution_kind = sqlc.arg(resolution_kind),
           resolution = sqlc.arg(resolution),
           resolved_at = now()
      FROM eligible_resolution
     WHERE waitpoints.org_id = eligible_resolution.org_id
       AND waitpoints.run_id = eligible_resolution.run_id
       AND waitpoints.id = eligible_resolution.waitpoint_id
       AND waitpoints.status = 'waiting'
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
    UPDATE run_queue_items
       SET status = 'queued',
           dispatch_message_id = NULL,
           reserved_by_worker_instance_id = NULL,
           reservation_expires_at = NULL,
           dispatch_generation = dispatch_generation + 1,
           last_error = '',
           enqueued_at = now(),
           updated_at = now(),
           finished_at = NULL
      FROM updated_run
      JOIN suspended_queue_entry ON suspended_queue_entry.run_id = updated_run.id
     WHERE run_queue_items.org_id = suspended_queue_entry.org_id
       AND run_queue_items.run_id = updated_run.id
       AND run_queue_items.status = 'suspended'
    RETURNING run_queue_items.run_id
),
event AS (
    INSERT INTO run_events (org_id, run_id, kind, payload)
    SELECT resolved.org_id, resolved.run_id, 'waitpoint.resolved', sqlc.arg(event_payload)
      FROM resolved
      JOIN updated_run ON true
      JOIN continuation_queue_entry ON true
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
