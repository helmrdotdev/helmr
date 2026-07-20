-- name: CreateOutboxMessage :one
INSERT INTO outbox_messages (
    id,
    lane,
    topic,
    partition_key,
    payload,
    available_at
)
VALUES (
    sqlc.arg(id),
    sqlc.arg(lane),
    sqlc.arg(topic),
    sqlc.arg(partition_key),
    sqlc.arg(payload),
    sqlc.arg(available_at)
)
RETURNING *;

-- name: CreateRunAdmissionOutbox :one
INSERT INTO outbox_messages (
    id,
    lane,
    topic,
    partition_key,
    payload,
    available_at
)
VALUES (
    sqlc.arg(id),
    'control',
    'run.admit',
    sqlc.arg(workspace_id)::uuid::text,
    jsonb_build_object(
        'environmentId', sqlc.arg(environment_id)::uuid::text,
        'runId', sqlc.arg(run_id)::uuid::text
    ),
    now()
)
RETURNING *;

-- name: ClaimOutboxMessages :many
WITH candidates AS (
    SELECT id
    FROM outbox_messages
    WHERE (
        (state = 'pending' AND available_at <= now())
        OR
        (state = 'claimed' AND claim_expires_at <= now())
      )
      AND outbox_messages.lane = sqlc.arg(lane)
      AND outbox_messages.topic = ANY(sqlc.arg(topics)::text[])
    ORDER BY available_at, id
    LIMIT sqlc.arg(row_limit)
    FOR UPDATE SKIP LOCKED
)
UPDATE outbox_messages
SET state = 'claimed',
    attempts = attempts + 1,
    claimed_by = sqlc.arg(claimed_by),
    claim_expires_at = sqlc.arg(claim_expires_at)
FROM candidates
WHERE outbox_messages.id = candidates.id
RETURNING outbox_messages.*;

-- name: DeliverOutboxMessage :one
UPDATE outbox_messages
SET state = 'delivered',
    claimed_by = NULL,
    claim_expires_at = NULL,
    last_error = NULL,
    delivered_at = now()
WHERE id = sqlc.arg(id)
  AND state = 'claimed'
  AND claimed_by = sqlc.arg(claimed_by)
  AND attempts = sqlc.arg(claim_attempt)
  AND claim_expires_at > now()
RETURNING *;

-- name: RetryOutboxMessage :one
UPDATE outbox_messages
SET state = 'pending',
    claimed_by = NULL,
    claim_expires_at = NULL,
    available_at = sqlc.arg(available_at),
    last_error = sqlc.arg(last_error)
WHERE id = sqlc.arg(id)
  AND state = 'claimed'
  AND claimed_by = sqlc.arg(claimed_by)
  AND attempts = sqlc.arg(claim_attempt)
  AND claim_expires_at > now()
RETURNING *;

-- name: DeadLetterOutboxMessage :one
UPDATE outbox_messages
SET state = 'dead_lettered',
    claimed_by = NULL,
    claim_expires_at = NULL,
    last_error = sqlc.arg(last_error)
WHERE id = sqlc.arg(id)
  AND state = 'claimed'
  AND claimed_by = sqlc.arg(claimed_by)
  AND attempts = sqlc.arg(claim_attempt)
  AND claim_expires_at > now()
RETURNING *;
