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
existing_delivery AS (
    SELECT waitpoint_deliveries.*
      FROM waitpoint_deliveries
      JOIN target_waitpoint ON target_waitpoint.org_id = waitpoint_deliveries.org_id
                           AND target_waitpoint.run_id = waitpoint_deliveries.run_id
                           AND target_waitpoint.id = waitpoint_deliveries.waitpoint_id
     WHERE waitpoint_deliveries.channel = 'email'
       AND waitpoint_deliveries.recipient_kind = 'email'
       AND waitpoint_deliveries.recipient = sqlc.arg(recipient)
       AND waitpoint_deliveries.status <> 'failed'
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
        target_waitpoint.org_id,
        target_waitpoint.run_id,
        target_waitpoint.id,
        sqlc.arg(token_hash),
        sqlc.arg(allowed_actions)::text[],
        sqlc.arg(expires_at),
        sqlc.arg(recipient),
        sqlc.arg(token_metadata)::jsonb
      FROM target_waitpoint
     WHERE NOT EXISTS (SELECT 1 FROM existing_delivery)
    RETURNING *
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
    response_token.org_id,
    response_token.run_id,
    response_token.waitpoint_id,
    response_token.id,
    'email',
    'email',
    sqlc.arg(recipient),
    'queued',
    sqlc.arg(message_id),
    sqlc.arg(delivery_metadata)::jsonb
  FROM response_token
ON CONFLICT (org_id, run_id, waitpoint_id, channel, recipient_kind, recipient)
    WHERE channel = 'email' AND recipient_kind = 'email' AND status <> 'failed'
DO UPDATE SET metadata = waitpoint_deliveries.metadata || EXCLUDED.metadata
RETURNING *
)
SELECT * FROM new_delivery
UNION ALL
SELECT * FROM existing_delivery
 WHERE NOT EXISTS (SELECT 1 FROM new_delivery);

-- name: MarkWaitpointDeliverySent :one
UPDATE waitpoint_deliveries
   SET status = 'sent',
       last_error = NULL,
       next_attempt_at = now(),
       sending_started_at = NULL,
       sent_at = now()
 WHERE org_id = sqlc.arg(org_id)
   AND id = sqlc.arg(delivery_id)
   AND status = 'sending'
   AND attempt_count = sqlc.arg(attempt_count)
   AND sending_started_at = sqlc.arg(sending_started_at)
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

-- name: RequeueStaleSendingWaitpointDeliveries :many
UPDATE waitpoint_deliveries
   SET status = 'retrying',
       last_error = 'notification worker stopped before completing delivery',
       next_attempt_at = now(),
       sending_started_at = NULL
 WHERE status = 'sending'
   AND sending_started_at < sqlc.arg(stale_before)
RETURNING *;

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
