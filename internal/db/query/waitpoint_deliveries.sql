-- name: CreateWaitpointDelivery :one
INSERT INTO waitpoint_deliveries (
    id,
    org_id,
    run_id,
    waitpoint_id,
    response_token_id,
    channel,
    recipient_kind,
    recipient,
    status,
    message_id,
    last_error,
    metadata
) VALUES (
    sqlc.arg(delivery_id),
    sqlc.arg(org_id),
    sqlc.arg(run_id),
    sqlc.arg(waitpoint_id),
    sqlc.narg(response_token_id),
    sqlc.arg(channel),
    sqlc.arg(recipient_kind),
    sqlc.arg(recipient),
    sqlc.arg(status)::waitpoint_delivery_status,
    sqlc.narg(message_id),
    sqlc.narg(last_error),
    sqlc.arg(metadata)
)
RETURNING *;

-- name: CreateQueuedWaitpointEmailDelivery :one
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
),
new_delivery AS (
INSERT INTO waitpoint_deliveries (
    id,
    org_id,
    run_id,
    waitpoint_id,
    response_token_id,
    channel,
    recipient_kind,
    recipient,
    status,
    message_id,
    metadata
)
SELECT
    sqlc.arg(delivery_id),
    target_waitpoint.org_id,
    target_waitpoint.run_id,
    target_waitpoint.id,
    sqlc.arg(delivery_id),
    'email',
    'email',
    sqlc.arg(recipient),
    'queued',
    sqlc.arg(message_id),
    sqlc.arg(delivery_metadata)::jsonb
  FROM target_waitpoint
ON CONFLICT (org_id, run_id, waitpoint_id, channel, recipient_kind, recipient)
    WHERE channel = 'email' AND recipient_kind = 'email' AND status <> 'failed'
DO UPDATE SET metadata = waitpoint_deliveries.metadata || EXCLUDED.metadata
RETURNING *
),
response_token AS (
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
        sqlc.arg(delivery_id),
        new_delivery.org_id,
        new_delivery.run_id,
        new_delivery.waitpoint_id,
        sqlc.arg(token_hash),
        sqlc.arg(allowed_actions)::text[],
        sqlc.arg(expires_at),
        sqlc.arg(recipient),
        sqlc.arg(token_metadata)::jsonb
      FROM new_delivery
     WHERE new_delivery.id = sqlc.arg(delivery_id)
       AND new_delivery.response_token_id = sqlc.arg(delivery_id)
    RETURNING *
)
SELECT new_delivery.*
  FROM new_delivery
  LEFT JOIN response_token ON true;

-- name: MarkWaitpointDeliverySent :one
UPDATE waitpoint_deliveries
   SET status = 'sent',
       last_error = NULL,
       next_attempt_at = now(),
       sending_started_at = NULL,
       sent_at = now()
WHERE org_id = sqlc.arg(org_id)
  AND id = sqlc.arg(delivery_id)
  AND attempt_count = sqlc.arg(attempt_count)
  AND (
      (
          status = 'sending'
          AND sending_started_at = sqlc.arg(sending_started_at)
      )
      OR (
          status IN ('retrying', 'failed')
          AND sending_started_at IS NULL
          AND last_attempt_at = sqlc.arg(last_attempt_at)
      )
  )
RETURNING *;

-- name: ClaimWaitpointDeliveryForSend :one
WITH candidate AS (
    SELECT waitpoint_deliveries.id
      FROM waitpoint_deliveries
      JOIN waitpoints ON waitpoints.org_id = waitpoint_deliveries.org_id
                     AND waitpoints.run_id = waitpoint_deliveries.run_id
                     AND waitpoints.id = waitpoint_deliveries.waitpoint_id
      JOIN runs ON runs.org_id = waitpoint_deliveries.org_id
               AND runs.id = waitpoint_deliveries.run_id
      JOIN waitpoint_response_tokens ON waitpoint_response_tokens.org_id = waitpoint_deliveries.org_id
                                    AND waitpoint_response_tokens.run_id = waitpoint_deliveries.run_id
                                    AND waitpoint_response_tokens.waitpoint_id = waitpoint_deliveries.waitpoint_id
                                    AND waitpoint_response_tokens.id = waitpoint_deliveries.response_token_id
     WHERE waitpoint_deliveries.id = sqlc.arg(delivery_id)
       AND (
           waitpoint_deliveries.status = 'queued'
           OR (waitpoint_deliveries.status = 'retrying' AND waitpoint_deliveries.next_attempt_at <= now())
       )
       AND waitpoints.status = 'waiting'
       AND runs.status = 'waiting'
       AND runs.current_execution_id IS NULL
       AND waitpoint_response_tokens.status = 'pending'
       AND waitpoint_response_tokens.expires_at > now()
     FOR UPDATE OF waitpoint_deliveries SKIP LOCKED
)
UPDATE waitpoint_deliveries
   SET status = 'sending',
       attempt_count = attempt_count + 1,
       last_attempt_at = now(),
       sending_started_at = now(),
       last_error = NULL
 WHERE waitpoint_deliveries.id = (SELECT candidate.id FROM candidate)
RETURNING *;

-- name: MarkObsoleteWaitpointDeliveryFailed :one
UPDATE waitpoint_deliveries
   SET status = 'failed',
       last_error = 'waitpoint is no longer waiting',
       sending_started_at = NULL
 WHERE waitpoint_deliveries.id = sqlc.arg(delivery_id)
   AND waitpoint_deliveries.status IN ('queued', 'retrying')
   AND NOT EXISTS (
       SELECT 1
         FROM waitpoints
         JOIN runs ON runs.org_id = waitpoints.org_id
                  AND runs.id = waitpoints.run_id
         JOIN waitpoint_response_tokens ON waitpoint_response_tokens.org_id = waitpoints.org_id
                                       AND waitpoint_response_tokens.run_id = waitpoints.run_id
                                       AND waitpoint_response_tokens.waitpoint_id = waitpoints.id
        WHERE waitpoints.org_id = waitpoint_deliveries.org_id
          AND waitpoints.run_id = waitpoint_deliveries.run_id
          AND waitpoints.id = waitpoint_deliveries.waitpoint_id
          AND waitpoint_response_tokens.id = waitpoint_deliveries.response_token_id
          AND waitpoints.status = 'waiting'
          AND runs.status = 'waiting'
          AND runs.current_execution_id IS NULL
          AND waitpoint_response_tokens.status = 'pending'
          AND waitpoint_response_tokens.expires_at > now()
   )
RETURNING *;

-- name: MarkWaitpointDeliverySignaled :one
UPDATE waitpoint_deliveries
   SET status = 'queued',
       next_attempt_at = sqlc.arg(next_attempt_at)
 WHERE org_id = sqlc.arg(org_id)
   AND id = sqlc.arg(delivery_id)
   AND status IN ('queued', 'retrying')
RETURNING *;

-- name: MarkWaitpointDeliveryRetrying :one
UPDATE waitpoint_deliveries
   SET status = 'retrying',
       last_error = sqlc.arg(last_error),
       next_attempt_at = sqlc.arg(next_attempt_at),
       sending_started_at = NULL
 WHERE org_id = sqlc.arg(org_id)
   AND id = sqlc.arg(delivery_id)
   AND status = 'sending'
   AND attempt_count = sqlc.arg(attempt_count)
   AND sending_started_at = sqlc.arg(sending_started_at)
RETURNING *;

-- name: MarkWaitpointDeliveryFailed :one
UPDATE waitpoint_deliveries
   SET status = 'failed',
       last_error = sqlc.arg(last_error),
       sending_started_at = NULL
 WHERE org_id = sqlc.arg(org_id)
   AND id = sqlc.arg(delivery_id)
   AND status = 'sending'
   AND attempt_count = sqlc.arg(attempt_count)
   AND sending_started_at = sqlc.arg(sending_started_at)
RETURNING *;

-- name: RequeueStaleSendingWaitpointDeliveries :exec
UPDATE waitpoint_deliveries
   SET status = CASE
           WHEN attempt_count >= sqlc.arg(max_attempts)::int THEN 'failed'::waitpoint_delivery_status
           ELSE 'retrying'::waitpoint_delivery_status
       END,
       last_error = CASE
           WHEN attempt_count >= sqlc.arg(max_attempts)::int THEN 'notification delivery attempts exhausted after stale send'
           ELSE 'notification worker stopped before completing delivery'
       END,
       next_attempt_at = now(),
       sending_started_at = NULL
 WHERE status = 'sending'
   AND sending_started_at < sqlc.arg(stale_before);

-- name: ListDueWaitpointDeliveries :many
SELECT *
  FROM waitpoint_deliveries
 WHERE status IN ('queued', 'retrying')
   AND next_attempt_at <= now()
 ORDER BY next_attempt_at ASC, created_at ASC
 LIMIT sqlc.arg(row_limit);

-- name: GetWaitpointForDelivery :one
SELECT waitpoints.*
  FROM waitpoints
  JOIN waitpoint_deliveries ON waitpoint_deliveries.org_id = waitpoints.org_id
                           AND waitpoint_deliveries.run_id = waitpoints.run_id
                           AND waitpoint_deliveries.waitpoint_id = waitpoints.id
 WHERE waitpoint_deliveries.org_id = sqlc.arg(org_id)
   AND waitpoint_deliveries.id = sqlc.arg(delivery_id);

-- name: ListWaitpointDeliveries :many
SELECT *
  FROM waitpoint_deliveries
 WHERE org_id = sqlc.arg(org_id)
   AND run_id = sqlc.arg(run_id)
   AND waitpoint_id = sqlc.arg(waitpoint_id)
 ORDER BY created_at ASC;
